package handlers

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"aspirant-online/server/data_models"
)

// system_3 #12263: a payment is on at most one live draft, whichever button
// made it. "Create invoice", and a draft edit that adds a payment, refuse a
// payment another draft covers with 422 payment_on_draft naming that draft.
// Issue keeps its own check, so a pair made before the rule still issues
// once.

func countDrafts(t *testing.T, f *invoiceFixture) int {
	t.Helper()
	var n int
	if err := f.db.Model(&data_models.InvoicingInvoice{}).Where("status = ?", data_models.InvoiceStatusDraft).Count(&n).Error; err != nil {
		t.Fatalf("count drafts: %v", err)
	}
	return n
}

func expectGuard(t *testing.T, what string, code int, body, wantCode, wantText string) {
	t.Helper()
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, `"`+wantCode+`"`) || !strings.Contains(body, wantText) {
		t.Fatalf("%s: %d %s; want 422 %s with %q", what, code, body, wantCode, wantText)
	}
}

func paymentIDs(f *invoiceFixture, idx ...int) string {
	ids := make([]string, 0, len(idx))
	for _, i := range idx {
		ids = append(ids, fmt.Sprint(f.payments[i].ID))
	}
	return `{"payment_ids":[` + strings.Join(ids, ",") + `]}`
}

func TestCreateDraft_APaymentOnADraftIsRefused(t *testing.T) {
	f := newInvoiceFixture(t)
	a := f.draft(t, 0)
	before := countAuditEvents(t, f.db)

	// The review's case: the same payment twice.
	w := invoicingDo(f.r, "POST", "/invoicing/invoices", paymentIDs(f, 0))
	expectGuard(t, "same payment again", w.Code, w.Body.String(), "payment_on_draft",
		fmt.Sprintf("payment TX1 is already on draft %d", a))
	// With another payment beside it: nothing is made.
	w = invoicingDo(f.r, "POST", "/invoicing/invoices", paymentIDs(f, 1, 0))
	expectGuard(t, "beside a free payment", w.Code, w.Body.String(), "payment_on_draft",
		fmt.Sprintf("payment TX1 is already on draft %d", a))
	if n := countDrafts(t, f); n != 1 {
		t.Fatalf("drafts: %d, want 1", n)
	}
	if n := countAuditEvents(t, f.db); n != before {
		t.Fatalf("a refused create wrote %d audit rows", n-before)
	}

	// The free payment alone is fine.
	f.draft(t, 1)

	// Once the draft is deleted, the payment can go on a new one.
	if w := invoicingDo(f.r, "DELETE", fmt.Sprintf("/invoicing/invoices/%d", a), ""); w.Code != http.StatusNoContent && w.Code != http.StatusOK {
		t.Fatalf("delete draft: %d %s", w.Code, w.Body.String())
	}
	c := f.draft(t, 0)

	// Once it is issued, the issued invoice is named, as before.
	if code, _, body := f.issue(c); code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, body)
	}
	w = invoicingDo(f.r, "POST", "/invoicing/invoices", paymentIDs(f, 0))
	expectGuard(t, "on an issued invoice", w.Code, w.Body.String(), "payment_already_invoiced",
		fmt.Sprintf("payment TX1 is already on issued invoice %d", c))
}

func TestEditDraft_GainingAPaymentOnAnotherDraftIsRefused(t *testing.T) {
	f := newInvoiceFixture(t)
	a, b := f.draft(t, 0), f.draft(t, 1)
	put := func(id uint, body string) (int, string) {
		w := invoicingDo(f.r, "PUT", fmt.Sprintf("/invoicing/invoices/%d", id), body)
		return w.Code, w.Body.String()
	}
	code, body := put(b, paymentIDs(f, 1, 0))
	expectGuard(t, "gain a payment on another draft", code, body, "payment_on_draft",
		fmt.Sprintf("payment TX1 is already on draft %d", a))
	got, err := data_models.ListInvoicePayments(f.db, b)
	if err != nil || len(got) != 1 || got[0].ID != f.payments[1].ID {
		t.Fatalf("draft %d payments after the refusal: %v %v", b, got, err)
	}
	// Its own payment sent again, and a free one gained, are fine.
	if code, body := put(b, paymentIDs(f, 1)); code != http.StatusOK {
		t.Fatalf("re-send own payment: %d %s", code, body)
	}
	if code, body := put(b, paymentIDs(f, 1, 2)); code != http.StatusOK {
		t.Fatalf("gain a free payment: %d %s", code, body)
	}
}

// Pairs made before the rule: neither is stuck. Each can be edited with its
// own payment, and one issues, after which the other is refused at issue as
// before.
func TestIssue_OneDraftOfAnExistingPairIssues(t *testing.T) {
	f := newInvoiceFixture(t)
	a := f.draft(t, 0)
	b := f.copyDraft(t, a)
	for _, id := range []uint{a, b} {
		w := invoicingDo(f.r, "PUT", fmt.Sprintf("/invoicing/invoices/%d", id), paymentIDs(f, 0))
		if w.Code != http.StatusOK {
			t.Fatalf("edit draft %d of the pair: %d %s", id, w.Code, w.Body.String())
		}
	}
	if code, _, body := f.issue(b); code != http.StatusOK {
		t.Fatalf("issue one of the pair: %d %s", code, body)
	}
	code, _, body := f.issue(a)
	expectGuard(t, "issue the other", code, body, "payment_already_invoiced",
		fmt.Sprintf("payment TX1 is already on issued invoice %d", b))
}
