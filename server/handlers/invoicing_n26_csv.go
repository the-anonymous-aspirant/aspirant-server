package handlers

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// N26 CSV export for the N26 import (system_3 #10543). The operator ruled the
// CSV the preferred N26 input, with the PDFs as the fallback (system_3 #9656
// c65186). It goes through the same parse endpoint and the same review and
// confirm steps as the PDFs: a CSV row is an ordinary payment or expense
// candidate.
//
// The columns are found by NAME, through a per-language header table, never
// by position. The only real export seen so far has English headers, so the
// table holds English only. An export in a language the table does not know
// fails loudly (n26ErrCSVHeadersUnrecognised, naming the headers it saw)
// rather than being guessed at (system_3 #10534 c65204). The first such file
// is the sample that adds the next language.

const (
	n26KindCSVExport = "n26_csv_export"

	// n26ErrCSVHeadersUnrecognised is the per-file error for a CSV whose
	// header row matches no language in n26CSVLanguages.
	n26ErrCSVHeadersUnrecognised = "csv_headers_unrecognised"

	// n26CSVIDPrefix marks a CSV row's derived transaction id, beside the
	// statement's "stmt-".
	n26CSVIDPrefix = "csv-"

	// The confirm endpoints' limits, which a CSV row is cleaned to fit, so
	// that no offered row can be refused at confirm.
	n26CSVMaxNameRunes = maxManualReferenceRunes // payee_name: 100 runes (sender_name allows 200 bytes)
	n26CSVMaxNameBytes = 200
	n26CSVMaxNoteBytes = 500
)

// n26CSVField is one column the parser reads.
type n26CSVField int

const (
	n26CSVBookingDate n26CSVField = iota
	n26CSVValueDate
	n26CSVPartnerName
	n26CSVPartnerIBAN
	n26CSVType
	n26CSVReference
	n26CSVAccountName
	n26CSVAmount
)

// n26CSVLanguage is one language's header row: the text of each column the
// parser needs, and the formats its values are written in. Every listed
// column must be present for the language to match.
type n26CSVLanguage struct {
	name       string
	headers    map[n26CSVField]string
	currency   string // the currency the amount column is in (its header names it)
	dateLayout string
	amount     *regexp.Regexp // a signed decimal amount in this language's notation
}

// n26CSVLanguages is the per-language header table. English is proven on the
// operator's own export (system_3 #9656 upload 129: "Booking Date","Value
// Date","Partner Name","Partner Iban","Type","Payment Reference","Account
// Name","Amount (EUR)","Original Amount","Original Currency","Exchange
// Rate"; ISO dates, decimal point). A language is added only from a real
// file, never guessed: a wrong guess could map two columns silently.
var n26CSVLanguages = []n26CSVLanguage{
	{
		name: "en",
		headers: map[n26CSVField]string{
			n26CSVBookingDate: "Booking Date",
			n26CSVValueDate:   "Value Date",
			n26CSVPartnerName: "Partner Name",
			n26CSVPartnerIBAN: "Partner Iban",
			n26CSVType:        "Type",
			n26CSVReference:   "Payment Reference",
			n26CSVAccountName: "Account Name",
			n26CSVAmount:      "Amount (EUR)",
		},
		currency:   "EUR",
		dateLayout: "2006-01-02",
		amount:     regexp.MustCompile(`^([+-]?)(\d+)(?:\.(\d{1,2}))?$`),
	},
}

// n26CSVHeaderError is a CSV whose header row no language matches.
type n26CSVHeaderError struct {
	seen []string
}

func (e *n26CSVHeaderError) Error() string {
	shown := make([]string, 0, len(e.seen))
	for i, h := range e.seen {
		if i == 20 {
			shown = append(shown, "…")
			break
		}
		shown = append(shown, fmt.Sprintf("%q", truncateRunes(cleanN26CSVText(h), 40)))
	}
	return "column names not recognised: " + strings.Join(shown, ", ")
}

// looksLikeN26CSV reports whether an upload is CSV text rather than a PDF:
// valid UTF-8 with no NUL byte, whose first record parses into at least two
// fields. A one-field text (no delimiter at all) is not taken for a CSV, so
// it keeps the plain "not a PDF" answer.
func looksLikeN26CSV(data []byte) bool {
	if bytes.HasPrefix(data, []byte("%PDF-")) || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return false
	}
	r := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
	r.FieldsPerRecord = -1
	first, err := r.Read()
	return err == nil && len(first) >= 2
}

// n26CSVRow is one data row, read by column name.
type n26CSVRow struct {
	account, partner, iban, typ, reference string
	bookingDate, valueDate                 string
	cents                                  int64
	paidOn                                 time.Time
}

// readN26CSV parses the whole file: the header row through the language
// table, then every data row. Any malformed row fails the file; nothing is
// guessed.
func readN26CSV(text string) ([]n26CSVRow, *n26CSVLanguage, error) {
	r := csv.NewReader(strings.NewReader(strings.TrimPrefix(text, "\ufeff")))
	r.FieldsPerRecord = 0 // every row has the header's width
	header, err := r.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("header row: %w", err)
	}
	lang, cols := matchN26CSVHeader(header)
	if lang == nil {
		return nil, nil, &n26CSVHeaderError{seen: header}
	}
	var rows []n26CSVRow
	for line := 2; ; line++ {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("row %d: %w", line, err)
		}
		get := func(f n26CSVField) string { return strings.TrimSpace(rec[cols[f]]) }
		row := n26CSVRow{
			account:     get(n26CSVAccountName),
			partner:     get(n26CSVPartnerName),
			iban:        get(n26CSVPartnerIBAN),
			typ:         get(n26CSVType),
			reference:   get(n26CSVReference),
			bookingDate: get(n26CSVBookingDate),
			valueDate:   get(n26CSVValueDate),
		}
		row.cents, err = parseN26CSVAmountCents(lang, get(n26CSVAmount))
		if err != nil {
			return nil, nil, fmt.Errorf("row %d: %w", line, err)
		}
		// The value date is the day the money moved, as the PDF statement's
		// Wertstellung (system_3 #10534); the booking date backs it up.
		dateStr := row.valueDate
		if dateStr == "" {
			dateStr = row.bookingDate
		}
		row.paidOn, err = time.Parse(lang.dateLayout, dateStr)
		if err != nil {
			return nil, nil, fmt.Errorf("row %d: date %q is not %s", line, truncateRunes(dateStr, 20), lang.dateLayout)
		}
		rows = append(rows, row)
	}
	return rows, lang, nil
}

// matchN26CSVHeader returns the first language whose every column is in the
// header row exactly once, with each column's index. Matching ignores case
// and surrounding spaces; it never falls back to a column's position.
func matchN26CSVHeader(header []string) (*n26CSVLanguage, map[n26CSVField]int) {
	for li := range n26CSVLanguages {
		lang := &n26CSVLanguages[li]
		cols := map[n26CSVField]int{}
		ok := true
		for field, want := range lang.headers {
			found := -1
			for i, h := range header {
				if strings.EqualFold(strings.TrimSpace(h), want) {
					if found >= 0 { // the same column twice is ambiguous
						found = -2
						break
					}
					found = i
				}
			}
			if found < 0 {
				ok = false
				break
			}
			cols[field] = found
		}
		if ok {
			return lang, cols
		}
	}
	return nil, nil
}

// parseN26CSVAmountCents reads a signed decimal amount ("-12.34", "50") into
// cents, in the notation the language table names.
func parseN26CSVAmountCents(lang *n26CSVLanguage, s string) (int64, error) {
	m := lang.amount.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("amount %q is not a %s amount", truncateRunes(s, 20), lang.name)
	}
	whole, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil || whole > maxUnitPriceCents/100 {
		return 0, fmt.Errorf("amount %q is out of range", truncateRunes(s, 20))
	}
	frac := m[3]
	for len(frac) < 2 {
		frac += "0"
	}
	f, _ := strconv.ParseInt(frac, 10, 64)
	cents := whole*100 + f
	if m[1] == "-" {
		cents = -cents
	}
	return cents, nil
}

// n26CSVInternalRow reports whether a row is money moving between the account
// holder's own main account and Spaces. One CSV export holds every account,
// and such a transfer appears twice, once on each account, with the other
// account's name as its partner and no IBAN. Checked against the operator's
// real export (system_3 #10543): all 48 such rows have no IBAN, and every
// external transfer, direct debit and MoneyBeam row carries one. Both
// conditions are required, so a real counterparty that happens to share a
// Space's name, but has an IBAN, stays a candidate.
func n26CSVInternalRow(row n26CSVRow, accounts map[string]bool) bool {
	return row.iban == "" && accounts[row.partner]
}

// scanN26CSV reads the export's money-in rows as payment candidates and its
// money-out rows as expense candidates, leaving out zero-amount rows and the
// internal Space transfers, which it counts.
func scanN26CSV(text string) ([]n26Candidate, []n26ExpenseCandidate, int, error) {
	rows, lang, err := readN26CSV(text)
	if err != nil {
		return nil, nil, 0, err
	}
	accounts := map[string]bool{}
	for _, row := range rows {
		if row.account != "" {
			accounts[row.account] = true
		}
	}

	var in []n26Candidate
	var out []n26ExpenseCandidate
	skippedInternal := 0
	counts := map[string]int{}
	// The id hashes every field that tells two rows apart, plus the row's
	// occurrence number among identical tuples, so two genuinely separate
	// but identical rows stay two rows, while a re-upload of the same file
	// gives the same ids (the statement's scheme, security review F1, #9812).
	rowID := func(row n26CSVRow) string {
		key := strings.Join([]string{row.account, row.bookingDate, row.valueDate,
			strconv.FormatInt(row.cents, 10), row.partner, row.iban, row.reference}, "|")
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d", key, counts[key])))
		counts[key]++
		return n26CSVIDPrefix + hex.EncodeToString(sum[:])[:24]
	}

	for _, row := range rows {
		if row.cents == 0 {
			continue
		}
		if n26CSVInternalRow(row, accounts) {
			skippedInternal++
			continue
		}
		id := rowID(row)
		name := n26CSVName(row)
		note := truncateBytes(cleanN26CSVText(row.reference), n26CSVMaxNoteBytes)
		paidOn := row.paidOn.Format("2006-01-02")
		if row.cents > 0 {
			in = append(in, n26Candidate{
				DocumentKind:  n26KindCSVExport,
				TransactionID: id,
				PaidOn:        paidOn,
				Currency:      lang.currency,
				GrossCents:    row.cents,
				NetCents:      row.cents,
				SenderName:    name,
				Note:          note,
			})
		} else {
			out = append(out, n26ExpenseCandidate{
				TransactionID: id,
				PaidOn:        paidOn,
				Currency:      lang.currency,
				AmountCents:   -row.cents,
				PayeeName:     name,
				Note:          note,
			})
		}
	}
	return in, out, skippedInternal, nil
}

// n26CSVName is the row's counterparty, cleaned to the confirm endpoints'
// one-line name rule. A row with no partner name falls back to its type
// ("Direct Debit"), then to "N26", so an offered row is never refused at
// confirm for an empty name.
func n26CSVName(row n26CSVRow) string {
	for _, s := range []string{row.partner, row.typ, "N26"} {
		if name := truncateBytes(truncateRunes(cleanN26CSVText(s), n26CSVMaxNameRunes), n26CSVMaxNameBytes); name != "" {
			return name
		}
	}
	return "N26"
}

// cleanN26CSVText drops control and format characters and collapses every
// run of whitespace (a quoted CSV field may hold line breaks) to one space.
func cleanN26CSVText(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && !unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// truncateRunes cuts s to at most n runes.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// truncateBytes cuts s to at most n bytes at a rune boundary.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return strings.TrimSpace(s[:n])
}

func parseN26CSVPayments(text string) ([]n26Candidate, error) {
	in, _, _, err := scanN26CSV(text)
	return in, err
}

func parseN26CSVExpenses(text string) ([]n26ExpenseCandidate, error) {
	_, out, _, err := scanN26CSV(text)
	return out, err
}

func parseN26CSVSkipped(text string) (map[string]int, error) {
	_, _, internal, err := scanN26CSV(text)
	if err != nil || internal == 0 {
		return nil, err
	}
	return map[string]int{n26SkipInternalTransfer: internal}, nil
}

// n26CSVStrategy is the CSV export's strategy. It is not in n26Strategies,
// which detect a kind from a PDF's extracted text: a CSV is recognised from
// its bytes (looksLikeN26CSV) before any PDF extraction.
var n26CSVStrategy = n26Strategy{
	name:     n26KindCSVExport,
	matches:  func(string) bool { return true },
	parse:    parseN26CSVPayments,
	expenses: parseN26CSVExpenses,
	skipped:  parseN26CSVSkipped,
}

// checkN26CSVHeader reports a header the language table does not know, so the
// handler can answer with its own error code before the shared parse path.
func checkN26CSVHeader(text string) *n26CSVHeaderError {
	_, _, err := readN26CSV(text)
	var he *n26CSVHeaderError
	if errors.As(err, &he) {
		return he
	}
	return nil
}
