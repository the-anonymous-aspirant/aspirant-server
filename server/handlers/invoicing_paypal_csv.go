package handlers

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"aspirant-online/server/data_models"
)

// PayPal activity CSV parsing for the invoicing app (system_3 #8367-B1).
//
// Built against PayPal's documented Activity Download layout
// (developer.paypal.com/docs/reports/online-reports/activity-download) until
// the operator's anonymised sample arrives (#8367 decision 3). Columns are
// found by header name, never by position, in English or German, because the
// export's language and column set follow the account's settings. Nothing is
// guessed: a file whose dates cannot be read unambiguously is refused rather
// than imported with a coin-flip day/month order (spec §10).

// Payment kinds a PayPal row can import as.
const (
	paypalKindIncoming   = "incoming"
	paypalKindWithdrawal = "withdrawal"
)

// Skip reasons reported in the import summary.
const (
	paypalSkipNotCompleted = "not_completed"
	paypalSkipConversion   = "currency_conversion"
	paypalSkipHold         = "hold_or_authorization"
	paypalSkipBankTopUp    = "bank_topup"
	paypalSkipNonPositive  = "zero_or_negative_credit"
	paypalSkipFee          = "fee"
	paypalSkipOutgoing     = "outgoing"
	paypalSkipMemo         = "memo"
	paypalSkipNoID         = "missing_transaction_id"
	paypalSkipReversal     = "reversal"
	paypalSkipReversed     = "reversed"
)

// Date orders accepted for NN/NN/YYYY dates.
const (
	paypalDateOrderAuto = ""
	paypalDateOrderDMY  = "dmy"
	paypalDateOrderMDY  = "mdy"
)

// ErrPayPalDateOrderAmbiguous is returned when every slash date in the file
// reads both ways; the caller must pass the order explicitly.
var ErrPayPalDateOrderAmbiguous = errors.New("every date in the file reads as both day/month and month/day; re-upload with date_order=dmy or date_order=mdy")

type paypalRow struct {
	Kind           string
	TransactionID  string
	ReferenceTxnID string
	PaidOn         time.Time
	Currency       string
	GrossCents     int64
	FeeCents       int64
	NetCents       int64
	SenderName     string
	SenderEmail    string
	Note           string
	// Category is the payment category the row imports with: uncategorised,
	// or refund for money returned to her (#9862-A1).
	Category string
}

type paypalParseResult struct {
	Rows     []paypalRow
	RowsRead int
	Skipped  map[string]int
}

// paypalColumns maps each field to the header spellings it may carry, already
// normalised (lower case, single spaces). English from PayPal's report spec,
// German from the localised export.
var paypalColumns = map[string][]string{
	"date":           {"date", "datum"},
	"name":           {"name"},
	"type":           {"type", "typ", "beschreibung"},
	"status":         {"status"},
	"currency":       {"currency", "währung"},
	"gross":          {"gross", "brutto"},
	"fee":            {"fee", "gebühr"},
	"net":            {"net", "netto"},
	"from_email":     {"from email address", "absender e-mail-adresse", "absender-e-mail-adresse", "von e-mail-adresse"},
	"transaction_id": {"transaction id", "transaktionscode", "transaktions-id", "transaktions id"},
	"reference_id":   {"reference txn id", "zugehöriger transaktionscode"},
	"subject":        {"subject", "betreff"},
	"note":           {"note", "hinweis"},
	"item_title":     {"item title", "artikelbezeichnung"},
	"balance_impact": {"balance impact", "auswirkung auf guthaben"},
}

var paypalRequiredColumns = []string{"date", "name", "type", "status", "currency", "gross", "transaction_id"}

// Classification vocabularies, matched as lower-cased substrings of Type and
// Status. PayPal's Type is a free-text T-code description (report spec), so
// these lists are the tested fixture the operator's sample will extend.
var (
	paypalCompletedStatuses = []string{"completed", "abgeschlossen", "erfolgreich"}
	paypalConversionTypes   = []string{"currency conversion", "währungsumrechnung"}
	paypalHoldTypes         = []string{"hold", "einbehalt", "reserve", "authorization", "autorisierung"}
	paypalBankTopUpTypes    = []string{"bank deposit to pp account", "bankgutschrift", "einzahlung auf", "aufladung"}
	paypalWithdrawalTypes   = []string{"withdrawal", "abbuchung", "auszahlung", "transfer to bank", "überweisung auf bankkonto"}
	paypalFeeTypes          = []string{"fee", "gebühr"}
	paypalCreditImpacts     = []string{"credit", "haben", "gutschrift"}
	paypalDebitImpacts      = []string{"debit", "soll", "belastung"}
	paypalMemoImpacts       = []string{"memo"}
	// From the Type strings on a real statement (system_3 #9862, upload 112):
	// "Reversal of ACH Withdrawal Transaction" credits back a bounced
	// withdrawal under the withdrawal's own transaction id, and "Payment
	// Refund" is money returned, not earned. The German spellings are PayPal
	// UI terms, not yet seen in a real export.
	paypalReversalTypes = []string{"reversal", "storno", "rückbuchung"}
	paypalRefundTypes   = []string{"refund", "rückzahlung", "erstattung"}
)

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

var (
	spaceRun    = regexp.MustCompile(`\s+`)
	isoCurrency = regexp.MustCompile(`^[A-Z]{3}$`)
)

func normaliseHeader(h string) string {
	h = strings.TrimPrefix(h, "\ufeff")
	h = strings.Trim(h, "\" ")
	return strings.ToLower(spaceRun.ReplaceAllString(strings.TrimSpace(h), " "))
}

// windows1252High maps 0x80-0x9F, the only range where Windows-1252 differs
// from Latin-1 (0x81, 0x8D, 0x8F, 0x90, 0x9D are unassigned and kept as-is).
var windows1252High = [32]rune{
	'€', 0x81, '‚', 'ƒ', '„', '…', '†', '‡', 'ˆ', '‰', 'Š', '‹', 'Œ', 0x8D, 'Ž', 0x8F,
	0x90, '‘', '’', '“', '”', '•', '–', '—', '˜', '™', 'š', '›', 'œ', 0x9D, 'ž', 'Ÿ',
}

// toUTF8 accepts UTF-8 (with or without BOM) and falls back to Windows-1252,
// the encoding older Excel round-trips leave behind.
func toUTF8(b []byte) string {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	sb.Grow(len(b) + len(b)/8)
	for _, c := range b {
		switch {
		case c < 0x80:
			sb.WriteByte(c)
		case c < 0xA0:
			sb.WriteRune(windows1252High[c-0x80])
		default:
			sb.WriteRune(rune(c))
		}
	}
	return sb.String()
}

// sniffDelimiter picks the separator that splits the header line most.
func sniffDelimiter(headerLine string) rune {
	best, bestN := ',', strings.Count(headerLine, ",")
	for _, d := range []rune{';', '\t'} {
		if n := strings.Count(headerLine, string(d)); n > bestN {
			best, bestN = d, n
		}
	}
	return best
}

// parseCents reads a localised money string ("1.234,56", "1,234.56", "-0,35",
// "20.00", "€ 5") into integer cents without going through a float.
func parseCents(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	s = strings.NewReplacer(" ", "", " ", "", "€", "", "$", "", "£", "", "'", "").Replace(s)
	if s == "" {
		return 0, nil
	}
	neg := false
	if strings.HasPrefix(s, "-") {
		neg, s = true, s[1:]
	} else if strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	intPart, frac := s, ""
	lastComma, lastDot := strings.LastIndex(s, ","), strings.LastIndex(s, ".")
	decimalAt := -1
	switch {
	case lastComma >= 0 && lastDot >= 0:
		decimalAt = lastComma
		if lastDot > lastComma {
			decimalAt = lastDot
		}
	case lastComma >= 0 || lastDot >= 0:
		at := lastComma
		if lastDot >= 0 {
			at = lastDot
		}
		// One separator: a 1-2 digit tail is decimals, a 3-digit tail is a
		// thousands group ("1.234" is one thousand two hundred thirty-four).
		if tail := len(s) - at - 1; tail == 1 || tail == 2 {
			decimalAt = at
		}
	}
	if decimalAt >= 0 {
		intPart, frac = s[:decimalAt], s[decimalAt+1:]
	}
	intPart = strings.NewReplacer(",", "", ".", "").Replace(intPart)
	if intPart == "" {
		intPart = "0"
	}
	if len(frac) > 2 {
		return 0, fmt.Errorf("amount %q has more than two decimals", raw)
	}
	for len(frac) < 2 {
		frac += "0"
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount %q is not a number", raw)
	}
	cents, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount %q is not a number", raw)
	}
	v := whole*100 + cents
	if neg {
		v = -v
	}
	return v, nil
}

var (
	dottedDate = regexp.MustCompile(`^(\d{1,2})\.(\d{1,2})\.(\d{4})$`)
	isoDate    = regexp.MustCompile(`^(\d{4})-(\d{1,2})-(\d{1,2})$`)
	slashDate  = regexp.MustCompile(`^(\d{1,2})/(\d{1,2})/(\d{4})$`)
)

// resolveDateOrder decides how NN/NN/YYYY dates read across the whole file.
func resolveDateOrder(dates []string, requested string) (string, error) {
	if requested == paypalDateOrderDMY || requested == paypalDateOrderMDY {
		return requested, nil
	}
	if requested != paypalDateOrderAuto {
		return "", fmt.Errorf("date_order must be dmy or mdy, got %q", requested)
	}
	sawSlash, firstOver12, secondOver12 := false, false, false
	for _, d := range dates {
		m := slashDate.FindStringSubmatch(strings.TrimSpace(d))
		if m == nil {
			continue
		}
		sawSlash = true
		a, _ := strconv.Atoi(m[1])
		b, _ := strconv.Atoi(m[2])
		firstOver12 = firstOver12 || a > 12
		secondOver12 = secondOver12 || b > 12
	}
	switch {
	case !sawSlash:
		return paypalDateOrderAuto, nil
	case firstOver12 && secondOver12:
		return "", errors.New("dates in the file disagree on day/month order")
	case firstOver12:
		return paypalDateOrderDMY, nil
	case secondOver12:
		return paypalDateOrderMDY, nil
	default:
		return "", ErrPayPalDateOrderAmbiguous
	}
}

func parsePayPalDate(raw, order string) (time.Time, error) {
	s := strings.TrimSpace(raw)
	var y, mo, d int
	if m := dottedDate.FindStringSubmatch(s); m != nil {
		d, _ = strconv.Atoi(m[1])
		mo, _ = strconv.Atoi(m[2])
		y, _ = strconv.Atoi(m[3])
	} else if m := isoDate.FindStringSubmatch(s); m != nil {
		y, _ = strconv.Atoi(m[1])
		mo, _ = strconv.Atoi(m[2])
		d, _ = strconv.Atoi(m[3])
	} else if m := slashDate.FindStringSubmatch(s); m != nil {
		a, _ := strconv.Atoi(m[1])
		b, _ := strconv.Atoi(m[2])
		y, _ = strconv.Atoi(m[3])
		if order == paypalDateOrderMDY {
			mo, d = a, b
		} else {
			d, mo = a, b
		}
	} else {
		return time.Time{}, fmt.Errorf("date %q is not in a recognised format", raw)
	}
	t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
	if t.Year() != y || int(t.Month()) != mo || t.Day() != d {
		return time.Time{}, fmt.Errorf("date %q is not a calendar date", raw)
	}
	return t, nil
}

// mapPayPalColumns finds each field's column by header name.
func mapPayPalColumns(header []string) (map[string]int, error) {
	col := map[string]int{}
	for i, h := range header {
		n := normaliseHeader(h)
		for field, aliases := range paypalColumns {
			if _, seen := col[field]; seen {
				continue
			}
			for _, a := range aliases {
				if n == a {
					col[field] = i
				}
			}
		}
	}
	var missing []string
	for _, f := range paypalRequiredColumns {
		if _, ok := col[f]; !ok {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("not a PayPal activity export: missing column(s) %s", strings.Join(missing, ", "))
	}
	return col, nil
}

// ParsePayPalActivityCSV turns a PayPal activity CSV into payment rows.
func ParsePayPalActivityCSV(data []byte, dateOrder string) (*paypalParseResult, error) {
	text := toUTF8(data)
	firstLine := text
	if i := strings.IndexAny(text, "\r\n"); i >= 0 {
		firstLine = text[:i]
	}
	r := csv.NewReader(strings.NewReader(text))
	r.Comma = sniffDelimiter(firstLine)
	r.FieldsPerRecord = -1
	r.LazyQuotes = true

	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("could not read the header row: %w", err)
	}
	col, err := mapPayPalColumns(header)
	if err != nil {
		return nil, err
	}

	var records [][]string
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("malformed CSV: %w", err)
		}
		if len(rec) == 1 && strings.TrimSpace(rec[0]) == "" {
			continue
		}
		records = append(records, rec)
	}
	return parsePayPalRecords(col, records, dateOrder)
}

// excelSerialDate converts an Excel date serial to a calendar date. In the
// 1900 system, day 1 is 1900-01-01 and Excel counts a 29 February 1900 that
// never was, so from 1 March 1900 on the serial counts from 1899-12-30. The
// 1904 system counts from 1904-01-01. The fraction is the time of day.
func excelSerialDate(serial float64, date1904 bool) (time.Time, error) {
	if serial < 61 && !date1904 || serial < 0 || serial > 2958465 {
		return time.Time{}, fmt.Errorf("date serial %v is outside the dates a payment can have", serial)
	}
	epoch := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)
	if date1904 {
		epoch = time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return epoch.AddDate(0, 0, int(math.Floor(serial))), nil
}

// ParsePayPalActivityXLSX turns a PayPal activity Excel file into payment
// rows. The first non-empty row of the first worksheet is the header, read by
// name as in the CSV. A numeric date cell is an Excel date serial, and a
// numeric money cell is rounded to cents (Excel stores 17.72 as
// 17.719999999999999).
func ParsePayPalActivityXLSX(data []byte, dateOrder string) (*paypalParseResult, error) {
	sheet, err := readXLSXFirstSheet(data)
	if err != nil {
		return nil, err
	}
	empty := func(row []xlsxInCell) bool {
		for _, c := range row {
			if strings.TrimSpace(c.Text) != "" {
				return false
			}
		}
		return true
	}
	rows := sheet.Rows
	for len(rows) > 0 && empty(rows[0]) {
		rows = rows[1:]
	}
	if len(rows) == 0 {
		return nil, errors.New("the worksheet is empty")
	}
	header := make([]string, len(rows[0]))
	for i, c := range rows[0] {
		header[i] = c.Text
	}
	col, err := mapPayPalColumns(header)
	if err != nil {
		return nil, err
	}
	money := map[int]bool{}
	for _, f := range []string{"gross", "fee", "net"} {
		if i, ok := col[f]; ok {
			money[i] = true
		}
	}
	var records [][]string
	for _, row := range rows[1:] {
		if empty(row) {
			continue
		}
		rec := make([]string, len(row))
		for i, c := range row {
			switch {
			case !c.IsNum:
				rec[i] = c.Text
			case i == col["date"]:
				d, err := excelSerialDate(c.Num, sheet.Date1904)
				if err != nil {
					return nil, err
				}
				rec[i] = d.Format("2006-01-02")
			case money[i]:
				rec[i] = strconv.FormatFloat(math.Round(c.Num*100)/100, 'f', 2, 64)
			default:
				rec[i] = c.Text
			}
		}
		records = append(records, rec)
	}
	return parsePayPalRecords(col, records, dateOrder)
}

// parsePayPalRecords classifies the records of either export format. Rows
// that are not money in (fees, conversions, holds, outgoing payments) are
// counted per reason in Skipped; withdrawals to the bank come back as
// withdrawal rows so payments can be grouped by bank deposit. A reversal
// cancels the row it reverses, so a bounced withdrawal or a reversed payment
// imports as neither row.
func parsePayPalRecords(col map[string]int, records [][]string, dateOrder string) (*paypalParseResult, error) {
	get := func(rec []string, field string) string {
		i, ok := col[field]
		if !ok || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}

	dates := make([]string, 0, len(records))
	for _, rec := range records {
		dates = append(dates, get(rec, "date"))
	}
	order, err := resolveDateOrder(dates, dateOrder)
	if err != nil {
		return nil, err
	}

	res := &paypalParseResult{RowsRead: len(records), Skipped: map[string]int{}}
	// reversals maps a transaction id to the signed gross of each reversal
	// row naming it, by its own id (a reversal can carry the id of the row it
	// reverses) or by its reference id.
	reversals := map[string][]int64{}
	var candidates []paypalRow
	for i, rec := range records {
		line := i + 2 // 1-based, after the header
		typ := strings.ToLower(get(rec, "type"))
		status := strings.ToLower(get(rec, "status"))
		impact := strings.ToLower(get(rec, "balance_impact"))

		gross, err := parseCents(get(rec, "gross"))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		fee, err := parseCents(get(rec, "fee"))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		net, err := parseCents(get(rec, "net"))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if _, hasNet := col["net"]; !hasNet {
			net = gross + fee
		}

		// Without a Balance Impact column, the sign of Gross says which way
		// the money moved.
		credit, debit, memo := containsAny(impact, paypalCreditImpacts), containsAny(impact, paypalDebitImpacts), containsAny(impact, paypalMemoImpacts)
		if impact == "" {
			credit, debit = gross > 0, gross < 0
		}

		kind := ""
		switch {
		case !containsAny(status, paypalCompletedStatuses):
			res.Skipped[paypalSkipNotCompleted]++
		case containsAny(typ, paypalReversalTypes):
			res.Skipped[paypalSkipReversal]++
			for _, id := range []string{get(rec, "transaction_id"), get(rec, "reference_id")} {
				if id != "" {
					reversals[id] = append(reversals[id], gross)
				}
			}
		case containsAny(typ, paypalConversionTypes):
			res.Skipped[paypalSkipConversion]++
		case containsAny(typ, paypalHoldTypes):
			res.Skipped[paypalSkipHold]++
		case memo:
			res.Skipped[paypalSkipMemo]++
		case credit && containsAny(typ, paypalBankTopUpTypes):
			res.Skipped[paypalSkipBankTopUp]++
		case credit && gross <= 0:
			res.Skipped[paypalSkipNonPositive]++
		case credit:
			kind = paypalKindIncoming
		case debit && containsAny(typ, paypalWithdrawalTypes):
			kind = paypalKindWithdrawal
		case debit && containsAny(typ, paypalFeeTypes):
			res.Skipped[paypalSkipFee]++
		default:
			res.Skipped[paypalSkipOutgoing]++
		}
		if kind == "" {
			continue
		}

		txnID := get(rec, "transaction_id")
		if txnID == "" {
			res.Skipped[paypalSkipNoID]++
			continue
		}
		paidOn, err := parsePayPalDate(get(rec, "date"), order)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		// The currency is printed and exported, so it must be a real ISO
		// code, not free text from the file (@security N6, c51502 on #8374).
		currency := strings.ToUpper(get(rec, "currency"))
		if !isoCurrency.MatchString(currency) {
			return nil, fmt.Errorf("line %d: currency %q is not a three-letter ISO code", line, get(rec, "currency"))
		}

		var notes []string
		for _, f := range []string{"item_title", "subject", "note"} {
			if v := get(rec, f); v != "" {
				notes = append(notes, v)
			}
		}
		row := paypalRow{
			Kind:           kind,
			TransactionID:  txnID,
			ReferenceTxnID: get(rec, "reference_id"),
			PaidOn:         paidOn,
			Currency:       currency,
			GrossCents:     gross,
			FeeCents:       fee,
			NetCents:       net,
			SenderName:     get(rec, "name"),
			SenderEmail:    strings.ToLower(get(rec, "from_email")),
			Note:           strings.Join(notes, " | "),
			Category:       data_models.PaymentCategoryUncategorised,
		}
		if kind == paypalKindIncoming && containsAny(typ, paypalRefundTypes) {
			row.Category = data_models.PaymentCategoryRefund
		}
		if kind == paypalKindWithdrawal {
			// A withdrawal is money leaving PayPal for the bank: keep it as
			// a positive deposit amount, and it has no sender.
			row.GrossCents, row.NetCents = -gross, -net
			row.SenderEmail = ""
		}
		candidates = append(candidates, row)
	}
	// A reversal may come before or after the row it reverses in the file,
	// so the cancelling runs once every row is classified. A reversal undoes
	// a row only when it moves the same amount the other way: a "Chargeback
	// Reversal" returns money to her under the original payment's reference,
	// and that payment stands.
	for _, row := range candidates {
		signed := row.GrossCents
		if row.Kind == paypalKindWithdrawal {
			signed = -signed
		}
		undone := false
		for _, g := range reversals[row.TransactionID] {
			if g == -signed {
				undone = true
			}
		}
		if undone {
			res.Skipped[paypalSkipReversed]++
			continue
		}
		res.Rows = append(res.Rows, row)
	}
	return res, nil
}
