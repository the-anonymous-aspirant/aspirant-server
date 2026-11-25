package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"aspirant-online/server/data_models"
)

// Bad requests that answered 500 now answer a refusal (system_3 #12268, the
// #11498 review's C3-14). The out-of-range ids are in invoicing_id_range_test.go.

func profileWithNext(next string) string {
	return strings.Replace(validProfileBody, `"next_invoice_number":1`, `"next_invoice_number":`+next, 1)
}

func TestProfile_NextInvoiceNumberHasTheColumnsCeiling(t *testing.T) {
	r := setupInvoicingRouter(setupInvoicingDB(t), 42)
	w := invoicingDo(r, "PUT", "/invoicing/profile", profileWithNext("2147483648"))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "next_invoice_number must be at most 2147483647") {
		t.Fatalf("one past the ceiling: %d %s", w.Code, w.Body.String())
	}
	if w := invoicingDo(r, "PUT", "/invoicing/profile", profileWithNext("2147483647")); w.Code != http.StatusOK {
		t.Fatalf("the ceiling itself: %d %s", w.Code, w.Body.String())
	}
}

func TestIssue_AtTheLargestNumberIsRefusedAndTheDraftStaysADraft(t *testing.T) {
	f := newInvoiceFixture(t)
	id := f.draft(t, 0)
	f.db.Model(&data_models.InvoicingProfile{}).Where("owner_user_id = ?", invoiceFixtureUser).
		UpdateColumn("next_invoice_number", data_models.MaxInvoiceNumber)

	code, _, body := f.issue(id)
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, `"invoice_numbers_exhausted"`) {
		t.Fatalf("want 422 invoice_numbers_exhausted, got %d %s", code, body)
	}
	inv, err := data_models.GetInvoicingInvoice(f.db, invoiceFixtureUser, id)
	if err != nil || inv == nil || inv.Status != data_models.InvoiceStatusDraft || inv.InvoiceNumber != nil {
		t.Errorf("the draft changed: %+v %v", inv, err)
	}
	if got := f.profileNext(t); got != data_models.MaxInvoiceNumber {
		t.Errorf("the counter moved to %d", got)
	}
	// One below still issues, and leaves the counter at the largest value.
	f.db.Model(&data_models.InvoicingProfile{}).Where("owner_user_id = ?", invoiceFixtureUser).
		UpdateColumn("next_invoice_number", data_models.MaxInvoiceNumber-1)
	if code, _, body := f.issue(id); code != http.StatusOK {
		t.Fatalf("issue at %d: %d %s", data_models.MaxInvoiceNumber-1, code, body)
	}
}

func TestClientAndProfile_RefuseANULCharacter(t *testing.T) {
	r := setupInvoicingRouter(setupInvoicingDB(t), 42)
	// The name and the address keep #12264's sentences; a NUL is one of
	// the control characters they refuse.
	w := invoicingDo(r, "POST", "/invoicing/clients", `{"name":"Atelier\u0000Probe"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "The name has a tab, a control or a direction character.") {
		t.Fatalf("new client: %d %s", w.Code, w.Body.String())
	}
	w = invoicingDo(r, "POST", "/invoicing/clients", `{"name":"Atelier Probe","email":"a\u0000@example.test"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "email must not hold a NUL character") {
		t.Fatalf("new client, email: %d %s", w.Code, w.Body.String())
	}
	w = invoicingDo(r, "POST", "/invoicing/clients", `{"name":"Atelier Probe"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("seed client: %d %s", w.Code, w.Body.String())
	}
	var cl struct {
		Client data_models.InvoicingClient `json:"client"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &cl); err != nil {
		t.Fatalf("decode client: %v", err)
	}
	w = invoicingDo(r, "PUT", fmt.Sprintf("/invoicing/clients/%d", cl.Client.ID), `{"name":"Atelier Probe","address":"Probeweg 1\u0000"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "The address has a tab, a control or a direction character.") {
		t.Fatalf("client edit: %d %s", w.Code, w.Body.String())
	}
	body := strings.Replace(validProfileBody, `"full_name":"Flower Example"`, `"full_name":"Flower\u0000Example"`, 1)
	w = invoicingDo(r, "PUT", "/invoicing/profile", body)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "full_name must not hold a NUL character") {
		t.Fatalf("profile: %d %s", w.Code, w.Body.String())
	}
	// A line break in an address is still free text.
	if w := invoicingDo(r, "PUT", "/invoicing/profile", validProfileBody); w.Code != http.StatusOK {
		t.Fatalf("a valid profile: %d %s", w.Code, w.Body.String())
	}
}

func TestParsePayPal_ANULInACellIsDropped(t *testing.T) {
	csv := "Date,Name,Type,Status,Currency,Gross,Transaction ID,Note\n" +
		"14/07/2026,Mara\x00 Esempio,Payment,Completed,EUR,10.00,T1,Wand\x00bild\n"
	res, err := ParsePayPalActivityCSV([]byte(csv), "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0].SenderName != "Mara Esempio" || res.Rows[0].Note != "Wandbild" {
		t.Fatalf("got %+v", res.Rows)
	}
}

// Two first uploads on one expense raced: each read "no receipt yet" and
// inserted, and the unique index refused the loser with a 500. The save now
// takes the book lock before it reads, so the second waits for the first and
// replaces it. sqlite cannot run the race; this pins that the lock is taken.
func TestReceiptSave_TakesTheBookLock(t *testing.T) {
	x := newIDRangeFixture(t)
	count := func() int {
		var n int
		x.db.Raw("SELECT COUNT(*) FROM invoicing_owner_locks WHERE owner_user_id = ?", invoiceFixtureUser).Row().Scan(&n)
		return n
	}
	for _, step := range []struct {
		name string
		do   func() int
	}{
		{"upload", func() int {
			return receiptUpload(x.r, fmt.Sprintf("/invoicing/expenses/%d/receipt", x.expenseID), testReceiptPNG(t)).Code
		}},
		{"delete", func() int {
			return invoicingDo(x.r, "DELETE", fmt.Sprintf("/invoicing/expenses/%d/receipt", x.expenseID), "").Code
		}},
	} {
		x.db.Exec("DELETE FROM invoicing_owner_locks WHERE owner_user_id = ?", invoiceFixtureUser)
		if code := step.do(); code != http.StatusOK {
			t.Fatalf("%s: %d", step.name, code)
		}
		if n := count(); n != 1 {
			t.Errorf("%s did not take the book lock", step.name)
		}
	}
}
