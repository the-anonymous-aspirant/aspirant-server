package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// #8367-B1: the PayPal activity CSV parser. The fixtures follow PayPal's
// documented Activity Download layout (header names from the report spec) in
// English and German; the operator's anonymised sample joins them when it
// arrives (#8367 decision 3).

// paypalFixtureEN is a comma-separated English export: two payments in, a fee
// row, a currency conversion pair, a bank top-up, a pending payment, an
// outgoing purchase and a withdrawal to the bank.
const paypalFixtureEN = `"Date","Time","TimeZone","Name","Type","Status","Currency","Gross","Fee","Net","From Email Address","To Email Address","Transaction ID","Item Title","Reference Txn ID","Subject","Note","Balance Impact"
"07/14/2026","10:01:02","CEST","Galerie Nord","Mobile Payment","Completed","EUR","350.00","-10.50","339.50","pay@galerie.test","flower@example.test","1AA11111AA1111111","","","","Wandbild Juli","Credit"
"07/20/2026","18:00:00","CEST","Anna Example","General Payment","Completed","EUR","1,200.00","0.00","1,200.00","anna@example.test","flower@example.test","2BB22222BB2222222","","","Portrait","","Credit"
"07/20/2026","18:00:00","CEST","PayPal","Fee","Completed","EUR","-0.35","0.00","-0.35","","","3CC33333CC3333333","","","","","Debit"
"07/22/2026","09:00:00","CEST","","General Currency Conversion","Completed","USD","-50.00","0.00","-50.00","","","4DD44444DD4444444","","","","","Debit"
"07/22/2026","09:00:00","CEST","","General Currency Conversion","Completed","EUR","45.80","0.00","45.80","","","5EE55555EE5555555","","","","","Credit"
"07/23/2026","09:00:00","CEST","","Bank Deposit to PP Account","Completed","EUR","100.00","0.00","100.00","","","6FF66666FF6666666","","","","","Credit"
"07/24/2026","09:00:00","CEST","Bob","General Payment","Pending","EUR","80.00","0.00","80.00","bob@example.test","","7GG77777GG7777777","","","","","Credit"
"07/25/2026","12:00:00","CEST","Some Shop","Express Checkout Payment","Completed","EUR","-25.00","0.00","-25.00","","shop@example.test","8HH88888HH8888888","","","","","Debit"
"08/01/2026","08:00:00","CEST","","General Withdrawal","Completed","EUR","-1,500.00","0.00","-1,500.00","","","9JJ99999JJ9999999","","","","","Debit"
`

// paypalFixtureDE is the same activity as a German, semicolon-separated export
// with a UTF-8 BOM and German amounts and dates.
const paypalFixtureDE = "\ufeff" + `"Datum";"Uhrzeit";"Zeitzone";"Name";"Typ";"Status";"Währung";"Brutto";"Gebühr";"Netto";"Absender E-Mail-Adresse";"Empfänger E-Mail-Adresse";"Transaktionscode";"Artikelbezeichnung";"Zugehöriger Transaktionscode";"Betreff";"Hinweis";"Auswirkung auf Guthaben"
"14.07.2026";"10:01:02";"MESZ";"Galerie Nord";"Mobile Zahlung";"Abgeschlossen";"EUR";"350,00";"-10,50";"339,50";"pay@galerie.test";"flower@example.test";"1AA11111AA1111111";"";"";"";"Wandbild Juli";"Haben"
"20.07.2026";"18:00:00";"MESZ";"Anna Example";"Allgemeine Zahlung";"Abgeschlossen";"EUR";"1.200,00";"0,00";"1.200,00";"anna@example.test";"flower@example.test";"2BB22222BB2222222";"";"";"Portrait";"";"Haben"
"20.07.2026";"18:00:00";"MESZ";"PayPal";"Gebühr";"Abgeschlossen";"EUR";"-0,35";"0,00";"-0,35";"";"";"3CC33333CC3333333";"";"";"";"";"Soll"
"22.07.2026";"09:00:00";"MESZ";"";"Allgemeine Währungsumrechnung";"Abgeschlossen";"USD";"-50,00";"0,00";"-50,00";"";"";"4DD44444DD4444444";"";"";"";"";"Soll"
"22.07.2026";"09:00:00";"MESZ";"";"Allgemeine Währungsumrechnung";"Abgeschlossen";"EUR";"45,80";"0,00";"45,80";"";"";"5EE55555EE5555555";"";"";"";"";"Haben"
"23.07.2026";"09:00:00";"MESZ";"";"Bankgutschrift auf PayPal-Konto";"Abgeschlossen";"EUR";"100,00";"0,00";"100,00";"";"";"6FF66666FF6666666";"";"";"";"";"Haben"
"24.07.2026";"09:00:00";"MESZ";"Bob";"Allgemeine Zahlung";"Ausstehend";"EUR";"80,00";"0,00";"80,00";"bob@example.test";"";"7GG77777GG7777777";"";"";"";"";"Haben"
"25.07.2026";"12:00:00";"MESZ";"Some Shop";"Express-Checkout-Zahlung";"Abgeschlossen";"EUR";"-25,00";"0,00";"-25,00";"";"shop@example.test";"8HH88888HH8888888";"";"";"";"";"Soll"
"01.08.2026";"08:00:00";"MESZ";"";"Allgemeine Abbuchung";"Abgeschlossen";"EUR";"-1.500,00";"0,00";"-1.500,00";"";"";"9JJ99999JJ9999999";"";"";"";"";"Soll"
`

func mustParse(t *testing.T, data string, order string) *paypalParseResult {
	t.Helper()
	res, err := ParsePayPalActivityCSV([]byte(data), order)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return res
}

func TestParsePayPal_EnglishFixture(t *testing.T) {
	res := mustParse(t, paypalFixtureEN, "")
	if res.RowsRead != 9 {
		t.Fatalf("rows read: got %d, want 9", res.RowsRead)
	}
	// Positive control: exactly the two payments in and the withdrawal.
	if len(res.Rows) != 3 {
		t.Fatalf("rows: got %d, want 3 (2 incoming + 1 withdrawal): %+v", len(res.Rows), res.Rows)
	}
	galerie := res.Rows[0]
	if galerie.Kind != "incoming" || galerie.TransactionID != "1AA11111AA1111111" || galerie.GrossCents != 35000 ||
		galerie.FeeCents != -1050 || galerie.NetCents != 33950 || galerie.SenderEmail != "pay@galerie.test" ||
		galerie.SenderName != "Galerie Nord" || galerie.Note != "Wandbild Juli" || galerie.Currency != "EUR" ||
		!galerie.PaidOn.Equal(time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("galerie row: %+v", galerie)
	}
	if anna := res.Rows[1]; anna.GrossCents != 120000 || anna.Note != "Portrait" {
		t.Errorf("anna row: %+v", anna)
	}
	w := res.Rows[2]
	if w.Kind != "withdrawal" || w.GrossCents != 150000 || w.SenderEmail != "" ||
		!w.PaidOn.Equal(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("withdrawal row: %+v", w)
	}
	want := map[string]int{"fee": 1, "currency_conversion": 2, "bank_topup": 1, "not_completed": 1, "outgoing": 1}
	for reason, n := range want {
		if res.Skipped[reason] != n {
			t.Errorf("skipped[%s]: got %d, want %d (all: %v)", reason, res.Skipped[reason], n, res.Skipped)
		}
	}
	total := len(res.Rows)
	for _, n := range res.Skipped {
		total += n
	}
	if total != res.RowsRead {
		t.Errorf("rows (%d) + skipped do not add up to rows read (%d): %v", len(res.Rows), res.RowsRead, res.Skipped)
	}
}

func TestParsePayPal_GermanFixtureMatchesEnglish(t *testing.T) {
	en := mustParse(t, paypalFixtureEN, "")
	de := mustParse(t, paypalFixtureDE, "")
	if len(de.Rows) != len(en.Rows) {
		t.Fatalf("DE rows %d != EN rows %d", len(de.Rows), len(en.Rows))
	}
	for i := range en.Rows {
		e, d := en.Rows[i], de.Rows[i]
		if e.Kind != d.Kind || e.TransactionID != d.TransactionID || e.GrossCents != d.GrossCents ||
			e.FeeCents != d.FeeCents || e.NetCents != d.NetCents || !e.PaidOn.Equal(d.PaidOn) ||
			e.SenderEmail != d.SenderEmail || e.Note != d.Note {
			t.Errorf("row %d differs:\n EN %+v\n DE %+v", i, e, d)
		}
	}
	for reason, n := range en.Skipped {
		if de.Skipped[reason] != n {
			t.Errorf("skipped[%s]: DE %d, EN %d", reason, de.Skipped[reason], n)
		}
	}
}

func TestParsePayPal_Windows1252IsDecoded(t *testing.T) {
	csv := "Datum;Name;Typ;Status;Währung;Brutto;Transaktionscode\n14.07.2026;Jürgen Müßig;Zahlung;Abgeschlossen;EUR;10,00;X1\n"
	// Re-encode the UTF-8 literal as Windows-1252 bytes.
	var b []byte
	for _, r := range csv {
		b = append(b, byte(r))
	}
	res, err := ParsePayPalActivityCSV(b, "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0].SenderName != "Jürgen Müßig" {
		t.Fatalf("got %+v", res.Rows)
	}
}

func TestParsePayPal_NoBalanceImpactColumnUsesTheSign(t *testing.T) {
	csv := "Date,Name,Type,Status,Currency,Gross,Transaction ID\n" +
		"14/07/2026,A,Payment,Completed,EUR,10.00,T1\n" +
		"15/07/2026,B,Payment,Completed,EUR,-10.00,T2\n"
	res := mustParse(t, csv, "")
	if len(res.Rows) != 1 || res.Rows[0].TransactionID != "T1" || res.Skipped["outgoing"] != 1 {
		t.Fatalf("got rows %+v skipped %v", res.Rows, res.Skipped)
	}
	// 14/07 can only be day-first.
	if !res.Rows[0].PaidOn.Equal(time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("paid on: %v", res.Rows[0].PaidOn)
	}
}

func TestParsePayPal_AmbiguousSlashDatesAreRefusedNotGuessed(t *testing.T) {
	csv := "Date,Name,Type,Status,Currency,Gross,Transaction ID\n" +
		"07/08/2026,A,Payment,Completed,EUR,10.00,T1\n"
	_, err := ParsePayPalActivityCSV([]byte(csv), "")
	if !errors.Is(err, ErrPayPalDateOrderAmbiguous) {
		t.Fatalf("got %v, want ErrPayPalDateOrderAmbiguous", err)
	}
	dmy := mustParse(t, csv, "dmy")
	mdy := mustParse(t, csv, "mdy")
	if !dmy.Rows[0].PaidOn.Equal(time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)) ||
		!mdy.Rows[0].PaidOn.Equal(time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("explicit orders: dmy %v, mdy %v", dmy.Rows[0].PaidOn, mdy.Rows[0].PaidOn)
	}
	if _, err := ParsePayPalActivityCSV([]byte(csv), "ymd"); err == nil {
		t.Errorf("unknown date_order accepted")
	}
}

func TestParsePayPal_MissingColumnIsNamed(t *testing.T) {
	_, err := ParsePayPalActivityCSV([]byte("Date,Name,Type,Status,Currency,Gross\n01.01.2026,A,P,Completed,EUR,1\n"), "")
	if err == nil || !strings.Contains(err.Error(), "transaction_id") {
		t.Fatalf("got %v, want an error naming transaction_id", err)
	}
}

func TestParsePayPal_BadAmountFailsWithItsLine(t *testing.T) {
	csv := "Date,Name,Type,Status,Currency,Gross,Transaction ID\n01.07.2026,A,Payment,Completed,EUR,ten,T1\n"
	_, err := ParsePayPalActivityCSV([]byte(csv), "")
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("got %v, want an error citing line 2", err)
	}
}

func TestParseCents(t *testing.T) {
	cases := map[string]int64{
		"1.234,56": 123456, "1,234.56": 123456, "-0,35": -35, "20.00": 2000, "20": 2000,
		"1.234": 123400, "1,234": 123400, "0,5": 50, "€ 5,00": 500, "1 234,56": 123456,
		"": 0, "-1.500,00": -150000, "+12.30": 1230,
	}
	for in, want := range cases {
		got, err := parseCents(in)
		if err != nil || got != want {
			t.Errorf("parseCents(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"abc", "1.2.3,456", "12,345,6789.123"} {
		if _, err := parseCents(bad); err == nil {
			t.Errorf("parseCents(%q) accepted", bad)
		}
	}
}

func TestParsePayPalDate_RejectsNonCalendarDates(t *testing.T) {
	for _, bad := range []string{"31.02.2026", "2026-13-01", "July 4"} {
		if _, err := parsePayPalDate(bad, paypalDateOrderDMY); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// @security N6 (c51502 on #8374): the currency is printed and exported, so a
// row must carry a real ISO code.
func TestParsePayPal_CurrencyMustBeAnISOCode(t *testing.T) {
	for _, bad := range []string{"=1+1", "eur1", "EURO", ""} {
		csv := "Date,Name,Type,Status,Currency,Gross,Transaction ID\n14.07.2026,A,Payment,Completed," + bad + ",10.00,T1\n"
		_, err := ParsePayPalActivityCSV([]byte(csv), "")
		if err == nil || !strings.Contains(err.Error(), "line 2") {
			t.Errorf("currency %q: got %v", bad, err)
		}
	}
	for _, good := range []string{"EUR", "usd"} {
		csv := "Date,Name,Type,Status,Currency,Gross,Transaction ID\n14.07.2026,A,Payment,Completed," + good + ",10.00,T1\n"
		res, err := ParsePayPalActivityCSV([]byte(csv), "")
		if err != nil || res.Rows[0].Currency != strings.ToUpper(good) {
			t.Errorf("currency %q: %v %+v", good, err, res)
		}
	}
}

// #9862-A1: the Type strings of a real statement (upload 112, read in c60929
// on system_3 #9862), as synthetic rows. Her statement itself never enters
// the repo.
const reversalHeader = "Date,Name,Type,Status,Currency,Gross,Transaction ID,Reference Txn ID\n"

func TestParsePayPal_BouncedWithdrawalImportsNeitherRow(t *testing.T) {
	withdrawal := "02.07.2026,,User Initiated Withdrawal,Completed,EUR,-175.00,W1,\n"
	reversal := "03.07.2026,,Reversal of ACH Withdrawal Transaction,Completed,EUR,175.00,W1,\n"
	payment := "02.07.2026,Client,Mobile Payment,Completed,EUR,175.00,P1,\n"
	// Either file order: PayPal lists newest first, a re-saved file may not.
	for _, body := range []string{withdrawal + reversal + payment, payment + reversal + withdrawal} {
		res := mustParse(t, reversalHeader+body, "")
		if len(res.Rows) != 1 || res.Rows[0].TransactionID != "P1" {
			t.Fatalf("rows: %+v", res.Rows)
		}
		if res.Skipped["reversal"] != 1 || res.Skipped["reversed"] != 1 {
			t.Errorf("skipped: %v", res.Skipped)
		}
	}
}

func TestParsePayPal_PaymentReversedByReferenceIsDropped(t *testing.T) {
	res := mustParse(t, reversalHeader+
		"10.07.2026,Client,Mobile Payment,Completed,EUR,50.00,P1,\n"+
		"12.07.2026,Client,Payment Reversal,Completed,EUR,-50.00,R1,P1\n"+
		"13.07.2026,Other,Mobile Payment,Completed,EUR,50.00,P2,\n", "")
	if len(res.Rows) != 1 || res.Rows[0].TransactionID != "P2" || res.Skipped["reversed"] != 1 {
		t.Fatalf("rows %+v skipped %v", res.Rows, res.Skipped)
	}
}

// A reversal only undoes a row moving the same amount the other way. A
// chargeback reversal returns money to her under the payment's reference,
// and the payment stands; so does one whose amount does not match.
func TestParsePayPal_ReversalInTheSameDirectionKeepsThePayment(t *testing.T) {
	res := mustParse(t, reversalHeader+
		"10.07.2026,Client,Mobile Payment,Completed,EUR,80.00,P1,\n"+
		"20.07.2026,Client,Chargeback,Completed,EUR,-80.00,C1,P1\n"+
		"25.07.2026,Client,Chargeback Reversal,Completed,EUR,80.00,C2,P1\n"+
		"11.07.2026,Client,Mobile Payment,Completed,EUR,60.00,P2,\n"+
		"12.07.2026,Client,Payment Reversal,Completed,EUR,-20.00,R2,P2\n", "")
	if len(res.Rows) != 2 || res.Rows[0].TransactionID != "P1" || res.Rows[1].TransactionID != "P2" {
		t.Fatalf("rows: %+v", res.Rows)
	}
	if res.Skipped["reversed"] != 0 || res.Skipped["reversal"] != 2 {
		t.Errorf("skipped: %v", res.Skipped)
	}
}

func TestParsePayPal_HoldReversalIsSkipped(t *testing.T) {
	res := mustParse(t, reversalHeader+
		"26.07.2026,,Account Hold for Open Authorization,Completed,EUR,-4.00,H1,\n"+
		"26.07.2026,,Reversal of General Account Hold,Completed,EUR,4.00,H2,\n", "")
	if len(res.Rows) != 0 || res.Skipped["hold_or_authorization"] != 1 || res.Skipped["reversal"] != 1 {
		t.Fatalf("rows %+v skipped %v", res.Rows, res.Skipped)
	}
}

func TestParsePayPal_IncomingRefundIsPreCategorised(t *testing.T) {
	res := mustParse(t, reversalHeader+
		"02.07.2026,Client,Payment Refund,Completed,EUR,175.00,F1,\n"+
		"05.07.2026,Buyer,Payment Refund,Completed,EUR,-15.00,F2,\n"+
		"08.07.2026,Client,Mobile Payment,Completed,EUR,90.00,P1,\n", "")
	if len(res.Rows) != 2 {
		t.Fatalf("rows: %+v", res.Rows)
	}
	if r := res.Rows[0]; r.TransactionID != "F1" || r.Kind != "incoming" || r.Category != "refund" {
		t.Errorf("incoming refund: %+v", r)
	}
	if r := res.Rows[1]; r.TransactionID != "P1" || r.Category != "uncategorised" {
		t.Errorf("payment: %+v", r)
	}
	if res.Skipped["outgoing"] != 1 {
		t.Errorf("the refund she paid out is skipped as outgoing: %v", res.Skipped)
	}
}

// system_3 #10023: PayPal's statement CSV, as the operator's real export has
// it. The header has 18 columns, names the Type column "Description", carries
// no Status, and the file opens with a BOM whose bytes were re-encoded as
// UTF-8 text, inside the first quoted cell. Every value below is invented.
const statementHeader = "\"\u00ef\u00bb\u00bf\"\"Date\"\"\",\"Time\",\"Time Zone\",\"Description\",\"Currency\",\"Gross\",\"Fee\",\"Net\",\"Balance\",\"Transaction ID\",\"From Email Address\",\"Name\",\"Bank Name\",\"Bank Account\",\"Shipping and Handling Amount\",\"Sales Tax\",\"Invoice ID\",\"Reference Txn ID\"\n"

func statementRow(date, desc, gross, id, ref string) string {
	return fmt.Sprintf("%q,\"10:00:00\",\"Europe/Berlin\",%q,\"EUR\",%q,\"0,00\",%q,\"100,00\",%q,\"payer@example.test\",\"Erika Beispiel\",\"\",\"\",\"0,00\",\"0,00\",\"\",%q\n", date, desc, gross, gross, id, ref)
}

func TestParsePayPal_StatementCSVWithMojibakeBOMImports(t *testing.T) {
	data := statementHeader +
		statementRow("03.07.2026", "Mobile Payment", "120,00", "S1", "") +
		statementRow("04.07.2026", "Mass Pay Payment", "45,00", "S2", "") +
		statementRow("05.07.2026", "Express Checkout Payment", "-30,00", "S3", "") +
		statementRow("06.07.2026", "User Initiated Withdrawal", "-200,00", "S4", "") +
		statementRow("09.07.2026", "Reversal of ACH Withdrawal Transaction", "200,00", "S4", "") +
		statementRow("10.07.2026", "User Initiated Withdrawal", "-50,00", "S5", "") +
		statementRow("11.07.2026", "Payment Refund", "15,00", "S6", "") +
		statementRow("12.07.2026", "Bank Deposit to PP Account", "80,00", "S7", "")
	res, err := ParsePayPalActivityCSV([]byte(data), "")
	if err != nil {
		t.Fatalf("the statement CSV was refused: %v", err)
	}
	var got []string
	for _, r := range res.Rows {
		got = append(got, r.TransactionID+":"+r.Kind+":"+r.Category)
	}
	want := []string{"S1:incoming:uncategorised", "S2:incoming:uncategorised", "S5:withdrawal:uncategorised", "S6:incoming:refund"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("rows:\n got %v\nwant %v", got, want)
	}
	if res.RowsRead != 8 || res.Skipped["outgoing"] != 1 || res.Skipped["reversal"] != 1 || res.Skipped["bank_topup"] != 1 {
		t.Errorf("read %d, skipped %v", res.RowsRead, res.Skipped)
	}
}

// With a Status column the rule is unchanged: only completed rows import.
func TestParsePayPal_StatusStillDecidesWhereItExists(t *testing.T) {
	res := mustParse(t, "Date,Name,Description,Status,Currency,Gross,Transaction ID\n"+
		"03.07.2026,A,Mobile Payment,Completed,EUR,10.00,T1\n"+
		"04.07.2026,B,Mobile Payment,Pending,EUR,20.00,T2\n", "")
	if len(res.Rows) != 1 || res.Rows[0].TransactionID != "T1" || res.Skipped["not_completed"] != 1 {
		t.Errorf("rows %+v skipped %v", res.Rows, res.Skipped)
	}
}

func TestParsePayPal_NeitherTypeNorDescriptionIsRefused(t *testing.T) {
	_, err := ParsePayPalActivityCSV([]byte("Date,Name,Currency,Gross,Transaction ID,Balance\n03.07.2026,A,EUR,10.00,T1,10.00\n"), "")
	if err == nil || !strings.Contains(err.Error(), "type") || strings.Contains(err.Error(), "status") {
		t.Fatalf("got %v, want an error naming type and not status", err)
	}
}

// security on PR 168 (c61953): without Status, only a statement (a Balance
// on every row) may import. An activity export with its Status column cut
// is refused, so its Pending and Denied rows cannot land as paid.
func TestParsePayPal_NoStatusAndNoBalanceIsRefused(t *testing.T) {
	_, err := ParsePayPalActivityCSV([]byte("Date,Name,Type,Currency,Gross,Transaction ID\n"+
		"03.07.2026,A,Mobile Payment,EUR,10.00,T1\n"), "")
	if err == nil || !strings.Contains(err.Error(), "status") {
		t.Fatalf("got %v, want a refusal naming status", err)
	}
}

func TestNormaliseHeader_PeelsQuotesAndEitherBOM(t *testing.T) {
	for in, want := range map[string]string{
		"\ufeffDate":                   "date",
		"\u00ef\u00bb\u00bf\"Date\"":   "date",
		"\"\u00ef\u00bb\u00bf\"Date\"": "date",
		"  Transaction  ID ":           "transaction id",
		"Description":                  "description",
	} {
		if got := normaliseHeader(in); got != want {
			t.Errorf("normaliseHeader(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- #10414-B2: outgoing PayPal rows become expense candidates ---

// The fixture's one outgoing purchase is now an expense candidate. Every other
// row resolves to the bucket it always did: the same rows import, the same
// skip counts, and the purchase is still counted as outgoing because it is not
// imported as a payment.
func TestParsePayPal_OutgoingPurchaseIsAnExpenseCandidate(t *testing.T) {
	for name, fixture := range map[string]string{"en": paypalFixtureEN, "de": paypalFixtureDE} {
		res := mustParse(t, fixture, "")
		if len(res.Rows) != 3 || res.Rows[0].Kind != "incoming" || res.Rows[1].Kind != "incoming" || res.Rows[2].Kind != "withdrawal" {
			t.Fatalf("%s: payment rows changed: %+v", name, res.Rows)
		}
		want := map[string]int{"fee": 1, "currency_conversion": 2, "bank_topup": 1, "not_completed": 1, "outgoing": 1}
		for reason, n := range want {
			if res.Skipped[reason] != n {
				t.Errorf("%s: skipped[%s] = %d, want %d", name, reason, res.Skipped[reason], n)
			}
		}
		if len(res.Expenses) != 1 {
			t.Fatalf("%s: expenses: %+v", name, res.Expenses)
		}
		e := res.Expenses[0]
		if e.TransactionID != "8HH88888HH8888888" || e.AmountCents != 2500 || e.PayeeName != "Some Shop" || e.Currency != "EUR" ||
			!e.PaidOn.Equal(time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("%s: expense: %+v", name, e)
		}
	}
}

// A mixed file: each row lands in its own bucket. Only a debit that is neither
// a fee, a withdrawal nor a refund paid out is offered as an expense; a
// reversed purchase is not; an outgoing row the file cannot describe cleanly
// is left out instead of failing the whole file.
func TestParsePayPal_MixedFileEachRowInItsOwnBucket(t *testing.T) {
	const header = "Date,Name,Type,Status,Currency,Gross,Transaction ID,Reference Txn ID,Item Title,Balance Impact\n"
	res := mustParse(t, header+
		"01.07.2026,Client,Mobile Payment,Completed,EUR,200.00,IN1,,,Credit\n"+
		"01.07.2026,PayPal,Fee,Completed,EUR,-0.35,FEE1,,,Debit\n"+
		"02.07.2026,,General Withdrawal,Completed,EUR,-150.00,W1,,,Debit\n"+
		"03.07.2026,Colorificio,Express Checkout Payment,Completed,EUR,-89.90,OUT1,,Pigmenti,Debit\n"+
		"04.07.2026,Print Shop,PreApproved Payment Bill User Payment,Completed,USD,-40.00,OUT2,,,Debit\n"+
		"05.07.2026,Buyer,Payment Refund,Completed,EUR,-15.00,RF1,,,Debit\n"+
		"06.07.2026,Some Shop,Express Checkout Payment,Completed,EUR,-30.00,OUT3,,,Debit\n"+
		"07.07.2026,Some Shop,Payment Reversal,Completed,EUR,30.00,RV1,OUT3,,Credit\n"+
		"08.07.2026,No Id Shop,Express Checkout Payment,Completed,EUR,-12.00,,,,Debit\n"+
		"09.07.2026,Odd Shop,Express Checkout Payment,Completed,EURO,-9.00,OUT4,,,Debit\n", "")

	if len(res.Rows) != 2 || res.Rows[0].TransactionID != "IN1" || res.Rows[1].TransactionID != "W1" || res.Rows[1].Kind != "withdrawal" {
		t.Fatalf("payment rows: %+v", res.Rows)
	}
	if res.Skipped["fee"] != 1 || res.Skipped["reversal"] != 1 {
		t.Errorf("skipped: %v", res.Skipped)
	}
	// OUT1, OUT2, RF1, OUT3, the id-less row and OUT4 all fall through to outgoing.
	if res.Skipped["outgoing"] != 6 {
		t.Errorf("skipped[outgoing] = %d, want 6: %v", res.Skipped["outgoing"], res.Skipped)
	}
	got := map[string]paypalExpenseRow{}
	for _, e := range res.Expenses {
		got[e.TransactionID] = e
	}
	if len(got) != 2 {
		t.Fatalf("expense candidates: want OUT1 and OUT2 only, got %+v", res.Expenses)
	}
	if e := got["OUT1"]; e.AmountCents != 8990 || e.PayeeName != "Colorificio" || e.Note != "Pigmenti" {
		t.Errorf("OUT1: %+v", e)
	}
	if e := got["OUT2"]; e.AmountCents != 4000 || e.Currency != "USD" {
		t.Errorf("OUT2 (a foreign-currency purchase keeps its currency): %+v", e)
	}
}

func setupPayPalExpensesRouter(t *testing.T, userID uint) (*gorm.DB, *gin.Engine) {
	t.Helper()
	db := setupPaymentsDB(t)
	db.AutoMigrate(&data_models.InvoicingExpense{}, &data_models.InvoicingExpenseCategory{})
	r := setupPaymentsRouter(db, userID)
	r.POST("/invoicing/expenses/import-paypal/confirm", ImportPayPalExpensesConfirmHandler)
	r.POST("/invoicing/expense-categories", CreateExpenseCategoryHandler)
	return db, r
}

func decodePayPalExpenseCandidates(t *testing.T, w *httptest.ResponseRecorder) []paypalExpenseCandidate {
	t.Helper()
	var resp struct {
		ExpenseCandidates []paypalExpenseCandidate `json:"expense_candidates"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	return resp.ExpenseCandidates
}

func confirmPayPalExpenses(r *gin.Engine, cands []paypalExpenseCandidate, category string) *httptest.ResponseRecorder {
	rows := make([]paypalExpenseConfirmRow, 0, len(cands))
	for _, c := range cands {
		rows = append(rows, paypalExpenseConfirmRow{
			TransactionID: c.TransactionID, PaidOn: c.PaidOn, Currency: c.Currency,
			AmountCents: c.AmountCents, PayeeName: c.PayeeName, Note: c.Note, Category: category,
		})
	}
	body, _ := json.Marshal(map[string]any{"rows": rows})
	return invoicingDo(r, "POST", "/invoicing/expenses/import-paypal/confirm", string(body))
}

// The upload offers the outgoing purchase for review and stores no expense;
// the confirm stores it once, in the owner's own book, never as a payment;
// a re-upload flags it as already imported.
func TestImportPayPal_OutgoingRowsAreOfferedThenConfirmed(t *testing.T) {
	db, r := setupPayPalExpensesRouter(t, 7)
	w := uploadCSV(t, r, paypalFixtureEN, "")
	if w.Code != http.StatusOK {
		t.Fatalf("import: %d %s", w.Code, w.Body.String())
	}
	if s := decodeSummary(t, w); s.Imported != 3 || s.SkippedReasons["outgoing"] != 1 {
		t.Fatalf("payment import changed: %+v", s)
	}
	cands := decodePayPalExpenseCandidates(t, w)
	if len(cands) != 1 || cands[0].TransactionID != "8HH88888HH8888888" || cands[0].AmountCents != 2500 ||
		cands[0].PaidOn != "2026-07-25" || cands[0].PayeeName != "Some Shop" || cands[0].AlreadyImported {
		t.Fatalf("candidates: %+v", cands)
	}
	var n int
	db.Model(&data_models.InvoicingExpense{}).Count(&n)
	if n != 0 {
		t.Fatalf("the upload stored %d expenses; it must only offer them", n)
	}

	w = confirmPayPalExpenses(r, cands, "materials and supplies")
	if w.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", w.Code, w.Body.String())
	}
	var cresp struct {
		Summary paypalExpenseConfirmSummary `json:"summary"`
	}
	json.Unmarshal(w.Body.Bytes(), &cresp)
	if cresp.Summary.Confirmed != 1 || cresp.Summary.Duplicates != 0 {
		t.Fatalf("summary: %+v", cresp.Summary)
	}
	var stored []data_models.InvoicingExpense
	db.Where("owner_user_id = ?", 7).Find(&stored)
	if len(stored) != 1 {
		t.Fatalf("stored: %+v", stored)
	}
	if e := stored[0]; e.Source != invoicingPaymentSourcePayPal || e.Entry != data_models.ExpenseEntryImport ||
		e.AmountCents != 2500 || e.Category != "materials and supplies" || e.Reference != "Some Shop" ||
		e.TransactionID != "8HH88888HH8888888" {
		t.Fatalf("stored expense: %+v", e)
	}
	var payments int
	db.Model(&data_models.InvoicingPayment{}).Where("transaction_id = ?", "8HH88888HH8888888").Count(&payments)
	if payments != 0 {
		t.Fatal("the purchase was stored as a payment")
	}

	// Re-confirm dedupes; a re-upload flags the stored row.
	w = confirmPayPalExpenses(r, cands, "")
	json.Unmarshal(w.Body.Bytes(), &cresp)
	if cresp.Summary.Confirmed != 0 || cresp.Summary.Duplicates != 1 {
		t.Fatalf("re-confirm: %+v", cresp.Summary)
	}
	if again := decodePayPalExpenseCandidates(t, uploadCSV(t, r, paypalFixtureEN, "")); len(again) != 1 || !again[0].AlreadyImported {
		t.Fatalf("re-upload flags: %+v", again)
	}

	// Another owner's import is flagged against her own book only, and her
	// confirm stores into it with the default category.
	_, r8 := setupPayPalExpensesRouterOn(db, 8)
	theirs := decodePayPalExpenseCandidates(t, uploadCSV(t, r8, paypalFixtureEN, ""))
	if len(theirs) != 1 || theirs[0].AlreadyImported || theirs[0].PossibleDuplicateOf != 0 {
		t.Fatalf("another owner sees this owner's expense: %+v", theirs)
	}
	if w := confirmPayPalExpenses(r8, theirs, ""); w.Code != http.StatusOK {
		t.Fatalf("owner 8 confirm: %d %s", w.Code, w.Body.String())
	}
	var e8 data_models.InvoicingExpense
	db.Where("owner_user_id = ?", 8).First(&e8)
	if e8.Category != data_models.ExpenseCategoryOther {
		t.Errorf("default category: %q", e8.Category)
	}

	var events int
	db.Model(&data_models.InvoicingAuditEvent{}).Where("action = ? AND entity_type = ?", invoicingActionImport, invoicingEntityExpense).Count(&events)
	if events == 0 {
		t.Fatal("no audit event for the expense confirm")
	}
}

func setupPayPalExpensesRouterOn(db *gorm.DB, userID uint) (*gorm.DB, *gin.Engine) {
	r := setupPaymentsRouter(db, userID)
	r.POST("/invoicing/expenses/import-paypal/confirm", ImportPayPalExpensesConfirmHandler)
	r.POST("/invoicing/expense-categories", CreateExpenseCategoryHandler)
	return db, r
}

// A hand-entered expense of the same date and amount is named as a possible
// duplicate.
func TestImportPayPal_ExpenseCandidateFlagsAHandEnteredTwin(t *testing.T) {
	db, r := setupPayPalExpensesRouter(t, 7)
	paidOn := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)
	twin := data_models.InvoicingExpense{OwnerUserID: 7, Source: "manual", TransactionID: "M-1", PaidOn: paidOn,
		Currency: "EUR", AmountCents: 2500, Category: "other", Entry: "manual"}
	if err := db.Create(&twin).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	cands := decodePayPalExpenseCandidates(t, uploadCSV(t, r, paypalFixtureEN, ""))
	if len(cands) != 1 || cands[0].PossibleDuplicateOf != twin.ID || cands[0].AlreadyImported {
		t.Fatalf("candidates: %+v (twin %d)", cands, twin.ID)
	}
}

func TestImportPayPalExpensesConfirm_Refusals(t *testing.T) {
	db, r := setupPayPalExpensesRouter(t, 7)
	_, r8 := setupPayPalExpensesRouterOn(db, 8)
	if w := invoicingDo(r8, "POST", "/invoicing/expense-categories", `{"name":"pigments","counts_as":"materials and supplies"}`); w.Code != http.StatusCreated {
		t.Fatalf("seed owner 8's category: %d %s", w.Code, w.Body.String())
	}
	good := paypalExpenseCandidate{TransactionID: "8HH88888HH8888888", PaidOn: "2026-07-25", Currency: "EUR", AmountCents: 2500, PayeeName: "Some Shop"}
	cases := map[string]func(*paypalExpenseCandidate, *string){
		"empty id":         func(c *paypalExpenseCandidate, _ *string) { c.TransactionID = "" },
		"id with spaces":   func(c *paypalExpenseCandidate, _ *string) { c.TransactionID = "8HH 888" },
		"multi-line payee": func(c *paypalExpenseCandidate, _ *string) { c.PayeeName = "a\nb" },
		"non-ISO currency": func(c *paypalExpenseCandidate, _ *string) { c.Currency = "EURO" },
		"bad date":         func(c *paypalExpenseCandidate, _ *string) { c.PaidOn = "25.07.2026" },
		"future date": func(c *paypalExpenseCandidate, _ *string) {
			c.PaidOn = time.Now().AddDate(0, 0, 3).Format("2006-01-02")
		},
		"zero amount":              func(c *paypalExpenseCandidate, _ *string) { c.AmountCents = 0 },
		"negative amount":          func(c *paypalExpenseCandidate, _ *string) { c.AmountCents = -2500 },
		"unknown category":         func(_ *paypalExpenseCandidate, cat *string) { *cat = "holidays" },
		"another owner's category": func(_ *paypalExpenseCandidate, cat *string) { *cat = "pigments" },
	}
	for name, mutate := range cases {
		c, cat := good, ""
		mutate(&c, &cat)
		w := confirmPayPalExpenses(r, []paypalExpenseCandidate{c}, cat)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_row") {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	if w := confirmPayPalExpenses(r, nil, ""); w.Code != http.StatusBadRequest {
		t.Errorf("empty rows: %d", w.Code)
	}
	var n int
	db.Model(&data_models.InvoicingExpense{}).Count(&n)
	if n != 0 {
		t.Fatalf("a refused confirm stored %d expenses", n)
	}
	noPayee := good
	noPayee.PayeeName = ""
	if w := confirmPayPalExpenses(r, []paypalExpenseCandidate{noPayee}, ""); w.Code != http.StatusOK {
		t.Fatalf("an outgoing row with no payee is still a valid expense: %d %s", w.Code, w.Body.String())
	}
}
