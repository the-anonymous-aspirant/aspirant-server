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

// system_3 #10555-A2 (design §3.143 Decision 6): remove, restore and delete
// for good, for payments and expenses, single and bulk, all or nothing.

func newRemoveFixture(t *testing.T) *invoiceFixture {
	t.Helper()
	f := newEditFixture(t)
	f.db.AutoMigrate(&data_models.InvoicingExpenseTombstone{},
		&data_models.InvoicingBookSelection{}, &data_models.InvoicingBookPreset{})
	f.r.GET("/invoicing/expenses", ListInvoicingExpensesHandler)
	f.r.GET("/invoicing/export/book-report.csv", ExportBookReportHandler)
	for _, a := range []string{"remove", "restore", "purge"} {
		f.r.POST("/invoicing/payments/"+a, PaymentsRowActionHandler(a))
		f.r.POST("/invoicing/payments/:id/"+a, PaymentsRowActionHandler(a))
		f.r.POST("/invoicing/expenses/"+a, ExpensesRowActionHandler(a))
		f.r.POST("/invoicing/expenses/:id/"+a, ExpensesRowActionHandler(a))
	}
	return f
}

func rowAction(f *invoiceFixture, kind, action string, ids ...uint) (int, errEnvelope, string) {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprint(id))
	}
	w := invoicingDo(f.r, "POST", fmt.Sprintf("/invoicing/%s/%s", kind, action), `{"ids":[`+strings.Join(parts, ",")+`]}`)
	var e errEnvelope
	json.Unmarshal(w.Body.Bytes(), &e)
	return w.Code, e, w.Body.String()
}

func listedPaymentIDs(t *testing.T, f *invoiceFixture, query string) map[uint]bool {
	t.Helper()
	w := invoicingDo(f.r, "GET", "/invoicing/payments"+query, "")
	if w.Code != http.StatusOK {
		t.Fatalf("list payments%s: %d %s", query, w.Code, w.Body.String())
	}
	var resp struct {
		Payments []data_models.InvoicingPayment `json:"payments"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	out := map[uint]bool{}
	for _, p := range resp.Payments {
		out[p.ID] = true
	}
	return out
}

func listedExpenseIDs(t *testing.T, f *invoiceFixture, query string) map[uint]bool {
	t.Helper()
	w := invoicingDo(f.r, "GET", "/invoicing/expenses"+query, "")
	if w.Code != http.StatusOK {
		t.Fatalf("list expenses%s: %d %s", query, w.Code, w.Body.String())
	}
	var resp struct {
		Expenses []data_models.InvoicingExpense `json:"expenses"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	out := map[uint]bool{}
	for _, e := range resp.Expenses {
		out[e.ID] = true
	}
	return out
}

func auditActions(t *testing.T, f *invoiceFixture, entity string, id uint) []string {
	t.Helper()
	ev, err := data_models.ListInvoicingAuditEvents(f.db, invoiceFixtureUser, entity, id)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	out := make([]string, 0, len(ev))
	for _, e := range ev {
		out = append(out, e.Action)
	}
	return out // newest first
}

func removedAt(t *testing.T, f *invoiceFixture, id uint) *time.Time {
	t.Helper()
	p, err := data_models.GetInvoicingPayment(f.db, invoiceFixtureUser, id)
	if err != nil || p == nil {
		t.Fatalf("read payment %d: %v", id, err)
	}
	return p.RemovedAt
}

func TestRemoveRestorePayment_ListsAndAudit(t *testing.T) {
	f := newRemoveFixture(t)
	p := f.payments[3] // gift, on no invoice
	// The single form.
	if w := invoicingDo(f.r, "POST", fmt.Sprintf("/invoicing/payments/%d/remove", p.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("remove: %d %s", w.Code, w.Body.String())
	}
	if removedAt(t, f, p.ID) == nil {
		t.Fatal("remove did not record when")
	}
	if listedPaymentIDs(t, f, "")[p.ID] || !listedPaymentIDs(t, f, "?removed=true")[p.ID] {
		t.Error("a removed payment must leave the list and appear in the removed view")
	}
	if listedPaymentIDs(t, f, "?removed=true")[f.payments[0].ID] {
		t.Error("the removed view lists only removed rows")
	}
	if code, _, body := rowAction(f, "payments", "restore", p.ID); code != http.StatusOK {
		t.Fatalf("restore: %d %s", code, body)
	}
	if removedAt(t, f, p.ID) != nil || !listedPaymentIDs(t, f, "")[p.ID] || listedPaymentIDs(t, f, "?removed=true")[p.ID] {
		t.Error("restore must bring the payment back to the list")
	}
	if got := auditActions(t, f, "payment", p.ID); len(got) != 2 || got[0] != "restore" || got[1] != "remove" {
		t.Errorf("one audit row per write, newest first: %v", got)
	}
	// Removing twice is refused, not a second event.
	rowAction(f, "payments", "remove", p.ID)
	if code, e, _ := rowAction(f, "payments", "remove", p.ID); code != http.StatusConflict || e.Error.Code != "already_removed" {
		t.Errorf("remove a removed payment: %d %s", code, e.Error.Code)
	}
}

func TestRemovePayment_ComesOffItsDraftButKeepsACancelledInvoice(t *testing.T) {
	f := newRemoveFixture(t)
	draftID := f.draft(t, 0, 1)
	// A cancelled invoice over payment 2.
	cancelled := f.draft(t, 2)
	if code, _, raw := f.issue(cancelled); code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, raw)
	}
	if w := invoicingDo(f.r, "POST", fmt.Sprintf("/invoicing/invoices/%d/cancel", cancelled), `{"reason":"wrong amount"}`); w.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}

	if code, _, body := rowAction(f, "payments", "remove", f.payments[0].ID, f.payments[2].ID); code != http.StatusOK {
		t.Fatalf("remove: %d %s", code, body)
	}
	links, _ := data_models.InvoiceIDsForPayment(f.db, f.payments[0].ID)
	if len(links) != 0 {
		t.Errorf("a removed payment comes off its draft: still on %v", links)
	}
	still, _ := data_models.InvoiceIDsForPayment(f.db, f.payments[1].ID)
	if len(still) != 1 || still[0] != draftID {
		t.Errorf("the draft keeps its other payment: %v", still)
	}
	kept, _ := data_models.InvoiceIDsForPayment(f.db, f.payments[2].ID)
	if len(kept) != 1 || kept[0] != cancelled {
		t.Errorf("a cancelled invoice keeps its record of the payment: %v", kept)
	}
	ev, _ := data_models.ListInvoicingAuditEvents(f.db, invoiceFixtureUser, "payment", f.payments[0].ID)
	if len(ev) == 0 || ev[0].Action != "remove" || !strings.Contains(ev[0].Details, fmt.Sprintf(`"unlinked_draft_ids":[%d]`, draftID)) {
		t.Errorf("the remove records the draft it came off: %+v", ev)
	}
	// Restore re-links nothing.
	rowAction(f, "payments", "restore", f.payments[0].ID)
	if links, _ := data_models.InvoiceIDsForPayment(f.db, f.payments[0].ID); len(links) != 0 {
		t.Errorf("restore must not re-link a draft: %v", links)
	}
}

func TestRemovePayment_OnIssuedInvoiceRefused(t *testing.T) {
	f := newRemoveFixture(t)
	id := f.draft(t, 0)
	code, detail, raw := f.issue(id)
	if code != http.StatusOK || detail.Invoice.InvoiceNumber == nil {
		t.Fatalf("issue: %d %s", code, raw)
	}
	code, e, _ := rowAction(f, "payments", "remove", f.payments[0].ID)
	if code != http.StatusConflict || e.Error.Code != "payment_on_invoice" || !strings.Contains(e.Error.Message, *detail.Invoice.InvoiceNumber) {
		t.Errorf("remove on an issued invoice: %d %+v", code, e)
	}
	if removedAt(t, f, f.payments[0].ID) != nil {
		t.Error("a refused remove changed the payment")
	}
}

func TestRowAction_BulkIsAllOrNothing(t *testing.T) {
	f := newRemoveFixture(t)
	issued := f.draft(t, 0)
	if code, _, raw := f.issue(issued); code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, raw)
	}
	before := countAuditEvents(t, f.db)
	for _, tc := range []struct {
		name   string
		ids    []uint
		status int
		code   string
	}{
		{"unknown id", []uint{f.payments[3].ID, f.payments[2].ID, 999999}, http.StatusNotFound, "not_found"},
		{"one on an issued invoice", []uint{f.payments[3].ID, f.payments[0].ID}, http.StatusConflict, "payment_on_invoice"},
	} {
		code, e, body := rowAction(f, "payments", "remove", tc.ids...)
		if code != tc.status || e.Error.Code != tc.code {
			t.Errorf("%s: %d %s", tc.name, code, body)
		}
	}
	if removedAt(t, f, f.payments[3].ID) != nil || removedAt(t, f, f.payments[2].ID) != nil {
		t.Error("a refused batch removed a row")
	}
	// Restore over one removed and one active row refuses whole too.
	rowAction(f, "payments", "remove", f.payments[3].ID)
	mid := countAuditEvents(t, f.db)
	if code, e, _ := rowAction(f, "payments", "restore", f.payments[3].ID, f.payments[2].ID); code != http.StatusConflict || e.Error.Code != "not_removed" {
		t.Errorf("restore over an active row: %d %+v", code, e)
	}
	if removedAt(t, f, f.payments[3].ID) == nil {
		t.Error("a refused restore restored a row")
	}
	if got := countAuditEvents(t, f.db); got != mid || mid != before+1 {
		t.Errorf("refusals wrote audit rows: before %d, after the one remove %d, end %d", before, mid, got)
	}
	// Bad bodies.
	for _, body := range []string{`{"ids":[]}`, fmt.Sprintf(`{"ids":[%d,%d]}`, f.payments[2].ID, f.payments[2].ID), `{"ids":[0]}`, `[]`} {
		if w := invoicingDo(f.r, "POST", "/invoicing/payments/remove", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
}

func TestPurge_OnlyRemovedRowsAndLeavesATombstone(t *testing.T) {
	f := newRemoveFixture(t)
	p := f.payments[3]
	if code, e, _ := rowAction(f, "payments", "purge", p.ID); code != http.StatusConflict || e.Error.Code != "not_removed" {
		t.Errorf("purge an active payment: %d %+v", code, e)
	}
	rowAction(f, "payments", "remove", p.ID)
	if code, _, body := rowAction(f, "payments", "purge", p.ID); code != http.StatusOK {
		t.Fatalf("purge: %d %s", code, body)
	}
	if got, _ := data_models.GetInvoicingPayment(f.db, invoiceFixtureUser, p.ID); got != nil {
		t.Error("purge must erase the payment")
	}
	if seen, err := data_models.InvoicingPaymentSeen(f.db, invoiceFixtureUser, p.Source, p.TransactionID); err != nil || !seen {
		t.Errorf("a purged payment leaves a tombstone a re-import reads: %v %v", seen, err)
	}
	if got := auditActions(t, f, "payment", p.ID); len(got) != 2 || got[0] != "delete" {
		t.Errorf("purge audits as delete: %v", got)
	}

	// Expenses: a hand-entered one and an imported one, in one batch.
	_, manual, _ := createExpense(t, f.r, `{"paid_on":"2026-07-01","amount_cents":1500,"payee":"Thomann"}`)
	imported := data_models.InvoicingExpense{
		OwnerUserID: invoiceFixtureUser, Source: "n26", TransactionID: "N26-9",
		PaidOn: time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC), Currency: "EUR", AmountCents: 999,
		Category: "other", Entry: data_models.ExpenseEntryImport, Reference: "REWE",
	}
	if err := f.db.Create(&imported).Error; err != nil {
		t.Fatalf("seed imported expense: %v", err)
	}
	if code, e, _ := rowAction(f, "expenses", "purge", manual.ID); code != http.StatusConflict || e.Error.Code != "not_removed" {
		t.Errorf("purge an active expense: %d %+v", code, e)
	}
	if code, _, body := rowAction(f, "expenses", "remove", manual.ID, imported.ID); code != http.StatusOK {
		t.Fatalf("remove expenses: %d %s", code, body)
	}
	if ids := listedExpenseIDs(t, f, ""); ids[manual.ID] || ids[imported.ID] {
		t.Error("removed expenses must leave the list")
	}
	if ids := listedExpenseIDs(t, f, "?removed=true"); !ids[manual.ID] || !ids[imported.ID] {
		t.Error("removed expenses appear in the removed view")
	}
	if code, _, body := rowAction(f, "expenses", "purge", manual.ID, imported.ID); code != http.StatusOK {
		t.Fatalf("purge expenses: %d %s", code, body)
	}
	for _, e := range []data_models.InvoicingExpense{manual, imported} {
		if got, _ := data_models.GetInvoicingExpense(f.db, invoiceFixtureUser, e.ID); got != nil {
			t.Errorf("expense %d survived purge", e.ID)
		}
		var n int
		f.db.Model(&data_models.InvoicingExpenseTombstone{}).Where("owner_user_id = ? AND source = ? AND transaction_id = ?", invoiceFixtureUser, e.Source, e.TransactionID).Count(&n)
		if n != 1 {
			t.Errorf("expense %d tombstones: %d", e.ID, n)
		}
		if got := auditActions(t, f, "expense", e.ID); len(got) < 2 || got[0] != "delete" || got[1] != "remove" {
			t.Errorf("expense %d audit: %v", e.ID, got)
		}
	}
}

func TestRemovedPayment_CannotBeInvoiced(t *testing.T) {
	f := newRemoveFixture(t)
	rowAction(f, "payments", "remove", f.payments[1].ID)
	w := invoicingDo(f.r, "POST", "/invoicing/invoices", fmt.Sprintf(`{"payment_ids":[%d]}`, f.payments[1].ID))
	if w.Code != http.StatusUnprocessableEntity || errOf(t, w.Body.Bytes()).Error.Code != "payment_removed" {
		t.Errorf("draft over a removed payment: %d %s", w.Code, w.Body.String())
	}
	id := f.draft(t, 0)
	w = invoicingDo(f.r, "PUT", fmt.Sprintf("/invoicing/invoices/%d", id), fmt.Sprintf(`{"payment_ids":[%d,%d]}`, f.payments[0].ID, f.payments[1].ID))
	if w.Code != http.StatusUnprocessableEntity || errOf(t, w.Body.Bytes()).Error.Code != "payment_removed" {
		t.Errorf("adding a removed payment to a draft: %d %s", w.Code, w.Body.String())
	}
}

func TestRemovedPayment_SkippedByPayerMatching(t *testing.T) {
	f := newRemoveFixture(t)
	loose := data_models.InvoicingPayment{
		OwnerUserID: invoiceFixtureUser, Source: "paypal", TransactionID: "TX-LOOSE", Kind: "incoming",
		PaidOn: time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC), Currency: "EUR", GrossCents: 5000, Category: "paid_work",
		SenderName: "Atelier Süd",
	}
	if err := f.db.Create(&loose).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	has := func() bool {
		ps, err := unlinkedIncomingPayments(f.db, invoiceFixtureUser)
		if err != nil {
			t.Fatalf("unlinked: %v", err)
		}
		for _, p := range ps {
			if p.ID == loose.ID {
				return true
			}
		}
		return false
	}
	if !has() {
		t.Fatal("control: the unlinked payment must be matched before it is removed")
	}
	rowAction(f, "payments", "remove", loose.ID)
	if has() {
		t.Error("a removed payment must not be offered to payer matching")
	}
}

func TestBookReport_LeavesOutRemovedRows(t *testing.T) {
	f := newRemoveFixture(t)
	report := func() string {
		w := invoicingDo(f.r, "GET", "/invoicing/export/book-report.csv", "")
		if w.Code != http.StatusOK {
			t.Fatalf("book report: %d %s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	_, e, _ := createExpense(t, f.r, `{"paid_on":"2026-07-01","amount_cents":1500}`)
	full := report()
	rowAction(f, "payments", "remove", f.payments[0].ID)
	rowAction(f, "expenses", "remove", e.ID)
	without := report()
	if without == full {
		t.Error("removing a payment and an expense must change the Book")
	}
	rowAction(f, "payments", "restore", f.payments[0].ID)
	rowAction(f, "expenses", "restore", e.ID)
	if again := report(); again != full {
		t.Errorf("restoring both must give the Book back exactly:\n%s\nvs\n%s", again, full)
	}
}

func TestRowAction_AnotherOwnersRowsAreNotFound(t *testing.T) {
	f := newRemoveFixture(t)
	_, e, _ := createExpense(t, f.r, `{"paid_on":"2026-07-01","amount_cents":1500}`)
	other := setupInvoiceRouter(f.db, invoiceFixtureUser+1)
	other.POST("/invoicing/payments/:id/remove", PaymentsRowActionHandler("remove"))
	other.POST("/invoicing/expenses/:id/remove", ExpensesRowActionHandler("remove"))
	for _, path := range []string{
		fmt.Sprintf("/invoicing/payments/%d/remove", f.payments[3].ID),
		fmt.Sprintf("/invoicing/expenses/%d/remove", e.ID),
	} {
		if w := invoicingDo(other, "POST", path, ""); w.Code != http.StatusNotFound {
			t.Errorf("%s as another owner: %d %s", path, w.Code, w.Body.String())
		}
	}
	if removedAt(t, f, f.payments[3].ID) != nil {
		t.Error("another owner removed the payment")
	}
	if got, _ := data_models.GetInvoicingExpense(f.db, invoiceFixtureUser, e.ID); got == nil || got.RemovedAt != nil {
		t.Error("another owner removed the expense")
	}
}
