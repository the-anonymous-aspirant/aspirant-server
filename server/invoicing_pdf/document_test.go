package invoicing_pdf

import (
	"strings"
	"testing"
	"time"

	"aspirant-online/server/data_models"
)

// #8367-C1: the printed content. The legal sentences are compared byte for
// byte with the spec (c51399, §4.5), placeholders filled.

func day(y, m, d int) *time.Time {
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	return &t
}

func issuedInput(klein bool) Input {
	num, n := "2026-001", 1
	return Input{
		Profile: data_models.InvoicingProfile{
			FullName: "Flower Example", Address: "Beispielstraße 1\n10117 Berlin",
			Steuernummer: "12/345/67890", Email: "flower@example.test", IsKleinunternehmer: klein,
			InvoiceNumberPrefix: "2026",
		},
		Client: data_models.InvoicingClient{Name: "Łukasz Müller GmbH", Address: "Nordweg 2\n13353 Berlin"},
		Invoice: data_models.InvoicingInvoice{
			ID: 1, Status: "issued", Number: &n, InvoiceNumber: &num, Currency: "EUR",
			IssueDate: day(2026, 10, 21), ServiceDateFrom: day(2026, 7, 14), ServiceDateTo: day(2026, 7, 14),
			VatRateBP: 1900, NetCents: 123456, VatCents: 23457, TotalCents: 146913,
		},
		Items: []data_models.InvoicingLineItem{
			{Position: 1, Description: "Wandbild „Sommer“", QuantityMilli: 2500, UnitPriceCents: 1999, LineTotalCents: 4998},
		},
		Payments: []data_models.InvoicingPayment{
			{Source: "paypal", TransactionID: "1AA11111AA1111111", PaidOn: time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)},
		},
	}
}

func notes(d Document) string { return strings.Join(d.Notes, "\n") }

func TestBuild_KleinunternehmerinCarriesEveryParagraph14Field(t *testing.T) {
	in := issuedInput(true)
	in.Invoice.TotalCents = 4998
	d, err := Build(in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if d.Title != "Rechnung" {
		t.Errorf("title %q", d.Title)
	}
	wantIssuer := []string{"Flower Example", "Beispielstraße 1", "10117 Berlin", "Steuernummer: 12/345/67890", "flower@example.test"}
	if strings.Join(d.Issuer, "|") != strings.Join(wantIssuer, "|") {
		t.Errorf("issuer: %q", d.Issuer)
	}
	if strings.Join(d.Recipient, "|") != "Łukasz Müller GmbH|Nordweg 2|13353 Berlin" {
		t.Errorf("recipient (names print exactly, beyond Latin-1 too): %q", d.Recipient)
	}
	wantMeta := []Field{{"Rechnungsnummer", "2026-001"}, {"Rechnungsdatum", "21.10.2026"}, {"Leistungsdatum", "14.07.2026"}}
	for i, f := range wantMeta {
		if i >= len(d.Meta) || d.Meta[i] != f {
			t.Errorf("meta[%d]: got %+v, want %+v", i, d.Meta, f)
		}
	}
	if len(d.Items) != 1 || d.Items[0] != (ItemRow{"1", "Wandbild „Sommer“", "2,5", "19,99 €", "49,98 €"}) {
		t.Errorf("items: %+v", d.Items)
	}
	if len(d.Totals) != 1 || d.Totals[0] != (Field{"Gesamtbetrag", "49,98 €"}) {
		t.Errorf("totals without VAT rows: %+v", d.Totals)
	}
	want := []string{
		"Gemäß § 19 UStG wird keine Umsatzsteuer berechnet.",
		"Zahlung erhalten am 14.07.2026 per PayPal (Transaktions-ID: 1AA11111AA1111111)",
		"Diese Rechnung wurde am 21.10.2026 nachträglich für die bereits erbrachte und bezahlte Leistung ausgestellt.",
	}
	if strings.Join(d.Notes, "\n") != strings.Join(want, "\n") {
		t.Errorf("legal text differs from the spec:\n got: %q\nwant: %q", d.Notes, want)
	}
}

func TestBuild_RegularVatShowsNetRateVatGrossAndNoParagraph19(t *testing.T) {
	d, err := Build(issuedInput(false))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	want := []Field{
		{"Nettobetrag", "1.234,56 €"},
		{"Umsatzsteuer 19 %", "234,57 €"},
		{"Gesamtbetrag (brutto)", "1.469,13 €"},
	}
	if len(d.Totals) != 3 {
		t.Fatalf("totals: %+v", d.Totals)
	}
	for i := range want {
		if d.Totals[i] != want[i] {
			t.Errorf("totals[%d]: got %+v, want %+v", i, d.Totals[i], want[i])
		}
	}
	if strings.Contains(notes(d), "§ 19") {
		t.Errorf("a VAT-charging invoice must not carry the § 19 line")
	}
}

func TestBuild_ServicePeriodAndSeveralPayments(t *testing.T) {
	in := issuedInput(true)
	in.Invoice.ServiceDateTo = day(2026, 8, 3)
	in.Payments = append(in.Payments, data_models.InvoicingPayment{Source: "stripe", TransactionID: "ch_123", PaidOn: time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)})
	d, err := Build(in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if d.Meta[2] != (Field{"Leistungszeitraum", "14.07.2026 – 03.08.2026"}) {
		t.Errorf("period: %+v", d.Meta[2])
	}
	if !strings.Contains(notes(d), "Zahlung erhalten am 03.08.2026 per Stripe (Transaktions-ID: ch_123)") {
		t.Errorf("one confirmation per payment: %q", d.Notes)
	}
}

func TestBuild_RefusesAnUnissuedInvoice(t *testing.T) {
	in := issuedInput(true)
	in.Invoice.InvoiceNumber, in.Invoice.IssueDate = nil, nil
	if _, err := Build(in); err == nil {
		t.Fatal("built a document for a draft")
	}
}

func TestFormatters(t *testing.T) {
	money := map[int64]string{0: "0,00 €", 5: "0,05 €", 123456: "1.234,56 €", 100000000: "1.000.000,00 €", -1050: "-10,50 €"}
	for in, want := range money {
		if got := FormatEuro(in, "EUR"); got != want {
			t.Errorf("FormatEuro(%d) = %q, want %q", in, got, want)
		}
	}
	if got := FormatEuro(500, "USD"); got != "5,00 USD" {
		t.Errorf("non-EUR: %q", got)
	}
	qty := map[int64]string{1000: "1", 2500: "2,5", 1250: "1,25", 500: "0,5", 12000: "12"}
	for in, want := range qty {
		if got := formatDecimalMilli(in); got != want {
			t.Errorf("formatDecimalMilli(%d) = %q, want %q", in, got, want)
		}
	}
	for bp, want := range map[int]string{1900: "19 %", 700: "7 %", 1950: "19,5 %"} {
		if got := FormatVatRate(bp); got != want {
			t.Errorf("FormatVatRate(%d) = %q, want %q", bp, got, want)
		}
	}
}
