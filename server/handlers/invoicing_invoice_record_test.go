package handlers

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"aspirant-online/server/data_models"
)

// A numbered invoice's record is fixed at issue, cancelled or not (system_3
// #12266, the #11498 review's C3-11). After a Storno its payments are free:
// they can be edited, removed and deleted for good. The invoice's CSV row and
// its detail read the invoice's own copy of them, never the live rows.

// recordedPayment is what the readers show of a payment on an invoice.
type recordedPayment struct {
	ID        uint
	PaidOn    string
	Reference string
	Cents     int64
	Currency  string
}

func invoiceRecord(t *testing.T, f *invoiceFixture, id uint) (string, []recordedPayment, []string) {
	t.Helper()
	w := invoicingDo(f.r, "GET", fmt.Sprintf("/invoicing/invoices/%d", id), "")
	if w.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", w.Code, w.Body.String())
	}
	d := decodeDetail(t, w.Body.Bytes())
	var ps []recordedPayment
	for _, p := range d.Payments {
		ps = append(ps, recordedPayment{p.ID, p.PaidOn.Format("2006-01-02"), p.ShownReference(), p.GrossCents, p.Currency})
	}
	csv := invoicingDo(f.r, "GET", "/invoicing/export/invoices.csv", "").Body.String()
	row := ""
	for _, line := range strings.Split(csv, "\n") {
		if d.Invoice.InvoiceNumber != nil && strings.HasPrefix(line, *d.Invoice.InvoiceNumber+";") {
			row = line
		}
	}
	if row == "" {
		t.Fatalf("no CSV row for invoice %d in:\n%s", id, csv)
	}
	return strings.TrimRight(row, "\r"), ps, d.Warnings
}

// cancelledOverTwoHandPayments issues an invoice over two hand-entered
// payments, as the review reproduced it, and cancels it.
func cancelledOverTwoHandPayments(t *testing.T, beforeCopy bool) (*invoiceFixture, uint, data_models.InvoicingPayment, data_models.InvoicingPayment) {
	t.Helper()
	f := newInvoiceFixture(t)
	mk := func(day string, cents int, ref string) data_models.InvoicingPayment {
		code, p, body := createManual(t, f, fmt.Sprintf(`{"paid_on":"%s","gross_cents":%d,"method":"bank_transfer","reference":"%s","sender_name":"Mara Esempio","note":"Wandbild","client_id":%d}`, day, cents, ref, f.clientID))
		if code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", ref, code, body)
		}
		return p
	}
	purged, edited := mk("2026-08-01", 4321, "C3-PURGE"), mk("2026-08-02", 5000, "C3-EDIT")
	w := invoicingDo(f.r, "POST", "/invoicing/invoices", fmt.Sprintf(`{"payment_ids":[%d,%d]}`, purged.ID, edited.ID))
	if w.Code != http.StatusCreated {
		t.Fatalf("draft: %d %s", w.Code, w.Body.String())
	}
	id := decodeDetail(t, w.Body.Bytes()).Invoice.ID
	if code, _, body := f.issue(id); code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, body)
	}
	if beforeCopy {
		// An invoice issued before the copy existed has none.
		f.db.Where("invoice_id = ?", id).Delete(&data_models.InvoicingInvoicePaymentCopy{})
	}
	if w := invoicingDo(f.r, "POST", fmt.Sprintf("/invoicing/invoices/%d/cancel", id), `{"reason":"wrong amount"}`); w.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	return f, id, purged, edited
}

func copies(f *invoiceFixture, id uint) int {
	var n int
	f.db.Model(&data_models.InvoicingInvoicePaymentCopy{}).Where("invoice_id = ?", id).Count(&n)
	return n
}

// editAndDeleteForGood does what the review did after the Storno: one payment
// takes a new date, amount, reference and category, and the other is removed
// and deleted for good.
func editAndDeleteForGood(t *testing.T, f *invoiceFixture, purged, edited data_models.InvoicingPayment) {
	t.Helper()
	if code, _, body := patchPayment(f, edited.ID, `{"paid_on":"2026-09-03","gross_cents":4000,"reference":"C3-CHANGED","category":"gift"}`); code != http.StatusOK {
		t.Fatalf("edit the freed payment: %d %s", code, body)
	}
	if code, body := deletePayment(f, purged.ID); code != http.StatusOK {
		t.Fatalf("delete the freed payment for good: %d %s", code, body)
	}
}

func TestCancelledInvoice_RecordStaysThroughAnEditAndADeleteForGood(t *testing.T) {
	f, id, purged, edited := cancelledOverTwoHandPayments(t, false)
	if n := copies(f, id); n != 2 {
		t.Fatalf("copies after issue and Storno: %d, want 2 (the Storno keeps the one issue wrote)", n)
	}
	row, payments, warnings := invoiceRecord(t, f, id)
	if !strings.Contains(row, "C3-PURGE C3-EDIT") {
		t.Fatalf("CSV row before: %s", row)
	}
	want := []recordedPayment{{purged.ID, "2026-08-01", "C3-PURGE", 4321, "EUR"}, {edited.ID, "2026-08-02", "C3-EDIT", 5000, "EUR"}}
	if fmt.Sprint(payments) != fmt.Sprint(want) || len(warnings) != 0 {
		t.Fatalf("detail before: %v %v", payments, warnings)
	}

	editAndDeleteForGood(t, f, purged, edited)

	rowAfter, paymentsAfter, warningsAfter := invoiceRecord(t, f, id)
	if rowAfter != row {
		t.Errorf("the CSV row changed:\n before %s\n after  %s", row, rowAfter)
	}
	if fmt.Sprint(paymentsAfter) != fmt.Sprint(want) {
		t.Errorf("the detail's payments changed: %v, want %v", paymentsAfter, want)
	}
	if len(warningsAfter) != 0 {
		t.Errorf("the detail now warns: %v", warningsAfter)
	}
	if n := copies(f, id); n != 2 {
		t.Errorf("copies after the delete for good: %d, want 2", n)
	}
}

func TestStorno_CopiesAnInvoiceIssuedBeforeTheCopyExisted(t *testing.T) {
	f, id, purged, edited := cancelledOverTwoHandPayments(t, true)
	if n := copies(f, id); n != 2 {
		t.Fatalf("copies the Storno wrote: %d, want 2", n)
	}
	row, payments, _ := invoiceRecord(t, f, id)
	editAndDeleteForGood(t, f, purged, edited)
	rowAfter, paymentsAfter, _ := invoiceRecord(t, f, id)
	if rowAfter != row || fmt.Sprint(paymentsAfter) != fmt.Sprint(payments) {
		t.Errorf("record changed:\n %s %v\n %s %v", row, payments, rowAfter, paymentsAfter)
	}
}

func TestIssuedInvoiceWithoutCopy_ReadsTheLivePayments(t *testing.T) {
	// An invoice issued before the copy existed, and not cancelled since,
	// keeps reading the live rows; they are locked while it is issued.
	f := newInvoiceFixture(t)
	id := f.draft(t, 0, 1)
	if code, _, body := f.issue(id); code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, body)
	}
	f.db.Where("invoice_id = ?", id).Delete(&data_models.InvoicingInvoicePaymentCopy{})
	_, payments, _ := invoiceRecord(t, f, id)
	if len(payments) != 2 || payments[0].ID != f.payments[0].ID || payments[1].ID != f.payments[1].ID {
		t.Fatalf("live payments: %v", payments)
	}
}

func TestDraft_ReadsItsLivePayments(t *testing.T) {
	f := newInvoiceFixture(t)
	code, p, body := createManual(t, f, fmt.Sprintf(`{"paid_on":"2026-08-01","gross_cents":5000,"method":"cash","reference":"D-1","client_id":%d}`, f.clientID))
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	w := invoicingDo(f.r, "POST", "/invoicing/invoices", fmt.Sprintf(`{"payment_ids":[%d]}`, p.ID))
	id := decodeDetail(t, w.Body.Bytes()).Invoice.ID
	if code, _, body := patchPayment(f, p.ID, `{"reference":"D-2"}`); code != http.StatusOK {
		t.Fatalf("edit: %d %s", code, body)
	}
	if n := copies(f, id); n != 0 {
		t.Errorf("a draft has a copy: %d", n)
	}
	d := decodeDetail(t, invoicingDo(f.r, "GET", fmt.Sprintf("/invoicing/invoices/%d", id), "").Body.Bytes())
	if len(d.Payments) != 1 || d.Payments[0].ShownReference() != "D-2" {
		t.Errorf("draft detail: %+v", d.Payments)
	}
}
