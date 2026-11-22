package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"aspirant-online/server/data_models"
)

// system_3 #12084 (#11551; security c75108): a client can carry an IBAN, and
// it stays on the client. It never reaches an issued invoice (C1), an audit
// row (C2) or the clients export, and it is stored in one normalised form
// (C3). Every IBAN here is a published example, never a real account.
const (
	exampleIBAN        = "DE89370400440532013000" // the ISO 13616 / Bundesbank example
	exampleIBANSpaced  = "de89 3704 0044 0532 0130 00"
	otherExampleIBAN   = "GB82WEST12345698765432" // the ECBS example
	badChecksumIBAN    = "DE89370400440532013001"
	ibanClientsAPIUser = uint(7)
)

func decodeClient(t *testing.T, body string) data_models.InvoicingClient {
	t.Helper()
	var resp struct {
		Client data_models.InvoicingClient `json:"client"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return resp.Client
}

func TestClientIBAN_NormalisedValidatedAndKeptWhenAbsent(t *testing.T) {
	db := setupInvoicingDB(t)
	r := setupInvoicingRouter(db, ibanClientsAPIUser)

	w := invoicingDo(r, "POST", "/invoicing/clients", fmt.Sprintf(`{"name":"Atelier Beispiel","iban":%q}`, exampleIBANSpaced))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	cl := decodeClient(t, w.Body.String())
	if cl.IBAN != exampleIBAN {
		t.Fatalf("stored IBAN %q, want the normalised %q", cl.IBAN, exampleIBAN)
	}
	path := fmt.Sprintf("/invoicing/clients/%d", cl.ID)

	// A form that does not send the field keeps the stored value.
	if w := invoicingDo(r, "PUT", path, `{"name":"Atelier Beispiel GmbH"}`); w.Code != http.StatusOK {
		t.Fatalf("update without iban: %d %s", w.Code, w.Body.String())
	}
	got, _ := data_models.GetInvoicingClient(db, ibanClientsAPIUser, cl.ID)
	if got.IBAN != exampleIBAN || got.Name != "Atelier Beispiel GmbH" {
		t.Errorf("an update without iban changed it: %+v", got)
	}

	for name, value := range map[string]string{
		"bad checksum": badChecksumIBAN,
		"bad shape":    "DE89-NOT-AN-IBAN",
		"too short":    "DE8937",
	} {
		w := invoicingDo(r, "PUT", path, fmt.Sprintf(`{"name":"Atelier Beispiel GmbH","iban":%q}`, value))
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_iban") {
			t.Errorf("%s: %d %s, want 400 invalid_iban", name, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), value) {
			t.Errorf("%s: the refusal echoes the value: %s", name, w.Body.String())
		}
	}
	got, _ = data_models.GetInvoicingClient(db, ibanClientsAPIUser, cl.ID)
	if got.IBAN != exampleIBAN {
		t.Errorf("a refused update changed the IBAN: %q", got.IBAN)
	}

	if w := invoicingDo(r, "PUT", path, `{"name":"Atelier Beispiel GmbH","iban":"  "}`); w.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", w.Code, w.Body.String())
	}
	got, _ = data_models.GetInvoicingClient(db, ibanClientsAPIUser, cl.ID)
	if got.IBAN != "" {
		t.Errorf("an empty iban should clear it: %q", got.IBAN)
	}
}

func TestClientIBAN_AuditRowsSayOnlyWhetherItChanged(t *testing.T) {
	db := setupInvoicingDB(t)
	r := setupInvoicingRouter(db, ibanClientsAPIUser)

	w := invoicingDo(r, "POST", "/invoicing/clients", fmt.Sprintf(`{"name":"Atelier Beispiel","phone":"+49 30 1234567","iban":%q}`, exampleIBAN))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	path := fmt.Sprintf("/invoicing/clients/%d", decodeClient(t, w.Body.String()).ID)
	if w := invoicingDo(r, "PUT", path, `{"name":"Atelier Beispiel GmbH","phone":"+49 30 1234567"}`); w.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", w.Code, w.Body.String())
	}
	if w := invoicingDo(r, "PUT", path, fmt.Sprintf(`{"name":"Atelier Beispiel GmbH","phone":"+49 30 1234567","iban":%q}`, otherExampleIBAN)); w.Code != http.StatusOK {
		t.Fatalf("new iban: %d %s", w.Code, w.Body.String())
	}

	var events []data_models.InvoicingAuditEvent
	if err := db.Where("entity_type = ?", invoicingEntityClient).Order("id asc").Find(&events).Error; err != nil {
		t.Fatalf("audit rows: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("client audit rows: %d, want 3", len(events))
	}
	for i, want := range []bool{true, false, true} {
		d := events[i].Details
		if strings.Contains(d, exampleIBAN) || strings.Contains(d, otherExampleIBAN) {
			t.Errorf("audit row %d carries an IBAN: %s", i, d)
		}
		if !strings.Contains(d, "1234567") {
			t.Errorf("audit row %d lost the rest of the client: %s", i, d)
		}
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(d), &parsed); err != nil {
			t.Fatalf("audit row %d: %v", i, err)
		}
		if parsed["iban_changed"] != want {
			t.Errorf("audit row %d iban_changed = %v, want %v", i, parsed["iban_changed"], want)
		}
	}
}

// The "Add payers…" path that gives an existing client a PayPal email writes
// a client update row; a client with an IBAN must not leak it there.
func TestAddPayers_ExistingClientUpdateAuditHasNoIBAN(t *testing.T) {
	f := newPayerFixture(t)
	u := uint(invoiceFixtureUser)
	bare := data_models.InvoicingClient{OwnerUserID: u, Name: "Lia Beispiel", IBAN: exampleIBAN}
	f.db.Create(&bare)
	l1 := f.pay(t, u, "paypal", "Lia", "lia@example.test", "uncategorised", 3000, 3)
	if code, body := addPayers(f, fmt.Sprintf(`{"payers":[{"payment_ids":[%d],"client_id":%d}]}`, l1, bare.ID)); code != http.StatusOK {
		t.Fatalf("add: %d %s", code, body)
	}
	var events []data_models.InvoicingAuditEvent
	f.db.Where("entity_type = ? AND entity_id = ?", invoicingEntityClient, bare.ID).Find(&events)
	if len(events) == 0 {
		t.Fatalf("no client audit row for the PayPal email update")
	}
	for _, e := range events {
		if strings.Contains(e.Details, exampleIBAN) {
			t.Errorf("payers audit row carries the IBAN: %s", e.Details)
		}
		if !strings.Contains(e.Details, `"iban_changed":false`) || !strings.Contains(e.Details, `"via":"payers"`) {
			t.Errorf("payers audit row: %s", e.Details)
		}
	}
	got, _ := data_models.GetInvoicingClient(f.db, u, bare.ID)
	if got.IBAN != exampleIBAN {
		t.Errorf("the client lost its IBAN: %q", got.IBAN)
	}
}

// C1: an issued invoice keeps no IBAN, though the live client does.
func TestIssue_ClientSnapshotLeavesOutIBAN(t *testing.T) {
	f := newInvoiceFixture(t)
	if err := f.db.Model(&data_models.InvoicingClient{}).Where("id = ?", f.clientID).Update("iban", exampleIBAN).Error; err != nil {
		t.Fatalf("setting iban: %v", err)
	}
	id := f.draft(t, 0, 1)
	code, d, body := f.issue(id)
	if code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, body)
	}
	if !strings.Contains(d.Invoice.ClientSnapshot, "Galerie Nord") {
		t.Fatalf("snapshot lost the client itself: %q", d.Invoice.ClientSnapshot)
	}
	var stored data_models.InvoicingInvoice
	f.db.First(&stored, id)
	for where, s := range map[string]string{"snapshot": stored.ClientSnapshot, "issue response": body} {
		if strings.Contains(s, exampleIBAN) {
			t.Errorf("the IBAN reached the %s", where)
		}
	}
	live, _ := data_models.GetInvoicingClient(f.db, invoiceFixtureUser, f.clientID)
	if live == nil || live.IBAN != exampleIBAN {
		t.Errorf("live client lost its IBAN: %+v", live)
	}
}

// ForInvoice keeps exactly these fields. A new client field is off invoices
// until it is added here and to ForInvoice, on purpose.
func TestForInvoice_KeepsOnlyTheInvoiceFields(t *testing.T) {
	keep := map[string]bool{"ID": true, "OwnerUserID": true, "Name": true, "Address": true,
		"Email": true, "PayPalEmail": true, "CreatedAt": true, "UpdatedAt": true}
	var full data_models.InvoicingClient
	v := reflect.ValueOf(&full).Elem()
	for i := 0; i < v.NumField(); i++ {
		switch fv := v.Field(i); fv.Kind() {
		case reflect.String:
			fv.SetString("x-" + v.Type().Field(i).Name)
		case reflect.Uint:
			fv.SetUint(uint64(i + 1))
		case reflect.Struct:
			fv.Set(reflect.ValueOf(time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)))
		default:
			t.Fatalf("field %s has kind %s; extend this test", v.Type().Field(i).Name, fv.Kind())
		}
	}
	out := reflect.ValueOf(full.ForInvoice())
	for i := 0; i < out.NumField(); i++ {
		name := out.Type().Field(i).Name
		kept := !out.Field(i).IsZero()
		if kept != keep[name] {
			t.Errorf("ForInvoice: %s kept=%v, want %v", name, kept, keep[name])
		}
		if kept && !reflect.DeepEqual(out.Field(i).Interface(), v.Field(i).Interface()) {
			t.Errorf("ForInvoice changed %s", name)
		}
	}
}

func TestExportClientsCSV_HasNoIBAN(t *testing.T) {
	db := setupInvoicingDB(t)
	r := clientsCSVRouter(db)
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	seedClient(t, db, data_models.InvoicingClient{Name: "Atelier Beispiel", IBAN: exampleIBAN}, now, now)
	w := invoicingDo(r, "GET", "/invoicing/export/clients.csv", "")
	if w.Code != http.StatusOK {
		t.Fatalf("export: %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Atelier Beispiel") {
		t.Fatalf("export lost the client: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), exampleIBAN) {
		t.Errorf("the clients export carries the IBAN")
	}
}

// The "Add payers…" new-client path records its client through clientAudit
// too, so when #12085 lets it set an IBAN the row already carries only
// iban_changed.
func TestAddPayers_NewClientAuditGoesThroughClientAudit(t *testing.T) {
	f := newPayerFixture(t)
	u := uint(invoiceFixtureUser)
	m1 := f.pay(t, u, "n26", "Mara Esempio", "", "uncategorised", 10000, 1)
	if code, body := addPayers(f, fmt.Sprintf(`{"payers":[{"payment_ids":[%d],"new_client_name":"Mara Esempio"}]}`, m1)); code != http.StatusOK {
		t.Fatalf("add: %d %s", code, body)
	}
	var events []data_models.InvoicingAuditEvent
	f.db.Where("entity_type = ? AND action = ?", invoicingEntityClient, invoicingActionCreate).Find(&events)
	found := false
	for _, e := range events {
		if strings.Contains(e.Details, "Mara Esempio") {
			found = true
			if !strings.Contains(e.Details, `"iban_changed":false`) || !strings.Contains(e.Details, `"via":"payers"`) {
				t.Errorf("new-client audit row: %s", e.Details)
			}
		}
	}
	if !found {
		t.Fatalf("no create row for the new client: %+v", events)
	}
}
