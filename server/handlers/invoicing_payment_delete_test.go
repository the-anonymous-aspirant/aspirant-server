package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"aspirant-online/server/data_models"
)

// system_3 #10009 (operator c61587, scope c61853/c61868 on #9973): any payment
// not on an issued invoice can be deleted, imported or entered by hand. A
// deleted imported payment is tombstoned so a re-import of the same file does
// not bring it back, and the delete's audit event keeps the whole row.

func deletePayment(f *invoiceFixture, id uint) (int, string) {
	w := invoicingDo(f.r, "DELETE", fmt.Sprintf("/invoicing/payments/%d", id), "")
	return w.Code, w.Body.String()
}

func tombstoned(t *testing.T, f *invoiceFixture, source, txn string) bool {
	t.Helper()
	var n int
	f.db.Model(&data_models.InvoicingPaymentTombstone{}).Where("source = ? AND transaction_id = ?", source, txn).Count(&n)
	return n == 1
}

func TestDeletePayment_ImportedOnNoInvoiceIsDeletedTombstonedAndAudited(t *testing.T) {
	f := newInvoiceFixture(t)
	p := f.payments[0] // paypal TX1, on no invoice
	if code, body := deletePayment(f, p.ID); code != http.StatusNoContent {
		t.Fatalf("delete an imported payment: %d %s", code, body)
	}
	if got, _ := data_models.GetInvoicingPayment(f.db, p.ID); got != nil {
		t.Fatal("payment still stored after delete")
	}
	if !tombstoned(t, f, "paypal", "TX1") {
		t.Fatal("no tombstone for paypal TX1")
	}
	var ts data_models.InvoicingPaymentTombstone
	f.db.Where("transaction_id = ?", "TX1").First(&ts)
	if ts.DeletedBy != 9 || ts.DeletedOn.IsZero() {
		t.Errorf("tombstone: %+v", ts)
	}
	events, _ := data_models.ListInvoicingAuditEvents(f.db, "payment", p.ID)
	if len(events) == 0 || events[0].Action != "delete" {
		t.Fatalf("delete audit: %+v", events)
	}
	var details struct {
		Before   data_models.InvoicingPayment `json:"before"`
		Unlinked []uint                       `json:"unlinked_invoice_ids"`
	}
	if err := json.Unmarshal([]byte(events[0].Details), &details); err != nil {
		t.Fatalf("audit details: %v (%s)", err, events[0].Details)
	}
	b := details.Before
	if b.ID != p.ID || b.Source != "paypal" || b.TransactionID != "TX1" || b.GrossCents != p.GrossCents || b.Note != p.Note || b.Category != p.Category {
		t.Errorf("audit before is not the whole row: %+v", b)
	}
	if len(details.Unlinked) != 0 {
		t.Errorf("unlinked: %v", details.Unlinked)
	}
}

func TestDeletePayment_OnAnIssuedInvoiceIsRefused(t *testing.T) {
	f := newInvoiceFixture(t)
	id := f.draft(t, 0)
	if code, _, body := f.issue(id); code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, body)
	}
	code, body := deletePayment(f, f.payments[0].ID)
	if code != http.StatusConflict || !strings.Contains(body, "payment_on_invoice") || !strings.Contains(body, "cancel that invoice first") || !strings.Contains(body, "2026-") {
		t.Fatalf("delete a payment on an issued invoice: %d %s", code, body)
	}
	if got, _ := data_models.GetInvoicingPayment(f.db, f.payments[0].ID); got == nil {
		t.Fatal("refused delete removed the payment")
	}
	if tombstoned(t, f, "paypal", "TX1") {
		t.Fatal("refused delete wrote a tombstone")
	}
	// The list says which payments refuse a delete.
	for _, p := range listPayments(t, f.r, "") {
		want := p.ID == f.payments[0].ID
		if p.OnIssuedInvoice != want || (want && !p.OnInvoice) {
			t.Errorf("payment %s: on_invoice %v, on_issued_invoice %v", p.TransactionID, p.OnInvoice, p.OnIssuedInvoice)
		}
	}
}

func TestDeletePayment_OnADraftUnlinksItAndTheDraftSaysItsTotalIsUncovered(t *testing.T) {
	f := newInvoiceFixture(t)
	id := f.draft(t, 0, 1) // 100.00 + 200.00
	before := decodeDetail(t, invoicingDo(f.r, "GET", fmt.Sprintf("/invoicing/invoices/%d", id), "").Body.Bytes())
	if len(before.Payments) != 2 || len(before.Warnings) != 0 {
		t.Fatalf("draft before: %d payments, warnings %v", len(before.Payments), before.Warnings)
	}
	for _, p := range listPayments(t, f.r, "") {
		if p.ID == f.payments[0].ID && (!p.OnInvoice || p.OnIssuedInvoice) {
			t.Errorf("a draft's payment: on_invoice %v, on_issued_invoice %v", p.OnInvoice, p.OnIssuedInvoice)
		}
	}

	if code, body := deletePayment(f, f.payments[0].ID); code != http.StatusNoContent {
		t.Fatalf("delete a payment on a draft: %d %s", code, body)
	}
	after := decodeDetail(t, invoicingDo(f.r, "GET", fmt.Sprintf("/invoicing/invoices/%d", id), "").Body.Bytes())
	if len(after.Payments) != 1 || after.Payments[0].ID != f.payments[1].ID {
		t.Fatalf("draft payments after: %+v", after.Payments)
	}
	// The line items are hers and stay; the total is unchanged and now says
	// the payments no longer cover it.
	if len(after.LineItems) != len(before.LineItems) || after.Invoice.TotalCents != before.Invoice.TotalCents {
		t.Errorf("line items %d->%d, total %d->%d", len(before.LineItems), len(after.LineItems), before.Invoice.TotalCents, after.Invoice.TotalCents)
	}
	if len(after.Warnings) != 1 || !strings.HasPrefix(after.Warnings[0], "total_mismatch") {
		t.Errorf("warnings after one of two payments is deleted: %v", after.Warnings)
	}

	// The last payment too: the draft keeps its money and still says so.
	if code, body := deletePayment(f, f.payments[1].ID); code != http.StatusNoContent {
		t.Fatalf("delete the draft's last payment: %d %s", code, body)
	}
	last := decodeDetail(t, invoicingDo(f.r, "GET", fmt.Sprintf("/invoicing/invoices/%d", id), "").Body.Bytes())
	if len(last.Payments) != 0 || last.Invoice.TotalCents != before.Invoice.TotalCents {
		t.Fatalf("draft after its last payment: %d payments, total %d", len(last.Payments), last.Invoice.TotalCents)
	}
	if len(last.Warnings) != 1 || !strings.HasPrefix(last.Warnings[0], "total_mismatch") {
		t.Errorf("a draft with no payments left must warn: %v", last.Warnings)
	}
	events, _ := data_models.ListInvoicingAuditEvents(f.db, "payment", f.payments[0].ID)
	if len(events) == 0 || !strings.Contains(events[0].Details, fmt.Sprintf(`"unlinked_invoice_ids":[%d]`, id)) {
		t.Errorf("audit does not name the draft it unlinked: %+v", events)
	}
}

func TestDeletePayment_OnACancelledInvoiceLeavesItsPDFUntouched(t *testing.T) {
	f := newInvoiceFixture(t)
	id := f.draft(t, 0, 1)
	if code, _, body := f.issue(id); code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, body)
	}
	if w := invoicingDo(f.r, "POST", fmt.Sprintf("/invoicing/invoices/%d/cancel", id), `{"reason":"wrong amount"}`); w.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	pdfBefore, _ := data_models.GetInvoicePDF(f.db, id)
	if pdfBefore == nil {
		t.Fatal("cancelled invoice has no stored PDF")
	}
	invBefore, _ := data_models.GetInvoicingInvoice(f.db, id)

	if code, body := deletePayment(f, f.payments[0].ID); code != http.StatusNoContent {
		t.Fatalf("delete a payment on a cancelled invoice: %d %s", code, body)
	}
	pdfAfter, _ := data_models.GetInvoicePDF(f.db, id)
	if pdfAfter == nil || !bytes.Equal(pdfAfter.Content, pdfBefore.Content) {
		t.Fatal("the cancelled invoice's stored PDF changed")
	}
	served := invoicingDo(f.r, "GET", fmt.Sprintf("/invoicing/invoices/%d/pdf", id), "")
	if served.Code != http.StatusOK || !bytes.Equal(served.Body.Bytes(), pdfBefore.Content) {
		t.Fatalf("served PDF after delete: %d, equal %v", served.Code, bytes.Equal(served.Body.Bytes(), pdfBefore.Content))
	}
	invAfter, _ := data_models.GetInvoicingInvoice(f.db, id)
	if invAfter.Status != data_models.InvoiceStatusCancelled || *invAfter.InvoiceNumber != *invBefore.InvoiceNumber || invAfter.TotalCents != invBefore.TotalCents {
		t.Errorf("cancelled invoice changed: %+v", invAfter)
	}
	ids, _ := data_models.InvoiceIDsForPayment(f.db, f.payments[0].ID)
	if len(ids) != 0 {
		t.Errorf("link left behind: %v", ids)
	}
	// The other payment stays linked to the cancelled invoice.
	if ids, _ := data_models.InvoiceIDsForPayment(f.db, f.payments[1].ID); len(ids) != 1 || ids[0] != id {
		t.Errorf("the other payment's link: %v", ids)
	}
}

func TestDeletePayment_APayPalReImportSkipsTheDeletedRow(t *testing.T) {
	db := setupPaymentsDB(t)
	r := setupPaymentsRouter(db, 5)
	if s := decodeSummary(t, uploadCSV(t, r, paypalFixtureEN, "")); s.Imported != 3 {
		t.Fatalf("first import: %+v", s)
	}
	stored := listPayments(t, r, "")
	gone := stored[0]
	if w := invoicingDo(r, "DELETE", fmt.Sprintf("/invoicing/payments/%d", gone.ID), ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	s := decodeSummary(t, uploadCSV(t, r, paypalFixtureEN, ""))
	if s.Imported != 0 || s.Duplicates != 3 {
		t.Fatalf("re-import after a delete: %+v", s)
	}
	var rows int
	db.Model(&data_models.InvoicingPayment{}).Where("transaction_id = ?", gone.TransactionID).Count(&rows)
	if rows != 0 {
		t.Fatalf("re-import brought back %s", gone.TransactionID)
	}
}

func TestDeletePayment_AnN26ReConfirmSkipsTheDeletedRow(t *testing.T) {
	db := setupPaymentsDB(t)
	r := setupN26Router(db, 7)
	r.DELETE("/invoicing/payments/:id", DeletePaymentHandler)
	row := n26Candidate{DocumentKind: n26KindTransferConfirmation, TransactionID: "a1b2c3d4-0e15-4a6f-9bd0-123456789abc",
		PaidOn: "2026-07-14", Currency: "EUR", GrossCents: 123456, NetCents: 123456, SenderName: "Mara Esempio", Note: "x"}
	var cresp struct {
		Summary n26ConfirmSummary `json:"summary"`
	}
	w := confirmN26(t, r, []n26Candidate{row})
	json.Unmarshal(w.Body.Bytes(), &cresp)
	if w.Code != http.StatusOK || cresp.Summary.Confirmed != 1 {
		t.Fatalf("first confirm: %d %s", w.Code, w.Body.String())
	}
	p := listPayments(t, r, "")[0]
	if w := invoicingDo(r, "DELETE", fmt.Sprintf("/invoicing/payments/%d", p.ID), ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	w = confirmN26(t, r, []n26Candidate{row})
	json.Unmarshal(w.Body.Bytes(), &cresp)
	if cresp.Summary.Confirmed != 0 || cresp.Summary.Duplicates != 1 {
		t.Fatalf("re-confirm after a delete: %+v", cresp.Summary)
	}
	if len(listPayments(t, r, "")) != 0 {
		t.Fatal("re-confirm brought the deleted payment back")
	}
}

func TestDeletePayment_AnN26ReParseFlagsTheDeletedRowAsAlreadyImported(t *testing.T) {
	requirePdftotext(t)
	db := setupPaymentsDB(t)
	r := setupN26Router(db, 7)
	r.DELETE("/invoicing/payments/:id", DeletePaymentHandler)
	pdf := buildN26LookalikePDF(t)
	resp := decodeN26Parse(t, uploadN26(t, r, map[string][]byte{"conferma.pdf": pdf}))
	confirmN26(t, r, resp.Files[0].Candidates)
	p := listPayments(t, r, "")[0]
	if w := invoicingDo(r, "DELETE", fmt.Sprintf("/invoicing/payments/%d", p.ID), ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	resp = decodeN26Parse(t, uploadN26(t, r, map[string][]byte{"conferma.pdf": pdf}))
	if !resp.Files[0].Candidates[0].AlreadyImported {
		t.Fatal("a deleted N26 row is not flagged already_imported on re-parse")
	}
}

func TestDeletePayment_ADeletedPayPalPaymentCanBeEnteredByHand(t *testing.T) {
	f := newInvoiceFixture(t)
	if code, body := deletePayment(f, f.payments[0].ID); code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", code, body)
	}
	// The tombstone keeps it out of re-imports, not out of the owner's hands.
	code, p, body := createManual(t, f, `{"paid_on":"2026-07-10","gross_cents":10000,"method":"paypal","reference":"TX1"}`)
	if code != http.StatusCreated || p.Reference != "TX1" {
		t.Fatalf("enter a deleted PayPal payment by hand: %d %s", code, body)
	}
}

func TestDeletePayment_ManualDeleteIsUnchanged(t *testing.T) {
	f := newInvoiceFixture(t)
	_, loose, _ := createManual(t, f, `{"paid_on":"2026-07-14","gross_cents":1000,"method":"cash"}`)
	if code, body := deletePayment(f, loose.ID); code != http.StatusNoContent {
		t.Fatalf("delete a loose manual payment: %d %s", code, body)
	}
	if got, _ := data_models.GetInvoicingPayment(f.db, loose.ID); got != nil {
		t.Fatal("manual payment still stored")
	}
	if code, _ := deletePayment(f, loose.ID); code != http.StatusNotFound {
		t.Errorf("second delete: %d", code)
	}
	if code, _ := deletePayment(f, 99999); code != http.StatusNotFound {
		t.Errorf("delete unknown: %d", code)
	}
}
