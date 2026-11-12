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
// CSV import and manual entry, which stay unchanged. N26's own CSV export is
// a third input, and the preferred one (system_3 #10543, invoicing_n26_csv.go).
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

	// n26SkipInternalTransfer is the per-file skipped reason for a German-
	// layout statement row whose detail-block type the parser does not
	// recognise as one of N26's three real transaction types (Gutschriften,
	// Lastschriften, a Mastercard purchase) — the shape an N26 Space's own
	// internal transfer takes (e.g. a move into "Investments": no IBAN, no
	// recognised type), confirmed against a real Kontoaktivität export
	// (system_3 #10534). Such a row is neither a payment nor a work expense.
	n26SkipInternalTransfer = "space_transfer"
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
	Removed             bool   `json:"removed,omitempty"`
	DeletedEarlier      bool   `json:"deleted_earlier,omitempty"`
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
	// CopyExpiresAt is set only when this file failed and a copy of it was
	// kept for the administrator; it is when that copy is deleted (#10567).
	CopyExpiresAt *time.Time `json:"copy_expires_at,omitempty"`
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
	// skipped reports the strategy's own per-file skip reasons beyond
	// PayPal (system_3 #10534, e.g. a German statement's internal Space
	// transfers); nil for a kind that carries none.
	skipped func(text string) (map[string]int, error)
}

var n26Strategies = []n26Strategy{
	{n26KindTransferConfirmation, isN26TransferConfirmation, parseN26TransferConfirmation, nil, nil},
	{n26KindAccountStatement, isN26AccountStatement, parseN26AccountStatement, parseN26StatementExpenses, parseN26StatementSkipped},
}

// mergeSkipCounts adds every reason from each map into one, nil when both are
// empty so the wire shape omits an empty "skipped" field as before.
func mergeSkipCounts(a, b map[string]int) map[string]int {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	out := make(map[string]int, len(a)+len(b))
	for k, v := range a {
		out[k] += v
	}
	for k, v := range b {
		out[k] += v
	}
	return out
}

var (
	// n26AmountPattern is an Italian/German-format amount with its currency
	// mark: thousands dots, decimal comma ("1.234,56 EUR", "0,00 EUR",
	// "400,00 €").
	n26AmountPattern = regexp.MustCompile(`(\d[\d.]*,\d{2})\s*(?:EUR|€)`)
	n26DatePattern   = regexp.MustCompile(`\d{2}\.\d{2}\.\d{4}`)
	// n26IBANPattern recognises an IBAN standing alone where a name should
	// be, the second half of the F2 fix (c60279).
	n26IBANPattern = regexp.MustCompile(`^[A-Z]{2}\d{2}[A-Z0-9]{10,30}$`)
	n26UUIDPattern = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	// n26StatementRowPattern is one Italian-layout statement transaction
	// line (the one real file at authoring time, system_3 #9760 c60234):
	// booking date, counterparty, signed amount at line end. Continuation
	// lines (no date) extend the row's note.
	n26StatementRowPattern = regexp.MustCompile(`^\s*(\d{2}\.\d{2}\.\d{4})\s+(.+?)\s{2,}([+-]?\d[\d.]*,\d{2})\s*(?:EUR|€)?\s*$`)
	// n26GermanStatementRowPattern is one statement transaction line in N26's
	// real German layout (system_3 #10534, against the operator's own
	// statement and Kontoaktivität export — her N26 is in German, unlike the
	// Italian look-alike this importer was first built against): counterparty
	// first, the booking date (Verbuchungsdatum) in the middle, the signed
	// amount glued to its currency mark at line end. A detail block of a few
	// more lines follows (see scanN26Statement); a blank line ends it.
	n26GermanStatementRowPattern = regexp.MustCompile(`^\s*(.+?)\s{2,}(\d{2}\.\d{2}\.\d{4})\s+([+-]?\d[\d.]*,\d{2})€\s*$`)
)

// n26InternalStatementRow reports whether a German-layout statement row is
// money moving between the account holder's own main account and Spaces,
// judged by its detail-block type line (the block's first line; "" when the
// block opens straight on its Wertstellung line). Checked against the
// operator's real October statement and N26's own CSV export of the same
// month (system_3 #10534): every internal row is a Round-up ("2x Round-up"),
// a "Monthly Rule", or carries no type line at all (a plain "zu/von <Space>"
// transfer, mirrored by the opposite row on the other account). Every other
// type is real money, on the main pages and the per-Space pages alike:
// Gutschriften, Lastschriften, Belastungen, Mastercard (incl. "Mastercard •
// <category>"), MoneyBeam. The internal kinds are enumerated rather than the
// real ones, so a type this list has never seen is offered to the user as a
// candidate they can untick, never dropped unseen.
func n26InternalStatementRow(typeLine string) bool {
	return typeLine == "" || strings.Contains(typeLine, "Round-up") || typeLine == "Monthly Rule"
}

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
		// The Kontoaktivität export (an account-activity PDF for a chosen
		// date range, system_3 #10534) shares the German statement's row
		// layout and detail-block grammar; it is the same document kind.
		strings.Contains(text, "Kontoaktivität") ||
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
	Removed             bool   `json:"removed,omitempty"`
	DeletedEarlier      bool   `json:"deleted_earlier,omitempty"`
	PossibleDuplicateOf uint   `json:"possible_duplicate_of,omitempty"`
}

// parseN26AccountStatement reads a statement's money-in rows as payment
// candidates. Outgoing rows are the owner's own spending, not payments to
// invoice: they come back from parseN26StatementExpenses instead.
func parseN26AccountStatement(text string) ([]n26Candidate, error) {
	in, _, _, err := scanN26Statement(text)
	return in, err
}

// parseN26StatementExpenses reads a statement's money-out rows as expense
// candidates (system_3 #10414-B).
func parseN26StatementExpenses(text string) ([]n26ExpenseCandidate, error) {
	_, out, _, err := scanN26Statement(text)
	return out, err
}

// parseN26StatementSkipped reports a German-layout statement's internal-
// transfer skip count (system_3 #10534), the n26Strategy.skipped hook.
func parseN26StatementSkipped(text string) (map[string]int, error) {
	_, _, skipped, err := scanN26Statement(text)
	if err != nil {
		return nil, err
	}
	if skipped == 0 {
		return nil, nil
	}
	return map[string]int{n26SkipInternalTransfer: skipped}, nil
}

// scanN26Statement reads a statement's transaction table under either of the
// two row grammars this importer understands: the Italian layout (booking
// date, counterparty, signed amount, with any following date-less line
// extending the row's note until a blank line) this importer was first built
// against (system_3 #9760 c60234), and the real German layout (system_3
// #10534, against the operator's own statement and Kontoaktivität export):
// counterparty, booking date (Verbuchungsdatum), signed amount, then a
// detail block of up to a few more lines — the transaction type
// (Gutschriften, Lastschriften, or a Mastercard purchase), the
// counterparty's IBAN/BIC when it prints one, an optional reference line,
// and the value date (Wertstellung) — ended by a blank line. Both stay
// supported: her N26 export is German, Flowah's (#9760) may be Italian, and
// neither real file proves the other layout's statement path (operator,
// comment 65185 on #9656).
//
// A positive amount is a payment candidate, a negative one an expense
// candidate (#10414-B); PaidOn is a German row's Wertstellung date when its
// block carries one (the confirmation importer's own value-date rule), the
// booking date otherwise — an Italian row has no Wertstellung concept, so
// PaidOn is always its own booking date. A zero amount is neither.
//
// A German row whose type line is none of the three known ones — the shape
// an N26 Space's own internal transfer takes (e.g. a move into
// "Investments": no IBAN, no recognised type) — is not a payment or
// expense; it is counted as a skip (n26SkipInternalTransfer) instead of
// offered as either, and a per-Space summary page's own four aggregate
// lines never match the row pattern at all (they carry no date), so they
// contribute nothing here. The Italian layout has no such concept.
//
// Statement rows carry no transaction id, so a deterministic one is derived
// from the row's fields: the same file re-uploaded produces the same ids and
// dedupes to nothing. The id carries the row's occurrence number among
// identical (date, amount, counterparty, note) tuples, so two genuinely
// separate but identical rows in one statement stay two rows (security
// review F1, #9812 c60279). A payment's key is unchanged from before
// outgoing rows were read, so statements imported earlier still dedupe. An
// expense's key is salted with "out|", so the two directions never share an
// id.
func scanN26Statement(text string) ([]n26Candidate, []n26ExpenseCandidate, int, error) {
	var in []n26Candidate
	var out []n26ExpenseCandidate
	skippedInternal := 0

	lines := strings.Split(text, "\n")
	// The Italian layout's continuation state: lastDir names the slice ('i'
	// in, 'o' out, 0 none) and lastIdx its row, carried across the lines
	// following an Italian row match until a blank line or a new row ends
	// it (unchanged from the original #9812 implementation). A German row
	// consumes its own detail block inline and never touches this state.
	var lastDir byte
	lastIdx := -1

	for i := 0; i < len(lines); i++ {
		if m := n26GermanStatementRowPattern.FindStringSubmatch(lines[i]); m != nil {
			lastDir, lastIdx = 0, -1
			counterparty := strings.TrimSpace(m[1])
			bookingDateStr, amountStr := m[2], m[3]
			cents, err := parseN26AmountCents(amountStr)
			if err != nil {
				return nil, nil, 0, err
			}

			// Read the detail block that follows the row line, up to the
			// blank line that ends it: the type line (the block's first
			// line, unless the block opens on Wertstellung or IBAN), the
			// IBAN/BIC line (dropped — structural, not a note), the value
			// date off the Wertstellung line, and any other line as the
			// row's note.
			typeLine := ""
			valueDateStr := ""
			var noteParts []string
			j := i + 1
			for ; j < len(lines); j++ {
				t := strings.TrimSpace(lines[j])
				if t == "" {
					break
				}
				if strings.HasPrefix(t, "Wertstellung") {
					if d := n26DatePattern.FindString(t); d != "" {
						valueDateStr = d
					}
					continue
				}
				if strings.HasPrefix(t, "IBAN:") {
					continue
				}
				if j == i+1 {
					typeLine = t
					continue
				}
				noteParts = append(noteParts, t)
			}
			i = j // resume scanning after the consumed detail block

			if cents == 0 {
				continue
			}
			if n26InternalStatementRow(typeLine) {
				skippedInternal++
				continue
			}

			paidOnStr := bookingDateStr
			if valueDateStr != "" {
				paidOnStr = valueDateStr
			}
			paidOn, err := parseN26Date(paidOnStr)
			if err != nil {
				return nil, nil, 0, err
			}
			note := strings.Join(noteParts, " ")

			if cents > 0 {
				in = append(in, n26Candidate{
					DocumentKind: n26KindAccountStatement,
					PaidOn:       paidOn.Format("2006-01-02"),
					Currency:     "EUR",
					GrossCents:   cents,
					NetCents:     cents,
					SenderName:   counterparty,
					Note:         note,
				})
			} else {
				out = append(out, n26ExpenseCandidate{
					PaidOn:      paidOn.Format("2006-01-02"),
					Currency:    "EUR",
					AmountCents: -cents,
					PayeeName:   counterparty,
					Note:        note,
				})
			}
			continue
		}

		if m := n26StatementRowPattern.FindStringSubmatch(lines[i]); m != nil {
			date, counterparty, amount := m[1], strings.TrimSpace(m[2]), m[3]
			paidOn, err := parseN26Date(date)
			if err != nil {
				return nil, nil, 0, err
			}
			cents, err := parseN26AmountCents(amount)
			if err != nil {
				return nil, nil, 0, err
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

		// A date-less content line directly under a kept Italian row
		// extends its note (unchanged from the original implementation); a
		// German row's own lines are already consumed above.
		t := strings.TrimSpace(lines[i])
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
	return in, out, skippedInternal, nil
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

// flagN26Candidates marks candidates already stored under their id (and
// whether that row was removed or deleted for good, #10555-B1), or colliding
// on date+amount with any stored payment (the statement-overlaps-
// confirmation case, and manual entries of the same transfer).
func flagN26Candidates(db *gorm.DB, owner uint, cands []n26Candidate) error {
	for i := range cands {
		c := &cands[i]
		prior, err := data_models.InvoicingPaymentPrior(db, owner, invoicingPaymentSourceN26, c.TransactionID)
		if err != nil {
			return err
		}
		markPrior(prior, &c.AlreadyImported, &c.Removed, &c.DeletedEarlier)
		if c.AlreadyImported {
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
		// Removed and deleted-for-good expenses count as seen too: before
		// #10555-B1 the expense tombstone was never read here, so a re-import
		// offered an expense she had deleted for good as new.
		prior, err := data_models.InvoicingExpensePrior(db, owner, invoicingPaymentSourceN26, c.TransactionID)
		if err != nil {
			return err
		}
		markPrior(prior, &c.AlreadyImported, &c.Removed, &c.DeletedEarlier)
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

// ImportN26ParseHandler takes one or more N26 files (multipart field
// "files"): PDFs, and since system_3 #10543 N26's CSV export, the preferred
// input. It returns their candidate payments without storing anything. Per-file
// problems come back as per-file errors; the request itself fails only when
// it carries no usable field at all.
func ImportN26ParseHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	form, err := c.MultipartForm()
	if err != nil {
		RespondWithError(c, http.StatusBadRequest, "Upload N26 files in the form field \"files\"")
		return
	}
	files := form.File["files"]
	if len(files) == 0 {
		RespondWithError(c, http.StatusBadRequest, "Upload N26 files in the form field \"files\"")
		return
	}
	if len(files) > maxN26FilesPerRequest {
		respondError(c, http.StatusBadRequest, "too_many_files",
			fmt.Sprintf("upload at most %d files per request", maxN26FilesPerRequest))
		return
	}

	ownerID := invoicingUserID(c)
	out := make([]n26ParsedFile, 0, len(files))
	for _, fh := range files {
		entry := n26ParsedFile{Filename: fh.Filename}
		// Set as the loop learns them, read by fail at whichever point it is
		// called (system_3 #10536): data stays nil for a file refused before
		// being fully read, which also means no copy is kept for it, the
		// same cap the import itself just enforced.
		var data []byte
		var documentKind string
		fail := func(code, message string) {
			entry.Error = &n26FileError{Code: code, Message: message}
			entry.CopyExpiresAt = recordImportFailure(db, ownerID, "n26", fh.Filename, fh.Size, data, code, documentKind, 0)
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
		data, err = io.ReadAll(io.LimitReader(f, maxN26PDFBytes+1))
		f.Close()
		if err != nil || int64(len(data)) > maxN26PDFBytes {
			data = nil
			fail(n26ErrTooLarge, "the file is larger than 5 MB")
			continue
		}
		// The CSV export (system_3 #10543) is recognised from its bytes and
		// read as text; everything else must be a PDF, read through the
		// strategy chain. Both then share the path below.
		var text string
		var strategy n26Strategy
		if looksLikeN26CSV(data) {
			text = string(data)
			if he := checkN26CSVHeader(text); he != nil {
				fail(n26ErrCSVHeadersUnrecognised, he.Error())
				continue
			}
			strategy = n26CSVStrategy
			documentKind = strategy.name
		} else {
			text, err = ExtractN26PDFText(data)
			if err == errNotAPDF {
				fail(n26ErrNotAPDF, "the file is neither a PDF nor an N26 CSV export")
				continue
			}
			if err != nil {
				log.Printf("N26 import: extracting %q: %v", fh.Filename, err)
				fail(n26ErrExtractFailed, "could not read the PDF's text")
				continue
			}
			var ok bool
			strategy, ok = detectN26Document(text)
			if !ok {
				fail(n26ErrNotN26Document, "not an N26 transfer confirmation or account statement")
				continue
			}
			documentKind = strategy.name
		}
		// The parser's error quotes the file's values (a date, an amount), so
		// these log lines carry the fixed code only; the uploader's per-file
		// message keeps the detail (system_3 #10583, security c65625).
		cands, err := strategy.parse(text)
		if err != nil {
			log.Printf("N26 import: parsing %q as %s: %s", fh.Filename, strategy.name, n26ErrParseFailed)
			fail(n26ErrParseFailed, fmt.Sprintf("read as %s but a field is missing or malformed: %v", strategy.name, err))
			continue
		}
		cands, entry.Skipped = dropPayPalTransfers(cands)
		if strategy.skipped != nil {
			extra, err := strategy.skipped(text)
			if err != nil {
				log.Printf("N26 import: reading %q's skip reasons: %s", fh.Filename, n26ErrParseFailed)
				fail(n26ErrParseFailed, fmt.Sprintf("read as %s but a field is missing or malformed: %v", strategy.name, err))
				continue
			}
			entry.Skipped = mergeSkipCounts(entry.Skipped, extra)
		}
		if err := flagN26Candidates(db, invoicingUserID(c), cands); err != nil {
			log.Printf("N26 import: flagging candidates of %q: %v", fh.Filename, err)
			RespondWithError(c, http.StatusInternalServerError, "Error checking candidates against stored payments")
			return
		}
		if strategy.expenses != nil {
			expenses, err := strategy.expenses(text)
			if err != nil {
				log.Printf("N26 import: parsing %q's outgoing rows: %s", fh.Filename, n26ErrParseFailed)
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
		// Recognised but nothing usable (system_3 #10536, security ruling
		// c65222): still a failure the operator needs visibility into, not
		// a silent empty response. Not routed through fail(), which sets
		// entry.Error for the uploader -- a 0-row statement is a normal
		// server response, just also a recorded failure. Both candidate
		// lists must be empty: a statement with real outgoing rows and no
		// incoming ones imported correctly and is not a failure (security
		// review F1 on #10536, c65539).
		if len(cands) == 0 && len(entry.ExpenseCandidates) == 0 {
			entry.CopyExpiresAt = recordImportFailure(db, ownerID, "n26", fh.Filename, fh.Size, data, importFailureCodeNoCandidates, documentKind, 0)
		}
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
	priorCounts
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
		if row.DocumentKind != n26KindTransferConfirmation && row.DocumentKind != n26KindAccountStatement && row.DocumentKind != n26KindCSVExport {
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
		prior, err := data_models.InvoicingPaymentPrior(tx, invoicingUserID(c), invoicingPaymentSourceN26, v.row.TransactionID)
		if err != nil {
			tx.Rollback()
			log.Printf("Error checking N26 payment %s: %v", v.row.TransactionID, err)
			RespondWithError(c, http.StatusInternalServerError, "Error storing payments")
			return
		}
		if prior.Seen() {
			summary.Duplicates++
			summary.count(prior)
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
		// Only statement and CSV export rows reach here, and their ids are
		// always derived.
		if !(strings.HasPrefix(row.TransactionID, "stmt-") || strings.HasPrefix(row.TransactionID, n26CSVIDPrefix)) || len(row.TransactionID) > 100 {
			bad("transaction_id is not an N26 statement or CSV export row id")
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
		prior, err := data_models.InvoicingExpensePrior(tx, owner, invoicingPaymentSourceN26, v.row.TransactionID)
		if err != nil {
			tx.Rollback()
			log.Printf("Error checking N26 expense %s: %v", v.row.TransactionID, err)
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
