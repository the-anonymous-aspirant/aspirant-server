package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Failed invoice-import uploads: a durable record plus a time-limited copy
// of the file (system_3 #10536, operator ask c65185 on #9656; security
// ruling c65222). Shared by the N26 and PayPal import handlers.

const (
	invoicingEntityImportFailure = "import_failure"
	invoicingActionDownload      = "download"

	// importFailureMaxWindowHours is security's hard ceiling (c65222 §3): an
	// env value may shorten the window, never lengthen it.
	importFailureMaxWindowHours = 7 * 24
	// importFailureDefaultWindowHours is the communicator's proposed default
	// (c65189 on #10536), which the operator's silence on #9656 confirmed.
	importFailureDefaultWindowHours = 7 * 24
	// importFailureRowMaxAgeHours is how long a failure row survives after
	// its copy (if any) is gone -- the database's own backups keep a dump
	// for up to 30 days, so the filename a user chose should not outlive
	// that by much (security ruling c65222 §4).
	importFailureRowMaxAgeHours = 90 * 24
	// importFailureDirMaxBytes bounds the copies directory as a whole, so a
	// burst of large failures cannot fill the disk between sweeps.
	importFailureDirMaxBytes = 500 << 20
)

// importFailureDir is the dedicated bind mount security required (c65222
// §1): never the database, never an existing upload or asset directory.
// Unset disables keeping copies -- rows are still written, CopyPath stays
// empty -- the same convention as the monitor's control token.
func importFailureDir() string {
	return os.Getenv("IMPORT_FAILURE_COPIES_DIR")
}

// importFailureWindow is the retention window, env-configurable downward
// only from the 7-day ceiling (security ruling c65222 §3).
func importFailureWindow() time.Duration {
	hours := importFailureDefaultWindowHours
	if raw := os.Getenv("IMPORT_FAILURE_WINDOW_HOURS"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= importFailureMaxWindowHours {
			hours = n
		} else {
			log.Printf("import failure: ignoring IMPORT_FAILURE_WINDOW_HOURS=%q (must be 1-%d)", raw, importFailureMaxWindowHours)
		}
	}
	return time.Duration(hours) * time.Hour
}

// fixed, per-code messages -- never the parser's own error text, which can
// quote document values (security ruling c65222 §4).
var n26FailureMessages = map[string]string{
	n26ErrTooLarge:                "the file is larger than 5 MB",
	n26ErrNotAPDF:                 "the file is neither a PDF nor an N26 CSV export",
	n26ErrExtractFailed:           "could not read the file",
	n26ErrNotN26Document:          "not an N26 transfer confirmation or account statement",
	n26ErrCSVHeadersUnrecognised:  "the CSV's column headers were not recognised",
	n26ErrParseFailed:             "the file was recognised but a field was missing or malformed",
	importFailureCodeNoCandidates: "the file was recognised but contained no usable rows",
}

var paypalFailureMessages = map[string]string{
	"too_large":                   "the file is larger than 5 MB",
	"read_failed":                 "could not read the uploaded file",
	"xls_unsupported":             "this is an Excel 97-2003 (.xls) file; save it as .xlsx or CSV",
	"invalid_csv":                 "the file was recognised as CSV but could not be parsed",
	"invalid_xlsx":                "the file was recognised as Excel but could not be parsed",
	importFailureCodeNoCandidates: "the file was recognised but contained no rows",
}

// importFailureCodeNoCandidates is shared across both imports: a file that
// parsed cleanly but produced nothing usable is still a failure the
// operator needs to see (security ruling c65222, the task's own ask).
const importFailureCodeNoCandidates = "no_candidates"

// recordImportFailure writes one durable row and, when the file is within
// the import's own size cap and the copies directory has room, a copy of
// it. Best-effort: a failure here is logged, never returned to the
// caller -- the import's own response to the uploader is unaffected.
// size is the file's own claimed size (the multipart header's, or the
// parsed response's content-length), always accurate. data is the full
// bytes when this call holds them, used only for the SHA-256 and the kept
// copy; nil when the file was refused before being fully read (for example
// "too large"), in which case no copy is kept and SHA256 stays empty --
// consistent with security's own cap, since such a file was never going to
// get a copy anyway.
func recordImportFailure(db *gorm.DB, ownerID uint, importKind, filename string, size int64, data []byte, code string, documentKind string, candidateCount int) {
	messages := n26FailureMessages
	if importKind == "paypal" {
		messages = paypalFailureMessages
	}
	message, ok := messages[code]
	if !ok {
		log.Printf("import failure: no fixed message registered for %s code %q; recording without one", importKind, code)
		message = "import failed"
	}

	row := data_models.InvoicingImportFailure{
		OwnerUserID:    ownerID,
		CreatedAt:      time.Now().UTC(),
		ImportKind:     importKind,
		Filename:       filename,
		SizeBytes:      size,
		ErrorCode:      code,
		ErrorMessage:   message,
		DocumentKind:   documentKind,
		CandidateCount: candidateCount,
	}

	if len(data) > 0 {
		sum := sha256.Sum256(data)
		row.SHA256 = hex.EncodeToString(sum[:])
		if path, err := writeImportFailureCopy(data); err != nil {
			log.Printf("import failure: not keeping a copy of %q: %v", filename, err)
		} else if path != "" {
			row.CopyPath = path
		}
	}
	if row.CopyPath == "" {
		now := time.Now().UTC()
		row.CopyDeletedAt = &now
	}

	if err := db.Create(&row).Error; err != nil {
		log.Printf("import failure: could not store the record for %q: %v", filename, err)
	}
}

// writeImportFailureCopy stores data under the copies directory with a
// server-generated name (security ruling c65222 §1: never the uploaded
// filename), mode 0600 in a 0700 directory. Returns "", nil when copies are
// disabled (no directory configured) or the directory is at its cap --
// both are not-an-error conditions the caller logs and moves on from.
func writeImportFailureCopy(data []byte) (string, error) {
	dir := importFailureDir()
	if dir == "" {
		return "", nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	used, _, err := dirSize(dir)
	if err != nil {
		return "", fmt.Errorf("reading %s's size: %w", dir, err)
	}
	if used+int64(len(data)) > importFailureDirMaxBytes {
		log.Printf("import failure: copies directory at its %d-byte cap (currently %d); dropping this copy", importFailureDirMaxBytes, used)
		return "", nil
	}
	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:]) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	return name, nil
}

// ListImportFailuresHandler returns the most recent failures, newest first
// (Admin-tier: security ruling c65222 §2).
func ListImportFailuresHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	const limit = 200
	var rows []data_models.InvoicingImportFailure
	if err := db.Order("created_at desc, id desc").Limit(limit).Find(&rows).Error; err != nil {
		log.Printf("import failure: listing: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error reading import failures")
		return
	}
	RespondWithSuccess(c, gin.H{"failures": rows}, "Recent import failures")
}

// DownloadImportFailureHandler serves a kept copy as an opaque attachment
// (Admin-tier; security ruling c65222 §2: never inline, since a failed
// upload is by definition not a valid PDF or CSV and could be HTML). Every
// download is audited.
func DownloadImportFailureHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		RespondWithError(c, http.StatusBadRequest, "Invalid id")
		return
	}
	var row data_models.InvoicingImportFailure
	if err := db.First(&row, uint(id)).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			RespondWithError(c, http.StatusNotFound, "No import failure with that id")
			return
		}
		log.Printf("import failure: reading %d: %v", id, err)
		RespondWithError(c, http.StatusInternalServerError, "Error reading import failures")
		return
	}
	if row.CopyPath == "" || row.CopyDeletedAt != nil {
		RespondWithError(c, http.StatusNotFound, "No copy is kept for this failure")
		return
	}
	dir := importFailureDir()
	if dir == "" {
		RespondWithError(c, http.StatusNotFound, "No copy is kept for this failure")
		return
	}
	data, err := os.ReadFile(filepath.Join(dir, row.CopyPath))
	if err != nil {
		log.Printf("import failure: reading copy of %d: %v", id, err)
		RespondWithError(c, http.StatusNotFound, "No copy is kept for this failure")
		return
	}

	adminID := invoicingUserID(c)
	if err := data_models.RecordInvoicingAuditEvent(db, adminID, invoicingActionDownload, invoicingEntityImportFailure, row.ID,
		auditDetails(gin.H{"owner_user_id": row.OwnerUserID, "filename": row.Filename})); err != nil {
		log.Printf("import failure: auditing download of %d: %v", id, err)
	}

	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fmt.Sprintf("import-failure-%d.bin", row.ID)))
	c.Data(http.StatusOK, "application/octet-stream", data)
}
