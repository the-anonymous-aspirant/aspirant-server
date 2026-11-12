package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"aspirant-online/server/data_models"
)

// system_3 #10555-A1 (design §3.143 Decision 5): editing a payment or an
// expense. A hand-entered row may change every field she typed; an imported
// row only her own notes on it. The invoice locks hold on every edit.

// newEditFixture is the invoice fixture (four imported PayPal payments, a
// client, a profile, the clock at 2026-10-21) plus the expense routes.
func newEditFixture(t *testing.T) *invoiceFixture {
	t.Helper()
	f := newInvoiceFixture(t)
	f.db.AutoMigrate(&data_models.InvoicingExpense{}, &data_models.InvoicingExpenseCategory{}, &data_models.InvoicingExpenseReceipt{})
	f.r.POST("/invoicing/expenses/manual", CreateManualExpenseHandler)
	f.r.PATCH("/invoicing/expenses/:id", PatchInvoicingExpenseHandler)
	return f
}

type errEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func errOf(t *testing.T, body []byte) errEnvelope {
	t.Helper()
	var e errEnvelope
	json.Unmarshal(body, &e)
	return e
}

func patchPayment(f *invoiceFixture, id uint, body string) (int, data_models.InvoicingPayment, []byte) {
	w := invoicingDo(f.r, "PATCH", fmt.Sprintf("/invoicing/payments/%d", id), body)
	var resp struct {
		Payment data_models.InvoicingPayment `json:"payment"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	return w.Code, resp.Payment, w.Body.Bytes()
}

func patchExpense(f *invoiceFixture, id uint, body string) (int, data_models.InvoicingExpense, []byte) {
	w := invoicingDo(f.r, "PATCH", fmt.Sprintf("/invoicing/expenses/%d", id), body)
	var resp struct {
		Expense data_models.InvoicingExpense `json:"expense"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	return w.Code, resp.Expense, w.Body.Bytes()
}

func paymentEvents(t *testing.T, f *invoiceFixture, id uint) []data_models.InvoicingAuditEvent {
	t.Helper()
	ev, err := data_models.ListInvoicingAuditEvents(f.db, invoiceFixtureUser, "payment", id)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	return ev
}

func TestEditPayment_HandEnteredChangesEveryFieldWithOneAuditRow(t *testing.T) {
	f := newEditFixture(t)
	code, p, body := createManual(t, f, `{"paid_on":"2026-07-14","gross_cents":42000,"method":"bank_transfer","reference":"RE-1","sender_name":"Galerie Nord","note":"Wandbild"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	code, got, raw := patchPayment(f, p.ID, `{"paid_on":"2026-07-15","gross_cents":43000,"method":"cash","reference":"RE-2","sender_name":"Galerie Süd","note":"Wandbild, zweiter Teil"}`)
	if code != http.StatusOK {
		t.Fatalf("patch: %d %s", code, raw)
	}
	stored, _ := data_models.GetInvoicingPayment(f.db, invoiceFixtureUser, p.ID)
	if stored.PaidOn.Format("2006-01-02") != "2026-07-15" || stored.GrossCents != 43000 || stored.NetCents != 43000 ||
		stored.Method != "cash" || stored.Reference != "RE-2" || stored.SenderName != "Galerie Süd" || stored.Note != "Wandbild, zweiter Teil" {
		t.Fatalf("stored after edit: %+v (response %+v)", stored, got)
	}
	ev := paymentEvents(t, f, p.ID)
	if len(ev) != 2 || ev[0].Action != "edit" {
		t.Fatalf("want create_manual + one edit, newest first: %+v", ev)
	}
	// Only the changed fields are in the record, before and after.
	if !strings.Contains(ev[0].Details, `"gross_cents":42000`) || !strings.Contains(ev[0].Details, `"gross_cents":43000`) ||
		strings.Contains(ev[0].Details, `"category"`) {
		t.Errorf("edit details: %s", ev[0].Details)
	}
}

func TestEditPayment_ImportedChangesOnlyHerNotes(t *testing.T) {
	f := newEditFixture(t)
	p := f.payments[3] // imported PayPal, gift, on no invoice
	if code, _, raw := patchPayment(f, p.ID, `{"note":"Geburtstag"}`); code != http.StatusOK {
		t.Fatalf("note: %d %s", code, raw)
	}
	if code, _, raw := patchPayment(f, p.ID, `{"category":"loan","client_id":null}`); code != http.StatusOK {
		t.Fatalf("category + client: %d %s", code, raw)
	}
	ev := paymentEvents(t, f, p.ID)
	if len(ev) != 2 || ev[0].Action != "categorise" || ev[1].Action != "edit" {
		t.Errorf("a note is an edit, a category/client change a categorise: %+v", ev)
	}

	before := countAuditEvents(t, f.db)
	for _, tc := range []struct{ field, body string }{
		{"paid_on", `{"paid_on":"2026-07-01"}`},
		{"gross_cents", `{"gross_cents":1}`},
		{"method", `{"method":"cash"}`},
		{"reference", `{"reference":"X"}`},
		{"sender_name", `{"sender_name":"Someone"}`},
		{"gross_cents", `{"note":"ok","gross_cents":1}`}, // one bank fact refuses the whole edit
	} {
		code, _, raw := patchPayment(f, p.ID, tc.body)
		e := errOf(t, raw)
		if code != http.StatusBadRequest || e.Error.Code != "bank_fact_field" || !strings.HasPrefix(e.Error.Message, tc.field+" ") {
			t.Errorf("%s: %d %s", tc.body, code, raw)
		}
	}
	if after := countAuditEvents(t, f.db); after != before {
		t.Errorf("refused edits wrote %d audit rows", after-before)
	}
	stored, _ := data_models.GetInvoicingPayment(f.db, invoiceFixtureUser, p.ID)
	if stored.Note != "Geburtstag" || stored.GrossCents != p.GrossCents || !stored.PaidOn.Equal(p.PaidOn) {
		t.Errorf("a refused edit changed the payment: %+v", stored)
	}
}

func TestEditPayment_SourceAndTransactionIDNeverChange(t *testing.T) {
	f := newEditFixture(t)
	_, manual, _ := createManual(t, f, `{"paid_on":"2026-07-14","gross_cents":1000,"method":"cash"}`)
	for _, id := range []uint{manual.ID, f.payments[3].ID} {
		for _, body := range []string{`{"source":"manual"}`, `{"transaction_id":"TX9"}`} {
			code, _, raw := patchPayment(f, id, body)
			if code != http.StatusBadRequest || errOf(t, raw).Error.Code != "unknown_field" {
				t.Errorf("payment %d %s: %d %s", id, body, code, raw)
			}
		}
	}
}

func TestEditPayment_IssuedInvoiceRefusesEveryRealChange(t *testing.T) {
	f := newEditFixture(t)
	id := f.draft(t, 0)
	code, detail, raw := f.issue(id)
	if code != http.StatusOK || detail.Invoice.InvoiceNumber == nil {
		t.Fatalf("issue: %d %s", code, raw)
	}
	p := f.payments[0]
	for _, body := range []string{`{"note":"changed"}`, `{"client_id":null}`, `{"category":"gift"}`} {
		code, _, raw := patchPayment(f, p.ID, body)
		e := errOf(t, raw)
		if code != http.StatusConflict || e.Error.Code != "payment_on_issued_invoice" || !strings.Contains(e.Error.Message, *detail.Invoice.InvoiceNumber) {
			t.Errorf("%s on an issued payment: %d %s", body, code, raw)
		}
	}
	// Re-sending what it already holds is no change, and passes.
	if code, _, raw := patchPayment(f, p.ID, fmt.Sprintf(`{"note":%q,"category":"paid_work"}`, p.Note)); code != http.StatusOK {
		t.Errorf("no-op on an issued payment: %d %s", code, raw)
	}
}

func TestEditPayment_DraftKeepsItsPaymentsPaidWork(t *testing.T) {
	f := newEditFixture(t)
	draftID := f.draft(t, 1)
	p := f.payments[1]
	code, _, raw := patchPayment(f, p.ID, `{"category":"gift"}`)
	e := errOf(t, raw)
	if code != http.StatusConflict || e.Error.Code != "payment_on_draft" || !strings.Contains(e.Error.Message, fmt.Sprintf("D-%d", draftID)) {
		t.Errorf("category off paid work on a draft: %d %s", code, raw)
	}
	if code, _, raw := patchPayment(f, p.ID, `{"note":"changed on a draft"}`); code != http.StatusOK {
		t.Errorf("note on a draft payment: %d %s", code, raw)
	}

	// A hand-entered payment on a draft may change its amount: the draft
	// keeps the amount it was made with.
	_, manual, _ := createManual(t, f, fmt.Sprintf(`{"paid_on":"2026-07-14","gross_cents":1000,"method":"cash","client_id":%d}`, f.clientID))
	if w := invoicingDo(f.r, "POST", "/invoicing/invoices", fmt.Sprintf(`{"payment_ids":[%d]}`, manual.ID)); w.Code != http.StatusCreated {
		t.Fatalf("draft the manual payment: %d %s", w.Code, w.Body.String())
	}
	if code, got, raw := patchPayment(f, manual.ID, `{"gross_cents":2000}`); code != http.StatusOK || got.GrossCents != 2000 {
		t.Errorf("amount on a drafted manual payment: %d %s", code, raw)
	}
}

func TestEditPayment_ChangedPayPalReferenceIsDeduplicated(t *testing.T) {
	f := newEditFixture(t)
	_, manual, _ := createManual(t, f, `{"paid_on":"2026-07-14","gross_cents":1000,"method":"bank_transfer","reference":"RE-1"}`)
	// TX1 is an imported PayPal payment in the fixture; the lower-case
	// spelling is the same id once stored upper-case.
	code, _, raw := patchPayment(f, manual.ID, `{"method":"paypal","reference":"tx1"}`)
	if code != http.StatusConflict || errOf(t, raw).Error.Code != "duplicate_payment" {
		t.Errorf("change to an imported PayPal id: %d %s", code, raw)
	}
	if code, got, raw := patchPayment(f, manual.ID, `{"method":"paypal","reference":"tx77"}`); code != http.StatusOK || got.Reference != "TX77" {
		t.Errorf("change to a fresh PayPal id: %d %s", code, raw)
	}
	// An unrelated edit leaves the stored reference exactly as it is.
	f.db.Model(&data_models.InvoicingPayment{}).Where("id = ?", manual.ID).UpdateColumn("reference", "tx77")
	if code, _, raw := patchPayment(f, manual.ID, `{"note":"n"}`); code != http.StatusOK {
		t.Fatalf("note: %d %s", code, raw)
	}
	stored, _ := data_models.GetInvoicingPayment(f.db, invoiceFixtureUser, manual.ID)
	if stored.Reference != "tx77" {
		t.Errorf("a note edit rewrote the reference: %q", stored.Reference)
	}
}

func TestEditExpense_SplitPayeeAndOwner(t *testing.T) {
	f := newEditFixture(t)
	code, e, body := createExpense(t, f.r, `{"paid_on":"2026-07-01","amount_cents":1500,"payee":"Musikhaus Thomann","note":"Saiten"}`)
	if code != http.StatusCreated || e.Payee != "Musikhaus Thomann" {
		t.Fatalf("create with payee: %d %s", code, body)
	}
	code, got, raw := patchExpense(f, e.ID, `{"paid_on":"2026-07-02","amount_cents":1600,"payee":"Thomann","reference":"R-9","note":"Saiten, Satz","category":"travel"}`)
	if code != http.StatusOK || got.AmountCents != 1600 || got.Payee != "Thomann" || got.Category != "travel" || got.Reference != "R-9" {
		t.Fatalf("edit: %d %s", code, raw)
	}
	ev, _ := data_models.ListInvoicingAuditEvents(f.db, invoiceFixtureUser, "expense", e.ID)
	if len(ev) != 2 || ev[0].Action != "edit" {
		t.Errorf("expense audit: %+v", ev)
	}
	if code, _, raw := patchExpense(f, e.ID, `{"category":"not mine"}`); code != http.StatusBadRequest {
		t.Errorf("unknown category: %d %s", code, raw)
	}

	imported := data_models.InvoicingExpense{
		OwnerUserID: invoiceFixtureUser, Source: "n26", TransactionID: "N26-1",
		PaidOn: time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC), Currency: "EUR", AmountCents: 999,
		Category: "other", Entry: data_models.ExpenseEntryImport, Reference: "REWE",
	}
	if err := f.db.Create(&imported).Error; err != nil {
		t.Fatalf("seed imported expense: %v", err)
	}
	for _, b := range []string{`{"amount_cents":1}`, `{"paid_on":"2026-07-01"}`, `{"payee":"x"}`, `{"reference":"x"}`} {
		if code, _, raw := patchExpense(f, imported.ID, b); code != http.StatusBadRequest || errOf(t, raw).Error.Code != "bank_fact_field" {
			t.Errorf("imported %s: %d %s", b, code, raw)
		}
	}
	if code, _, raw := patchExpense(f, imported.ID, `{"note":"Wocheneinkauf","category":"travel"}`); code != http.StatusOK {
		t.Errorf("imported note + category: %d %s", code, raw)
	}

	other := setupInvoiceRouter(f.db, invoiceFixtureUser+1)
	other.PATCH("/invoicing/expenses/:id", PatchInvoicingExpenseHandler)
	if w := invoicingDo(other, "PATCH", fmt.Sprintf("/invoicing/expenses/%d", e.ID), `{"note":"x"}`); w.Code != http.StatusNotFound {
		t.Errorf("another owner's expense: %d %s", w.Code, w.Body.String())
	}
}

// The edit rules restate the hand-entry rules from the same constants; this
// pins them to the same refusal, message for message, so a later change to
// one side cannot leave the other behind.
func TestEdit_RefusesWhatHandEntryRefuses(t *testing.T) {
	f := newEditFixture(t)
	_, payment, _ := createManual(t, f, `{"paid_on":"2026-07-14","gross_cents":1000,"method":"cash"}`)
	_, expense, _ := createExpense(t, f.r, `{"paid_on":"2026-07-01","amount_cents":1500}`)

	long := strings.Repeat("x", 300)
	paymentBase := map[string]interface{}{"paid_on": "2026-07-14", "gross_cents": 1000, "method": "cash"}
	expenseBase := map[string]interface{}{"paid_on": "2026-07-01", "amount_cents": 1500}
	cases := []struct {
		kind  string
		field string
		value interface{}
	}{
		{"payment", "paid_on", "2026-12-01"},
		{"payment", "paid_on", "2026-13-01"},
		{"payment", "paid_on", "1999-12-31"},
		{"payment", "gross_cents", 0},
		{"payment", "method", "bitcoin"},
		{"payment", "reference", long},
		{"payment", "sender_name", "two\nlines"},
		{"payment", "note", "bell\u0007"},
		{"expense", "paid_on", "2026-12-01"},
		{"expense", "amount_cents", -5},
		{"expense", "reference", long},
		{"expense", "note", "bell\u0007"},
		{"expense", "payee", long},
	}
	for _, tc := range cases {
		base, createPath, patchPath := paymentBase, "/invoicing/payments/manual", fmt.Sprintf("/invoicing/payments/%d", payment.ID)
		if tc.kind == "expense" {
			base, createPath, patchPath = expenseBase, "/invoicing/expenses/manual", fmt.Sprintf("/invoicing/expenses/%d", expense.ID)
		}
		create := map[string]interface{}{}
		for k, v := range base {
			create[k] = v
		}
		create[tc.field] = tc.value
		cb, _ := json.Marshal(create)
		pb, _ := json.Marshal(map[string]interface{}{tc.field: tc.value})

		cw := invoicingDo(f.r, "POST", createPath, string(cb))
		pw := invoicingDo(f.r, "PATCH", patchPath, string(pb))
		ce, pe := errOf(t, cw.Body.Bytes()), errOf(t, pw.Body.Bytes())
		if cw.Code != http.StatusBadRequest || pw.Code != http.StatusBadRequest || ce.Error.Message == "" || ce.Error.Message != pe.Error.Message {
			t.Errorf("%s %s=%v: create %d %q, edit %d %q", tc.kind, tc.field, tc.value, cw.Code, ce.Error.Message, pw.Code, pe.Error.Message)
		}
	}
}
