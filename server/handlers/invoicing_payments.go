package handlers

import (
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
// categorisation. Behind the flowah+Admin gate on the /invoicing group.

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
}

// ImportPayPalPaymentsHandler takes a PayPal activity CSV (multipart field
// "file", optional form value date_order=dmy|mdy) and stores its incoming
// payments and withdrawals. Re-uploading the same file imports nothing new.
func ImportPayPalPaymentsHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	fh, err := c.FormFile("file")
	if err != nil {
		RespondWithError(c, http.StatusBadRequest, "Upload a PayPal CSV in the form field \"file\"")
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

	parsed, err := ParsePayPalActivityCSV(data, c.PostForm("date_order"))
	if err != nil {
		code := "invalid_csv"
		if errors.Is(err, ErrPayPalDateOrderAmbiguous) {
			code = "date_order_ambiguous"
		}
		respondError(c, http.StatusBadRequest, code, err.Error())
		return
	}

	userID := invoicingUserID(c)
	summary := invoicingImportSummary{RowsRead: parsed.RowsRead, SkippedReasons: parsed.Skipped}
	for _, n := range parsed.Skipped {
		summary.Skipped += n
	}

	tx := db.Begin()
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
			Category:       data_models.PaymentCategoryUncategorised,
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

// PatchInvoicingPaymentHandler sets a payment's category, client link and/or
// bank-deposit grouping. Fields left out of the body are unchanged; null
// clears a link.
func PatchInvoicingPaymentHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		RespondWithError(c, http.StatusBadRequest, "Invalid payment id")
		return
	}
	var body map[string]json.RawMessage
	if err := c.ShouldBindJSON(&body); err != nil || len(body) == 0 {
		RespondWithError(c, http.StatusBadRequest, "Send at least one of category, client_id, bank_deposit_id")
		return
	}
	for k := range body {
		if k != "category" && k != "client_id" && k != "bank_deposit_id" {
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
	if raw, ok := body["bank_deposit_id"]; ok {
		did, err := optionalID(raw)
		if err != nil {
			refuse("bank_deposit_id " + err.Error())
			return
		}
		if did != nil {
			if p.Kind != data_models.PaymentKindIncoming {
				refuse("Only incoming payments are grouped under a bank deposit")
				return
			}
			dep, err := data_models.GetInvoicingPayment(tx, *did)
			if err != nil {
				tx.Rollback()
				log.Printf("Error reading deposit %d: %v", *did, err)
				RespondWithError(c, http.StatusInternalServerError, "Error updating payment")
				return
			}
			if dep == nil || dep.Kind != data_models.PaymentKindWithdrawal {
				refuse("bank_deposit_id must name a bank withdrawal row")
				return
			}
		}
		p.BankDepositID = did
	}

	if err := tx.Save(p).Error; err != nil {
		tx.Rollback()
		log.Printf("Error saving payment %d: %v", id, err)
		RespondWithError(c, http.StatusInternalServerError, "Error updating payment")
		return
	}
	details := auditDetails(gin.H{
		"before": gin.H{"category": before.Category, "client_id": before.ClientID, "bank_deposit_id": before.BankDepositID},
		"after":  gin.H{"category": p.Category, "client_id": p.ClientID, "bank_deposit_id": p.BankDepositID},
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
