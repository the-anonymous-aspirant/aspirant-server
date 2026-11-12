package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// One receipt attached to one work expense (system_3 #10537-A1, operator
// c65188: "attach to the expense in the book if its smal[l]"). The same
// shape as the issuer's logo (invoicing_logo.go / InvoicingProfileLogo) --
// a DB blob, sha256 recorded, audit-logged, owner-scoped -- with one row per
// expense instead of one row per owner, and the bytes kept as uploaded
// rather than normalized (a receipt may be a PDF or any photo of a paper
// receipt).

const (
	invoicingEntityExpenseReceipt = "expense_receipt"
	maxReceiptBytes               = 5 << 20 // 5 MiB, the same cap as the logo and a PayPal import

	// receiptFormSlack is the multipart framing allowed around one file at
	// the cap (mirrors cardAssetFormSlack in cardgen_assets.go).
	receiptFormSlack = 64 << 10
)

// allowedReceiptTypes is checked against the server-sniffed type of the
// uploaded bytes, never the client's Content-Type header (same discipline
// as allowedAvatarTypes in profile.go). The map value is the extension used
// to build the server-side download filename (security review of #10602,
// c65885 point 4): the client's filename is stored for display only and
// never placed in a header.
var allowedReceiptTypes = map[string]string{
	"image/jpeg":      "jpg",
	"image/png":       "png",
	"application/pdf": "pdf",
}

// receiptDownloadFilename builds the Content-Disposition filename from the
// server-sniffed type and the expense id alone -- never from the client's
// uploaded filename (c65885 point 4: a header must not carry user content).
func receiptDownloadFilename(expenseID uint, contentType string) string {
	ext := allowedReceiptTypes[contentType]
	if ext == "" {
		ext = "bin"
	}
	return fmt.Sprintf("receipt-%d.%s", expenseID, ext)
}

func expenseIDParam(c *gin.Context) (uint, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		RespondWithError(c, http.StatusBadRequest, "Invalid expense id")
		return 0, false
	}
	return uint(id), true
}

// ownedExpenseOrNotFound reads the expense for the caller, writing the 404
// itself (and rolling tx back) when it is not the caller's. Returns nil on
// either error or not-found; the caller returns immediately in both cases.
func ownedExpenseOrNotFound(c *gin.Context, tx *gorm.DB, owner, id uint, what string) *data_models.InvoicingExpense {
	e, err := data_models.GetInvoicingExpense(tx, owner, id)
	if err != nil {
		tx.Rollback()
		log.Printf("Error reading expense %d for %s: %v", id, what, err)
		RespondWithError(c, http.StatusInternalServerError, "Error "+what)
		return nil
	}
	if e == nil {
		tx.Rollback()
		RespondWithError(c, http.StatusNotFound, "Expense not found")
		return nil
	}
	return e
}

// PutInvoicingExpenseReceiptHandler replaces the expense's receipt with the
// multipart field "receipt". PUT /invoicing/expenses/:id/receipt.
func PutInvoicingExpenseReceiptHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	id, ok := expenseIDParam(c)
	if !ok {
		return
	}
	userID := invoicingUserID(c)

	// Bound the request before the multipart parser sees it: Gin spools a
	// form past 32 MiB to temp files before any handler check runs, so the
	// cap must sit on the body itself (same discipline as UploadCardAssetHandler,
	// security review #8634 c53354 item 3; re-raised on #10602, c65885 point 3).
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxReceiptBytes+receiptFormSlack)

	file, err := c.FormFile("receipt")
	if err != nil {
		if strings.Contains(err.Error(), "request body too large") {
			RespondWithError(c, http.StatusBadRequest, fmt.Sprintf("the receipt is larger than %d MB", maxReceiptBytes>>20))
			return
		}
		RespondWithError(c, http.StatusBadRequest, "No receipt file received")
		return
	}
	if file.Size > maxReceiptBytes {
		RespondWithError(c, http.StatusBadRequest, fmt.Sprintf("the receipt is larger than %d MB", maxReceiptBytes>>20))
		return
	}
	f, err := file.Open()
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to read the receipt")
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxReceiptBytes+1))
	if err != nil || int64(len(raw)) > maxReceiptBytes {
		RespondWithError(c, http.StatusBadRequest, fmt.Sprintf("the receipt is larger than %d MB", maxReceiptBytes>>20))
		return
	}
	if len(raw) == 0 {
		RespondWithError(c, http.StatusBadRequest, "Receipt is empty")
		return
	}
	// The type is decided here, from the bytes, never from the client's
	// declared Content-Type (c65885 point 2); that sniffed type is what gets
	// stored and served, and what builds the download filename below.
	contentType := http.DetectContentType(raw)
	if allowedReceiptTypes[contentType] == "" {
		RespondWithError(c, http.StatusBadRequest, "Unsupported file type (allowed: PDF, JPEG, PNG)")
		return
	}
	// filename is kept for display only (the JSON response below) and never
	// placed in a header -- c65885 point 4 builds the download filename from
	// the expense id and sniffed type instead.
	filename := file.Filename
	if !validOneLineName(filename) {
		filename = "receipt"
	}

	tx := db.Begin()
	e := ownedExpenseOrNotFound(c, tx, userID, id, "saving the receipt")
	if e == nil {
		return
	}
	before, err := data_models.GetInvoicingExpenseReceipt(tx, userID, id)
	if err != nil {
		respondInvoicingInternal(c, tx, "saving the receipt", err)
		return
	}
	sum := sha256.Sum256(raw)
	stored := data_models.InvoicingExpenseReceipt{
		Content:     raw,
		ContentType: contentType,
		Filename:    filename,
		SHA256:      hex.EncodeToString(sum[:]),
		SizeBytes:   int64(len(raw)),
	}
	if err := data_models.SaveInvoicingExpenseReceipt(tx, &stored, userID, id); err != nil {
		respondInvoicingInternal(c, tx, "saving the receipt", err)
		return
	}
	details := gin.H{"after": gin.H{"sha256": stored.SHA256, "content_type": stored.ContentType, "bytes": stored.SizeBytes}}
	if before != nil {
		details["before"] = gin.H{"sha256": before.SHA256, "content_type": before.ContentType, "bytes": before.SizeBytes}
	}
	if err := data_models.RecordInvoicingAuditEvent(tx, userID, invoicingActionUpload, invoicingEntityExpenseReceipt, id, auditDetails(details)); err != nil {
		respondInvoicingInternal(c, tx, "saving the receipt", err)
		return
	}
	if err := tx.Commit().Error; err != nil {
		respondInvoicingInternal(c, nil, "saving the receipt", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"receipt": gin.H{
		"content_type": stored.ContentType,
		"filename":     stored.Filename,
		"sha256":       stored.SHA256,
		"size_bytes":   stored.SizeBytes,
	}, "saved": true})
}

// GetInvoicingExpenseReceiptHandler serves the stored receipt bytes.
// GET /invoicing/expenses/:id/receipt.
func GetInvoicingExpenseReceiptHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	id, ok := expenseIDParam(c)
	if !ok {
		return
	}
	userID := invoicingUserID(c)

	e, err := data_models.GetInvoicingExpense(db, userID, id)
	if err != nil {
		log.Printf("Error reading expense %d for receipt download: %v", id, err)
		RespondWithError(c, http.StatusInternalServerError, "Error loading the receipt")
		return
	}
	if e == nil {
		RespondWithError(c, http.StatusNotFound, "Expense not found")
		return
	}
	receipt, err := data_models.GetInvoicingExpenseReceipt(db, userID, id)
	if err != nil {
		log.Printf("Error loading receipt for expense %d: %v", id, err)
		RespondWithError(c, http.StatusInternalServerError, "Error loading the receipt")
		return
	}
	if receipt == nil {
		RespondWithError(c, http.StatusNotFound, "No receipt is attached")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("X-Content-SHA256", receipt.SHA256)
	// attachment, with a server-built filename: never the uploader's own
	// filename, which this header must not carry (c65885 point 4). The
	// client previews through a blob URL, not by pointing an iframe or a
	// direct navigation at this URL (memory slug
	// pdf-attachment-iframe-preview-needs-blob-url).
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", receiptDownloadFilename(id, receipt.ContentType)))
	c.Header("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	c.Data(http.StatusOK, receipt.ContentType, receipt.Content)
}

// DeleteInvoicingExpenseReceiptHandler removes the expense's receipt.
// DELETE /invoicing/expenses/:id/receipt.
func DeleteInvoicingExpenseReceiptHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	id, ok := expenseIDParam(c)
	if !ok {
		return
	}
	userID := invoicingUserID(c)

	tx := db.Begin()
	e := ownedExpenseOrNotFound(c, tx, userID, id, "removing the receipt")
	if e == nil {
		return
	}
	before, err := data_models.GetInvoicingExpenseReceipt(tx, userID, id)
	if err != nil {
		respondInvoicingInternal(c, tx, "removing the receipt", err)
		return
	}
	if before == nil {
		tx.Rollback()
		RespondWithError(c, http.StatusNotFound, "No receipt is attached")
		return
	}
	if _, err := data_models.DeleteInvoicingExpenseReceipt(tx, userID, id); err != nil {
		respondInvoicingInternal(c, tx, "removing the receipt", err)
		return
	}
	details := gin.H{"before": gin.H{"sha256": before.SHA256, "content_type": before.ContentType, "bytes": before.SizeBytes}}
	if err := data_models.RecordInvoicingAuditEvent(tx, userID, invoicingActionDelete, invoicingEntityExpenseReceipt, id, auditDetails(details)); err != nil {
		respondInvoicingInternal(c, tx, "removing the receipt", err)
		return
	}
	if err := tx.Commit().Error; err != nil {
		respondInvoicingInternal(c, nil, "removing the receipt", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}
