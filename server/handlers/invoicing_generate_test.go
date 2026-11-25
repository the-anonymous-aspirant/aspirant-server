package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
)

// system_3 #11504 (epic #11497 items 1 and 2): Generate invoices creates one
// draft per paid-work payment, for a month or a list of payments, skips a
// payment an invoice already covers, never issues, and changes no existing row.

func generate(t *testing.T, f *invoiceFixture, body string) (int, generateResponse, string) {
	t.Helper()
	w := invoicingDo(f.r, "POST", "/invoicing/invoices/generate", body)
	var resp generateResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	return w.Code, resp, w.Body.String()
}

func countInvoices(t *testing.T, f *invoiceFixture) int {
	t.Helper()
	var n int
	if err := f.db.Model(&data_models.InvoicingInvoice{}).Count(&n).Error; err != nil {
		t.Fatalf("count invoices: %v", err)
	}
	return n
}

func paymentIDsBody(ids ...uint) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprint(id))
	}
	return `{"payment_ids":[` + strings.Join(parts, ",") + `]}`
}

// seedBookPayment adds one payment to the fixture's database, in any book.
func seedBookPayment(t *testing.T, f *invoiceFixture, p data_models.InvoicingPayment) data_models.InvoicingPayment {
	t.Helper()
	if p.Currency == "" {
		p.Currency = "EUR"
	}
	if p.Kind == "" {
		p.Kind = "incoming"
	}
	if err := f.db.Create(&p).Error; err != nil {
		t.Fatalf("seed payment: %v", err)
	}
	return p
}

func TestGenerate_MonthMakesOneDraftPerPaidWorkPayment(t *testing.T) {
	f := newInvoiceFixture(t)
	nextBefore := f.profileNext(t)
	auditBefore := countAuditEvents(t, f.db)

	code, resp, body := generate(t, f, `{"month":"2026-07"}`)
	if code != http.StatusCreated {
		t.Fatalf("generate: %d %s", code, body)
	}
	// Three paid-work payments in July; the gift is not a candidate.
	if len(resp.Invoices) != 3 || len(resp.Skipped) != 0 {
		t.Fatalf("got %d invoices and %d skipped, want 3 and 0: %s", len(resp.Invoices), len(resp.Skipped), body)
	}
	for i, d := range resp.Invoices {
		p := f.payments[i] // oldest first, as seeded
		if d.Invoice.Status != data_models.InvoiceStatusDraft || d.Invoice.Number != nil || d.Invoice.InvoiceNumber != nil || d.Invoice.IssueDate != nil {
			t.Errorf("invoice %d is not a bare draft: %+v", i, d.Invoice)
		}
		if len(d.Payments) != 1 || d.Payments[0].ID != p.ID {
			t.Errorf("invoice %d covers %+v, want exactly payment %d", i, d.Payments, p.ID)
		}
		if d.Invoice.ClientID == nil || *d.Invoice.ClientID != f.clientID || d.ClientName != "Galerie Nord" {
			t.Errorf("invoice %d client: %v %q, want the payment's client", i, d.Invoice.ClientID, d.ClientName)
		}
		if d.Invoice.ServiceDateFrom == nil || !d.Invoice.ServiceDateFrom.Equal(p.PaidOn) || d.Invoice.ServiceDateTo == nil || !d.Invoice.ServiceDateTo.Equal(p.PaidOn) {
			t.Errorf("invoice %d service dates %v..%v, want the payment date %v", i, d.Invoice.ServiceDateFrom, d.Invoice.ServiceDateTo, p.PaidOn)
		}
		if len(d.LineItems) != 1 || d.LineItems[0].Description != p.Note || d.LineItems[0].UnitPriceCents != p.GrossCents || d.Invoice.TotalCents != p.GrossCents {
			t.Errorf("invoice %d lines %+v total %d, want one line %q at %d", i, d.LineItems, d.Invoice.TotalCents, p.Note, p.GrossCents)
		}
		if len(d.Warnings) != 0 {
			t.Errorf("invoice %d warnings: %v", i, d.Warnings)
		}
	}
	// Generation never issues: no number was drawn.
	if got := f.profileNext(t); got != nextBefore {
		t.Errorf("next invoice number moved from %d to %d", nextBefore, got)
	}
	// One audit event per draft, marked as generated.
	if got := countAuditEvents(t, f.db) - auditBefore; got != 3 {
		t.Errorf("audit events added: %d, want 3", got)
	}
	var ev data_models.InvoicingAuditEvent
	f.db.Order("id desc").First(&ev)
	if !strings.Contains(ev.Details, `"generated":true`) {
		t.Errorf("audit details %q do not mark the draft as generated", ev.Details)
	}
}

func TestGenerate_SecondCallAddsNothingAndNamesTheDrafts(t *testing.T) {
	f := newInvoiceFixture(t)
	_, first, _ := generate(t, f, `{"month":"2026-07"}`)
	if len(first.Invoices) != 3 {
		t.Fatalf("first call made %d drafts, want 3", len(first.Invoices))
	}

	code, second, body := generate(t, f, `{"month":"2026-07"}`)
	if code != http.StatusOK {
		t.Fatalf("second generate: %d %s", code, body)
	}
	if len(second.Invoices) != 0 || len(second.Skipped) != 3 {
		t.Fatalf("second call: %d invoices and %d skipped, want 0 and 3: %s", len(second.Invoices), len(second.Skipped), body)
	}
	for i, s := range second.Skipped {
		if s.PaymentID != f.payments[i].ID || s.Reason != "already_on_invoice" || s.Invoice.ID != first.Invoices[i].Invoice.ID || s.Invoice.Status != "draft" {
			t.Errorf("skipped %d: %+v, want payment %d on draft %d", i, s, f.payments[i].ID, first.Invoices[i].Invoice.ID)
		}
	}
	if got := countInvoices(t, f); got != 3 {
		t.Errorf("invoices after the second call: %d, want 3", got)
	}
	// The same guard by ids.
	code, byIDs, body := generate(t, f, paymentIDsBody(f.payments[0].ID))
	if code != http.StatusOK || len(byIDs.Invoices) != 0 || len(byIDs.Skipped) != 1 {
		t.Errorf("ids over a drafted payment: %d %s", code, body)
	}
}

func TestGenerate_DraftAndIssuedAreSkippedCancelledIsNot(t *testing.T) {
	f := newInvoiceFixture(t)
	draftID := f.draft(t, 0)
	issuedID := f.draft(t, 1)
	if code, _, body := f.issue(issuedID); code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, body)
	}
	cancelledID := f.draft(t, 2)
	if code, _, body := f.issue(cancelledID); code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, body)
	}
	cancelInvoice(t, f, cancelledID)

	code, resp, body := generate(t, f, `{"month":"2026-07"}`)
	if code != http.StatusCreated {
		t.Fatalf("generate: %d %s", code, body)
	}
	if len(resp.Invoices) != 1 || resp.Invoices[0].Payments[0].ID != f.payments[2].ID {
		t.Fatalf("want one draft, for the payment whose invoice was cancelled: %s", body)
	}
	if len(resp.Skipped) != 2 {
		t.Fatalf("skipped: %+v, want the drafted and the issued payment", resp.Skipped)
	}
	if s := resp.Skipped[0]; s.PaymentID != f.payments[0].ID || s.Invoice.ID != draftID || s.Invoice.Status != "draft" || s.Invoice.InvoiceNumber != nil {
		t.Errorf("drafted payment skipped as %+v", s)
	}
	if s := resp.Skipped[1]; s.PaymentID != f.payments[1].ID || s.Invoice.ID != issuedID || s.Invoice.Status != "issued" || s.Invoice.InvoiceNumber == nil {
		t.Errorf("issued payment skipped as %+v", s)
	}
}

func TestGenerate_MonthLeavesOutOtherMonthsRemovedTransfersAndOtherBooks(t *testing.T) {
	f := newInvoiceFixture(t)
	august := seedBookPayment(t, f, data_models.InvoicingPayment{OwnerUserID: invoiceFixtureUser, Source: "paypal", TransactionID: "AUG1",
		PaidOn: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), GrossCents: 5000, Category: "paid_work"})
	seedBookPayment(t, f, data_models.InvoicingPayment{OwnerUserID: invoiceFixtureUser, Source: "n26", TransactionID: "N26-PP", SenderName: "PayPal Europe S.a.r.l.",
		PaidOn: time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC), GrossCents: 30000, Category: "paid_work"})
	other := seedBookPayment(t, f, data_models.InvoicingPayment{OwnerUserID: invoiceFixtureUser + 1, Source: "paypal", TransactionID: "OTHER1",
		PaidOn: time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC), GrossCents: 7000, Category: "paid_work"})
	removedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	f.db.Model(&data_models.InvoicingPayment{}).Where("id = ?", f.payments[2].ID).Update("removed_at", removedAt)

	code, resp, body := generate(t, f, `{"month":"2026-07"}`)
	if code != http.StatusCreated {
		t.Fatalf("generate: %d %s", code, body)
	}
	var got []uint
	for _, d := range resp.Invoices {
		got = append(got, d.Payments[0].ID)
	}
	want := []uint{f.payments[0].ID, f.payments[1].ID}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("drafted payments %v, want %v (no August, removed, PayPal-transfer or other-book payment)", got, want)
	}

	// A month with nothing to invoice answers 200 with two empty lists.
	code, empty, body := generate(t, f, `{"month":"2025-01"}`)
	if code != http.StatusOK || len(empty.Invoices) != 0 || len(empty.Skipped) != 0 || !strings.Contains(body, `"invoices":[]`) || !strings.Contains(body, `"skipped":[]`) {
		t.Errorf("empty month: %d %s", code, body)
	}

	// By id, another book's payment does not exist, and nothing is created.
	before := countInvoices(t, f)
	if code, _, body := generate(t, f, paymentIDsBody(august.ID, other.ID)); code != http.StatusUnprocessableEntity || !strings.Contains(body, "payment_not_found") {
		t.Errorf("another book's payment by id: %d %s", code, body)
	}
	if got := countInvoices(t, f); got != before {
		t.Errorf("a refused call created %d invoices", got-before)
	}
}

func TestGenerate_IDsModeDraftsEachAndRefusesWhatCannotBeInvoiced(t *testing.T) {
	f := newInvoiceFixture(t)

	// Listed newest first; the drafts come back oldest payment first.
	code, resp, body := generate(t, f, paymentIDsBody(f.payments[1].ID, f.payments[0].ID))
	if code != http.StatusCreated || len(resp.Invoices) != 2 {
		t.Fatalf("generate by ids: %d %s", code, body)
	}
	if resp.Invoices[0].Payments[0].ID != f.payments[0].ID || resp.Invoices[1].Payments[0].ID != f.payments[1].ID {
		t.Errorf("drafts are not in payment-date order: %s", body)
	}

	before := countInvoices(t, f)
	for name, tc := range map[string]struct {
		body string
		code int
		want string
	}{
		"a gift":            {paymentIDsBody(f.payments[2].ID, f.payments[3].ID), http.StatusUnprocessableEntity, "payment_not_paid_work"},
		"an unknown id":     {paymentIDsBody(f.payments[2].ID, 99999), http.StatusUnprocessableEntity, "payment_not_found"},
		"listed twice":      {paymentIDsBody(f.payments[2].ID, f.payments[2].ID), http.StatusBadRequest, codePaymentListedTwice},
		"an empty list":     {`{"payment_ids":[]}`, http.StatusBadRequest, "at least one payment"},
		"both fields":       {`{"month":"2026-07","payment_ids":[1]}`, http.StatusBadRequest, "exactly one of month or payment_ids"},
		"neither field":     {`{}`, http.StatusBadRequest, "exactly one of month or payment_ids"},
		"month 13":          {`{"month":"2026-13"}`, http.StatusBadRequest, "YYYY-MM"},
		"a one-digit month": {`{"month":"2026-7"}`, http.StatusBadRequest, "YYYY-MM"},
		"a full date":       {`{"month":"2026-07-01"}`, http.StatusBadRequest, "YYYY-MM"},
		"an unknown field":  {`{"month":"2026-07","issue":true}`, http.StatusBadRequest, "unknown_field"},
		"a server field":    {`{"month":"2026-07","status":"issued"}`, http.StatusBadRequest, "server_owned_field"},
	} {
		code, _, body := generate(t, f, tc.body)
		if code != tc.code || !strings.Contains(body, tc.want) {
			t.Errorf("%s: %d %s, want %d with %q", name, code, body, tc.code, tc.want)
		}
	}
	if got := countInvoices(t, f); got != before {
		t.Errorf("refused calls created %d invoices", got-before)
	}

	// A removed payment is refused by id, as Create invoice refuses it.
	f.db.Model(&data_models.InvoicingPayment{}).Where("id = ?", f.payments[2].ID).Update("removed_at", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if code, _, body := generate(t, f, paymentIDsBody(f.payments[2].ID)); code != http.StatusUnprocessableEntity || !strings.Contains(body, "payment_removed") {
		t.Errorf("a removed payment by id: %d %s", code, body)
	}
}

func TestGenerate_TooManyIDsIsRefused(t *testing.T) {
	f := newInvoiceFixture(t)
	ids := make([]uint, 0, maxGeneratePayments+1)
	for i := 1; i <= maxGeneratePayments+1; i++ {
		ids = append(ids, uint(100000+i))
	}
	if code, _, body := generate(t, f, paymentIDsBody(ids...)); code != http.StatusBadRequest || !strings.Contains(body, "at most") {
		t.Errorf("over the limit: %d %s", code, body)
	}
}

// The operator's bar on #11497 (c70838 on #10985): the work may not change the
// owner's existing rows. Generation only adds invoices, lines, links and audit
// events; every payment and client row reads the same before and after.
func TestGenerate_ChangesNoExistingPaymentOrClientRow(t *testing.T) {
	f := newInvoiceFixture(t)
	existing := f.draft(t, 0)
	snapshot := func() string {
		var payments []data_models.InvoicingPayment
		var clients []data_models.InvoicingClient
		var invoice data_models.InvoicingInvoice
		f.db.Order("id asc").Find(&payments)
		f.db.Order("id asc").Find(&clients)
		f.db.First(&invoice, existing)
		b, _ := json.Marshal(gin.H{"payments": payments, "clients": clients, "invoice": invoice})
		return string(b)
	}
	before := snapshot()

	if code, resp, body := generate(t, f, `{"month":"2026-07"}`); code != http.StatusCreated || len(resp.Invoices) != 2 {
		t.Fatalf("generate: %d %s", code, body)
	}
	if after := snapshot(); after != before {
		t.Errorf("existing rows changed:\nbefore %s\nafter  %s", before, after)
	}
}

// A generated draft goes through the same accept and reject as any draft:
// issue assigns the number, delete removes it and frees the payment.
func TestGenerate_DraftsIssueAndDeleteLikeAnyDraft(t *testing.T) {
	f := newInvoiceFixture(t)
	_, resp, _ := generate(t, f, `{"month":"2026-07"}`)
	if len(resp.Invoices) != 3 {
		t.Fatalf("want 3 drafts, got %d", len(resp.Invoices))
	}
	next := f.profileNext(t)

	code, issued, body := f.issue(resp.Invoices[0].Invoice.ID)
	if code != http.StatusOK || issued.Invoice.Number == nil || *issued.Invoice.Number != next {
		t.Fatalf("accept: %d %s", code, body)
	}
	if w := invoicingDo(f.r, "DELETE", fmt.Sprintf("/invoicing/invoices/%d", resp.Invoices[1].Invoice.ID), ""); w.Code != http.StatusOK && w.Code != http.StatusNoContent {
		t.Fatalf("reject: %d %s", w.Code, w.Body.String())
	}
	if got := f.profileNext(t); got != next+1 {
		t.Errorf("next number is %d after one accept and one reject, want %d", got, next+1)
	}
	// The rejected payment is free again, and only it is drafted on a re-run.
	code, again, body := generate(t, f, `{"month":"2026-07"}`)
	if code != http.StatusCreated || len(again.Invoices) != 1 || again.Invoices[0].Payments[0].ID != f.payments[1].ID || len(again.Skipped) != 2 {
		t.Errorf("re-run after a reject: %d %s", code, body)
	}
}
