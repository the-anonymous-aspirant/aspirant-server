package handlers

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"aspirant-online/server/data_models"
)

// system_3 #12258: a total never adds two currencies, and the Book counts
// euros (design ruling on #12258).

func seedPaymentInCurrency(t *testing.T, f *invoiceFixture, txn, currency string, cents int64) data_models.InvoicingPayment {
	t.Helper()
	cid := f.clientID
	p := data_models.InvoicingPayment{
		OwnerUserID: invoiceFixtureUser, Source: "paypal", Entry: data_models.PaymentEntryImport, TransactionID: txn,
		Kind: "incoming", PaidOn: time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC), Currency: currency,
		GrossCents: cents, Note: "Invented work", Category: "paid_work", ClientID: &cid,
	}
	if err := f.db.Create(&p).Error; err != nil {
		t.Fatalf("seed %s: %v", txn, err)
	}
	return p
}

func expectMixedCurrencies(t *testing.T, what string, code int, body string) {
	t.Helper()
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, `"mixed_currencies"`) || !strings.Contains(body, "EUR, USD") {
		t.Errorf("%s: got %d %s, want 422 mixed_currencies naming EUR, USD", what, code, body)
	}
}

func TestCreateInvoice_PaymentsInTwoCurrenciesAreRefused(t *testing.T) {
	f := newInvoiceFixture(t)
	usd := seedPaymentInCurrency(t, f, "TX-USD", "USD", 10000)
	w := invoicingDo(f.r, "POST", "/invoicing/invoices", fmt.Sprintf(`{"payment_ids":[%d,%d]}`, f.payments[0].ID, usd.ID))
	expectMixedCurrencies(t, "create over EUR and USD", w.Code, w.Body.String())
	var n int
	f.db.Model(&data_models.InvoicingInvoice{}).Count(&n)
	if n != 0 {
		t.Errorf("%d invoices were made", n)
	}

	// One currency other than euros stays allowed: its PDF prints its own.
	w = invoicingDo(f.r, "POST", "/invoicing/invoices", fmt.Sprintf(`{"payment_ids":[%d]}`, usd.ID))
	if w.Code != http.StatusCreated || decodeDetail(t, w.Body.Bytes()).Invoice.Currency != "USD" {
		t.Errorf("a USD-only draft: %d %s", w.Code, w.Body.String())
	}
}

func TestUpdateDraft_AddingAPaymentInAnotherCurrencyIsRefused(t *testing.T) {
	f := newInvoiceFixture(t)
	usd := seedPaymentInCurrency(t, f, "TX-USD", "USD", 10000)
	draft := f.draft(t, 0)
	path := fmt.Sprintf("/invoicing/invoices/%d", draft)
	w := invoicingDo(f.r, "PUT", path, fmt.Sprintf(`{"payment_ids":[%d,%d]}`, f.payments[0].ID, usd.ID))
	expectMixedCurrencies(t, "edit adding USD to a EUR draft", w.Code, w.Body.String())
	// Swapping the EUR payment for the USD one is a second currency too: the
	// draft's own is EUR.
	w = invoicingDo(f.r, "PUT", path, fmt.Sprintf(`{"payment_ids":[%d]}`, usd.ID))
	expectMixedCurrencies(t, "edit swapping a EUR draft's payment for a USD one", w.Code, w.Body.String())
	var links int
	f.db.Model(&data_models.InvoicingInvoicePayment{}).Where("invoice_id = ?", draft).Count(&links)
	if links != 1 {
		t.Errorf("the draft has %d payments, want its one EUR payment", links)
	}
	if w := invoicingDo(f.r, "PUT", path, fmt.Sprintf(`{"payment_ids":[%d,%d]}`, f.payments[0].ID, f.payments[1].ID)); w.Code != http.StatusOK {
		t.Errorf("a EUR edit: %d %s", w.Code, w.Body.String())
	}
}

// A draft made before #12258 may already hold two currencies: it is not
// issued as one total.
func TestIssue_ADraftHoldingTwoCurrenciesIsRefused(t *testing.T) {
	f := newInvoiceFixture(t)
	usd := seedPaymentInCurrency(t, f, "TX-USD", "USD", 10000)
	draft := f.draft(t, 0)
	if err := f.db.Create(&data_models.InvoicingInvoicePayment{InvoiceID: draft, PaymentID: usd.ID}).Error; err != nil {
		t.Fatalf("link the USD payment as an old draft would have: %v", err)
	}
	code, _, body := f.issue(draft)
	expectMixedCurrencies(t, "issue", code, body)
	var inv data_models.InvoicingInvoice
	f.db.First(&inv, draft)
	if inv.Status != data_models.InvoiceStatusDraft || inv.InvoiceNumber != nil {
		t.Errorf("the draft was issued: %+v", inv)
	}
}

// Generate makes one draft per payment, so a selection in two currencies is
// two one-currency drafts.
func TestGenerate_ASelectionInTwoCurrenciesMakesOneDraftEach(t *testing.T) {
	f := newInvoiceFixture(t)
	usd := seedPaymentInCurrency(t, f, "TX-USD", "USD", 10000)
	w := invoicingDo(f.r, "POST", "/invoicing/invoices/generate", fmt.Sprintf(`{"payment_ids":[%d,%d]}`, f.payments[0].ID, usd.ID))
	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Fatalf("generate: %d %s", w.Code, w.Body.String())
	}
	var currencies []string
	f.db.Model(&data_models.InvoicingInvoice{}).Order("currency").Pluck("currency", &currencies)
	if strings.Join(currencies, ",") != "EUR,USD" {
		t.Errorf("drafts made in %v, want one EUR and one USD", currencies)
	}
}

func TestManualEntry_IsInEuros(t *testing.T) {
	f := newInvoiceFixture(t)
	for _, cur := range []string{"USD", "XXX", "eur"} {
		code, _, body := createManual(t, f, fmt.Sprintf(`{"paid_on":"2026-07-14","gross_cents":4200,"method":"cash","currency":%q,"client_id":%d}`, cur, f.clientID))
		if code != http.StatusBadRequest || !strings.Contains(body, "must be EUR") {
			t.Errorf("payment in %s: %d %s, want 400 must be EUR", cur, code, body)
		}
	}
	for _, cur := range []string{`,"currency":"EUR"`, ""} {
		code, p, body := createManual(t, f, fmt.Sprintf(`{"paid_on":"2026-07-14","gross_cents":4200,"method":"cash"%s,"client_id":%d}`, cur, f.clientID))
		if code != http.StatusCreated || p.Currency != "EUR" {
			t.Errorf("payment with %q: %d %s", cur, code, body)
		}
	}

	r := setupExpensesRouter(setupExpensesDB(t), invoiceFixtureUser)
	for _, cur := range []string{"USD", "XXX"} {
		code, _, body := createExpense(t, r, fmt.Sprintf(`{"paid_on":"2026-07-14","amount_cents":1299,"currency":%q}`, cur))
		if code != http.StatusBadRequest || !strings.Contains(body, "must be EUR") {
			t.Errorf("expense in %s: %d %s, want 400 must be EUR", cur, code, body)
		}
	}
	for _, cur := range []string{`,"currency":"EUR"`, ""} {
		code, e, body := createExpense(t, r, fmt.Sprintf(`{"paid_on":"2026-07-14","amount_cents":1299%s}`, cur))
		if code != http.StatusCreated || e.Currency != "EUR" {
			t.Errorf("expense with %q: %d %s", cur, code, body)
		}
	}
}

// The Book CSV sums euros only. Two USD rows are counted by the selection:
// a payment in July beside euro rows, and an expense alone in August. Neither
// is in any sum, August gets no column of its own, and the last row says
// what was left out.
func TestExportBookReport_RowsInAnotherCurrencyAreNotCounted(t *testing.T) {
	db := setupBookReportDB(t)
	seedPayment(t, db, bookReportUser, "incoming", "P1", "2026-07-14", 35000)
	seedExpense(t, db, bookReportUser, "E1", "2026-07-20", "materials and supplies", 4599)
	usd := data_models.InvoicingPayment{
		OwnerUserID: bookReportUser, Source: "paypal", Entry: "import", TransactionID: "PP-USD", Kind: "incoming",
		Category: data_models.PaymentCategoryPaidWork, PaidOn: mustParseDay(t, "2026-07-16"), Currency: "USD", GrossCents: 10000,
	}
	usdExpense := data_models.InvoicingExpense{
		OwnerUserID: bookReportUser, Source: "paypal", Entry: "import", TransactionID: "PP-USD-E", PaidOn: mustParseDay(t, "2026-08-03"),
		Currency: "USD", AmountCents: 2500, Category: "materials and supplies",
	}
	for _, row := range []interface{}{&usd, &usdExpense} {
		if err := db.Create(row).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	r := bookReportRouter(db)
	withInvoicingClock(t, exportClock)

	rows, _ := bookReportCSV(t, r)
	want := [][]string{
		{"Kategorie", "2026-07", "Gesamt"},
		{"Einnahmen", "350,00", "350,00"},
	}
	for i, w := range want {
		if joinRow(rows[i]) != joinRow(w) {
			t.Errorf("row %d: got %q want %q", i, rows[i], w)
		}
	}
	if got := readBookReportRow(rows, "materials and supplies"); joinRow(got) != "materials and supplies|45,99|45,99" {
		t.Errorf("materials: %q", got)
	}
	if got := readBookReportRow(rows, "Ergebnis"); joinRow(got) != "Ergebnis|304,01|304,01" {
		t.Errorf("Ergebnis: %q", got)
	}
	if got := rows[len(rows)-1]; joinRow(got) != "Nicht enthalten: 2 Einträge in anderer Währung (USD)||" {
		t.Errorf("last row: %q", got)
	}

	month := func(m string) [][]string {
		w := invoicingDo(r, "GET", "/invoicing/export/book-report.csv?month="+m, "")
		if w.Code != http.StatusOK {
			t.Fatalf("export %s: %d %s", m, w.Code, w.Body.String())
		}
		return readClientsCSV(t, w.Body.String())
	}
	if rows := month("2026-07"); joinRow(rows[len(rows)-1]) != "Nicht enthalten: 1 Eintrag in anderer Währung (USD)||" {
		t.Errorf("July's last row: %q", rows[len(rows)-1])
	}
	if rows := month("2026-08"); joinRow(readBookReportRow(rows, "materials and supplies")) != "materials and supplies|0,00|0,00" ||
		joinRow(rows[len(rows)-1]) != "Nicht enthalten: 1 Eintrag in anderer Währung (USD)||" {
		t.Errorf("August: %q", rows)
	}
	if rows := month("2026-09"); rows[len(rows)-1][0] != "Ergebnis" {
		t.Errorf("September has no rows left out, so no line: %q", rows[len(rows)-1])
	}
}

// A book with euros only gets the file it got before #12258: Ergebnis last.
func TestExportBookReport_AEuroOnlyBookHasNoNotCountedLine(t *testing.T) {
	db := setupBookReportDB(t)
	seedPayment(t, db, bookReportUser, "incoming", "P1", "2026-07-14", 35000)
	withInvoicingClock(t, exportClock)
	rows, _ := bookReportCSV(t, bookReportRouter(db))
	if rows[len(rows)-1][0] != "Ergebnis" {
		t.Errorf("last row %q", rows[len(rows)-1])
	}
	for _, r := range rows {
		if strings.Contains(joinRow(r), "Nicht enthalten") {
			t.Errorf("row %q", r)
		}
	}
}
