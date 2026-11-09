package handlers

import (
	"crypto/sha256"
	"encoding/hex"
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

// N26 PDF import for the invoicing app (system_3 #9812, operator decision on
// #9760): one importer reads both N26 document kinds — the per-transfer
// confirmation PDF and the periodic account statement — next to the PayPal
// CSV import and manual entry, which stay unchanged.
//
// Unlike the PayPal import, nothing is stored at upload time: the parse
// endpoint returns candidate rows, the owner reviews each one in the client,
// and only the rows they confirm are persisted (the confirm endpoint). The
// document kinds are an ordered (predicate, strategy) chain over the
// extracted text, so a third kind later is one more entry, not a new
// endpoint.
//
// The confirmation strategy is built from the operator's real files' template
// (field labels only — no personal value from them appears here or in the
// tests). The statement strategy is built against N26's documented statement
// layout and look-alike fixtures; no real statement existed at authoring time
// (system_3 #9760 c60234), so its first real file validates it (#9814).

const (
	invoicingPaymentSourceN26 = "n26"

	n26KindTransferConfirmation = "n26_transfer_confirmation"
	n26KindAccountStatement     = "n26_account_statement"

	// maxN26ConfirmRows bounds one confirm request (security review N3).
	maxN26ConfirmRows = 500

	// n26SkipPayPalTransfer is the per-file skipped reason for a credit whose
	// payer is PayPal (system_3 #9881).
	n26SkipPayPalTransfer = "paypal_transfer"
)

// isPayPalSender reports whether an N26 credit's payer is PayPal. Money that
// reaches her N26 account from PayPal is her own withdrawal: the same money
// as the PayPal payments it sums, which the PayPal import already holds (her
// main income channel, system_3 #9881). Imported as an incoming payment it
// could be invoiced a second time. Her real transfer confirmations print the
// bare word PAYPAL as the sender; statements are expected to print PayPal's
// company name, which contains it too. Only the sender name is read, never
// the note, so a customer's own transfer that mentions PayPal stays a payment.
func isPayPalSender(name string) bool {
	return strings.Contains(strings.ToLower(name), "paypal")
}

// isPayPalTransferIntoN26 is the stored-row form of isPayPalSender: an N26
// payment whose payer is PayPal. Such a row can never be paid work, so it can
// never be invoiced, whenever it was stored.
func isPayPalTransferIntoN26(p data_models.InvoicingPayment) bool {
	return p.Source == invoicingPaymentSourceN26 && isPayPalSender(p.SenderName)
}

// Per-file parse error codes, machine-readable for the client.
const (
	n26ErrTooLarge       = "too_large"
	n26ErrNotAPDF        = "not_a_pdf"
	n26ErrExtractFailed  = "extract_failed"
	n26ErrNotN26Document = "not_n26_document"
	n26ErrParseFailed    = "parse_failed"
)

// n26Candidate is one would-be payment, parsed but not stored. PaidOn is ISO
// yyyy-mm-dd. The two flags warn the owner before confirming: the row is
// already imported under its transaction id, or some stored payment (any
// source, e.g. a confirmation this statement overlaps) has the same date and
// amount.
type n26Candidate struct {
	DocumentKind        string `json:"document_kind"`
	TransactionID       string `json:"transaction_id"`
	PaidOn              string `json:"paid_on"`
	Currency            string `json:"currency"`
	GrossCents          int64  `json:"gross_cents"`
	FeeCents            int64  `json:"fee_cents"`
	NetCents            int64  `json:"net_cents"`
	SenderName          string `json:"sender_name"`
	Note                string `json:"note"`
	AlreadyImported     bool   `json:"already_imported"`
	PossibleDuplicateOf uint   `json:"possible_duplicate_of,omitempty"`
}

type n26FileError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// n26ParsedFile is one uploaded file's result. Skipped counts the rows read
// from the file but left out of Candidates, per reason (today only
// n26SkipPayPalTransfer), so the owner is told why they are missing.
type n26ParsedFile struct {
	Filename     string         `json:"filename"`
	DocumentKind string         `json:"document_kind,omitempty"`
	Candidates   []n26Candidate `json:"candidates,omitempty"`
	// ExpenseCandidates are the file's outgoing rows (system_3 #10414-B),
	// confirmed through their own endpoint, never the payment confirm.
	ExpenseCandidates []n26ExpenseCandidate `json:"expense_candidates,omitempty"`
	Skipped           map[string]int        `json:"skipped,omitempty"`
	Error             *n26FileError         `json:"error,omitempty"`
}

// dropPayPalTransfers removes the candidates whose payer is PayPal and
// returns the rest with the count it removed. A statement's synthesized ids
// are derived before this runs, so a kept row's id does not depend on it.
func dropPayPalTransfers(cands []n26Candidate) ([]n26Candidate, map[string]int) {
	kept := make([]n26Candidate, 0, len(cands))
	var skipped map[string]int
	for _, c := range cands {
		if isPayPalSender(c.SenderName) {
			if skipped == nil {
				skipped = map[string]int{}
			}
			skipped[n26SkipPayPalTransfer]++
			continue
		}
		kept = append(kept, c)
	}
	return kept, skipped
}

// n26Strategy pairs a document-kind predicate with its parse function; the
// chain is ordered and the first matching predicate wins.
type n26Strategy struct {
	name    string
	matches func(text string) bool
	parse   func(text string) ([]n26Candidate, error)
	// expenses reads the document's outgoing rows as expense candidates;
	// nil for a kind that carries none (system_3 #10414-B).
	expenses func(text string) ([]n26ExpenseCandidate, error)
}

var n26Strategies = []n26Strategy{
	{n26KindTransferConfirmation, isN26TransferConfirmation, parseN26TransferConfirmation, nil},
	{n26KindAccountStatement, isN26AccountStatement, parseN26AccountStatement, parseN26StatementExpenses},
}

var (
	// n26AmountPattern is an Italian/German-format amount with its currency
	// mark: thousands dots, decimal comma ("1.234,56 EUR", "0,00 EUR",
	// "400,00 €").
	n26AmountPattern = regexp.MustCompile(`(\d[\d.]*,\d{2})\s*(?:EUR|€)`)
	n26DatePattern = regexp.MustCompile(`\d{2}\.\d{2}\.\d{4}`)
	// n26IBANPattern recognises an IBAN standing alone where a name should
	// be, the second half of the F2 fix (c60279).
	n26IBANPattern = regexp.MustCompile(`^[A-Z]{2}\d{2}[A-Z0-9]{10,30}$`)
	n26UUIDPattern         = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	// n26StatementRowPattern is one statement transaction line: booking date,
	// counterparty, signed amount at line end. Continuation lines (no date)
	// extend the row's note.
	n26StatementRowPattern = regexp.MustCompile(`^\s*(\d{2}\.\d{2}\.\d{4})\s+(.+?)\s{2,}([+-]?\d[\d.]*,\d{2})\s*(?:EUR|€)?\s*$`)
)

// parseN26AmountCents reads "1.234,56" into 123456.
func parseN26AmountCents(s string) (int64, error) {
	s = strings.ReplaceAll(s, ".", "")
	s = strings.ReplaceAll(s, ",", ".")
	var euros, cents int64
	if _, err := fmt.Sscanf(s, "%d.%d", &euros, &cents); err != nil {
		return 0, fmt.Errorf("amount %q: %v", s, err)
	}
	sign := int64(1)
	if euros < 0 || strings.HasPrefix(strings.TrimSpace(s), "-") {
		sign = -1
		if euros < 0 {
			euros = -euros
		}
	}
	return sign * (euros*100 + cents), nil
}

func parseN26Date(s string) (time.Time, error) {
	return time.Parse("02.01.2006", s)
}

func isN26TransferConfirmation(text string) bool {
	return strings.Contains(text, "Conferma dell'operazione") &&
		strings.Contains(text, "Dettagli operazione")
}

// parseN26TransferConfirmation reads the single payment a confirmation PDF
// describes. The template is label-keyed two-column `pdftotext -layout`
// output: a header line names the left and right fields, the next content
// line carries their values.
func parseN26TransferConfirmation(text string) ([]n26Candidate, error) {
	lines := strings.Split(text, "\n")

	// nextContentLine returns the first non-blank line after index i that
	// satisfies want, or -1.
	nextContentLine := func(start int, want func(string) bool) int {
		for i := start + 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "" {
				continue
			}
			if want(lines[i]) {
				return i
			}
		}
		return -1
	}
	findHeader := func(needles ...string) int {
		for i, l := range lines {
			ok := true
			for _, n := range needles {
				if !strings.Contains(l, n) {
					ok = false
					break
				}
			}
			if ok {
				return i
			}
		}
		return -1
	}

	// Importo: the first amount after the "Tipo di operazione / Importo"
	// header line.
	importoHeader := findHeader("Tipo di operazione", "Importo")
	if importoHeader < 0 {
		return nil, fmt.Errorf("missing the %q header", "Tipo di operazione / Importo")
	}
	amountLine := nextContentLine(importoHeader, func(l string) bool { return n26AmountPattern.MatchString(l) })
	if amountLine < 0 {
		return nil, fmt.Errorf("no amount after the Importo header")
	}
	grossCents, err := parseN26AmountCents(n26AmountPattern.FindStringSubmatch(lines[amountLine])[1])
	if err != nil {
		return nil, err
	}
	if grossCents <= 0 {
		return nil, fmt.Errorf("the transfer amount is not positive")
	}

	// Data valuta / Data operazione: the first line with two dates after
	// that header. PaidOn is the value date — the day the money reached the
	// account, which is what "Zahlung erhalten am" prints.
	datesHeader := findHeader("Data valuta", "Data operazione")
	if datesHeader < 0 {
		return nil, fmt.Errorf("missing the %q header", "Data valuta / Data operazione")
	}
	datesLine := nextContentLine(datesHeader, func(l string) bool { return len(n26DatePattern.FindAllString(l, -1)) >= 2 })
	if datesLine < 0 {
		return nil, fmt.Errorf("no dates after the Data valuta header")
	}
	paidOn, err := parseN26Date(n26DatePattern.FindAllString(lines[datesLine], -1)[0])
	if err != nil {
		return nil, err
	}

	// Identificativo operazione / Commissione: the transaction UUID, and the
	// fee on the same value line.
	idHeader := findHeader("Identificativo operazione")
	if idHeader < 0 {
		return nil, fmt.Errorf("missing the %q header", "Identificativo operazione")
	}
	idLine := nextContentLine(idHeader, func(l string) bool { return n26UUIDPattern.MatchString(l) })
	if idLine < 0 {
		return nil, fmt.Errorf("no operation id after the Identificativo header")
	}
	transactionID := n26UUIDPattern.FindString(lines[idLine])
	var feeCents int64
	if m := n26AmountPattern.FindStringSubmatch(lines[idLine]); m != nil {
		if feeCents, err = parseN26AmountCents(m[1]); err != nil {
			return nil, err
		}
	}
	if feeCents < 0 || feeCents >= grossCents {
		return nil, fmt.Errorf("the fee does not fit the amount")
	}

	// Causale: the content lines between its header and the sender block.
	var note string
	if causaleHeader := findHeader("Causale"); causaleHeader >= 0 {
		var parts []string
		for i := causaleHeader + 1; i < len(lines); i++ {
			t := strings.TrimSpace(lines[i])
			if strings.Contains(t, "DETTAGLI MITTENTE") {
				break
			}
			if t != "" {
				parts = append(parts, t)
			}
		}
		note = strings.Join(parts, " ")
	}

	// Sender: the first content line of the DETTAGLI MITTENTE block that is
	// not one of its own field labels.
	mittenteHeader := findHeader("DETTAGLI MITTENTE")
	if mittenteHeader < 0 {
		return nil, fmt.Errorf("missing the %q block", "DETTAGLI MITTENTE")
	}
	// The name is only ever the content line BEFORE the block's first IBAN
	// label; without one the block has no name, and stopping there keeps an
	// IBAN value from being stored as the sender (security review F2, #9812
	// c60279).
	senderName := ""
	for i := mittenteHeader + 1; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			continue
		}
		if strings.Contains(t, "IBAN") || strings.Contains(t, "DETTAGLI") {
			break
		}
		senderName = t
		break
	}
	if senderName == "" {
		return nil, fmt.Errorf("no sender name in the DETTAGLI MITTENTE block")
	}
	if n26IBANPattern.MatchString(senderName) {
		return nil, fmt.Errorf("the DETTAGLI MITTENTE block carries an IBAN where the name belongs")
	}

	return []n26Candidate{{
		DocumentKind:  n26KindTransferConfirmation,
		TransactionID: transactionID,
		PaidOn:        paidOn.Format("2006-01-02"),
		Currency:      "EUR",
		GrossCents:    grossCents,
		FeeCents:      feeCents,
		NetCents:      grossCents - feeCents,
		SenderName:    senderName,
		Note:          note,
	}}, nil
}

func isN26AccountStatement(text string) bool {
	return strings.Contains(text, "Estratto conto") ||
		strings.Contains(text, "Kontoauszug") ||
		(strings.Contains(text, "N26") && strings.Contains(text, "Account statement"))
}

// n26ExpenseCandidate is one would-be work expense: an outgoing statement row,
// parsed but not stored (system_3 #10414-B). It travels beside the payment
// candidates, never inside them, because the client posts every payment
// candidate back to the payment confirm. AmountCents is the money that went
// out, as a positive number. PayeeName is the counterparty the statement
// names. The two flags mirror n26Candidate's, read against the owner's
// expenses instead of her payments.
type n26ExpenseCandidate struct {
	TransactionID       string `json:"transaction_id"`
	PaidOn              string `json:"paid_on"`
	Currency            string `json:"currency"`
	AmountCents         int64  `json:"amount_cents"`
	PayeeName           string `json:"payee_name"`
	Note                string `json:"note"`
	AlreadyImported     bool   `json:"already_imported"`
	PossibleDuplicateOf uint   `json:"possible_duplicate_of,omitempty"`
}

// parseN26AccountStatement reads a statement's money-in rows as payment
// candidates. Outgoing rows are the owner's own spending, not payments to
// invoice: they come back from parseN26StatementExpenses instead.
func parseN26AccountStatement(text string) ([]n26Candidate, error) {
	in, _, err := scanN26Statement(text)
	return in, err
}

// parseN26StatementExpenses reads a statement's money-out rows as expense
// candidates (system_3 #10414-B).
func parseN26StatementExpenses(text string) ([]n26ExpenseCandidate, error) {
	_, out, err := scanN26Statement(text)
	return out, err
}

// scanN26Statement reads a statement's transaction table: one row per booking
// line (date, counterparty, signed amount), indented date-less lines
// extending the previous row's note. A positive amount is a payment
// candidate, a negative one an expense candidate. A continuation line extends
// whichever row it sits under, in either direction, and a blank line ends it.
// A zero amount is neither, and its continuations are dropped with it.
//
// Statement rows carry no transaction id, so a deterministic one is derived
// from the row's fields: the same file re-uploaded produces the same ids and
// dedupes to nothing. A payment's key is unchanged from before outgoing rows
// were read, so statements imported earlier still dedupe. An expense's key
// is salted with "out|", so the two directions never share an id.
func scanN26Statement(text string) ([]n26Candidate, []n26ExpenseCandidate, error) {
	var in []n26Candidate
	var out []n26ExpenseCandidate
	// A continuation line extends the last kept row of either direction:
	// lastDir names the slice ('i' in, 'o' out, 0 none) and lastIdx its row.
	// An index, not a pointer, so a later append cannot leave it dangling.
	var lastDir byte
	lastIdx := -1
	for _, raw := range strings.Split(text, "\n") {
		if m := n26StatementRowPattern.FindStringSubmatch(raw); m != nil {
			date, counterparty, amount := m[1], strings.TrimSpace(m[2]), m[3]
			paidOn, err := parseN26Date(date)
			if err != nil {
				return nil, nil, err
			}
			cents, err := parseN26AmountCents(amount)
			if err != nil {
				return nil, nil, err
			}
			switch {
			case cents > 0:
				in = append(in, n26Candidate{
					DocumentKind: n26KindAccountStatement,
					PaidOn:       paidOn.Format("2006-01-02"),
					Currency:     "EUR",
					GrossCents:   cents,
					NetCents:     cents,
					SenderName:   counterparty,
				})
				lastDir, lastIdx = 'i', len(in)-1
			case cents < 0:
				out = append(out, n26ExpenseCandidate{
					PaidOn:      paidOn.Format("2006-01-02"),
					Currency:    "EUR",
					AmountCents: -cents,
					PayeeName:   counterparty,
				})
				lastDir, lastIdx = 'o', len(out)-1
			default:
				lastDir, lastIdx = 0, -1
			}
			continue
		}
		// A date-less content line directly under a kept row extends its note.
		t := strings.TrimSpace(raw)
		if lastDir != 0 && t != "" && !strings.Contains(t, "Estratto conto") {
			var note *string
			if lastDir == 'i' {
				note = &in[lastIdx].Note
			} else {
				note = &out[lastIdx].Note
			}
			if *note != "" {
				*note += " "
			}
			*note += t
		} else if t == "" {
			lastDir, lastIdx = 0, -1
		}
	}
	// The id carries the row's occurrence number among identical (date,
	// amount, counterparty, note) tuples, so two genuinely separate but
	// identical rows in one statement stay two rows, while a re-upload of the
	// same file still produces the same ids (security review F1, #9812
	// c60279).
	counts := map[string]int{}
	stmtID := func(key string) string {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d", key, counts[key])))
		counts[key]++
		return "stmt-" + hex.EncodeToString(sum[:])[:24]
	}
	for i := range in {
		c := &in[i]
		c.TransactionID = stmtID(c.PaidOn + "|" + fmt.Sprint(c.GrossCents) + "|" + c.SenderName + "|" + c.Note)
	}
	for i := range out {
		e := &out[i]
		e.TransactionID = stmtID("out|" + e.PaidOn + "|" + fmt.Sprint(e.AmountCents) + "|" + e.PayeeName + "|" + e.Note)
	}
	return in, out, nil
}

// detectN26Document runs the strategy chain; empty name means no kind matched.
func detectN26Document(text string) (n26Strategy, bool) {
	for _, s := range n26Strategies {
		if s.matches(text) {
			return s, true
		}
	}
	return n26Strategy{}, false
}

// flagN26Candidates marks candidates already stored under their id, or
// colliding on date+amount with any stored payment (the statement-overlaps-
// confirmation case, and manual entries of the same transfer).
func flagN26Candidates(db *gorm.DB, owner uint, cands []n26Candidate) error {
	for i := range cands {
		c := &cands[i]
		exists, err := data_models.InvoicingPaymentSeen(db, owner, invoicingPaymentSourceN26, c.TransactionID)
		if err != nil {
			return err
		}
		c.AlreadyImported = exists
		if exists {
			continue
		}
		paidOn, err := time.Parse("2006-01-02", c.PaidOn)
		if err != nil {
			return err
		}
		var twin data_models.InvoicingPayment
		err = db.Where("owner_user_id = ? AND paid_on = ? AND gross_cents = ?", owner, paidOn, c.GrossCents).
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

// flagN26ExpenseCandidates is flagN26Candidates for outgoing rows: a
// candidate already stored as an N26 expense under its id, or colliding on
// date and amount with any of the owner's expenses (one she entered by hand
// before importing the statement, typically).
func flagN26ExpenseCandidates(db *gorm.DB, owner uint, cands []n26ExpenseCandidate) error {
	for i := range cands {
		c := &cands[i]
		var n int
		if err := db.Model(&data_models.InvoicingExpense{}).
			Where("owner_user_id = ? AND source = ? AND transaction_id = ?", owner, invoicingPaymentSourceN26, c.TransactionID).
			Count(&n).Error; err != nil {
			return err
		}
		c.AlreadyImported = n > 0
		if c.AlreadyImported {
			continue
		}
		paidOn, err := time.Parse("2006-01-02", c.PaidOn)
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

// ImportN26ParseHandler takes one or more N26 PDFs (multipart field "files")
// and returns their candidate payments without storing anything. Per-file
// problems come back as per-file errors; the request itself fails only when
// it carries no usable field at all.
func ImportN26ParseHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	form, err := c.MultipartForm()
	if err != nil {
		RespondWithError(c, http.StatusBadRequest, "Upload N26 PDFs in the form field \"files\"")
		return
	}
	files := form.File["files"]
	if len(files) == 0 {
		RespondWithError(c, http.StatusBadRequest, "Upload N26 PDFs in the form field \"files\"")
		return
	}
	if len(files) > maxN26FilesPerRequest {
		respondError(c, http.StatusBadRequest, "too_many_files",
			fmt.Sprintf("upload at most %d files per request", maxN26FilesPerRequest))
		return
	}

	out := make([]n26ParsedFile, 0, len(files))
	for _, fh := range files {
		entry := n26ParsedFile{Filename: fh.Filename}
		fail := func(code, message string) {
			entry.Error = &n26FileError{Code: code, Message: message}
			out = append(out, entry)
		}
		if fh.Size > maxN26PDFBytes {
			fail(n26ErrTooLarge, "the file is larger than 5 MB")
			continue
		}
		f, err := fh.Open()
		if err != nil {
			fail(n26ErrExtractFailed, "could not read the uploaded file")
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, maxN26PDFBytes+1))
		f.Close()
		if err != nil || int64(len(data)) > maxN26PDFBytes {
			fail(n26ErrTooLarge, "the file is larger than 5 MB")
			continue
		}
		text, err := ExtractN26PDFText(data)
		if err == errNotAPDF {
			fail(n26ErrNotAPDF, "the file is not a PDF")
			continue
		}
		if err != nil {
			log.Printf("N26 import: extracting %q: %v", fh.Filename, err)
			fail(n26ErrExtractFailed, "could not read the PDF's text")
			continue
		}
		strategy, ok := detectN26Document(text)
		if !ok {
			fail(n26ErrNotN26Document, "not an N26 transfer confirmation or account statement")
			continue
		}
		cands, err := strategy.parse(text)
		if err != nil {
			log.Printf("N26 import: parsing %q as %s: %v", fh.Filename, strategy.name, err)
			fail(n26ErrParseFailed, fmt.Sprintf("read as %s but a field is missing or malformed: %v", strategy.name, err))
			continue
		}
		cands, entry.Skipped = dropPayPalTransfers(cands)
		if err := flagN26Candidates(db, invoicingUserID(c), cands); err != nil {
			log.Printf("N26 import: flagging candidates of %q: %v", fh.Filename, err)
			RespondWithError(c, http.StatusInternalServerError, "Error checking candidates against stored payments")
			return
		}
		if strategy.expenses != nil {
			expenses, err := strategy.expenses(text)
			if err != nil {
				log.Printf("N26 import: parsing %q's outgoing rows: %v", fh.Filename, err)
				fail(n26ErrParseFailed, fmt.Sprintf("read as %s but a field is missing or malformed: %v", strategy.name, err))
				continue
			}
			if err := flagN26ExpenseCandidates(db, invoicingUserID(c), expenses); err != nil {
				log.Printf("N26 import: flagging expense candidates of %q: %v", fh.Filename, err)
				RespondWithError(c, http.StatusInternalServerError, "Error checking candidates against stored expenses")
				return
			}
			entry.ExpenseCandidates = expenses
		}
		entry.DocumentKind = strategy.name
		entry.Candidates = cands
		out = append(out, entry)
	}
	c.JSON(http.StatusOK, gin.H{"files": out})
}

// n26ConfirmRequest is the rows the owner confirmed, echoing the candidate
// fields (the owner may have deselected rows; edits happen later in the
// payments view, like any imported payment).
type n26ConfirmRequest struct {
	Rows []n26Candidate `json:"rows"`
}

type n26ConfirmSummary struct {
	Confirmed  int `json:"confirmed"`
	Duplicates int `json:"duplicates"`
}

// ImportN26ConfirmHandler stores the confirmed candidate rows as payments
// (source "n26", incoming, uncategorised). Rows already stored under their
// (source, transaction id) count as duplicates and are skipped; an invalid
// row fails the whole request, because the client only ever posts back rows
// the parse endpoint produced.
func ImportN26ConfirmHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	var req n26ConfirmRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid request body")
		return
	}
	if len(req.Rows) == 0 {
		RespondWithError(c, http.StatusBadRequest, "rows is empty")
		return
	}
	// Each row runs an existence query under the issuing lock, so the batch
	// is bounded (security review N3, #9812 c60279). 20 files of statement
	// rows stay well below it.
	if len(req.Rows) > maxN26ConfirmRows {
		respondError(c, http.StatusBadRequest, "too_many_rows",
			fmt.Sprintf("confirm at most %d rows per request", maxN26ConfirmRows))
		return
	}
	type validated struct {
		row    n26Candidate
		paidOn time.Time
	}
	rows := make([]validated, 0, len(req.Rows))
	for i, row := range req.Rows {
		bad := func(msg string) {
			respondError(c, http.StatusBadRequest, "invalid_row", fmt.Sprintf("row %d: %s", i, msg))
		}
		row.TransactionID = strings.TrimSpace(row.TransactionID)
		row.SenderName = strings.TrimSpace(row.SenderName)
		row.Note = strings.TrimSpace(row.Note)
		if row.DocumentKind != n26KindTransferConfirmation && row.DocumentKind != n26KindAccountStatement {
			bad("document_kind is not an N26 document kind")
			return
		}
		if row.TransactionID == "" || len(row.TransactionID) > 100 {
			bad("transaction_id is missing or too long")
			return
		}
		if row.SenderName == "" || len(row.SenderName) > 200 {
			bad("sender_name is missing or too long")
			return
		}
		// The parse never offers such a row; the client posts rows back,
		// so the rule is enforced here too.
		if isPayPalSender(row.SenderName) {
			bad("the payer is PayPal: a transfer from PayPal is already counted in the PayPal import")
			return
		}
		if len(row.Note) > 500 {
			bad("note is longer than 500 characters")
			return
		}
		if row.Currency != "EUR" {
			bad("currency must be EUR")
			return
		}
		paidOn, err := time.Parse("2006-01-02", row.PaidOn)
		if err != nil {
			bad("paid_on is not a yyyy-mm-dd date")
			return
		}
		if paidOn.After(time.Now().AddDate(0, 0, 1)) {
			bad("paid_on is in the future")
			return
		}
		if row.GrossCents <= 0 {
			bad("gross_cents must be positive")
			return
		}
		if row.FeeCents < 0 || row.FeeCents >= row.GrossCents {
			bad("fee_cents does not fit gross_cents")
			return
		}
		row.NetCents = row.GrossCents - row.FeeCents
		rows = append(rows, validated{row: row, paidOn: paidOn})
	}

	userID := invoicingUserID(c)
	summary := n26ConfirmSummary{}
	tx := db.Begin()
	// Dedupe runs under the issuing lock like the PayPal import's, so two
	// concurrent confirms of the same rows cannot both pass the exists check
	// (#8409 security F2).
	if _, err := lockIssuing(tx, invoicingUserID(c)); err != nil {
		tx.Rollback()
		log.Printf("Error locking for an N26 confirm: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error storing payments")
		return
	}
	seen := map[string]bool{}
	for _, v := range rows {
		if seen[v.row.TransactionID] {
			summary.Duplicates++
			continue
		}
		seen[v.row.TransactionID] = true
		exists, err := data_models.InvoicingPaymentSeen(tx, invoicingUserID(c), invoicingPaymentSourceN26, v.row.TransactionID)
		if err != nil {
			tx.Rollback()
			log.Printf("Error checking N26 payment %s: %v", v.row.TransactionID, err)
			RespondWithError(c, http.StatusInternalServerError, "Error storing payments")
			return
		}
		if exists {
			summary.Duplicates++
			continue
		}
		p := data_models.InvoicingPayment{
			OwnerUserID:   invoicingUserID(c),
			Source:        invoicingPaymentSourceN26,
			TransactionID: v.row.TransactionID,
			Kind:          data_models.PaymentKindIncoming,
			PaidOn:        v.paidOn,
			Currency:      "EUR",
			GrossCents:    v.row.GrossCents,
			FeeCents:      v.row.FeeCents,
			NetCents:      v.row.NetCents,
			SenderName:    v.row.SenderName,
			Note:          v.row.Note,
			Category:      data_models.PaymentCategoryUncategorised,
			Entry:         data_models.PaymentEntryImport,
		}
		if err := tx.Create(&p).Error; err != nil {
			tx.Rollback()
			log.Printf("Error storing N26 payment %s: %v", v.row.TransactionID, err)
			RespondWithError(c, http.StatusInternalServerError, "Error storing payments")
			return
		}
		summary.Confirmed++
	}
	details := auditDetails(gin.H{"source": invoicingPaymentSourceN26, "summary": summary})
	if err := data_models.RecordInvoicingAuditEvent(tx, userID, invoicingActionImport, invoicingEntityImport, 0, details); err != nil {
		tx.Rollback()
		log.Printf("Error auditing N26 confirm: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error storing payments")
		return
	}
	if err := tx.Commit().Error; err != nil {
		log.Printf("Error committing N26 confirm: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error storing payments")
		return
	}
	c.JSON(http.StatusOK, gin.H{"summary": summary})
}

// n26ExpenseConfirmRow is one outgoing row the owner confirmed: the
// candidate's fields, plus the category she picked (empty means "other").
type n26ExpenseConfirmRow struct {
	TransactionID string `json:"transaction_id"`
	PaidOn        string `json:"paid_on"`
	Currency      string `json:"currency"`
	AmountCents   int64  `json:"amount_cents"`
	PayeeName     string `json:"payee_name"`
	Note          string `json:"note"`
	Category      string `json:"category"`
}

type n26ExpenseConfirmRequest struct {
	Rows []n26ExpenseConfirmRow `json:"rows"`
}

// ImportN26ExpensesConfirmHandler stores the outgoing statement rows the owner
// confirmed as work expenses (system_3 #10414-B): POST
// /invoicing/expenses/import-n26/confirm. Each becomes an InvoicingExpense
// with source "n26", entry "import", the payee in Reference, and the category
// she picked (one of her own expense categories, default "other"). Rows
// already stored under their (source, transaction id) count as duplicates
// and are skipped. An invalid row fails the whole request, because the
// client only ever posts back rows the parse endpoint produced. It is the
// payment confirm's twin, so the same bounds and dedupe apply, under the
// book lock every expense write takes.
func ImportN26ExpensesConfirmHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	var req n26ExpenseConfirmRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid request body")
		return
	}
	if len(req.Rows) == 0 {
		RespondWithError(c, http.StatusBadRequest, "rows is empty")
		return
	}
	if len(req.Rows) > maxN26ConfirmRows {
		respondError(c, http.StatusBadRequest, "too_many_rows",
			fmt.Sprintf("confirm at most %d rows per request", maxN26ConfirmRows))
		return
	}
	type validated struct {
		row    n26ExpenseConfirmRow
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
		row.Category = strings.TrimSpace(row.Category)
		if row.Category == "" {
			row.Category = data_models.ExpenseCategoryOther
		}
		// Only statement rows reach here, and their ids are always derived.
		if !strings.HasPrefix(row.TransactionID, "stmt-") || len(row.TransactionID) > 100 {
			bad("transaction_id is not an N26 statement row id")
			return
		}
		if row.PayeeName == "" || utf8.RuneCountInString(row.PayeeName) > maxManualReferenceRunes ||
			strings.Contains(row.PayeeName, "\n") || hasControlChars(row.PayeeName) || hasFormatChars(row.PayeeName) {
			bad(fmt.Sprintf("payee_name must be one line of 1-%d characters", maxManualReferenceRunes))
			return
		}
		if utf8.RuneCountInString(row.Note) > maxManualNoteRunes || hasControlChars(row.Note) {
			bad(fmt.Sprintf("note is longer than %d characters", maxManualNoteRunes))
			return
		}
		if row.Currency != "EUR" {
			bad("currency must be EUR")
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
	summary := n26ConfirmSummary{}
	tx := db.Begin()
	// The book lock, as every expense write takes: two concurrent confirms of
	// the same rows cannot both pass the exists check.
	if err := data_models.LockInvoicingBook(tx, owner); err != nil {
		tx.Rollback()
		log.Printf("Error locking for an N26 expense confirm: %v", err)
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
			Where("owner_user_id = ? AND source = ? AND transaction_id = ?", owner, invoicingPaymentSourceN26, v.row.TransactionID).
			Count(&n).Error; err != nil {
			tx.Rollback()
			log.Printf("Error checking N26 expense %s: %v", v.row.TransactionID, err)
			RespondWithError(c, http.StatusInternalServerError, "Error storing expenses")
			return
		}
		if n > 0 {
			summary.Duplicates++
			continue
		}
		e := data_models.InvoicingExpense{
			OwnerUserID:   owner,
			Source:        invoicingPaymentSourceN26,
			TransactionID: v.row.TransactionID,
			PaidOn:        v.paidOn,
			Currency:      "EUR",
			AmountCents:   v.row.AmountCents,
			Note:          v.row.Note,
			Category:      v.row.Category,
			Entry:         data_models.ExpenseEntryImport,
			Reference:     v.row.PayeeName,
		}
		if err := tx.Create(&e).Error; err != nil {
			tx.Rollback()
			log.Printf("Error storing N26 expense %s: %v", v.row.TransactionID, err)
			RespondWithError(c, http.StatusInternalServerError, "Error storing expenses")
			return
		}
		summary.Confirmed++
	}
	details := auditDetails(gin.H{"source": invoicingPaymentSourceN26, "entity": invoicingEntityExpense, "summary": summary})
	if err := data_models.RecordInvoicingAuditEvent(tx, owner, invoicingActionImport, invoicingEntityExpense, 0, details); err != nil {
		tx.Rollback()
		log.Printf("Error auditing N26 expense confirm: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error storing expenses")
		return
	}
	if err := tx.Commit().Error; err != nil {
		log.Printf("Error committing N26 expense confirm: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error storing expenses")
		return
	}
	c.JSON(http.StatusOK, gin.H{"summary": summary})
}
