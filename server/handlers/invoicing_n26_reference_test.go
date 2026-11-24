package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"aspirant-online/server/data_models"
)

// system_3 #12257: an invoice over an N26 payment prints "per
// Banküberweisung" and no Transaktions-ID when the import derived the id (a
// database key), and the bank's own operation id when a transfer
// confirmation carried one. The
// invoices CSV leaves it out, and the API serves `shown_reference` empty for
// it while the stored transaction_id stays as it was.
func TestN26Payment_IssuedInvoicePrintsBankTransferAndNoDerivedID(t *testing.T) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Fatalf("pdftotext is not installed; this test reads the issued PDF (the verify surface installs poppler-utils)")
	}
	f := newInvoiceFixture(t)
	const derived = "csv-f00dfeedcafe0123456789ab"
	cid := f.clientID
	n26 := data_models.InvoicingPayment{
		OwnerUserID: invoiceFixtureUser, Source: "n26", Entry: data_models.PaymentEntryImport, TransactionID: derived,
		Kind: "incoming", PaidOn: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Currency: "EUR",
		GrossCents: 15000, SenderName: "Galerie Nord", Note: "Invented work", Category: "paid_work", ClientID: &cid,
	}
	if err := f.db.Create(&n26).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A transfer confirmation carries the bank's own operation id: it prints.
	const opID = "a1b2c3d4-0000-4000-8000-000000000001"
	confirmed := n26
	confirmed.ID, confirmed.TransactionID, confirmed.PaidOn = 0, opID, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	if err := f.db.Create(&confirmed).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	w := invoicingDo(f.r, "POST", "/invoicing/invoices", fmt.Sprintf(`{"payment_ids":[%d,%d,%d]}`, n26.ID, confirmed.ID, f.payments[0].ID))
	if w.Code != http.StatusCreated {
		t.Fatalf("draft: %d %s", w.Code, w.Body.String())
	}
	draft := decodeDetail(t, w.Body.Bytes()).Invoice.ID
	assertShown := func(where string, body []byte, key string) {
		t.Helper()
		var payload map[string]json.RawMessage
		var rows []map[string]interface{}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("%s: decode: %v", where, err)
		}
		if err := json.Unmarshal(payload[key], &rows); err != nil {
			t.Fatalf("%s: decode %s: %v", where, key, err)
		}
		seen := 0
		for _, p := range rows {
			switch p["transaction_id"] {
			case derived:
				seen++
				if p["shown_reference"] != "" {
					t.Errorf("%s: the N26 row's shown_reference is %v, want empty", where, p["shown_reference"])
				}
			case opID, f.payments[0].TransactionID:
				seen++
				if p["shown_reference"] != p["transaction_id"] {
					t.Errorf("%s: the provider id %v is served as shown_reference %v", where, p["transaction_id"], p["shown_reference"])
				}
			}
		}
		if seen != 3 {
			t.Errorf("%s: found %d of the 3 payments", where, seen)
		}
	}
	assertShown("draft detail", w.Body.Bytes(), "payments")
	if code, _, body := f.issue(draft); code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, body)
	}
	assertShown("payments list", invoicingDo(f.r, "GET", "/invoicing/payments", "").Body.Bytes(), "payments")
	assertShown("invoice detail", invoicingDo(f.r, "GET", fmt.Sprintf("/invoicing/invoices/%d", draft), "").Body.Bytes(), "payments")

	pdf := invoicingDo(f.r, "GET", fmt.Sprintf("/invoicing/invoices/%d/pdf", draft), "")
	if pdf.Code != http.StatusOK {
		t.Fatalf("pdf: %d %s", pdf.Code, pdf.Body.String())
	}
	cmd := exec.Command("pdftotext", "-layout", "-", "-")
	cmd.Stdin = bytes.NewReader(pdf.Body.Bytes())
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("pdftotext: %v", err)
	}
	text := strings.Join(strings.Fields(string(out)), " ")
	if !strings.Contains(text, "Zahlung erhalten am 01.10.2026 per Banküberweisung") {
		t.Errorf("the PDF lacks the N26 payment line: %s", text)
	}
	if !strings.Contains(text, "Zahlung erhalten am 02.10.2026 per Banküberweisung (Transaktions-ID: "+opID+")") {
		t.Errorf("the PDF lacks the transfer confirmation's line with its operation id: %s", text)
	}
	if !strings.Contains(text, "per PayPal (Transaktions-ID: "+f.payments[0].TransactionID+")") {
		t.Errorf("the PDF lacks the PayPal payment line: %s", text)
	}
	for _, bad := range []string{"csv-", "per n26", "Transaktions-ID: csv"} {
		if strings.Contains(text, bad) {
			t.Errorf("the PDF prints %q: %s", bad, text)
		}
	}

	csv := invoicingDo(f.r, "GET", "/invoicing/export/invoices.csv", "").Body.String()
	if strings.Contains(csv, "csv-") || !strings.Contains(csv, f.payments[0].TransactionID) || !strings.Contains(csv, opID) {
		t.Errorf("invoices CSV: the provider ids belong in the cell, the derived id never: %s", csv)
	}

	var stored data_models.InvoicingPayment
	f.db.First(&stored, n26.ID)
	if stored.TransactionID != derived {
		t.Errorf("the stored transaction_id changed to %q", stored.TransactionID)
	}
}
