package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Failed invoice-import uploads: a durable record plus a time-limited copy
// (system_3 #10536, security ruling c65222).

func setupImportFailuresDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := setupPaymentsDB(t)
	db.AutoMigrate(&data_models.InvoicingImportFailure{})
	return db
}

func setupImportFailuresRouter(db *gorm.DB, userID uint) *gin.Engine {
	r := setupInvoicingRouter(db, userID)
	r.GET("/admin/invoicing/import-failures", ListImportFailuresHandler)
	r.GET("/admin/invoicing/import-failures/:id/download", DownloadImportFailureHandler)
	return r
}

func countImportFailures(t *testing.T, db *gorm.DB) int {
	t.Helper()
	var n int
	db.Model(&data_models.InvoicingImportFailure{}).Count(&n)
	return n
}

func lastImportFailure(t *testing.T, db *gorm.DB) data_models.InvoicingImportFailure {
	t.Helper()
	var row data_models.InvoicingImportFailure
	if err := db.Order("id desc").First(&row).Error; err != nil {
		t.Fatalf("reading last import failure: %v", err)
	}
	return row
}

// --- recordImportFailure / writeImportFailureCopy ---

func TestRecordImportFailureStoresTheFixedMessageNeverTheRawOne(t *testing.T) {
	db := setupImportFailuresDB(t)
	raw := `amount "1.234,56 EUR extra junk": strconv.ParseInt: parsing "junk": invalid syntax`
	recordImportFailure(db, 7, "n26", "statement.pdf", 42, []byte("x"), n26ErrParseFailed, "", 0)
	row := lastImportFailure(t, db)
	if row.ErrorMessage != n26FailureMessages[n26ErrParseFailed] {
		t.Fatalf("stored message = %q, want the fixed one", row.ErrorMessage)
	}
	if row.ErrorMessage == raw {
		t.Fatal("the raw parser text must never be stored")
	}
	if row.OwnerUserID != 7 || row.ImportKind != "n26" || row.Filename != "statement.pdf" || row.SizeBytes != 42 {
		t.Fatalf("row: %+v", row)
	}
}

func TestRecordImportFailureNoDataKeepsNoCopyAndMarksDeletedImmediately(t *testing.T) {
	db := setupImportFailuresDB(t)
	recordImportFailure(db, 1, "n26", "big.pdf", 999, nil, n26ErrTooLarge, "", 0)
	row := lastImportFailure(t, db)
	if row.CopyPath != "" {
		t.Fatalf("CopyPath = %q, want empty: no data means no copy", row.CopyPath)
	}
	if row.SHA256 != "" {
		t.Fatal("SHA256 must stay empty when no data was held")
	}
	if row.CopyDeletedAt == nil {
		t.Fatal("a row with no copy should read as already deleted, not pending")
	}
}

func TestRecordImportFailureWithDataWritesACopyNamedByServerNotFilename(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IMPORT_FAILURE_COPIES_DIR", dir)
	db := setupImportFailuresDB(t)
	recordImportFailure(db, 1, "paypal", "my secret export.csv", 5, []byte("hello"), "invalid_csv", "", 0)
	row := lastImportFailure(t, db)
	if row.CopyPath == "" {
		t.Fatal("expected a copy to be kept")
	}
	if row.CopyPath == "my secret export.csv" {
		t.Fatal("the copy's name must never be the uploaded filename")
	}
	if row.CopyDeletedAt != nil {
		t.Fatal("a freshly kept copy is not yet deleted")
	}
	data, err := os.ReadFile(filepath.Join(dir, row.CopyPath))
	if err != nil || string(data) != "hello" {
		t.Fatalf("copy contents: %q, %v", data, err)
	}
	info, err := os.Stat(filepath.Join(dir, row.CopyPath))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("copy mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestRecordImportFailureNoDirConfiguredKeepsNoCopy(t *testing.T) {
	t.Setenv("IMPORT_FAILURE_COPIES_DIR", "")
	db := setupImportFailuresDB(t)
	recordImportFailure(db, 1, "n26", "f.pdf", 5, []byte("hello"), n26ErrParseFailed, "", 0)
	row := lastImportFailure(t, db)
	if row.CopyPath != "" || row.CopyDeletedAt == nil {
		t.Fatalf("row: %+v", row)
	}
}

func TestWriteImportFailureCopyDropsOverTheDirectoryCap(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IMPORT_FAILURE_COPIES_DIR", dir)
	// Seed the directory past the cap directly, cheaper than writing 500MB
	// through the real path.
	if err := os.WriteFile(filepath.Join(dir, "seed"), make([]byte, importFailureDirMaxBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := writeImportFailureCopy([]byte("one more byte"))
	if err != nil {
		t.Fatalf("over-cap must not be an error: %v", err)
	}
	if path != "" {
		t.Fatal("expected no copy once the directory is at its cap")
	}
}

func TestImportFailureWindowRespectsTheEnvDownwardOnly(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", importFailureDefaultWindowHours * time.Hour},
		{"24", 24 * time.Hour},
		{"1", 1 * time.Hour},
		{"0", importFailureDefaultWindowHours * time.Hour},    // ignored: not > 0
		{"-5", importFailureDefaultWindowHours * time.Hour},   // ignored: not > 0
		{"1000", importFailureDefaultWindowHours * time.Hour}, // ignored: past the 7-day ceiling
		{"not-a-number", importFailureDefaultWindowHours * time.Hour},
	}
	for _, tc := range cases {
		t.Setenv("IMPORT_FAILURE_WINDOW_HOURS", tc.env)
		if got := importFailureWindow(); got != tc.want {
			t.Errorf("IMPORT_FAILURE_WINDOW_HOURS=%q: got %v, want %v", tc.env, got, tc.want)
		}
	}
}

// --- sweep ---

func TestSweepDeletesTrackedCopiesPastTheWindowAndStampsDeletedAt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IMPORT_FAILURE_COPIES_DIR", dir)
	t.Setenv("IMPORT_FAILURE_WINDOW_HOURS", "1")
	db := setupImportFailuresDB(t)

	old := data_models.InvoicingImportFailure{
		CreatedAt: time.Now().UTC().Add(-2 * time.Hour), ImportKind: "n26", Filename: "a.pdf",
		ErrorCode: n26ErrParseFailed, ErrorMessage: "x", CopyPath: "old-copy",
	}
	if err := db.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old-copy"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := data_models.InvoicingImportFailure{
		CreatedAt: time.Now().UTC(), ImportKind: "n26", Filename: "b.pdf",
		ErrorCode: n26ErrParseFailed, ErrorMessage: "x", CopyPath: "fresh-copy",
	}
	if err := db.Create(&fresh).Error; err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fresh-copy"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	sweepImportFailures(db)

	var gotOld, gotFresh data_models.InvoicingImportFailure
	db.First(&gotOld, old.ID)
	db.First(&gotFresh, fresh.ID)
	if gotOld.CopyDeletedAt == nil {
		t.Error("the old row should be stamped deleted")
	}
	if gotFresh.CopyDeletedAt != nil {
		t.Error("the fresh row should not be touched yet")
	}
	if _, err := os.Stat(filepath.Join(dir, "old-copy")); !os.IsNotExist(err) {
		t.Error("the old copy's file should be gone")
	}
	if _, err := os.Stat(filepath.Join(dir, "fresh-copy")); err != nil {
		t.Error("the fresh copy's file should still be there")
	}
}

func TestSweepDeletesOrphanFilesByMtimeEvenWithNoRow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IMPORT_FAILURE_COPIES_DIR", dir)
	t.Setenv("IMPORT_FAILURE_WINDOW_HOURS", "1")
	db := setupImportFailuresDB(t)

	orphan := filepath.Join(dir, "orphan")
	if err := os.WriteFile(orphan, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}

	sweepImportFailures(db)

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("an orphan file past the window must be deleted even with no DB row pointing at it")
	}
}

func TestSweepPrunesRowsOlderThan90DaysEntirely(t *testing.T) {
	db := setupImportFailuresDB(t)
	old := data_models.InvoicingImportFailure{
		CreatedAt: time.Now().UTC().Add(-91 * 24 * time.Hour), ImportKind: "n26", Filename: "a.pdf",
		ErrorCode: n26ErrParseFailed, ErrorMessage: "x",
	}
	db.Create(&old)
	fresh := data_models.InvoicingImportFailure{
		CreatedAt: time.Now().UTC().Add(-10 * 24 * time.Hour), ImportKind: "n26", Filename: "b.pdf",
		ErrorCode: n26ErrParseFailed, ErrorMessage: "x",
	}
	db.Create(&fresh)

	sweepImportFailures(db)

	if countImportFailures(t, db) != 1 {
		t.Fatalf("expected only the fresh row to survive, got %d rows", countImportFailures(t, db))
	}
	var remaining data_models.InvoicingImportFailure
	db.First(&remaining)
	if remaining.ID != fresh.ID {
		t.Fatalf("the wrong row survived: %+v", remaining)
	}
}

// --- N26 handler hook ---

func TestN26ImportTooLargeIsRecordedWithNoCopy(t *testing.T) {
	db := setupImportFailuresDB(t)
	r := setupN26Router(db, 7)
	big := make([]byte, maxN26PDFBytes+1)
	w := uploadN26(t, r, map[string][]byte{"big.pdf": big})
	if w.Code != http.StatusOK {
		t.Fatalf("parse: %d %s", w.Code, w.Body.String())
	}
	if countImportFailures(t, db) != 1 {
		t.Fatalf("expected 1 import failure row, got %d", countImportFailures(t, db))
	}
	row := lastImportFailure(t, db)
	if row.ErrorCode != n26ErrTooLarge || row.CopyPath != "" {
		t.Fatalf("row: %+v", row)
	}
}

func TestN26ImportNotAPDFIsRecordedWithACopyWhenConfigured(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IMPORT_FAILURE_COPIES_DIR", dir)
	db := setupImportFailuresDB(t)
	r := setupN26Router(db, 7)
	// No comma: a comma would parse as a two-field CSV record and this
	// would be classified as an unrecognised CSV instead (#10543's
	// looksLikeN26CSV checks exactly that shape).
	w := uploadN26(t, r, map[string][]byte{"not-a-pdf.pdf": []byte("just plain text that is neither a PDF nor a CSV export")})
	if w.Code != http.StatusOK {
		t.Fatalf("parse: %d %s", w.Code, w.Body.String())
	}
	resp := decodeN26Parse(t, w)
	if resp.Files[0].Error == nil || resp.Files[0].Error.Code != n26ErrNotAPDF {
		t.Fatalf("file response unaffected by recording: %+v", resp.Files[0])
	}
	row := lastImportFailure(t, db)
	if row.ErrorCode != n26ErrNotAPDF || row.CopyPath == "" {
		t.Fatalf("row: %+v", row)
	}
}

func TestN26ImportZeroCandidatesIsRecordedAsNoCandidates(t *testing.T) {
	requirePdftotext(t)
	db := setupImportFailuresDB(t)
	r := setupN26Router(db, 7)
	pdf := buildN26LookalikePDF(t)
	w := uploadN26(t, r, map[string][]byte{"conferma.pdf": pdf})
	if w.Code != http.StatusOK {
		t.Fatalf("parse: %d %s", w.Code, w.Body.String())
	}
	resp := decodeN26Parse(t, w)
	if resp.Files[0].Error != nil {
		t.Fatalf("a 0-candidate file is not a per-file error to the uploader: %+v", resp.Files[0])
	}
	// The real fixture has one candidate, so no failure should be recorded
	// for it -- this pins the negative case before the positive one below.
	if countImportFailures(t, db) != 0 {
		t.Fatalf("expected no import failure for a real 1-candidate statement, got %d", countImportFailures(t, db))
	}
}

// --- PayPal handler hook ---

func TestPayPalImportTooLargeIsRecorded(t *testing.T) {
	db := setupImportFailuresDB(t)
	r := setupPaymentsRouter(db, 7)
	big := make([]byte, maxPayPalImportBytes+1)
	w := uploadCSV(t, r, string(big), "")
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	row := lastImportFailure(t, db)
	if row.ErrorCode != "too_large" || row.ImportKind != "paypal" {
		t.Fatalf("row: %+v", row)
	}
}

func TestPayPalImportInvalidCSVIsRecordedWithTheFixedMessage(t *testing.T) {
	db := setupImportFailuresDB(t)
	r := setupPaymentsRouter(db, 7)
	w := uploadCSV(t, r, "not,a,recognised,header\n1,2,3,4", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	row := lastImportFailure(t, db)
	if row.ErrorCode != "invalid_csv" {
		t.Fatalf("row: %+v", row)
	}
	if row.ErrorMessage != paypalFailureMessages["invalid_csv"] {
		t.Fatalf("message = %q, want the fixed one", row.ErrorMessage)
	}
}

func TestPayPalImportDateOrderAmbiguousIsNotRecorded(t *testing.T) {
	db := setupImportFailuresDB(t)
	r := setupPaymentsRouter(db, 7)
	// Two rows whose day and month are both <=12, so the order is
	// genuinely ambiguous without the date_order hint.
	csv := "Date,Time,TimeZone,Name,Type,Status,Currency,Gross,Fee,Net,From Email Address,To Email Address,Transaction ID,Reference Txn ID\n" +
		"07/08/2026,10:00:00,CET,Someone,General Payment,Completed,EUR,10.00,0.00,10.00,a@b.com,,TX1,"
	w := uploadCSV(t, r, csv, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "date_order_ambiguous" {
		t.Skipf("fixture did not trigger date_order_ambiguous (got %v); not this test's concern", errObj["code"])
	}
	if countImportFailures(t, db) != 0 {
		t.Fatal("date_order_ambiguous must not be recorded: it resolves on an immediate retry of the same file")
	}
}

// --- admin list/download ---

func TestListImportFailuresReturnsNewestFirst(t *testing.T) {
	db := setupImportFailuresDB(t)
	r := setupImportFailuresRouter(db, 1)
	recordImportFailure(db, 1, "n26", "a.pdf", 1, nil, n26ErrTooLarge, "", 0)
	recordImportFailure(db, 1, "n26", "b.pdf", 1, nil, n26ErrTooLarge, "", 0)

	w := invoicingDo(r, "GET", "/admin/invoicing/import-failures", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Data struct {
			Failures []data_models.InvoicingImportFailure `json:"failures"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	if len(body.Data.Failures) != 2 || body.Data.Failures[0].Filename != "b.pdf" {
		t.Fatalf("failures: %+v", body.Data.Failures)
	}
}

func TestDownloadImportFailureServesTheCopyAndAudits(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IMPORT_FAILURE_COPIES_DIR", dir)
	db := setupImportFailuresDB(t)
	r := setupImportFailuresRouter(db, 9)
	recordImportFailure(db, 1, "n26", "a.pdf", 5, []byte("hello"), n26ErrParseFailed, "", 0)
	row := lastImportFailure(t, db)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/admin/invoicing/import-failures/"+strconv.Itoa(int(row.ID))+"/download", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	if w.Body.String() != "hello" {
		t.Fatalf("body: %q", w.Body.String())
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing nosniff")
	}
	if w.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("content-type: %q", w.Header().Get("Content-Type"))
	}
	var events int
	db.Model(&data_models.InvoicingAuditEvent{}).Where("action = ? AND entity_type = ? AND entity_id = ?",
		invoicingActionDownload, invoicingEntityImportFailure, row.ID).Count(&events)
	if events != 1 {
		t.Fatalf("expected 1 audit event for the download, got %d", events)
	}
}

func TestDownloadImportFailureWithNoCopyIs404(t *testing.T) {
	db := setupImportFailuresDB(t)
	r := setupImportFailuresRouter(db, 9)
	recordImportFailure(db, 1, "n26", "a.pdf", 5, nil, n26ErrTooLarge, "", 0) // too-large keeps no copy
	row := lastImportFailure(t, db)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/admin/invoicing/import-failures/"+strconv.Itoa(int(row.ID))+"/download", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
}

// --- security review c65539 on #10536 ---

const n26CSVHeaderLine = `"Booking Date","Value Date","Partner Name","Partner Iban","Type","Payment Reference","Account Name","Amount (EUR)","Original Amount","Original Currency","Exchange Rate"`

// The 0-row rule, positive side: an export with only its header row parses
// cleanly and yields nothing, so it is recorded as no_candidates.
func TestN26CSVHeaderOnlyIsRecordedAsNoCandidates(t *testing.T) {
	db := setupImportFailuresDB(t)
	r := setupN26Router(db, 7)
	w := uploadN26(t, r, map[string][]byte{"empty.csv": []byte(n26CSVHeaderLine + "\n")})
	if w.Code != http.StatusOK {
		t.Fatalf("parse: %d %s", w.Code, w.Body.String())
	}
	resp := decodeN26Parse(t, w)
	if resp.Files[0].Error != nil {
		t.Fatalf("a header-only export is not a per-file error to the uploader: %+v", resp.Files[0].Error)
	}
	if countImportFailures(t, db) != 1 {
		t.Fatalf("expected 1 import failure, got %d", countImportFailures(t, db))
	}
	if row := lastImportFailure(t, db); row.ErrorCode != importFailureCodeNoCandidates {
		t.Fatalf("row: %+v", row)
	}
}

// F1: a statement with real outgoing rows and no incoming ones imported
// correctly. It is not a failure and its bank document is not kept.
func TestN26CSVOutgoingOnlyRecordsNothing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IMPORT_FAILURE_COPIES_DIR", dir)
	db := setupImportFailuresDB(t)
	db.AutoMigrate(&data_models.InvoicingExpense{}, &data_models.InvoicingExpenseCategory{})
	r := setupN26Router(db, 7)
	csv := n26CSVHeaderLine + "\n" +
		`"2026-10-05","2026-10-05","Stadtwerke Beispiel","DE00123456780000000003","Direct Debit","Abschlag Oktober","Hauptkonto","-45.10","","",""` + "\n"
	w := uploadN26(t, r, map[string][]byte{"outgoing.csv": []byte(csv)})
	if w.Code != http.StatusOK {
		t.Fatalf("parse: %d %s", w.Code, w.Body.String())
	}
	resp := decodeN26Parse(t, w)
	f := resp.Files[0]
	if f.Error != nil || len(f.Candidates) != 0 || len(f.ExpenseCandidates) == 0 {
		t.Fatalf("fixture must be outgoing-only and parse cleanly for this test to mean anything: %+v", f)
	}
	if n := countImportFailures(t, db); n != 0 {
		t.Fatalf("an outgoing-only statement imported fine; expected no failure record, got %d", n)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("no copy may be kept of a file that did not fail; found %d", len(entries))
	}
}

// F2: the audit row is a condition of the download, not a side effect.
func TestDownloadImportFailureRefusesWhenTheAuditWriteFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IMPORT_FAILURE_COPIES_DIR", dir)
	db := setupImportFailuresDB(t)
	r := setupImportFailuresRouter(db, 9)
	recordImportFailure(db, 1, "n26", "a.pdf", 5, []byte("hello"), n26ErrParseFailed, "", 0)
	row := lastImportFailure(t, db)
	db.DropTable(&data_models.InvoicingAuditEvent{})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/admin/invoicing/import-failures/"+strconv.Itoa(int(row.ID))+"/download", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: %d, want 500 when the audit cannot be written", w.Code)
	}
	if w.Body.String() == "hello" {
		t.Fatal("the file must not be served when its download could not be audited")
	}
}

// Non-blocking 2: a copy past its window is refused even before the sweep
// stamps it deleted.
func TestDownloadImportFailureRefusesACopyPastItsWindow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IMPORT_FAILURE_COPIES_DIR", dir)
	t.Setenv("IMPORT_FAILURE_WINDOW_HOURS", "1")
	db := setupImportFailuresDB(t)
	r := setupImportFailuresRouter(db, 9)
	if err := os.WriteFile(filepath.Join(dir, "late-copy"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	row := data_models.InvoicingImportFailure{
		CreatedAt: time.Now().UTC().Add(-2 * time.Hour), ImportKind: "n26", Filename: "a.pdf",
		ErrorCode: n26ErrParseFailed, ErrorMessage: "x", CopyPath: "late-copy",
	}
	db.Create(&row)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/admin/invoicing/import-failures/"+strconv.Itoa(int(row.ID))+"/download", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: %d, want 404 for a copy past its window", w.Code)
	}
}

// Non-blocking 3: the list says when each kept copy expires, and flags one
// past its window that the sweep has not reached yet.
func TestListImportFailuresCarriesExpiryAndOverdue(t *testing.T) {
	t.Setenv("IMPORT_FAILURE_WINDOW_HOURS", "1")
	db := setupImportFailuresDB(t)
	r := setupImportFailuresRouter(db, 1)
	db.Create(&data_models.InvoicingImportFailure{
		CreatedAt: time.Now().UTC().Add(-2 * time.Hour), ImportKind: "n26", Filename: "late.pdf",
		ErrorCode: n26ErrParseFailed, ErrorMessage: "x", CopyPath: "late-copy",
	})
	db.Create(&data_models.InvoicingImportFailure{
		CreatedAt: time.Now().UTC(), ImportKind: "n26", Filename: "fresh.pdf",
		ErrorCode: n26ErrParseFailed, ErrorMessage: "x", CopyPath: "fresh-copy",
	})

	w := invoicingDo(r, "GET", "/admin/invoicing/import-failures", "")
	var body struct {
		Data struct {
			Failures []struct {
				Filename      string     `json:"Filename"`
				CopyExpiresAt *time.Time `json:"copy_expires_at"`
				Overdue       bool       `json:"overdue"`
			} `json:"failures"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	got := map[string]bool{}
	for _, f := range body.Data.Failures {
		if f.CopyExpiresAt == nil {
			t.Fatalf("%s: a kept copy must carry copy_expires_at", f.Filename)
		}
		got[f.Filename] = f.Overdue
	}
	if !got["late.pdf"] || got["fresh.pdf"] {
		t.Fatalf("overdue flags: %+v", got)
	}
}

// Non-blocking 1: with no directory configured, the sweep cannot delete
// anything, so it must not claim it did.
func TestSweepWithNoDirLeavesRowsUnstamped(t *testing.T) {
	t.Setenv("IMPORT_FAILURE_COPIES_DIR", "")
	t.Setenv("IMPORT_FAILURE_WINDOW_HOURS", "1")
	db := setupImportFailuresDB(t)
	row := data_models.InvoicingImportFailure{
		CreatedAt: time.Now().UTC().Add(-2 * time.Hour), ImportKind: "n26", Filename: "a.pdf",
		ErrorCode: n26ErrParseFailed, ErrorMessage: "x", CopyPath: "still-on-host",
	}
	db.Create(&row)

	sweepImportFailures(db)

	var got data_models.InvoicingImportFailure
	db.First(&got, row.ID)
	if got.CopyDeletedAt != nil {
		t.Fatal("a row must not be stamped deleted by a pass that deleted nothing")
	}
}
