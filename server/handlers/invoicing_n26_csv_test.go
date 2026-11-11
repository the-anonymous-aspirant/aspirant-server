package handlers

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"aspirant-online/server/data_models"
)

// system_3 #10543: N26's CSV export as an N26 import input. The fixture is a
// synthetic file in the layout of the operator's real export (system_3 #9656
// upload 129): the same header row, quoting and value formats, with invented
// names, IBANs and amounts. No real file is read by these tests.

func csvFixture(t *testing.T) string {
	t.Helper()
	return readN26Fixture(t, "n26_export_en.csv")
}

func TestN26CSVParse(t *testing.T) {
	in, out, internal, err := scanN26CSV(csvFixture(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Money in: Mara Esempio, the PayPal credit (left out later by
	// dropPayPalTransfers, as for the PDFs), and the counterparty that shares
	// a Space's name but carries an IBAN.
	if len(in) != 3 {
		t.Fatalf("in: %d %+v", len(in), in)
	}
	mara := in[0]
	if mara.DocumentKind != n26KindCSVExport || mara.SenderName != "Mara Esempio" || mara.GrossCents != 120000 ||
		mara.NetCents != 120000 || mara.Currency != "EUR" || mara.Note != "Rechnung 7" ||
		!strings.HasPrefix(mara.TransactionID, n26CSVIDPrefix) {
		t.Fatalf("mara: %+v", mara)
	}
	// The value date is the paid date, not the booking date (2026-10-02).
	if mara.PaidOn != "2026-10-03" {
		t.Errorf("paid_on %q, want the value date 2026-10-03", mara.PaidOn)
	}
	if in[2].SenderName != "Rücklagen" || in[2].GrossCents != 8000 || in[2].PaidOn != "2026-10-12" {
		t.Errorf("a counterparty named like a Space, with an IBAN, must stay a candidate: %+v", in[2])
	}

	// Money out: the direct debit, the card payment (its EUR amount, not the
	// original USD one), the two identical kiosk rows, and the external
	// payment out of the Space. The zero-amount row is dropped.
	wantOut := []struct {
		payee string
		cents int64
	}{{"Stadtwerke Beispiel", 4510}, {"Cafe Beispiel", 350}, {"Kiosk Beispiel", 200}, {"Kiosk Beispiel", 200}, {"Broker Beispiel", 5000}}
	if len(out) != len(wantOut) {
		t.Fatalf("out: %d %+v", len(out), out)
	}
	for i, w := range wantOut {
		if out[i].PayeeName != w.payee || out[i].AmountCents != w.cents || out[i].Currency != "EUR" {
			t.Errorf("out[%d] = %+v, want %s %d", i, out[i], w.payee, w.cents)
		}
	}

	// Both halves of the main-to-Space transfer, and nothing else.
	if internal != 2 {
		t.Errorf("space transfers: %d, want 2", internal)
	}
}

func TestN26CSVInternalRuleNeedsBothConditions(t *testing.T) {
	accounts := map[string]bool{"Hauptkonto": true, "Rücklagen": true}
	cases := []struct {
		name string
		row  n26CSVRow
		want bool
	}{
		{"partner is an account, no IBAN", n26CSVRow{partner: "Rücklagen"}, true},
		{"partner is an account, with an IBAN", n26CSVRow{partner: "Rücklagen", iban: "DE00123"}, false},
		{"no IBAN, partner not an account (a card payment)", n26CSVRow{partner: "Cafe Beispiel"}, false},
	}
	for _, tc := range cases {
		if got := n26CSVInternalRow(tc.row, accounts); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestN26CSVSkippedCounts(t *testing.T) {
	db := setupN26ExpensesDB(t)
	r := setupN26Router(db, 7)
	w := uploadN26(t, r, map[string][]byte{"export.csv": []byte(csvFixture(t))})
	if w.Code != http.StatusOK {
		t.Fatalf("parse: %d %s", w.Code, w.Body.String())
	}
	resp := decodeN26Parse(t, w)
	if len(resp.Files) != 1 || resp.Files[0].Error != nil {
		t.Fatalf("files: %+v", resp.Files)
	}
	f := resp.Files[0]
	if f.DocumentKind != n26KindCSVExport {
		t.Errorf("document_kind %q", f.DocumentKind)
	}
	if len(f.Candidates) != 2 || len(f.ExpenseCandidates) != 5 {
		t.Errorf("candidates %d, expense candidates %d", len(f.Candidates), len(f.ExpenseCandidates))
	}
	if f.Skipped[n26SkipInternalTransfer] != 2 || f.Skipped[n26SkipPayPalTransfer] != 1 || len(f.Skipped) != 2 {
		t.Errorf("skipped %+v", f.Skipped)
	}
}

func TestN26CSVHeaders(t *testing.T) {
	text := csvFixture(t)

	// A BOM in front of the header changes nothing.
	withBOM, _, _, err := scanN26CSV("\ufeff" + text)
	plain, _, _, _ := scanN26CSV(text)
	if err != nil || len(withBOM) != len(plain) || withBOM[0] != plain[0] {
		t.Errorf("BOM: %v %+v", err, withBOM)
	}

	// Columns are found by name: the same file with its columns reversed
	// reads the same rows.
	records, err := csv.NewReader(strings.NewReader(text)).ReadAll()
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	var buf strings.Builder
	cw := csv.NewWriter(&buf)
	for _, rec := range records {
		for i, j := 0, len(rec)-1; i < j; i, j = i+1, j-1 {
			rec[i], rec[j] = rec[j], rec[i]
		}
		cw.Write(rec)
	}
	cw.Flush()
	rin, rout, rinternal, err := scanN26CSV(buf.String())
	if err != nil || len(rin) != 3 || len(rout) != 5 || rinternal != 2 || rin[0].SenderName != "Mara Esempio" || rin[0].GrossCents != 120000 {
		t.Errorf("reversed columns: %v in=%d out=%d internal=%d %+v", err, len(rin), len(rout), rinternal, rin)
	}

	// Headers in a language the table does not know fail with the named
	// error and the headers seen, never a guess by position.
	italian := `"Data contabile","Data valuta","Nome controparte","IBAN controparte","Tipo","Riferimento","Nome conto","Importo (EUR)"` + "\n" +
		`"2026-10-02","2026-10-03","Mara Esempio","IT00","Bonifico","x","Principale","10.00"` + "\n"
	he := checkN26CSVHeader(italian)
	if he == nil {
		t.Fatal("unknown headers were accepted")
	}
	if msg := he.Error(); !strings.HasPrefix(msg, "column names not recognised: ") || !strings.Contains(msg, `"Data valuta"`) {
		t.Errorf("message: %q", msg)
	}

	// A missing required column is the same failure.
	missing := strings.Replace(text, `"Account Name"`, `"Konto"`, 1)
	if checkN26CSVHeader(missing) == nil {
		t.Error("a missing Account Name column was accepted")
	}
	// So is a required column given twice.
	twice := strings.Replace(text, `"Type"`, `"Value Date"`, 1)
	if checkN26CSVHeader(twice) == nil {
		t.Error("a doubled Value Date column was accepted")
	}

	// The known header passes the check.
	if he := checkN26CSVHeader(text); he != nil {
		t.Errorf("known header refused: %v", he)
	}
}

func TestN26CSVMalformedRows(t *testing.T) {
	header := strings.SplitN(csvFixture(t), "\n", 2)[0]
	cases := map[string]string{
		"wrong width": `"2026-10-02","2026-10-03","Mara Esempio"`,
		"bad amount":  `"2026-10-02","2026-10-03","A","","Credit Transfer","","Hauptkonto","1.200,00","","",""`,
		"bad date":    `"02.10.2026","03.10.2026","A","","Credit Transfer","","Hauptkonto","12.00","","",""`,
	}
	for name, row := range cases {
		_, _, _, err := scanN26CSV(header + "\n" + row + "\n")
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
		var he *n26CSVHeaderError
		if err != nil && strings.Contains(err.Error(), "column names") {
			t.Errorf("%s: reported as a header error: %v (%T)", name, err, he)
		}
	}
}

func TestN26CSVIDs(t *testing.T) {
	a, ao, _, _ := scanN26CSV(csvFixture(t))
	b, bo, _, _ := scanN26CSV(csvFixture(t))
	for i := range a {
		if a[i].TransactionID != b[i].TransactionID {
			t.Errorf("in[%d] id changed between two parses of the same bytes", i)
		}
	}
	for i := range ao {
		if ao[i].TransactionID != bo[i].TransactionID {
			t.Errorf("out[%d] id changed between two parses of the same bytes", i)
		}
	}
	// The two identical kiosk rows are two rows.
	if ao[2].TransactionID == ao[3].TransactionID {
		t.Error("identical rows share an id")
	}
	seen := map[string]bool{}
	for _, c := range a {
		seen[c.TransactionID] = true
	}
	for _, e := range ao {
		if seen[e.TransactionID] {
			t.Errorf("id %s used twice", e.TransactionID)
		}
		seen[e.TransactionID] = true
	}
}

func TestN26CSVCleansNamesAndNotes(t *testing.T) {
	header := strings.SplitN(csvFixture(t), "\n", 2)[0]
	long := strings.Repeat("Ä", 150)
	rows := header + "\n" +
		`"2026-10-02","2026-10-02","Line one` + "\n" + `line two` + "\u200b" + `","DE00","Credit Transfer","` + strings.Repeat("ref ", 200) + `","Hauptkonto","10.00","","",""` + "\n" +
		`"2026-10-02","2026-10-02","` + long + `","DE00","Debit Transfer","","Hauptkonto","-10.00","","",""` + "\n" +
		`"2026-10-02","2026-10-02","","","Direct Debit","","Hauptkonto","-1.00","","",""` + "\n"
	in, out, _, err := scanN26CSV(rows)
	if err != nil || len(in) != 1 || len(out) != 2 {
		t.Fatalf("parse: %v %+v %+v", err, in, out)
	}
	if in[0].SenderName != "Line one line two" {
		t.Errorf("multi-line name: %q", in[0].SenderName)
	}
	if len(in[0].Note) > n26CSVMaxNoteBytes {
		t.Errorf("note is %d bytes", len(in[0].Note))
	}
	if n := len([]rune(out[0].PayeeName)); n != n26CSVMaxNameRunes {
		t.Errorf("long payee: %d runes", n)
	}
	if out[1].PayeeName != "Direct Debit" {
		t.Errorf("empty partner falls back to the type: %q", out[1].PayeeName)
	}
}

func TestN26CSVSniff(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"the export", []byte(csvFixture(t)), true},
		{"a PDF", []byte("%PDF-1.7 a,b,c"), false},
		{"one field of text", []byte("just text"), false},
		{"not UTF-8", []byte("a,b\xff\n"), false},
		{"a NUL byte", []byte("a,b\x00\n"), false},
	}
	for _, tc := range cases {
		if got := looksLikeN26CSV(tc.data); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestN26CSVParseHandlerErrors(t *testing.T) {
	db := setupPaymentsDB(t)
	r := setupN26Router(db, 7)
	w := uploadN26(t, r, map[string][]byte{
		"italian.csv":   []byte(`"Data contabile","Importo"` + "\n" + `"2026-10-02","1.00"` + "\n"),
		"broken.csv":    []byte(strings.SplitN(csvFixture(t), "\n", 2)[0] + "\n" + `"2026-10-02","x"` + "\n"),
		"just-text.txt": []byte("just text"),
	})
	if w.Code != http.StatusOK {
		t.Fatalf("parse: %d %s", w.Code, w.Body.String())
	}
	codes := map[string]string{}
	for _, f := range decodeN26Parse(t, w).Files {
		if f.Error == nil {
			t.Fatalf("%s: expected an error, got %+v", f.Filename, f)
		}
		if len(f.Candidates) != 0 || len(f.ExpenseCandidates) != 0 {
			t.Errorf("%s: candidates beside an error", f.Filename)
		}
		codes[f.Filename] = f.Error.Code
	}
	want := map[string]string{
		"italian.csv":   n26ErrCSVHeadersUnrecognised,
		"broken.csv":    n26ErrParseFailed,
		"just-text.txt": n26ErrNotAPDF,
	}
	for name, code := range want {
		if codes[name] != code {
			t.Errorf("%s: %q, want %q", name, codes[name], code)
		}
	}
}

// A CSV row and the same payment already stored from a PDF (or by hand)
// compare as one payment: same paid date and amount, the review's "Same date
// and amount as ..." flag (design_agent on system_3 #10534 c65276).
func TestN26CSVFlagsTheSamePaymentFromAPDF(t *testing.T) {
	db := setupN26ExpensesDB(t)
	paidOn, _ := time.Parse("2006-01-02", "2026-10-03")
	stored := data_models.InvoicingPayment{
		OwnerUserID: 7, Source: invoicingPaymentSourceN26, TransactionID: "stmt-000000000000000000000000",
		Kind: data_models.PaymentKindIncoming, PaidOn: paidOn, Currency: "EUR", GrossCents: 120000, NetCents: 120000,
		SenderName: "Mara Esempio", Category: data_models.PaymentCategoryUncategorised, Entry: data_models.PaymentEntryImport,
	}
	if err := db.Create(&stored).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Another book's payment of the same date and amount never flags.
	other := stored
	other.ID, other.OwnerUserID = 0, 8
	if err := db.Create(&other).Error; err != nil {
		t.Fatalf("seed other: %v", err)
	}
	r := setupN26ExpensesRouter(db, 7)
	w := uploadN26(t, r, map[string][]byte{"export.csv": []byte(csvFixture(t))})
	f := decodeN26Parse(t, w).Files[0]
	if f.Candidates[0].PossibleDuplicateOf != stored.ID {
		t.Errorf("CSV row not flagged against the stored payment: %+v", f.Candidates[0])
	}
	if f.Candidates[1].PossibleDuplicateOf != 0 {
		t.Errorf("unrelated row flagged: %+v", f.Candidates[1])
	}

	r8 := setupN26ExpensesRouter(db, 9)
	w = uploadN26(t, r8, map[string][]byte{"export.csv": []byte(csvFixture(t))})
	if d := decodeN26Parse(t, w).Files[0].Candidates[0].PossibleDuplicateOf; d != 0 {
		t.Errorf("a third owner's row flagged against payment %d", d)
	}
}

// Every offered CSV row confirms: payments through the payment confirm,
// expenses through the expense confirm. A second upload then reads them as
// already imported.
func TestN26CSVRowsConfirmAndDedupe(t *testing.T) {
	db := setupN26ExpensesDB(t)
	r := setupN26ExpensesRouter(db, 7)
	f := decodeN26Parse(t, uploadN26(t, r, map[string][]byte{"export.csv": []byte(csvFixture(t))})).Files[0]

	w := confirmN26(t, r, f.Candidates)
	if w.Code != http.StatusOK {
		t.Fatalf("payment confirm: %d %s", w.Code, w.Body.String())
	}
	var presp struct {
		Summary n26ConfirmSummary `json:"summary"`
	}
	json.Unmarshal(w.Body.Bytes(), &presp)
	if presp.Summary.Confirmed != len(f.Candidates) {
		t.Errorf("payments confirmed: %+v", presp.Summary)
	}

	w = confirmN26Expenses(t, r, asExpenseConfirmRows(f.ExpenseCandidates, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expense confirm: %d %s", w.Code, w.Body.String())
	}
	var eresp struct {
		Summary n26ConfirmSummary `json:"summary"`
	}
	json.Unmarshal(w.Body.Bytes(), &eresp)
	if eresp.Summary.Confirmed != len(f.ExpenseCandidates) {
		t.Errorf("expenses confirmed: %+v", eresp.Summary)
	}

	again := decodeN26Parse(t, uploadN26(t, r, map[string][]byte{"export.csv": []byte(csvFixture(t))})).Files[0]
	for _, c := range again.Candidates {
		if !c.AlreadyImported {
			t.Errorf("payment not read as already imported: %+v", c)
		}
	}
	for _, e := range again.ExpenseCandidates {
		if !e.AlreadyImported {
			t.Errorf("expense not read as already imported: %+v", e)
		}
	}
}
