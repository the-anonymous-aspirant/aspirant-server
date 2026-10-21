// Package invoicing_pdf renders the invoicing app's German invoices
// (system_3 #8367-C1). Document builds the content from an issued invoice's
// snapshot; Render lays it out on A4.
//
// The legal sentences are copied byte for byte from the spec (c51399 on
// #4383, §4.5) and must not be reworded or extended: whether they suffice is
// the lawyer review on the operator checklist (#8368), not a code decision.
package invoicing_pdf

import (
	"fmt"
	"strings"
	"time"

	"aspirant-online/server/data_models"
)

// Legal text, verbatim from spec §4.5. The %s slots are the spec's
// [Datum]/[PayPal/Stripe]/… and [Ausstellungsdatum] placeholders.
const (
	TitleRechnung        = "Rechnung"
	Paragraph19UStGLine  = "Gemäß § 19 UStG wird keine Umsatzsteuer berechnet."
	paymentReceivedLine  = "Zahlung erhalten am %s per %s (Transaktions-ID: %s)"
	retroactiveIssueLine = "Diese Rechnung wurde am %s nachträglich für die bereits erbrachte und bezahlte Leistung ausgestellt."
)

// Field labels. These name the §14 UStG fields; they carry no legal claim.
const (
	labelSteuernummer      = "Steuernummer"
	labelRechnungsnummer   = "Rechnungsnummer"
	labelRechnungsdatum    = "Rechnungsdatum"
	labelLeistungsdatum    = "Leistungsdatum"
	labelLeistungszeitraum = "Leistungszeitraum"
	labelNetto             = "Nettobetrag"
	labelUmsatzsteuer      = "Umsatzsteuer"
	labelBrutto            = "Gesamtbetrag (brutto)"
	labelGesamt            = "Gesamtbetrag"
)

// ItemHeaders are the line-item table's column headings.
var ItemHeaders = [5]string{"Pos.", "Beschreibung", "Menge", "Einzelpreis", "Gesamt"}

// Field is a label with its value.
type Field struct {
	Label string
	Value string
}

// ItemRow is one formatted line item.
type ItemRow struct {
	Pos, Description, Quantity, UnitPrice, Total string
}

// Document is everything printed on the invoice, in reading order.
type Document struct {
	Issuer    []string // name, address lines, Steuernummer, email
	Recipient []string // client name and address lines
	Title     string
	Meta      []Field // number, issue date, service date or period
	Items     []ItemRow
	Totals    []Field
	Notes     []string // § 19 line, payment confirmations, retroactive note
}

// Input is an issued invoice with the snapshots frozen at issue.
type Input struct {
	Profile  data_models.InvoicingProfile
	Client   data_models.InvoicingClient
	Invoice  data_models.InvoicingInvoice
	Items    []data_models.InvoicingLineItem
	Payments []data_models.InvoicingPayment
}

// FormatDate prints a date the German way: 21.10.2026.
func FormatDate(t time.Time) string {
	return t.Format("02.01.2006")
}

// FormatEuro prints cents as German money: 1.234,56 €.
func FormatEuro(cents int64, currency string) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	whole := fmt.Sprint(cents / 100)
	var grouped []string
	for len(whole) > 3 {
		grouped = append([]string{whole[len(whole)-3:]}, grouped...)
		whole = whole[:len(whole)-3]
	}
	grouped = append([]string{whole}, grouped...)
	symbol := currency
	if currency == "" || currency == "EUR" {
		symbol = "€"
	}
	return fmt.Sprintf("%s%s,%02d %s", sign, strings.Join(grouped, "."), cents%100, symbol)
}

// formatDecimalMilli prints a value stored in thousandths with a German
// decimal comma and no trailing zeros: 1000 → "1", 2500 → "2,5".
func formatDecimalMilli(v int64) string {
	s := fmt.Sprintf("%d,%03d", v/1000, v%1000)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ",")
}

// FormatVatRate prints basis points as a percentage: 1900 → "19 %".
func FormatVatRate(bp int) string {
	return formatDecimalMilli(int64(bp)*10) + " %"
}

// sourceName is how a payment source reads in the confirmation sentence,
// matching the spec's [PayPal/Stripe] placeholder.
func sourceName(source string) string {
	switch strings.ToLower(source) {
	case "paypal":
		return "PayPal"
	case "stripe":
		return "Stripe"
	default:
		return source
	}
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// Build turns an issued invoice into the printed document. It fails on an
// invoice that has not been issued: there is no number or issue date to
// print, and an unissued PDF is not something this app produces.
func Build(in Input) (Document, error) {
	inv := in.Invoice
	if inv.InvoiceNumber == nil || inv.IssueDate == nil || inv.ServiceDateFrom == nil {
		return Document{}, fmt.Errorf("invoice %d is not issued", inv.ID)
	}
	issueDate := FormatDate(*inv.IssueDate)

	doc := Document{Title: TitleRechnung}
	doc.Issuer = append([]string{in.Profile.FullName}, nonEmptyLines(in.Profile.Address)...)
	doc.Issuer = append(doc.Issuer, labelSteuernummer+": "+in.Profile.Steuernummer)
	if in.Profile.Email != "" {
		doc.Issuer = append(doc.Issuer, in.Profile.Email)
	}
	doc.Recipient = append([]string{in.Client.Name}, nonEmptyLines(in.Client.Address)...)

	doc.Meta = []Field{
		{labelRechnungsnummer, *inv.InvoiceNumber},
		{labelRechnungsdatum, issueDate},
	}
	from := *inv.ServiceDateFrom
	if inv.ServiceDateTo == nil || inv.ServiceDateTo.Equal(from) {
		doc.Meta = append(doc.Meta, Field{labelLeistungsdatum, FormatDate(from)})
	} else {
		doc.Meta = append(doc.Meta, Field{labelLeistungszeitraum, FormatDate(from) + " – " + FormatDate(*inv.ServiceDateTo)})
	}

	for i, it := range in.Items {
		doc.Items = append(doc.Items, ItemRow{
			Pos:         fmt.Sprint(i + 1),
			Description: it.Description,
			Quantity:    formatDecimalMilli(it.QuantityMilli),
			UnitPrice:   FormatEuro(it.UnitPriceCents, inv.Currency),
			Total:       FormatEuro(it.LineTotalCents, inv.Currency),
		})
	}

	if in.Profile.IsKleinunternehmer {
		doc.Totals = []Field{{labelGesamt, FormatEuro(inv.TotalCents, inv.Currency)}}
		doc.Notes = append(doc.Notes, Paragraph19UStGLine)
	} else {
		doc.Totals = []Field{
			{labelNetto, FormatEuro(inv.NetCents, inv.Currency)},
			{labelUmsatzsteuer + " " + FormatVatRate(inv.VatRateBP), FormatEuro(inv.VatCents, inv.Currency)},
			{labelBrutto, FormatEuro(inv.TotalCents, inv.Currency)},
		}
	}

	for _, p := range in.Payments {
		doc.Notes = append(doc.Notes, fmt.Sprintf(paymentReceivedLine, FormatDate(p.PaidOn), sourceName(p.Source), p.TransactionID))
	}
	doc.Notes = append(doc.Notes, fmt.Sprintf(retroactiveIssueLine, issueDate))
	return doc, nil
}
