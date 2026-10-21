package handlers

import (
	"errors"
	"strings"
	"testing"
	"time"
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
