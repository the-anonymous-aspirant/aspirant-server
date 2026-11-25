package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"aspirant-online/server/data_models"
)

// system_3 #12318: "Restore brings a row back as it was" is made true for a
// payment that its removal took off a draft. It goes back on that draft when
// the draft is still a draft, unsaved since, and the payment is still one an
// invoice can cover; else it comes back on no draft and the answer says why.

type restoreAnswer struct {
	Restored []restoredPayment `json:"restored"`
}

func restorePayments(t *testing.T, f *invoiceFixture, ids ...uint) restoreAnswer {
	t.Helper()
	code, _, body := rowAction(f, "payments", "restore", ids...)
	if code != http.StatusOK {
		t.Fatalf("restore: %d %s", code, body)
	}
	var a restoreAnswer
	if err := json.Unmarshal([]byte(body), &a); err != nil {
		t.Fatalf("restore body: %v %s", err, body)
	}
	return a
}

func removePayments(t *testing.T, f *invoiceFixture, ids ...uint) {
	t.Helper()
	if code, _, body := rowAction(f, "payments", "remove", ids...); code != http.StatusOK {
		t.Fatalf("remove: %d %s", code, body)
	}
}

func draftDetail(t *testing.T, f *invoiceFixture, id uint) invoiceDetail {
	t.Helper()
	w := invoicingDo(f.r, "GET", fmt.Sprintf("/invoicing/invoices/%d", id), "")
	if w.Code != http.StatusOK {
		t.Fatalf("get invoice %d: %d %s", id, w.Code, w.Body.String())
	}
	return decodeDetail(t, w.Body.Bytes())
}

func linksOf(t *testing.T, f *invoiceFixture, paymentID uint) []uint {
	t.Helper()
	links, err := data_models.InvoiceIDsForPayment(f.db, paymentID)
	if err != nil {
		t.Fatalf("links: %v", err)
	}
	return links
}

func wantOneDraft(t *testing.T, a restoreAnswer, paymentID, draftID uint, relinked bool, reason string) {
	t.Helper()
	if len(a.Restored) != 1 || a.Restored[0].ID != paymentID || len(a.Restored[0].Drafts) != 1 {
		t.Fatalf("restore answer: %+v", a)
	}
	got := a.Restored[0].Drafts[0]
	if got.InvoiceID != draftID || got.Relinked != relinked || got.Reason != reason {
		t.Errorf("restore answer for draft %d: %+v, want relinked=%v reason=%q", draftID, got, relinked, reason)
	}
}

func TestRestorePayment_GoesBackOnItsDraft(t *testing.T) {
	f := newRemoveFixture(t)
	draftID := f.draft(t, 0, 1)
	p := f.payments[0].ID
	removePayments(t, f, p)
	if d := draftDetail(t, f, draftID); len(d.Payments) != 1 || len(d.RestorablePaymentIDs) != 0 {
		t.Fatalf("after the remove the draft covers one payment and has no restorable list: %+v", d)
	}
	a := restorePayments(t, f, p)
	wantOneDraft(t, a, p, draftID, true, "")
	if links := linksOf(t, f, p); len(links) != 1 || links[0] != draftID {
		t.Errorf("the payment is back on its draft: %v", links)
	}
	if d := draftDetail(t, f, draftID); len(d.Payments) != 2 {
		t.Errorf("the draft covers both payments again: %d", len(d.Payments))
	}
	ev, _ := data_models.ListInvoicingAuditEvents(f.db, invoiceFixtureUser, "payment", p)
	if len(ev) == 0 || ev[0].Action != "restore" || !strings.Contains(ev[0].Details, fmt.Sprintf(`"relinked_draft_ids":[%d]`, draftID)) {
		t.Errorf("the restore records the draft it went back on: %+v", ev)
	}
	// The draft can be issued again, with the payment on it.
	if code, d, raw := f.issue(draftID); code != http.StatusOK || len(d.Payments) != 2 {
		t.Errorf("issue after restore: %d %s", code, raw)
	}
}

func TestRestorePayment_EmptyDraftNamesItsWayBack(t *testing.T) {
	f := newRemoveFixture(t)
	draftID := f.draft(t, 0)
	p := f.payments[0].ID
	removePayments(t, f, p)
	d := draftDetail(t, f, draftID)
	if len(d.Payments) != 0 || len(d.RestorablePaymentIDs) != 1 || d.RestorablePaymentIDs[0] != p {
		t.Fatalf("an empty draft names the payment a restore would put back: %+v", d.RestorablePaymentIDs)
	}
	wantOneDraft(t, restorePayments(t, f, p), p, draftID, true, "")
	if links := linksOf(t, f, p); len(links) != 1 || links[0] != draftID {
		t.Errorf("the empty draft covers its payment again: %v", links)
	}
}

func TestRestorePayment_DraftDeletedInBetween(t *testing.T) {
	f := newRemoveFixture(t)
	draftID := f.draft(t, 0)
	p := f.payments[0].ID
	removePayments(t, f, p)
	if w := invoicingDo(f.r, "DELETE", fmt.Sprintf("/invoicing/invoices/%d", draftID), ""); w.Code != http.StatusOK && w.Code != http.StatusNoContent {
		t.Fatalf("delete draft: %d %s", w.Code, w.Body.String())
	}
	wantOneDraft(t, restorePayments(t, f, p), p, draftID, false, relinkDraftGone)
	if links := linksOf(t, f, p); len(links) != 0 {
		t.Errorf("no draft to go back on: %v", links)
	}
}

func TestRestorePayment_DraftIssuedInBetweenOverAnotherPayment(t *testing.T) {
	f := newRemoveFixture(t)
	draftID := f.draft(t, 0, 1)
	p := f.payments[0].ID
	removePayments(t, f, p)
	if code, _, raw := f.issue(draftID); code != http.StatusOK {
		t.Fatalf("issue over the other payment: %d %s", code, raw)
	}
	wantOneDraft(t, restorePayments(t, f, p), p, draftID, false, relinkDraftNotDraft)
	if links := linksOf(t, f, p); len(links) != 0 {
		t.Errorf("an issued invoice never gains a payment: %v", links)
	}
}

// Review point 1: a draft saved since may no longer bill the payment.
func TestRestorePayment_DraftSavedInBetween(t *testing.T) {
	f := newRemoveFixture(t)
	draftID := f.draft(t, 0, 1)
	p := f.payments[0].ID
	removePayments(t, f, p)
	if w := invoicingDo(f.r, "PUT", fmt.Sprintf("/invoicing/invoices/%d", draftID), `{"vat_rate_bp":0}`); w.Code != http.StatusOK {
		t.Fatalf("save draft: %d %s", w.Code, w.Body.String())
	}
	wantOneDraft(t, restorePayments(t, f, p), p, draftID, false, relinkDraftChanged)
	if links := linksOf(t, f, p); len(links) != 0 {
		t.Errorf("a draft saved since is not changed: %v", links)
	}
}

// Review point 2: a payment that is no longer paid work goes on no draft.
func TestRestorePayment_NoLongerPaidWork(t *testing.T) {
	f := newRemoveFixture(t)
	draftID := f.draft(t, 0)
	p := f.payments[0].ID
	removePayments(t, f, p)
	if err := f.db.Model(&data_models.InvoicingPayment{}).Where("id = ?", p).Update("category", "gift").Error; err != nil {
		t.Fatalf("seed category: %v", err)
	}
	if d := draftDetail(t, f, draftID); len(d.RestorablePaymentIDs) != 0 {
		t.Errorf("no way back is named for a payment the draft cannot list: %v", d.RestorablePaymentIDs)
	}
	wantOneDraft(t, restorePayments(t, f, p), p, draftID, false, relinkNotInvoiceable)
	if links := linksOf(t, f, p); len(links) != 0 {
		t.Errorf("a gift goes on no draft: %v", links)
	}
}

// Review point 3: only the latest removal is read.
func TestRestorePayment_OnlyTheLatestRemovalIsRead(t *testing.T) {
	f := newRemoveFixture(t)
	draftID := f.draft(t, 1)
	p := f.payments[0].ID
	// An older removal that named the draft, then the latest, which names none.
	old := auditDetails(map[string]interface{}{"unlinked_draft_ids": []uint{draftID}})
	if err := data_models.RecordInvoicingAuditEvent(f.db, invoiceFixtureUser, invoicingActionRemove, invoicingEntityPayment, p, old); err != nil {
		t.Fatalf("seed older removal: %v", err)
	}
	removePayments(t, f, p)
	a := restorePayments(t, f, p)
	if len(a.Restored) != 1 || len(a.Restored[0].Drafts) != 0 {
		t.Errorf("the latest removal took it off no draft: %+v", a)
	}
	if links := linksOf(t, f, p); len(links) != 0 {
		t.Errorf("an older removal's draft is never read: %v", links)
	}
}

// Another book's removal can never name, or reach, this book's draft.
func TestRestorePayment_AnotherOwnersRemovalNeverReachesThisDraft(t *testing.T) {
	f := newRemoveFixture(t)
	draftID := f.draft(t, 0)
	removePayments(t, f, f.payments[0].ID)
	removedAt := f.payments[0].PaidOn.AddDate(0, 1, 0)
	other := data_models.InvoicingPayment{OwnerUserID: invoiceFixtureUser + 1, Source: "manual", TransactionID: "M-OTHER",
		Kind: "incoming", PaidOn: f.payments[0].PaidOn, Currency: "EUR", GrossCents: 5000, Category: "paid_work",
		RemovedAt: &removedAt}
	if err := f.db.Create(&other).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	named := auditDetails(map[string]interface{}{"unlinked_draft_ids": []uint{draftID}})
	if err := data_models.RecordInvoicingAuditEvent(f.db, invoiceFixtureUser+1, invoicingActionRemove, invoicingEntityPayment, other.ID, named); err != nil {
		t.Fatalf("seed removal: %v", err)
	}
	if d := draftDetail(t, f, draftID); len(d.RestorablePaymentIDs) != 1 || d.RestorablePaymentIDs[0] != f.payments[0].ID {
		t.Errorf("only this book's payment is named: %v", d.RestorablePaymentIDs)
	}
	tx := f.db.Begin()
	back, err := restorePaymentDrafts(tx, invoiceFixtureUser+1, other)
	tx.Commit()
	if err != nil || len(back.Drafts) != 1 || back.Drafts[0].Relinked || back.Drafts[0].Reason != relinkDraftGone {
		t.Errorf("another owner's restore finds no such draft: %+v %v", back, err)
	}
	if links := linksOf(t, f, other.ID); len(links) != 0 {
		t.Errorf("another book's payment went on this draft: %v", links)
	}
}

// Review point 5: the list carries each invoice's payment count.
func TestListInvoices_CarriesPaymentCount(t *testing.T) {
	f := newRemoveFixture(t)
	two := f.draft(t, 0, 1)
	one := f.draft(t, 2)
	removePayments(t, f, f.payments[2].ID)
	w := invoicingDo(f.r, "GET", "/invoicing/invoices", "")
	var list struct {
		Invoices []invoiceListRow `json:"invoices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	got := map[uint]int{}
	for _, r := range list.Invoices {
		got[r.ID] = r.PaymentCount
	}
	if got[two] != 2 || got[one] != 0 {
		t.Errorf("payment counts: %v, want %d:2 and %d:0", got, two, one)
	}
	if !strings.Contains(w.Body.String(), `"payment_count":0`) {
		t.Errorf("the count is in the answer: %s", w.Body.String())
	}
}
