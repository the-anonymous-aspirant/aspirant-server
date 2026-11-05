package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Payments for the invoicing app (system_3 #8367-B1): PayPal import and
// categorisation. Behind the owner+Admin gate on the /invoicing group.

const (
	invoicingEntityPayment = "payment"
	invoicingEntityImport  = "import"

	invoicingActionImport     = "import"
	invoicingActionCategorise = "categorise"

	invoicingPaymentSourcePayPal = "paypal"

	// maxPayPalImportBytes bounds an upload. A year of a freelancer's PayPal
	// activity is a few hundred KB; 5 MB leaves room without letting a
	// mistaken upload pin the server's memory.
	maxPayPalImportBytes = 5 << 20
)

type invoicingImportSummary struct {
	RowsRead       int            `json:"rows_read"`
	Imported       int            `json:"imported"`
	Duplicates     int            `json:"duplicates"`
	Skipped        int            `json:"skipped"`
	SkippedReasons map[string]int `json:"skipped_reasons"`
	Withdrawals    int            `json:"withdrawals"`
	// Refunds counts imported payments pre-set to the refund category.
	Refunds int `json:"refunds"`
	// Format is the file format read: csv or xlsx.
	Format string `json:"format"`
}

// File signatures the import sniffs: an .xlsx is a zip; a legacy .xls is an
// OLE2 compound file, which the import does not read.
var (
	zipSignature  = []byte("PK\x03\x04")
	ole2Signature = []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}
)

// ImportPayPalPaymentsHandler takes a PayPal activity CSV or Excel (.xlsx)
// file (multipart field "file", optional form value date_order=dmy|mdy) and
// stores its incoming payments and withdrawals. The format is read from the
// bytes, not the filename. Re-uploading the same file imports nothing new.
func ImportPayPalPaymentsHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	fh, err := c.FormFile("file")
	if err != nil {
		RespondWithError(c, http.StatusBadRequest, "Upload a PayPal CSV or Excel file in the form field \"file\"")
		return
	}
	if fh.Size > maxPayPalImportBytes {
		RespondWithError(c, http.StatusRequestEntityTooLarge, "The file is larger than 5 MB")
		return
	}
	f, err := fh.Open()
	if err != nil {
		RespondWithError(c, http.StatusBadRequest, "Could not read the uploaded file")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxPayPalImportBytes+1))
	if err != nil || len(data) > maxPayPalImportBytes {
		RespondWithError(c, http.StatusRequestEntityTooLarge, "The file is larger than 5 MB")
		return
	}

	if bytes.HasPrefix(data, ole2Signature) {
		respondError(c, http.StatusBadRequest, "xls_unsupported",
			"This is an Excel 97-2003 (.xls) file. Save it as .xlsx or CSV and upload that.")
		return
	}
	format, code := "csv", "invalid_csv"
	parse := ParsePayPalActivityCSV
	if bytes.HasPrefix(data, zipSignature) {
		format, code, parse = "xlsx", "invalid_xlsx", ParsePayPalActivityXLSX
	}
	parsed, err := parse(data, c.PostForm("date_order"))
	if err != nil {
		if errors.Is(err, ErrPayPalDateOrderAmbiguous) {
			code = "date_order_ambiguous"
		}
		respondError(c, http.StatusBadRequest, code, err.Error())
		return
	}

	userID := invoicingUserID(c)
	summary := invoicingImportSummary{RowsRead: parsed.RowsRead, SkippedReasons: parsed.Skipped, Format: format}
	for _, n := range parsed.Skipped {
		summary.Skipped += n
	}

	tx := db.Begin()
	// The import's duplicate checks against hand-entered PayPal payments run
	// under the issuing lock, like a hand entry's (#8409 security F2).
	if _, err := lockIssuing(tx); err != nil {
		tx.Rollback()
		log.Printf("Error locking for a payment import: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error importing payments")
		return
	}
	seen := map[string]bool{}
	for _, row := range parsed.Rows {
		if seen[row.TransactionID] {
			summary.Duplicates++
			continue
		}
		seen[row.TransactionID] = true
		exists, err := data_models.InvoicingPaymentExists(tx, invoicingPaymentSourcePayPal, row.TransactionID)
		if err != nil {
			tx.Rollback()
			log.Printf("Error checking payment %s: %v", row.TransactionID, err)
			RespondWithError(c, http.StatusInternalServerError, "Error importing payments")
			return
		}
		if !exists {
			// The same PayPal payment may already have been entered by hand
			// with its transaction id as the reference (#8409).
			exists, err = data_models.ManualPayPalPaymentWithReference(tx, row.TransactionID)
			if err != nil {
				tx.Rollback()
				log.Printf("Error checking manual payments for %s: %v", row.TransactionID, err)
				RespondWithError(c, http.StatusInternalServerError, "Error importing payments")
				return
			}
		}
		if exists {
			summary.Duplicates++
			continue
		}
		p := data_models.InvoicingPayment{
			Source:         invoicingPaymentSourcePayPal,
			TransactionID:  row.TransactionID,
			ReferenceTxnID: row.ReferenceTxnID,
			Kind:           row.Kind,
			PaidOn:         row.PaidOn,
			Currency:       row.Currency,
			GrossCents:     row.GrossCents,
			FeeCents:       row.FeeCents,
			NetCents:       row.NetCents,
			SenderName:     row.SenderName,
			SenderEmail:    row.SenderEmail,
			Note:           row.Note,
			Category:       row.Category,
		}
		if row.Kind == data_models.PaymentKindIncoming {
			cl, err := data_models.FindInvoicingClientByPayPalEmail(tx, row.SenderEmail)
			if err != nil {
				tx.Rollback()
				log.Printf("Error matching client for payment %s: %v", row.TransactionID, err)
				RespondWithError(c, http.StatusInternalServerError, "Error importing payments")
				return
			}
			if cl != nil {
				id := cl.ID
				p.ClientID = &id
			}
		}
		if err := tx.Create(&p).Error; err != nil {
			tx.Rollback()
			log.Printf("Error storing payment %s: %v", row.TransactionID, err)
			RespondWithError(c, http.StatusInternalServerError, "Error importing payments")
			return
		}
		summary.Imported++
		if row.Kind == data_models.PaymentKindWithdrawal {
			summary.Withdrawals++
		}
		if row.Category == data_models.PaymentCategoryRefund {
			summary.Refunds++
		}
	}
	details := auditDetails(gin.H{"filename": fh.Filename, "summary": summary})
	if err := data_models.RecordInvoicingAuditEvent(tx, userID, invoicingActionImport, invoicingEntityImport, 0, details); err != nil {
		tx.Rollback()
		log.Printf("Error auditing payment import: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error importing payments")
		return
	}
	if err := tx.Commit().Error; err != nil {
		log.Printf("Error committing payment import: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error importing payments")
		return
	}
	c.JSON(http.StatusOK, gin.H{"summary": summary})
}

// ListInvoicingPaymentsHandler lists payments, filtered by ?kind= and ?category=.
func ListInvoicingPaymentsHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	kind, category := c.Query("kind"), c.Query("category")
	if kind != "" && kind != data_models.PaymentKindIncoming && kind != data_models.PaymentKindWithdrawal {
		RespondWithError(c, http.StatusBadRequest, "kind must be incoming or withdrawal")
		return
	}
	if category != "" && !data_models.IsPaymentCategory(category) {
		RespondWithError(c, http.StatusBadRequest, "Unknown category")
		return
	}
	payments, err := data_models.ListInvoicingPayments(db, kind, category)
	if err != nil {
		log.Printf("Error listing payments: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error listing payments")
		return
	}
	linked, err := data_models.PaymentIDsOnInvoices(db)
	if err != nil {
		log.Printf("Error listing invoice links: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error listing payments")
		return
	}
	for i := range payments {
		payments[i].OnInvoice = linked[payments[i].ID]
	}
	c.JSON(http.StatusOK, gin.H{"payments": payments, "categories": data_models.PaymentCategories})
}

// optionalID decodes a JSON id that may be absent, null (clear) or a number.
func optionalID(raw json.RawMessage) (*uint, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	var v uint
	if err := json.Unmarshal(raw, &v); err != nil || v == 0 {
		return nil, errors.New("must be a positive id or null")
	}
	return &v, nil
}

// PatchInvoicingPaymentHandler sets a payment's category and/or client link.
// Fields left out of the body are unchanged; null clears a link.
func PatchInvoicingPaymentHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		RespondWithError(c, http.StatusBadRequest, "Invalid payment id")
		return
	}
	var body map[string]json.RawMessage
	if err := c.ShouldBindJSON(&body); err != nil || len(body) == 0 {
		RespondWithError(c, http.StatusBadRequest, "Send at least one of category, client_id")
		return
	}
	for k := range body {
		if k != "category" && k != "client_id" {
			RespondWithError(c, http.StatusBadRequest, "Unknown field "+strconv.Quote(k))
			return
		}
	}

	tx := db.Begin()
	p, err := data_models.GetInvoicingPayment(tx, uint(id))
	if err != nil {
		tx.Rollback()
		log.Printf("Error reading payment %d: %v", id, err)
		RespondWithError(c, http.StatusInternalServerError, "Error updating payment")
		return
	}
	if p == nil {
		tx.Rollback()
		RespondWithError(c, http.StatusNotFound, "Payment not found")
		return
	}
	before := *p
	refuse := func(msg string) {
		tx.Rollback()
		RespondWithError(c, http.StatusBadRequest, msg)
	}

	if raw, ok := body["category"]; ok {
		var cat string
		if err := json.Unmarshal(raw, &cat); err != nil || !data_models.IsPaymentCategory(cat) {
			refuse("category must be one of uncategorised, paid_work, gift, loan, refund, other")
			return
		}
		if p.Kind != data_models.PaymentKindIncoming {
			refuse("Only incoming payments are categorised; this row is a bank withdrawal")
			return
		}
		// A transfer from PayPal into N26 is money the PayPal import
		// already holds, so it is never paid work (system_3 #9881). Any
		// other category stays open, so a row stored before this rule can
		// be moved off paid work.
		if cat == data_models.PaymentCategoryPaidWork && cat != p.Category && isPayPalTransferIntoN26(*p) {
			tx.Rollback()
			respondError(c, http.StatusUnprocessableEntity, "paypal_transfer_not_paid_work",
				"this is a transfer from PayPal into N26; the PayPal payments it sums are the paid work, so it cannot be paid work itself")
			return
		}
		if cat != p.Category {
			// An issued invoice is immutable (spec §10), and so is the
			// paid-work fact it rests on. Cancel the invoice first. The
			// issuing lock makes this check and an issue run one at a
			// time, so a payment cannot change category while an issue
			// that covers it is in flight.
			if _, err := lockIssuing(tx); err != nil {
				tx.Rollback()
				log.Printf("Error locking for payment %d: %v", id, err)
				RespondWithError(c, http.StatusInternalServerError, "Error updating payment")
				return
			}
			onInvoice, err := data_models.IssuedInvoiceForPayment(tx, p.ID, 0)
			if err != nil {
				tx.Rollback()
				log.Printf("Error checking invoices for payment %d: %v", id, err)
				RespondWithError(c, http.StatusInternalServerError, "Error updating payment")
				return
			}
			if onInvoice != 0 {
				tx.Rollback()
				respondError(c, http.StatusConflict, "payment_on_issued_invoice",
					fmt.Sprintf("payment is on issued invoice %d; cancel that invoice before changing its category", onInvoice))
				return
			}
		}
		p.Category = cat
	}
	if raw, ok := body["client_id"]; ok {
		cid, err := optionalID(raw)
		if err != nil {
			refuse("client_id " + err.Error())
			return
		}
		if cid != nil {
			cl, err := data_models.GetInvoicingClient(tx, *cid)
			if err != nil {
				tx.Rollback()
				log.Printf("Error reading client %d: %v", *cid, err)
				RespondWithError(c, http.StatusInternalServerError, "Error updating payment")
				return
			}
			if cl == nil {
				refuse("client_id does not name a client")
				return
			}
		}
		p.ClientID = cid
	}

	if err := tx.Save(p).Error; err != nil {
		tx.Rollback()
		log.Printf("Error saving payment %d: %v", id, err)
		RespondWithError(c, http.StatusInternalServerError, "Error updating payment")
		return
	}
	details := auditDetails(gin.H{
		"before": gin.H{"category": before.Category, "client_id": before.ClientID},
		"after":  gin.H{"category": p.Category, "client_id": p.ClientID},
	})
	if err := data_models.RecordInvoicingAuditEvent(tx, invoicingUserID(c), invoicingActionCategorise, invoicingEntityPayment, p.ID, details); err != nil {
		tx.Rollback()
		log.Printf("Error auditing payment %d: %v", id, err)
		RespondWithError(c, http.StatusInternalServerError, "Error updating payment")
		return
	}
	if err := tx.Commit().Error; err != nil {
		log.Printf("Error committing payment %d: %v", id, err)
		RespondWithError(c, http.StatusInternalServerError, "Error updating payment")
		return
	}
	c.JSON(http.StatusOK, gin.H{"payment": p})
}
