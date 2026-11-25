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
	// CopyExpiresAt is set only when the file read no rows and a copy of it
	// was kept for the administrator; it is when that copy is deleted
	// (#10567).
	CopyExpiresAt *time.Time `json:"copy_expires_at,omitempty"`
	// RemovedEarlier and DeletedEarlier (#10555-B1) are the duplicates the
	// owner had removed, or deleted for good. This import has no review, so
	// its summary is the only place a removed PayPal payment can be named.
	priorCounts
	// Differing names the rows, by transaction id, that are already in the
	// book under the same id with another amount, currency or date (system_3
	// #12271). They count among Duplicates and the stored row is kept: the
	// summary says so rather than nothing. Only a payment in the book is
	// compared; a removed or deleted one is not in it.
	Differing []string `json:"differing,omitempty"`
}

// importErrorResponse is the usual error body plus, when a copy of the
// failed file was kept, when it will be deleted (design ruling c65231 on
// system_3 #10536, #10567). Absent when no copy exists.
type importErrorResponse struct {
	Error         ErrorDetail `json:"error"`
	CopyExpiresAt *time.Time  `json:"copy_expires_at,omitempty"`
}

// respondImportError writes the import error body. Only the status and
// code are logged, never the message: on a parse
// failure the message is the parser's own text, which quotes the file's
// cells (an amount, a date) from a third party's bank export (system_3
// #10583, security c65625). The uploader still gets the full message.
// maxEchoedValueRunes bounds a file's value echoed in a refusal: enough to
// recognise it, never the whole of a long cell.
const maxEchoedValueRunes = 40

// clipValue is v cut to maxEchoedValueRunes, marked when cut.
func clipValue(v string) string {
	if utf8.RuneCountInString(v) <= maxEchoedValueRunes {
		return v
	}
	return string([]rune(v)[:maxEchoedValueRunes-1]) + "…"
}

func respondImportError(c *gin.Context, status int, code, message string, copyExpiresAt *time.Time) {
	log.Printf("Error response: %d - %s", status, code)
	c.JSON(status, importErrorResponse{Error: ErrorDetail{Code: code, Message: message}, CopyExpiresAt: copyExpiresAt})
}

// File signatures the import sniffs: an .xlsx is a zip; a legacy .xls is an
// OLE2 compound file, which the import does not read.
var (
	zipSignature  = []byte("PK\x03\x04")
	ole2Signature = []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}
)

// payPalRowDiffersFromStored reports whether the payment in the book under
// row's transaction id has another amount, currency or date than row. The
// day is compared, not the instant: both are dates.
func payPalRowDiffersFromStored(tx *gorm.DB, owner uint, row paypalRow) (bool, error) {
	var stored data_models.InvoicingPayment
	err := tx.Where("owner_user_id = ? AND source = ? AND transaction_id = ? AND removed_at IS NULL",
		owner, invoicingPaymentSourcePayPal, row.TransactionID).First(&stored).Error
	if err != nil {
		return false, err
	}
	return stored.GrossCents != row.GrossCents || stored.Currency != row.Currency ||
		stored.PaidOn.Format("2006-01-02") != row.PaidOn.Format("2006-01-02"), nil
}

// ImportPayPalPaymentsHandler takes a PayPal activity CSV or Excel (.xlsx)
// file (multipart field "file", optional form value date_order=dmy|mdy) and
// stores its incoming payments and withdrawals. The format is read from the
// bytes, not the filename. Re-uploading the same file imports nothing new.
func ImportPayPalPaymentsHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	fh, err := c.FormFile("file")
	if err != nil {
		respondImportError(c, http.StatusBadRequest, codeMissingFile, "Upload a PayPal CSV or Excel file in the form field \"file\"", nil)
		return
	}
	userID := invoicingUserID(c)
	if fh.Size > maxPayPalImportBytes {
		recordImportFailure(db, userID, "paypal", fh.Filename, fh.Size, nil, "too_large", "", 0)
		respondImportError(c, http.StatusRequestEntityTooLarge, "too_large", "This file is larger than 5 MB.", nil)
		return
	}
	f, err := fh.Open()
	if err != nil {
		recordImportFailure(db, userID, "paypal", fh.Filename, fh.Size, nil, "read_failed", "", 0)
		RespondWithError(c, http.StatusBadRequest, "Could not read the uploaded file")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxPayPalImportBytes+1))
	// A read that fails is the server's, not a size (c77188 point 9).
	if err != nil {
		log.Printf("Error reading a PayPal import: %v", err)
		recordImportFailure(db, userID, "paypal", fh.Filename, fh.Size, nil, "read_failed", "", 0)
		RespondWithError(c, http.StatusInternalServerError, "Could not read the uploaded file")
		return
	}
	if len(data) > maxPayPalImportBytes {
		recordImportFailure(db, userID, "paypal", fh.Filename, fh.Size, nil, "too_large", "", 0)
		respondImportError(c, http.StatusRequestEntityTooLarge, "too_large", "This file is larger than 5 MB.", nil)
		return
	}

	// An empty file is said so, not answered with the parser's "could not
	// read the header row: EOF" (system_3 #12271).
	if len(bytes.TrimSpace(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")))) == 0 {
		recordImportFailure(db, userID, "paypal", fh.Filename, fh.Size, nil, "empty_file", "", 0)
		respondImportError(c, http.StatusBadRequest, "empty_file", "This file is empty.", nil)
		return
	}
	if bytes.HasPrefix(data, ole2Signature) {
		expires := recordImportFailure(db, userID, "paypal", fh.Filename, fh.Size, data, "xls_unsupported", "", 0)
		respondImportError(c, http.StatusBadRequest, "xls_unsupported",
			"This is an Excel 97-2003 (.xls) file. Save it as .xlsx or CSV and upload that.", expires)
		return
	}
	format, code := "csv", "invalid_csv"
	parse := ParsePayPalActivityCSV
	if bytes.HasPrefix(data, zipSignature) {
		format, code, parse = "xlsx", "invalid_xlsx", ParsePayPalActivityXLSX
	}
	parsed, err := parse(data, c.PostForm("date_order"))
	if err != nil {
		var expires *time.Time
		if errors.Is(err, ErrPayPalDateOrderAmbiguous) {
			code = "date_order_ambiguous"
		} else {
			// date_order_ambiguous excluded: it always resolves on an
			// immediate retry of the same file (system_3 #10536), so it
			// reads as a normal interactive step, not a failure. The
			// failure keeps the format's code: one class per file kind.
			expires = recordImportFailure(db, userID, "paypal", fh.Filename, fh.Size, data, code, format, 0)
		}
		// A value on a line, and dates in two orders, answer a code of
		// their own; anything else is the file not being read (c77188
		// group 4).
		detail := ErrorDetail{Code: code, Message: err.Error(), Field: "file"}
		var ve *paypalValueError
		switch {
		case errors.As(err, &ve):
			detail.Code, detail.Line, detail.Value = ve.Code, ve.Line, clipValue(ve.Value)
		case errors.Is(err, ErrPayPalDateOrdersDisagree):
			detail.Code = codeDateOrdersDisagree
		}
		log.Printf("Error response: %d - %s", http.StatusBadRequest, detail.Code)
		c.JSON(http.StatusBadRequest, importErrorResponse{Error: detail, CopyExpiresAt: expires})
		return
	}

	summary := invoicingImportSummary{RowsRead: parsed.RowsRead, SkippedReasons: parsed.Skipped, Format: format}
	for _, n := range parsed.Skipped {
		summary.Skipped += n
	}
	// Recognised but nothing usable (security ruling c65222): still a
	// failure the operator needs visibility into. The response to the
	// uploader is unaffected -- rows_read: 0 already says this plainly.
	if parsed.RowsRead == 0 {
		summary.CopyExpiresAt = recordImportFailure(db, userID, "paypal", fh.Filename, fh.Size, data, importFailureCodeNoCandidates, format, 0)
	}

	tx := db.Begin()
	defer tx.RollbackUnlessCommitted()
	// The import's duplicate checks against hand-entered PayPal payments run
	// under the issuing lock, like a hand entry's (#8409 security F2).
	if _, err := lockIssuing(tx, invoicingUserID(c)); err != nil {
		tx.Rollback()
		log.Printf("Error locking for a payment import: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error importing payments")
		return
	}
	// What the book holds is read once for the file, and the new payments
	// are stored in a few statements, all in this one transaction (system_3
	// #12273 item 6): a failure still stores nothing.
	owner := invoicingUserID(c)
	txnIDs := make([]string, 0, len(parsed.Rows))
	emails := make([]string, 0, len(parsed.Rows))
	for _, row := range parsed.Rows {
		txnIDs = append(txnIDs, row.TransactionID)
		if row.Kind == data_models.PaymentKindIncoming {
			emails = append(emails, row.SenderEmail)
		}
	}
	priors, err := data_models.InvoicingPaymentPriors(tx, owner, invoicingPaymentSourcePayPal, txnIDs)
	if err != nil {
		tx.Rollback()
		log.Printf("Error checking the file's payments: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error importing payments")
		return
	}
	// The same PayPal payment may already have been entered by hand with its
	// transaction id as the reference (#8409).
	byHand, err := data_models.ManualPayPalReferences(tx, owner, txnIDs)
	if err != nil {
		tx.Rollback()
		log.Printf("Error checking manual payments for the file: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error importing payments")
		return
	}
	clientIDs, err := data_models.InvoicingClientIDsByPayPalEmail(tx, owner, emails)
	if err != nil {
		tx.Rollback()
		log.Printf("Error matching clients for the file: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error importing payments")
		return
	}
	seen := map[string]bool{}
	var fresh []data_models.InvoicingPayment
	for _, row := range parsed.Rows {
		if seen[row.TransactionID] {
			summary.Duplicates++
			continue
		}
		seen[row.TransactionID] = true
		prior := priors[row.TransactionID]
		summary.count(prior)
		if prior == data_models.PriorStored {
			differs, err := payPalRowDiffersFromStored(tx, invoicingUserID(c), row)
			if err != nil {
				tx.Rollback()
				log.Printf("Error comparing payment %s: %v", row.TransactionID, err)
				RespondWithError(c, http.StatusInternalServerError, "Error importing payments")
				return
			}
			if differs {
				summary.Differing = append(summary.Differing, row.TransactionID)
			}
		}
		if prior.Seen() || byHand[row.TransactionID] {
			summary.Duplicates++
			continue
		}
		p := data_models.InvoicingPayment{
			OwnerUserID:    owner,
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
		if row.Kind == data_models.PaymentKindIncoming && row.SenderEmail != "" {
			if id, ok := clientIDs[row.SenderEmail]; ok {
				p.ClientID = &id
			}
		}
		fresh = append(fresh, p)
		summary.Imported++
		if row.Kind == data_models.PaymentKindWithdrawal {
			summary.Withdrawals++
		}
		if row.Category == data_models.PaymentCategoryRefund {
			summary.Refunds++
		}
	}
	if err := data_models.InsertImportedPayments(tx, fresh); err != nil {
		tx.Rollback()
		log.Printf("Error storing the file's payments: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error importing payments")
		return
	}
	// The rows that undo money already in the book, each with the payment it
	// names (system_3 #12259). Read after the inserts, so a refund in the
	// same file as its payment finds it too. Nothing is changed: the owner
	// corrects the Book with the step the client offers.
	undone, refundCandidates, err := matchPayPalUndos(tx, invoicingUserID(c), parsed.Undos)
	if err != nil {
		tx.Rollback()
		log.Printf("Error matching PayPal reversals and refunds: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error importing payments")
		return
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
	// A partial refund or reversal joins them, last (ruling item 5).
	candidates := func() []paypalExpenseCandidate {
		out := paypalExpenseCandidates(parsed.Expenses)
		return append(out, refundCandidates...)
	}
	expenses := candidates()
	if err := flagPayPalExpenseCandidates(db, userID, expenses); err != nil {
		log.Printf("Error flagging PayPal expense candidates, returning them unflagged: %v", err)
		expenses = candidates()
	}
	c.JSON(http.StatusOK, gin.H{"summary": summary, "expense_candidates": expenses, "undone": undone})
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
	// ?removed=true serves the removed view (§3.143 Decision 6): every
	// removed payment, most recently removed first, whatever the other filters.
	var payments []data_models.InvoicingPayment
	var err error
	if c.Query("removed") == "true" {
		payments, err = data_models.ListRemovedInvoicingPayments(db, invoicingUserID(c))
	} else {
		payments, err = data_models.ListInvoicingPayments(db, invoicingUserID(c), kind, category)
	}
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
	live, err := data_models.LiveInvoicesForPayments(db, invoicingUserID(c))
	if err != nil {
		log.Printf("Error listing live invoices: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error listing payments")
		return
	}
	for i := range payments {
		payments[i].OnInvoice = linked[payments[i].ID]
		payments[i].OnIssuedInvoice = onIssued[payments[i].ID]
		if ref, ok := live[payments[i].ID]; ok {
			ref := ref
			payments[i].Invoice = &ref
		}
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
	Removed             bool   `json:"removed,omitempty"`
	DeletedEarlier      bool   `json:"deleted_earlier,omitempty"`
	PossibleDuplicateOf uint   `json:"possible_duplicate_of,omitempty"`
	// RefundOf is the stored payment a partial refund or reversal takes
	// money back from, and UndoKind which of the two it is (system_3
	// #12259). Empty on an ordinary outgoing payment.
	RefundOf uint   `json:"refund_of,omitempty"`
	UndoKind string `json:"undo_kind,omitempty"`
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
// of the owner's expenses (one ze entered by hand, typically).
func flagPayPalExpenseCandidates(db *gorm.DB, owner uint, cands []paypalExpenseCandidate) error {
	for i := range cands {
		c := &cands[i]
		// Removed and deleted-for-good expenses count as seen (#10555-B1).
		prior, err := data_models.InvoicingExpensePrior(db, owner, invoicingPaymentSourcePayPal, c.TransactionID)
		if err != nil {
			return err
		}
		markPrior(prior, &c.AlreadyImported, &c.Removed, &c.DeletedEarlier)
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
// candidate's fields plus the category ze picked (empty means "other").
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
	priorCounts
}

// ImportPayPalExpensesConfirmHandler stores the outgoing PayPal rows the owner
// confirmed as work expenses (system_3 #10414-B2): POST
// /invoicing/expenses/import-paypal/confirm with {rows: [...]}. Each becomes
// an InvoicingExpense with source "paypal", entry "import", the payee in
// Reference and the category ze picked (one of zir own, default "other").
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
		respondFieldRefusal(c, "rows", &refusal{code: "too_many_rows", limit: maxPayPalExpenseConfirmRows, whole: true,
			message: fmt.Sprintf("confirm at most %d rows per request", maxPayPalExpenseConfirmRows)})
		return
	}
	type validated struct {
		row    paypalExpenseConfirmRow
		paidOn time.Time
	}
	rows := make([]validated, 0, len(req.Rows))
	for _, row := range req.Rows {
		bad := func(msg string) {
			respondNamedRowRefusal(c, i, importRowName(row.PaidOn, row.PayeeName, "to"), msg)
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
		if utf8.RuneCountInString(row.PayeeName) > maxManualReferenceRunes || !oneLineRowText(row.PayeeName) {
			bad(fmt.Sprintf("payee_name must be one line of at most %d characters", maxManualReferenceRunes))
			return
		}
		if utf8.RuneCountInString(row.Note) > maxManualNoteRunes {
			bad(fmt.Sprintf("note is longer than %d characters", maxManualNoteRunes))
			return
		}
		if !rowNoteText(row.Note) {
			bad("note holds a control or direction-override character")
			return
		}
		if !isoCurrency.MatchString(row.Currency) {
			bad("currency must be a three-letter ISO code")
			return
		}
		paidOn, refusal := checkRowPaidOn(row.PaidOn, berlinToday())
		if refusal != "" {
			bad(refusal)
			return
		}
		if !rowAmountInBounds(row.AmountCents) {
			bad(rowAmountRefusal("amount_cents"))
			return
		}
		rows = append(rows, validated{row: row, paidOn: paidOn})
	}

	owner := invoicingUserID(c)
	summary := paypalExpenseConfirmSummary{}
	tx := db.Begin()
	defer tx.RollbackUnlessCommitted()
	// The book lock every expense write takes: two concurrent confirms of the
	// same rows cannot both pass the exists check.
	if err := data_models.LockInvoicingBook(tx, owner); err != nil {
		tx.Rollback()
		log.Printf("Error locking for a PayPal expense confirm: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error storing expenses")
		return
	}
	categoryOK := map[string]bool{}
	categoryRefusal := map[string]string{}
	seen := map[string]bool{}
	for i, v := range rows {
		ok, checked := categoryOK[v.row.Category]
		if !checked {
			// A removed category is refused here too: an import makes new
			// expenses (system_3 #11579 item 4).
			_, refusal, err := checkExpenseCategoryChoice(tx, owner, v.row.Category, "")
			if err != nil {
				tx.Rollback()
				log.Printf("Error checking expense category %q: %v", v.row.Category, err)
				RespondWithError(c, http.StatusInternalServerError, "Error storing expenses")
				return
			}
			categoryRefusal[v.row.Category] = refusal
			ok, categoryOK[v.row.Category] = refusal == "", refusal == ""
		}
		if !ok {
			tx.Rollback()
			respondRowRefusal(c, i, categoryRefusal[v.row.Category])
			return
		}
		if seen[v.row.TransactionID] {
			summary.Duplicates++
			continue
		}
		seen[v.row.TransactionID] = true
		prior, err := data_models.InvoicingExpensePrior(tx, owner, invoicingPaymentSourcePayPal, v.row.TransactionID)
		if err != nil {
			tx.Rollback()
			log.Printf("Error checking PayPal expense %s: %v", v.row.TransactionID, err)
			RespondWithError(c, http.StatusInternalServerError, "Error storing expenses")
			return
		}
		if prior.Seen() {
			summary.Duplicates++
			summary.count(prior)
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
