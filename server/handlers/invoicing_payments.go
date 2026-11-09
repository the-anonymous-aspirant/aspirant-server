package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
	if _, err := lockIssuing(tx, invoicingUserID(c)); err != nil {
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
		exists, err := data_models.InvoicingPaymentSeen(tx, invoicingUserID(c), invoicingPaymentSourcePayPal, row.TransactionID)
		if err != nil {
			tx.Rollback()
			log.Printf("Error checking payment %s: %v", row.TransactionID, err)
			RespondWithError(c, http.StatusInternalServerError, "Error importing payments")
			return
		}
		if !exists {
			// The same PayPal payment may already have been entered by hand
			// with its transaction id as the reference (#8409).
			exists, err = data_models.ManualPayPalPaymentWithReference(tx, invoicingUserID(c), row.TransactionID)
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
			OwnerUserID:    invoicingUserID(c),
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
			cl, err := data_models.FindInvoicingClientByPayPalEmail(tx, invoicingUserID(c), row.SenderEmail)
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
	// The file's outgoing rows are offered for review, never stored here:
	// the owner confirms the work expenses among them through
	// ImportPayPalExpensesConfirmHandler (system_3 #10414-B2).
	// The payments are committed by now, so a failure here must not read as
	// a failed import. The flags are advice for the review; the confirm
	// dedupes on its own. So the candidates go back unflagged instead.
	expenses := paypalExpenseCandidates(parsed.Expenses)
	if err := flagPayPalExpenseCandidates(db, userID, expenses); err != nil {
		log.Printf("Error flagging PayPal expense candidates, returning them unflagged: %v", err)
		expenses = paypalExpenseCandidates(parsed.Expenses)
	}
	c.JSON(http.StatusOK, gin.H{"summary": summary, "expense_candidates": expenses})
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
	payments, err := data_models.ListInvoicingPayments(db, invoicingUserID(c), kind, category)
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
	onIssued, err := data_models.PaymentIDsOnIssuedInvoices(db)
	if err != nil {
		log.Printf("Error listing issued-invoice links: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error listing payments")
		return
	}
	for i := range payments {
		payments[i].OnInvoice = linked[payments[i].ID]
		payments[i].OnIssuedInvoice = onIssued[payments[i].ID]
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
	p, err := data_models.GetInvoicingPayment(tx, invoicingUserID(c), uint(id))
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
			if _, err := lockIssuing(tx, invoicingUserID(c)); err != nil {
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
			cl, err := data_models.GetInvoicingClient(tx, invoicingUserID(c), *cid)
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

// paypalExpenseCandidate is one outgoing PayPal row offered as a work expense
// (system_3 #10414-B2). Its JSON field names match the N26 import's expense
// candidates, so the client reviews both sources the same way.
type paypalExpenseCandidate struct {
	TransactionID       string `json:"transaction_id"`
	PaidOn              string `json:"paid_on"`
	Currency            string `json:"currency"`
	AmountCents         int64  `json:"amount_cents"`
	PayeeName           string `json:"payee_name"`
	Note                string `json:"note"`
	AlreadyImported     bool   `json:"already_imported"`
	PossibleDuplicateOf uint   `json:"possible_duplicate_of,omitempty"`
}

func paypalExpenseCandidates(rows []paypalExpenseRow) []paypalExpenseCandidate {
	out := make([]paypalExpenseCandidate, 0, len(rows))
	for _, r := range rows {
		out = append(out, paypalExpenseCandidate{
			TransactionID: r.TransactionID,
			PaidOn:        r.PaidOn.Format(invoicingDateLayout),
			Currency:      r.Currency,
			AmountCents:   r.AmountCents,
			PayeeName:     r.PayeeName,
			Note:          r.Note,
		})
	}
	return out
}

// flagPayPalExpenseCandidates marks a candidate already stored as a PayPal
// expense under its transaction id, or colliding on date and amount with any
// of the owner's expenses (one she entered by hand, typically).
func flagPayPalExpenseCandidates(db *gorm.DB, owner uint, cands []paypalExpenseCandidate) error {
	for i := range cands {
		c := &cands[i]
		var n int
		if err := db.Model(&data_models.InvoicingExpense{}).
			Where("owner_user_id = ? AND source = ? AND transaction_id = ?", owner, invoicingPaymentSourcePayPal, c.TransactionID).
			Count(&n).Error; err != nil {
			return err
		}
		c.AlreadyImported = n > 0
		if c.AlreadyImported {
			continue
		}
		paidOn, err := time.Parse(invoicingDateLayout, c.PaidOn)
		if err != nil {
			return err
		}
		var twin data_models.InvoicingExpense
		err = db.Where("owner_user_id = ? AND paid_on = ? AND amount_cents = ?", owner, paidOn, c.AmountCents).
			Order("id asc").First(&twin).Error
		if err == gorm.ErrRecordNotFound {
			continue
		}
		if err != nil {
			return err
		}
		c.PossibleDuplicateOf = twin.ID
	}
	return nil
}

// maxPayPalExpenseConfirmRows bounds one confirm request, as the N26
// confirm's does: each row runs an existence query under the book lock.
const maxPayPalExpenseConfirmRows = 500

// paypalTransactionIDPattern is a PayPal transaction id as the import keeps
// it: PayPal's own code, never a derived one.
var paypalTransactionIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// paypalExpenseConfirmRow is one outgoing row the owner confirmed: the
// candidate's fields plus the category she picked (empty means "other").
type paypalExpenseConfirmRow struct {
	TransactionID string `json:"transaction_id"`
	PaidOn        string `json:"paid_on"`
	Currency      string `json:"currency"`
	AmountCents   int64  `json:"amount_cents"`
	PayeeName     string `json:"payee_name"`
	Note          string `json:"note"`
	Category      string `json:"category"`
}

type paypalExpenseConfirmSummary struct {
	Confirmed  int `json:"confirmed"`
	Duplicates int `json:"duplicates"`
}

// ImportPayPalExpensesConfirmHandler stores the outgoing PayPal rows the owner
// confirmed as work expenses (system_3 #10414-B2): POST
// /invoicing/expenses/import-paypal/confirm with {rows: [...]}. Each becomes
// an InvoicingExpense with source "paypal", entry "import", the payee in
// Reference and the category she picked (one of her own, default "other").
// Rows already stored under their (source, transaction id) count as
// duplicates. An invalid row fails the whole request, because the client
// only posts back rows the import offered.
func ImportPayPalExpensesConfirmHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	var req struct {
		Rows []paypalExpenseConfirmRow `json:"rows"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid request body")
		return
	}
	if len(req.Rows) == 0 {
		RespondWithError(c, http.StatusBadRequest, "rows is empty")
		return
	}
	if len(req.Rows) > maxPayPalExpenseConfirmRows {
		respondError(c, http.StatusBadRequest, "too_many_rows",
			fmt.Sprintf("confirm at most %d rows per request", maxPayPalExpenseConfirmRows))
		return
	}
	type validated struct {
		row    paypalExpenseConfirmRow
		paidOn time.Time
	}
	rows := make([]validated, 0, len(req.Rows))
	for i, row := range req.Rows {
		bad := func(msg string) {
			respondError(c, http.StatusBadRequest, "invalid_row", fmt.Sprintf("row %d: %s", i, msg))
		}
		row.TransactionID = strings.TrimSpace(row.TransactionID)
		row.PayeeName = strings.TrimSpace(row.PayeeName)
		row.Note = strings.TrimSpace(row.Note)
		row.Currency = strings.ToUpper(strings.TrimSpace(row.Currency))
		row.Category = strings.TrimSpace(row.Category)
		if row.Category == "" {
			row.Category = data_models.ExpenseCategoryOther
		}
		if !paypalTransactionIDPattern.MatchString(row.TransactionID) {
			bad("transaction_id is not a PayPal transaction id")
			return
		}
		// An outgoing PayPal row may name no payee; when it does, it is one line.
		if utf8.RuneCountInString(row.PayeeName) > maxManualReferenceRunes ||
			strings.Contains(row.PayeeName, "\n") || hasControlChars(row.PayeeName) || hasFormatChars(row.PayeeName) {
			bad(fmt.Sprintf("payee_name must be one line of at most %d characters", maxManualReferenceRunes))
			return
		}
		if utf8.RuneCountInString(row.Note) > maxManualNoteRunes || hasControlChars(row.Note) {
			bad(fmt.Sprintf("note is longer than %d characters", maxManualNoteRunes))
			return
		}
		if !isoCurrency.MatchString(row.Currency) {
			bad("currency must be a three-letter ISO code")
			return
		}
		paidOn, err := time.Parse(invoicingDateLayout, row.PaidOn)
		if err != nil {
			bad("paid_on is not a yyyy-mm-dd date")
			return
		}
		if paidOn.After(berlinToday()) || paidOn.Before(earliestPaymentDate) {
			bad("paid_on is in the future or before 2000-01-01")
			return
		}
		if row.AmountCents <= 0 || row.AmountCents > maxUnitPriceCents {
			bad("amount_cents must be a whole number of cents above zero")
			return
		}
		rows = append(rows, validated{row: row, paidOn: paidOn})
	}

	owner := invoicingUserID(c)
	summary := paypalExpenseConfirmSummary{}
	tx := db.Begin()
	// The book lock every expense write takes: two concurrent confirms of the
	// same rows cannot both pass the exists check.
	if err := data_models.LockInvoicingBook(tx, owner); err != nil {
		tx.Rollback()
		log.Printf("Error locking for a PayPal expense confirm: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error storing expenses")
		return
	}
	categoryOK := map[string]bool{}
	seen := map[string]bool{}
	for i, v := range rows {
		ok, checked := categoryOK[v.row.Category]
		if !checked {
			valid, err := data_models.IsExpenseCategoryForOwner(tx, owner, v.row.Category)
			if err != nil {
				tx.Rollback()
				log.Printf("Error checking expense category %q: %v", v.row.Category, err)
				RespondWithError(c, http.StatusInternalServerError, "Error storing expenses")
				return
			}
			ok, categoryOK[v.row.Category] = valid, valid
		}
		if !ok {
			tx.Rollback()
			respondError(c, http.StatusBadRequest, "invalid_row", fmt.Sprintf("row %d: category is not one of your expense categories", i))
			return
		}
		if seen[v.row.TransactionID] {
			summary.Duplicates++
			continue
		}
		seen[v.row.TransactionID] = true
		var n int
		if err := tx.Model(&data_models.InvoicingExpense{}).
			Where("owner_user_id = ? AND source = ? AND transaction_id = ?", owner, invoicingPaymentSourcePayPal, v.row.TransactionID).
			Count(&n).Error; err != nil {
			tx.Rollback()
			log.Printf("Error checking PayPal expense %s: %v", v.row.TransactionID, err)
			RespondWithError(c, http.StatusInternalServerError, "Error storing expenses")
			return
		}
		if n > 0 {
			summary.Duplicates++
			continue
		}
		e := data_models.InvoicingExpense{
			OwnerUserID:   owner,
			Source:        invoicingPaymentSourcePayPal,
			TransactionID: v.row.TransactionID,
			PaidOn:        v.paidOn,
			Currency:      v.row.Currency,
			AmountCents:   v.row.AmountCents,
			Note:          v.row.Note,
			Category:      v.row.Category,
			Entry:         data_models.ExpenseEntryImport,
			Reference:     v.row.PayeeName,
		}
		if err := tx.Create(&e).Error; err != nil {
			tx.Rollback()
			log.Printf("Error storing PayPal expense %s: %v", v.row.TransactionID, err)
			RespondWithError(c, http.StatusInternalServerError, "Error storing expenses")
			return
		}
		summary.Confirmed++
	}
	details := auditDetails(gin.H{"source": invoicingPaymentSourcePayPal, "entity": invoicingEntityExpense, "summary": summary})
	if err := data_models.RecordInvoicingAuditEvent(tx, owner, invoicingActionImport, invoicingEntityExpense, 0, details); err != nil {
		tx.Rollback()
		log.Printf("Error auditing PayPal expense confirm: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error storing expenses")
		return
	}
	if err := tx.Commit().Error; err != nil {
		log.Printf("Error committing PayPal expense confirm: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error storing expenses")
		return
	}
	c.JSON(http.StatusOK, gin.H{"summary": summary})
}
