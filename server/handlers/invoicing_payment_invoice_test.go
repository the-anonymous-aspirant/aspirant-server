package handlers

import (
	"fmt"
	"net/http"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/jinzhu/gorm"
)

// system_3 #10666: each listed payment names the invoice it is on now, so
// Transactions can read "Invoice 2026-014" / "Draft" / nothing and the edit
// dialog can name the draft (§3.143 Decisions 3 and 5).

func paymentInvoiceByID(t *testing.T, f *invoiceFixture) map[uint]*data_models.PaymentInvoiceRef {
	t.Helper()
	out := map[uint]*data_models.PaymentInvoiceRef{}
	for _, p := range listPayments(t, f.r, "") {
		out[p.ID] = p.Invoice
	}
	return out
}

func cancelInvoice(t *testing.T, f *invoiceFixture, id uint) {
	t.Helper()
	if w := invoicingDo(f.r, "POST", fmt.Sprintf("/invoicing/invoices/%d/cancel", id), `{"reason":"Falsche Adresse"}`); w.Code != http.StatusOK {
		t.Fatalf("cancel %d: %d %s", id, w.Code, w.Body.String())
	}
}

func TestPaymentInvoice_NoneDraftAndIssued(t *testing.T) {
	f := newInvoiceFixture(t)
	draftID := f.draft(t, 0)
	issuedID := f.draft(t, 1)
	if code, _, body := f.issue(issuedID); code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, body)
	}

	got := paymentInvoiceByID(t, f)

	if ref := got[f.payments[3].ID]; ref != nil {
		t.Errorf("a payment on no invoice names %+v, want null", *ref)
	}
	d := got[f.payments[0].ID]
	if d == nil || d.ID != draftID || d.Status != "draft" || d.InvoiceNumber != nil {
		t.Errorf("drafted payment: %+v, want draft %d with no number", d, draftID)
	}
	i := got[f.payments[1].ID]
	if i == nil || i.ID != issuedID || i.Status != "issued" || i.InvoiceNumber == nil || *i.InvoiceNumber == "" {
		t.Fatalf("issued payment: %+v, want issued %d with its number", i, issuedID)
	}
	var inv data_models.InvoicingInvoice
	f.db.First(&inv, issuedID)
	if inv.InvoiceNumber == nil || *i.InvoiceNumber != *inv.InvoiceNumber {
		t.Errorf("issued payment names number %q, the invoice has %v", *i.InvoiceNumber, inv.InvoiceNumber)
	}
}

func TestPaymentInvoice_CancelledIsNeverNamed(t *testing.T) {
	f := newInvoiceFixture(t)
	id := f.draft(t, 0)
	if code, _, body := f.issue(id); code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, body)
	}
	cancelInvoice(t, f, id)

	got := paymentInvoiceByID(t, f)
	if ref := got[f.payments[0].ID]; ref != nil {
		t.Errorf("a payment only on a cancelled invoice names %+v, want null", *ref)
	}
	// on_invoice still reports the cancelled link, unchanged for its readers.
	for _, p := range listPayments(t, f.r, "") {
		if p.ID == f.payments[0].ID && !p.OnInvoice {
			t.Errorf("on_invoice changed: a cancelled invoice still lists the payment")
		}
	}

	// Drafted again after the cancellation: the newer draft is named.
	again := f.draft(t, 0)
	ref := paymentInvoiceByID(t, f)[f.payments[0].ID]
	if ref == nil || ref.ID != again || ref.Status != "draft" {
		t.Errorf("cancelled then drafted again: %+v, want draft %d", ref, again)
	}
}

func TestPaymentInvoice_AnotherBooksInvoiceIsNeverNamed(t *testing.T) {
	f := newInvoiceFixture(t)
	number := "OTHER-1"
	other := data_models.InvoicingInvoice{OwnerUserID: invoiceFixtureUser + 1, Status: "issued", InvoiceNumber: &number}
	if err := f.db.Create(&other).Error; err != nil {
		t.Fatalf("seed other book's invoice: %v", err)
	}
	f.db.Create(&data_models.InvoicingInvoicePayment{InvoiceID: other.ID, PaymentID: f.payments[2].ID})

	if ref := paymentInvoiceByID(t, f)[f.payments[2].ID]; ref != nil {
		t.Errorf("another book's invoice is named: %+v", *ref)
	}
}

func TestPaymentInvoice_RemovedViewCarriesIt(t *testing.T) {
	f := newInvoiceFixture(t)
	id := f.draft(t, 0)
	f.db.Model(&data_models.InvoicingPayment{}).Where("id = ?", f.payments[0].ID).Update("removed_at", gorm.Expr("CURRENT_TIMESTAMP"))
	var found bool
	for _, p := range listPayments(t, f.r, "?removed=true") {
		if p.ID != f.payments[0].ID {
			continue
		}
		found = true
		if p.Invoice == nil || p.Invoice.ID != id {
			t.Errorf("removed view: %+v, want draft %d", p.Invoice, id)
		}
	}
	if !found {
		t.Fatalf("the removed payment is not in the removed view")
	}
}

// One query names every payment's invoice, whatever the number of rows: the
// list makes as many queries with three drafted payments as with one.
func TestPaymentInvoice_OneQueryForTheList(t *testing.T) {
	count := func(f *invoiceFixture) int {
		n := 0
		f.db.Callback().Query().After("gorm:query").Register("count_queries_10666", func(*gorm.Scope) { n++ })
		defer f.db.Callback().Query().Remove("count_queries_10666")
		listPayments(t, f.r, "")
		return n
	}
	one := newInvoiceFixture(t)
	one.draft(t, 0)
	three := newInvoiceFixture(t)
	three.draft(t, 0)
	three.draft(t, 1)
	three.draft(t, 2)
	if a, b := count(one), count(three); a == 0 || a != b {
		t.Errorf("list queries: %d with one drafted payment, %d with three; want equal and non-zero", a, b)
	}
}
