package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"aspirant-online/server/data_models"
)

// system_3 #10539 (design §3.142 Decisions 2 and 3): a hand-entered payment
// can add its payer to the client book. Client and payment are saved
// together or not at all.

func countRows(t *testing.T, f *invoiceFixture, model interface{}) int {
	t.Helper()
	var n int
	if err := f.db.Model(model).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestManualPayment_NewClientNameAddsThePayerAndLinksThePayment(t *testing.T) {
	f := newInvoiceFixture(t)
	clients0 := countRows(t, f, &data_models.InvoicingClient{})
	audits0 := countRows(t, f, &data_models.InvoicingAuditEvent{})

	w := invoicingDo(f.r, "POST", "/invoicing/payments/manual",
		`{"paid_on":"2026-07-14","gross_cents":35000,"method":"bank_transfer","sender_name":"MARA ESEMPIO","new_client_name":"Mara Esempio"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Payment data_models.InvoicingPayment `json:"payment"`
		Client  *data_models.InvoicingClient `json:"client"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Client == nil || resp.Client.Name != "Mara Esempio" || resp.Client.ID == 0 {
		t.Fatalf("response client: %+v", resp.Client)
	}
	if resp.Payment.ClientID == nil || *resp.Payment.ClientID != resp.Client.ID {
		t.Errorf("payment not linked to the new client: %+v", resp.Payment.ClientID)
	}
	if resp.Payment.SenderName != "MARA ESEMPIO" {
		t.Errorf("the payer name as typed is stored on the payment: %q", resp.Payment.SenderName)
	}
	if got := countRows(t, f, &data_models.InvoicingClient{}); got != clients0+1 {
		t.Errorf("clients: %d, want %d", got, clients0+1)
	}
	// One audit event for the client, one for the payment.
	if got := countRows(t, f, &data_models.InvoicingAuditEvent{}); got != audits0+2 {
		t.Errorf("audit events: %d, want %d", got, audits0+2)
	}
	stored, err := data_models.GetInvoicingClient(f.db, invoiceFixtureUser, resp.Client.ID)
	if err != nil || stored == nil {
		t.Fatalf("client not in the owner's book: %v %v", stored, err)
	}
	// Saving the payment made no draft (Decision 1).
	if got := countRows(t, f, &data_models.InvoicingInvoice{}); got != 0 {
		t.Errorf("a hand-entered payment made %d invoices", got)
	}
}

func TestManualPayment_NewClientNameAlreadyInTheBookIsRefusedWithThatClient(t *testing.T) {
	f := newInvoiceFixture(t) // the book holds "Galerie Nord"
	clients0 := countRows(t, f, &data_models.InvoicingClient{})
	payments0 := countRows(t, f, &data_models.InvoicingPayment{})
	for _, name := range []string{"Galerie Nord", "GALERIE NORD", "galerie-nord.", "Galérie  Nord"} {
		w := invoicingDo(f.r, "POST", "/invoicing/payments/manual",
			fmt.Sprintf(`{"paid_on":"2026-07-14","gross_cents":1000,"method":"cash","new_client_name":%q}`, name))
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "client_exists") {
			t.Errorf("%q: %d %s", name, w.Code, w.Body.String())
			continue
		}
		var resp struct {
			Client data_models.InvoicingClient `json:"client"`
		}
		json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.Client.ID != f.clientID {
			t.Errorf("%q: the refusal names client %d, want %d", name, resp.Client.ID, f.clientID)
		}
	}
	if got := countRows(t, f, &data_models.InvoicingClient{}); got != clients0 {
		t.Errorf("refusals added clients: %d, want %d", got, clients0)
	}
	if got := countRows(t, f, &data_models.InvoicingPayment{}); got != payments0 {
		t.Errorf("refusals stored payments: %d, want %d", got, payments0)
	}
}

// Another owner's client of the same name is not in this owner's book.
func TestManualPayment_NewClientNameIgnoresAnotherOwnersBook(t *testing.T) {
	f := newInvoiceFixture(t)
	other := data_models.InvoicingClient{OwnerUserID: invoiceFixtureUser + 1, Name: "Mara Esempio"}
	if err := f.db.Create(&other).Error; err != nil {
		t.Fatalf("seed another owner's client: %v", err)
	}
	w := invoicingDo(f.r, "POST", "/invoicing/payments/manual",
		`{"paid_on":"2026-07-14","gross_cents":1000,"method":"cash","new_client_name":"Mara Esempio"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("another owner's client blocked this owner's new client: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Client data_models.InvoicingClient `json:"client"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Client.ID == other.ID {
		t.Error("the payment was linked to another owner's client")
	}
}

func TestManualPayment_NewClientNameRefusals(t *testing.T) {
	f := newInvoiceFixture(t)
	clients0 := countRows(t, f, &data_models.InvoicingClient{})
	ok := `"paid_on":"2026-07-14","gross_cents":1000,"method":"cash"`
	for name, body := range map[string]string{
		"with client_id":   fmt.Sprintf(`{%s,"client_id":%d,"new_client_name":"Mara Esempio"}`, ok, f.clientID),
		"newline":          `{` + ok + `,"new_client_name":"Mara\nEsempio"}`,
		"zero-width space": `{` + ok + `,"new_client_name":"Mara​Esempio"}`,
		"too long":         `{` + ok + `,"new_client_name":"` + strings.Repeat("x", 201) + `"}`,
		"not a string":     `{` + ok + `,"new_client_name":42}`,
	} {
		w := invoicingDo(f.r, "POST", "/invoicing/payments/manual", body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_field") {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	// An empty name and a null client_id beside it are a payment with no
	// client, as before.
	w := invoicingDo(f.r, "POST", "/invoicing/payments/manual", `{`+ok+`,"client_id":null,"new_client_name":"  "}`)
	if w.Code != http.StatusCreated || strings.Contains(w.Body.String(), `"client":`) {
		t.Errorf("blank name: %d %s", w.Code, w.Body.String())
	}
	if got := countRows(t, f, &data_models.InvoicingClient{}); got != clients0 {
		t.Errorf("refusals added clients: %d, want %d", got, clients0)
	}
}

// A refusal that comes after the client insert rolls the client back too:
// the duplicate PayPal check runs after it in the same transaction.
func TestManualPayment_NewClientRolledBackWhenThePaymentIsRefused(t *testing.T) {
	f := newInvoiceFixture(t) // seeds imported PayPal payment TX1
	clients0 := countRows(t, f, &data_models.InvoicingClient{})
	w := invoicingDo(f.r, "POST", "/invoicing/payments/manual",
		`{"paid_on":"2026-07-10","gross_cents":10000,"method":"paypal","reference":"TX1","new_client_name":"Mara Esempio"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "duplicate_payment") {
		t.Fatalf("duplicate PayPal payment: %d %s", w.Code, w.Body.String())
	}
	if got := countRows(t, f, &data_models.InvoicingClient{}); got != clients0 {
		t.Errorf("a refused payment left its new client behind: %d clients, want %d", got, clients0)
	}
}

// Two hand entries of one new name at once make one client: the second
// waits, then finds the first one's client. sqlite :memory: needs one shared
// connection, so here the two serialise on it (as in
// TestManualPayment_DeleteAndDraftNeverLeaveALinkWithoutItsPayment); on
// Postgres the issuing lock the handler takes before the lookup does the
// ordering. This pins the outcome either ordering must produce.
func TestManualPayment_ConcurrentNewClientNameMakesOneClient(t *testing.T) {
	f := newInvoiceFixture(t)
	f.db.DB().SetMaxOpenConns(1)
	clients0 := countRows(t, f, &data_models.InvoicingClient{})
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := invoicingDo(f.r, "POST", "/invoicing/payments/manual",
				`{"paid_on":"2026-07-14","gross_cents":1000,"method":"cash","new_client_name":"Mara Esempio"}`)
			codes[i] = w.Code
		}(i)
	}
	wg.Wait()
	created, conflicted := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicted++
		}
	}
	if created != 1 || conflicted != 1 {
		t.Errorf("codes %v: want one 201 and one 409", codes)
	}
	if got := countRows(t, f, &data_models.InvoicingClient{}); got != clients0+1 {
		t.Errorf("clients: %d, want %d", got, clients0+1)
	}
}
