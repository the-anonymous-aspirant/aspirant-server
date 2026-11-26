package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"aspirant-online/server/data_models"
)

// system_3 #11507 (epic #11497 item 5): a client added from a hand entry or
// from a PayPal payer gets what that entry knows about the payer. Only a
// client being created gets it. No existing client or payment row is changed.

func TestManualPayment_NewClientTakesTheTypedAddressAndEmail(t *testing.T) {
	f := newInvoiceFixture(t)
	w := invoicingDo(f.r, "POST", "/invoicing/payments/manual",
		`{"paid_on":"2026-07-14","gross_cents":35000,"method":"bank_transfer","sender_name":"Mara Esempio","new_client_name":"Mara Esempio",`+
			`"new_client_address":"  Nordweg 2\r\n13353 Berlin  ","new_client_email":" mara@example.test "}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Client *data_models.InvoicingClient `json:"client"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Client == nil {
		t.Fatalf("no client in the response: %s", w.Body.String())
	}
	stored, err := data_models.GetInvoicingClient(f.db, invoiceFixtureUser, resp.Client.ID)
	if err != nil || stored == nil {
		t.Fatalf("client not stored: %v %v", stored, err)
	}
	// Trimmed, and a Windows line end becomes a plain one.
	if stored.Address != "Nordweg 2\n13353 Berlin" || stored.Email != "mara@example.test" {
		t.Errorf("stored address %q, email %q", stored.Address, stored.Email)
	}
	// Nothing else is guessed.
	if stored.PayPalEmail != "" || stored.Phone != "" || stored.Notes != "" {
		t.Errorf("fields nobody typed were set: %+v", stored)
	}
}

func TestManualPayment_NewClientDetailsAreOptional(t *testing.T) {
	f := newInvoiceFixture(t)
	w := invoicingDo(f.r, "POST", "/invoicing/payments/manual",
		`{"paid_on":"2026-07-14","gross_cents":35000,"method":"cash","new_client_name":"Mara Esempio","new_client_address":"","new_client_email":null}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Client *data_models.InvoicingClient `json:"client"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Client == nil || resp.Client.Address != "" || resp.Client.Email != "" {
		t.Errorf("client: %+v", resp.Client)
	}
}

func TestManualPayment_NewClientDetailsRefusals(t *testing.T) {
	f := newInvoiceFixture(t)
	clients0 := countRows(t, f, &data_models.InvoicingClient{})
	payments0 := countRows(t, f, &data_models.InvoicingPayment{})
	ok := `"paid_on":"2026-07-14","gross_cents":1000,"method":"cash"`
	named := ok + `,"new_client_name":"Mara Esempio"`
	for name, tc := range map[string]struct{ body, field string }{
		// With no new client they would be dropped silently, so they are refused.
		"address with no new client": {`{` + ok + `,"new_client_address":"Nordweg 2"}`, "new_client_address"},
		"email with no new client":   {`{` + ok + `,"new_client_email":"mara@example.test"}`, "new_client_email"},
		"address with client_id":     {fmt.Sprintf(`{%s,"client_id":%d,"new_client_address":"Nordweg 2"}`, ok, f.clientID), "new_client_address"},
		"email not an address":       {`{` + named + `,"new_client_email":"mara at example"}`, "new_client_email"},
		"email with a display name":  {`{` + named + `,"new_client_email":"Mara <mara@example.test>"}`, "new_client_email"},
		"address too long":           {`{` + named + `,"new_client_address":"` + strings.Repeat("x", 501) + `"}`, "new_client_address"},
		"address with a control":     {`{` + named + `,"new_client_address":"Nordweg\u0007 2"}`, "new_client_address"},
		"address with a zero-width":  {`{` + named + `,"new_client_address":"Nord​weg 2"}`, "new_client_address"},
		"address not a string":       {`{` + named + `,"new_client_address":42}`, "new_client_address"},
		"email not a string":         {`{` + named + `,"new_client_email":["a@b.test"]}`, "new_client_email"},
	} {
		w := invoicingDo(f.r, "POST", "/invoicing/payments/manual", tc.body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"field":"`+tc.field+`"`) {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	if got := countRows(t, f, &data_models.InvoicingClient{}); got != clients0 {
		t.Errorf("refusals added clients: %d, want %d", got, clients0)
	}
	if got := countRows(t, f, &data_models.InvoicingPayment{}); got != payments0 {
		t.Errorf("refusals added payments: %d, want %d", got, payments0)
	}
}

// The name of a client already in the book is refused with that client, as
// before, and what was typed beside it never reaches the existing row.
func TestManualPayment_NewClientDetailsNeverChangeAnExistingClient(t *testing.T) {
	f := newInvoiceFixture(t)
	before, err := data_models.GetInvoicingClient(f.db, invoiceFixtureUser, f.clientID)
	if err != nil || before == nil {
		t.Fatalf("fixture client: %v %v", before, err)
	}
	body := fmt.Sprintf(`{"paid_on":"2026-07-14","gross_cents":1000,"method":"cash","new_client_name":%q,"new_client_address":"Somewhere Else 9","new_client_email":"other@example.test"}`, before.Name)
	w := invoicingDo(f.r, "POST", "/invoicing/payments/manual", body)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "client_exists") {
		t.Fatalf("existing name: %d %s", w.Code, w.Body.String())
	}
	after, _ := data_models.GetInvoicingClient(f.db, invoiceFixtureUser, f.clientID)
	if after.Address != before.Address || after.Email != before.Email || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("the existing client changed: before %+v after %+v", before, after)
	}
}

func TestEditPayment_NewClientTakesTheTypedAddressAndEmail(t *testing.T) {
	f := newInvoiceFixture(t)
	p := handPaymentWithoutClient(t, f)
	code, _, client, body := patchPaymentNewClient(t, f, p.ID,
		`{"new_client_name":"Atelier Ost","new_client_address":"Ostweg 5\n10243 Berlin","new_client_email":"atelier@example.test"}`)
	if code != http.StatusOK || client == nil {
		t.Fatalf("edit: %d %s", code, body)
	}
	stored, err := data_models.GetInvoicingClient(f.db, invoiceFixtureUser, client.ID)
	if err != nil || stored == nil {
		t.Fatalf("client not stored: %v %v", stored, err)
	}
	if stored.Address != "Ostweg 5\n10243 Berlin" || stored.Email != "atelier@example.test" {
		t.Errorf("stored address %q, email %q", stored.Address, stored.Email)
	}
}

func TestEditPayment_NewClientDetailsRefusals(t *testing.T) {
	f := newInvoiceFixture(t)
	p := handPaymentWithoutClient(t, f)
	clients0 := countRows(t, f, &data_models.InvoicingClient{})
	for name, body := range map[string]string{
		"address with no new client": `{"note":"x","new_client_address":"Ostweg 5"}`,
		"email not an address":       `{"new_client_name":"Atelier Ost","new_client_email":"nope"}`,
		"address not a string":       `{"new_client_name":"Atelier Ost","new_client_address":7}`,
	} {
		code, _, _, resp := patchPaymentNewClient(t, f, p.ID, body)
		if code != http.StatusBadRequest || !strings.Contains(resp, `"field":"new_client_`) {
			t.Errorf("%s: %d %s", name, code, resp)
		}
	}
	// An imported payment's payer is the bank's record: nothing about a new
	// client can be sent for it.
	code, _, _, resp := patchPaymentNewClient(t, f, f.payments[0].ID, `{"new_client_address":"Ostweg 5"}`)
	if code != http.StatusBadRequest || !strings.Contains(resp, "bank_fact_field") {
		t.Errorf("imported: %d %s", code, resp)
	}
	if n := countRows(t, f, &data_models.InvoicingClient{}); n != clients0 {
		t.Errorf("refusals added clients: %d, want %d", n, clients0)
	}
}

// "Add payers": a client created from a PayPal payer shows that payer's email
// on its card. An N26 payer has none to give, and an existing client keeps
// what it has.
func TestAddPayers_NewClientFromPayPalAlsoGetsTheContactEmail(t *testing.T) {
	f := newPayerFixture(t)
	u := uint(invoiceFixtureUser)
	m1 := f.pay(t, u, "n26", "Mara Esempio", "", "paid_work", 10000, 1)
	l1 := f.pay(t, u, "paypal", "Lia", "lia@example.test", "uncategorised", 3000, 2)
	b1 := f.pay(t, u, "paypal", "Bo", "not an address", "uncategorised", 3000, 3)
	g1 := f.pay(t, u, "paypal", "Galerie Nord", "other@galerie.test", "uncategorised", 3000, 4)
	gBefore, _ := data_models.GetInvoicingClient(f.db, u, f.clientID)

	code, body := addPayers(f, fmt.Sprintf(`{"payers":[
		{"payment_ids":[%d],"new_client_name":"Mara Esempio"},
		{"payment_ids":[%d],"new_client_name":"Lia Beispiel"},
		{"payment_ids":[%d],"new_client_name":"Bo Beispiel"},
		{"payment_ids":[%d],"client_id":%d}]}`, m1, l1, b1, g1, f.clientID))
	if code != http.StatusOK {
		t.Fatalf("add: %d %s", code, body)
	}
	var resp struct {
		Clients []data_models.InvoicingClient `json:"clients"`
	}
	json.Unmarshal([]byte(body), &resp)
	if len(resp.Clients) != 3 {
		t.Fatalf("response: %s", body)
	}
	mara, lia, bo := resp.Clients[0], resp.Clients[1], resp.Clients[2]
	if lia.Email != "lia@example.test" || lia.PayPalEmail != "lia@example.test" {
		t.Errorf("Lia: email %q, paypal_email %q", lia.Email, lia.PayPalEmail)
	}
	if mara.Email != "" {
		t.Errorf("an N26 payer has no email to give: %q", mara.Email)
	}
	// What the client form would refuse is not put in its email field.
	if bo.Email != "" {
		t.Errorf("a sender email that is not an address became the contact email: %q", bo.Email)
	}
	if lia.Address != "" || mara.Address != "" {
		t.Errorf("no import carries an address, yet one was set: %q %q", lia.Address, mara.Address)
	}
	gAfter, _ := data_models.GetInvoicingClient(f.db, u, f.clientID)
	if gAfter.Email != gBefore.Email || gAfter.Address != gBefore.Address {
		t.Errorf("the existing client's details changed: before %+v after %+v", gBefore, gAfter)
	}
}
