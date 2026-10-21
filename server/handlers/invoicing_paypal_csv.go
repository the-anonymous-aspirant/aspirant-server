package handlers

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
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
)

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

var spaceRun = regexp.MustCompile(`\s+`)

func normaliseHeader(h string) string {
	h = strings.TrimPrefix(h, "﻿")
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

// ParsePayPalActivityCSV turns a PayPal activity export into payment rows.
// Rows that are not money in (fees, conversions, holds, outgoing payments) are
// counted per reason in Skipped; withdrawals to the bank come back as
// withdrawal rows so payments can be grouped by bank deposit.
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
			Currency:       strings.ToUpper(get(rec, "currency")),
			GrossCents:     gross,
			FeeCents:       fee,
			NetCents:       net,
			SenderName:     get(rec, "name"),
			SenderEmail:    strings.ToLower(get(rec, "from_email")),
			Note:           strings.Join(notes, " | "),
		}
		if kind == paypalKindWithdrawal {
			// A withdrawal is money leaving PayPal for the bank: keep it as
			// a positive deposit amount, and it has no sender.
			row.GrossCents, row.NetCents = -gross, -net
			row.SenderEmail = ""
		}
		res.Rows = append(res.Rows, row)
	}
	return res, nil
}
