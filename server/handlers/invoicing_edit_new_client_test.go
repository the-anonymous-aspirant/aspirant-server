package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"aspirant-online/server/data_models"
)

// system_3 #10696 (design §3.143 Decision 5, design_agent c66539 on #10694):
// editing a hand-entered payment can add its payer to the client book, as
// creating one can (§3.142 Decisions 2 and 3), with the same rules and the
// same refusals. An imported payment's payer is the bank's record, so it is
// refused there.

func patchPaymentNewClient(t *testing.T, f *invoiceFixture, id uint, body string) (int, data_models.InvoicingPayment, *data_models.InvoicingClient, string) {
	t.Helper()
	w := invoicingDo(f.r, "PATCH", fmt.Sprintf("/invoicing/payments/%d", id), body)
	var resp struct {
		Payment data_models.InvoicingPayment `json:"payment"`
		Client  *data_models.InvoicingClient `json:"client"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	return w.Code, resp.Payment, resp.Client, w.Body.String()
}

func handPaymentWithoutClient(t *testing.T, f *invoiceFixture) data_models.InvoicingPayment {
	t.Helper()
	code, p, body := createManual(t, f, `{"paid_on":"2026-07-14","gross_cents":35000,"method":"bank_transfer","sender_name":"Atelier"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	if p.ClientID != nil {
		t.Fatalf("fixture payment already has a client: %v", *p.ClientID)
	}
	return p
}

func TestEditPayment_NewClientNameAddsThePayerAndLinksThePayment(t *testing.T) {
	f := newInvoiceFixture(t)
	p := handPaymentWithoutClient(t, f)
	clients0 := countRows(t, f, &data_models.InvoicingClient{})
	audits0 := countRows(t, f, &data_models.InvoicingAuditEvent{})

	code, got, client, body := patchPaymentNewClient(t, f, p.ID, `{"sender_name":"Atelier Ost","new_client_name":"Atelier Ost"}`)
	if code != http.StatusOK {
		t.Fatalf("edit: %d %s", code, body)
	}
	if client == nil || client.Name != "Atelier Ost" || client.ID == 0 {
		t.Fatalf("response client: %+v", client)
	}
	if got.ClientID == nil || *got.ClientID != client.ID || got.SenderName != "Atelier Ost" {
		t.Errorf("payment after edit: client %v sender %q", got.ClientID, got.SenderName)
	}
	stored, err := data_models.GetInvoicingClient(f.db, invoiceFixtureUser, client.ID)
	if err != nil || stored == nil {
		t.Fatalf("client not in the owner's book: %v %v", stored, err)
	}
	if n := countRows(t, f, &data_models.InvoicingClient{}); n != clients0+1 {
		t.Errorf("clients: %d, want %d", n, clients0+1)
	}
	// One audit event for the client, one for the payment edit, which records
	// the new link even though the body never sent client_id.
	if n := countRows(t, f, &data_models.InvoicingAuditEvent{}); n != audits0+2 {
		t.Errorf("audit events: %d, want %d", n, audits0+2)
	}
	var ev data_models.InvoicingAuditEvent
	f.db.Where("entity_type = ? AND entity_id = ?", "payment", p.ID).Order("id desc").First(&ev)
	if !strings.Contains(ev.Details, fmt.Sprintf(`"client_id":%d`, client.ID)) || !strings.Contains(ev.Details, `"before"`) {
		t.Errorf("payment edit audit does not record the new client link: %s", ev.Details)
	}
}

func TestEditPayment_NewClientNameRefusalsAddNothing(t *testing.T) {
	f := newInvoiceFixture(t)
	p := handPaymentWithoutClient(t, f)
	clients0 := countRows(t, f, &data_models.InvoicingClient{})
	cases := []struct {
		name, body, wantCode, wantText string
	}{
		{"no letter or digit", `{"new_client_name":"--"}`, "invalid_field", "at least one letter or digit"},
		{"two lines", `{"new_client_name":"Atelier\nOst"}`, "invalid_field", "one line"},
		{"too long", fmt.Sprintf(`{"new_client_name":%q}`, strings.Repeat("a", maxManualSenderRunes+1)), "invalid_field", "one line"},
		{"with client_id", fmt.Sprintf(`{"new_client_name":"Atelier Ost","client_id":%d}`, f.clientID), "invalid_field", "together with client_id"},
	}
	for _, tc := range cases {
		code, _, _, body := patchPaymentNewClient(t, f, p.ID, tc.body)
		if code != http.StatusBadRequest || !strings.Contains(body, tc.wantCode) || !strings.Contains(body, tc.wantText) {
			t.Errorf("%s: %d %s", tc.name, code, body)
		}
	}
	// Decision 4's "the same" client is already in the book: refused with it.
	code, _, _, body := patchPaymentNewClient(t, f, p.ID, `{"new_client_name":"galerie  NORD"}`)
	if code != http.StatusConflict || !strings.Contains(body, "client_exists") || !strings.Contains(body, fmt.Sprintf(`"id":%d`, f.clientID)) {
		t.Errorf("existing client: %d %s", code, body)
	}
	if n := countRows(t, f, &data_models.InvoicingClient{}); n != clients0 {
		t.Errorf("a refused edit added a client: %d, want %d", n, clients0)
	}
	var stored data_models.InvoicingPayment
	f.db.First(&stored, p.ID)
	if stored.ClientID != nil {
		t.Errorf("a refused edit linked a client: %v", *stored.ClientID)
	}
}

func TestEditPayment_NewClientNameRefusedOnAnImportedPayment(t *testing.T) {
	f := newInvoiceFixture(t)
	clients0 := countRows(t, f, &data_models.InvoicingClient{})
	code, _, _, body := patchPaymentNewClient(t, f, f.payments[0].ID, `{"new_client_name":"Atelier Ost"}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "bank_fact_field") || !strings.Contains(body, "new_client_name") {
		t.Errorf("imported: %d %s", code, body)
	}
	if n := countRows(t, f, &data_models.InvoicingClient{}); n != clients0 {
		t.Errorf("clients: %d, want %d", n, clients0)
	}
}

// A refusal found after the client is added (an issued invoice) rolls the
// client back with the edit.
func TestEditPayment_RefusedEditRollsTheNewClientBack(t *testing.T) {
	f := newInvoiceFixture(t)
	code, p, body := createManual(t, f, fmt.Sprintf(`{"paid_on":"2026-07-14","gross_cents":35000,"method":"bank_transfer","sender_name":"Galerie Nord","note":"Mural","client_id":%d}`, f.clientID))
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	w := invoicingDo(f.r, "POST", "/invoicing/invoices", fmt.Sprintf(`{"payment_ids":[%d]}`, p.ID))
	if w.Code != http.StatusCreated {
		t.Fatalf("draft: %d %s", w.Code, w.Body.String())
	}
	if code, _, body := f.issue(decodeDetail(t, w.Body.Bytes()).Invoice.ID); code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, body)
	}
	clients0 := countRows(t, f, &data_models.InvoicingClient{})
	code, _, _, body = patchPaymentNewClient(t, f, p.ID, `{"sender_name":"Atelier Ost","new_client_name":"Atelier Ost"}`)
	if code != http.StatusConflict || !strings.Contains(body, "payment_on_issued_invoice") {
		t.Fatalf("issued: %d %s", code, body)
	}
	if n := countRows(t, f, &data_models.InvoicingClient{}); n != clients0 {
		t.Errorf("the refused edit kept its new client: %d, want %d", n, clients0)
	}
}

func TestEditPayment_NewClientNameStaysInTheOwnersBook(t *testing.T) {
	f := newInvoiceFixture(t)
	other := data_models.InvoicingClient{OwnerUserID: invoiceFixtureUser + 1, Name: "Atelier Ost"}
	if err := f.db.Create(&other).Error; err != nil {
		t.Fatalf("seed other book's client: %v", err)
	}
	p := handPaymentWithoutClient(t, f)
	// Another book's "Atelier Ost" is not "the same" for this owner.
	code, _, client, body := patchPaymentNewClient(t, f, p.ID, `{"new_client_name":"Atelier Ost"}`)
	if code != http.StatusOK || client == nil || client.ID == other.ID {
		t.Fatalf("edit: %d %s", code, body)
	}
	if client.OwnerUserID != 0 && client.OwnerUserID != invoiceFixtureUser {
		t.Errorf("new client's owner: %d", client.OwnerUserID)
	}
	var stored data_models.InvoicingClient
	f.db.First(&stored, client.ID)
	if stored.OwnerUserID != invoiceFixtureUser {
		t.Errorf("stored owner: %d, want %d", stored.OwnerUserID, invoiceFixtureUser)
	}
	var otherAfter data_models.InvoicingClient
	f.db.First(&otherAfter, other.ID)
	if otherAfter.Name != "Atelier Ost" || otherAfter.OwnerUserID != invoiceFixtureUser+1 {
		t.Errorf("another book's client changed: %+v", otherAfter)
	}
}
