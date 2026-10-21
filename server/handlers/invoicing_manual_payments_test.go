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

// #8409: payments entered by hand, so an invoice does not depend on a PayPal
// import. The fixture clock is 2026-10-21 (Berlin).

func createManual(t *testing.T, f *invoiceFixture, body string) (int, data_models.InvoicingPayment, string) {
	t.Helper()
	w := invoicingDo(f.r, "POST", "/invoicing/payments/manual", body)
	var resp struct {
		Payment data_models.InvoicingPayment `json:"payment"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	return w.Code, resp.Payment, w.Body.String()
}

func TestManualPayment_DraftIssueAndExportsPrintTheReference(t *testing.T) {
	f := newInvoiceFixture(t)
	code, p, body := createManual(t, f, fmt.Sprintf(`{"paid_on":"2026-07-14","gross_cents":42000,"method":"bank_transfer","reference":"RE-2026-17","sender_name":"Galerie Nord","note":"Wandbild August","client_id":%d}`, f.clientID))
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	if p.Entry != "manual" || p.Source != "manual" || !strings.HasPrefix(p.TransactionID, "M-") || p.Category != "paid_work" ||
		p.Kind != "incoming" || p.Currency != "EUR" || p.Method != "bank_transfer" || p.Reference != "RE-2026-17" || p.ClientID == nil {
		t.Fatalf("stored payment: %+v", p)
	}
	events, _ := data_models.ListInvoicingAuditEvents(f.db, "payment", p.ID)
	if len(events) != 1 || events[0].Action != "create_manual" || !strings.Contains(events[0].Details, "RE-2026-17") {
		t.Errorf("audit: %+v", events)
	}

	w := invoicingDo(f.r, "POST", "/invoicing/invoices", fmt.Sprintf(`{"payment_ids":[%d]}`, p.ID))
	if w.Code != http.StatusCreated {
		t.Fatalf("draft: %d %s", w.Code, w.Body.String())
	}
	id := decodeDetail(t, w.Body.Bytes()).Invoice.ID
	if c, d, b := f.issue(id); c != http.StatusOK || d.Invoice.InvoiceNumber == nil {
		t.Fatalf("issue: %d %s", c, b)
	}

	// The all-invoices CSV and the workbook carry the transaction ids.
	csv := invoicingDo(f.r, "GET", "/invoicing/export/invoices.csv", "").Body.String()
	if !strings.Contains(csv, "RE-2026-17") || strings.Contains(csv, p.TransactionID) {
		t.Errorf("invoices CSV must print the reference, never the internal id: %s", csv)
	}

	f.r.GET("/invoicing/export/workbook.xlsx", ExportWorkbookHandler)
	f.r.GET("/invoicing/export/jobcenter-bundle.zip", ExportJobcenterBundleHandler)
	wb := unzipXLSX(t, invoicingDo(f.r, "GET", "/invoicing/export/workbook.xlsx", "").Body.Bytes())
	var manualRow []string
	for _, row := range readSheet(t, wb, "3").Rows {
		var cells []string
		for _, cell := range row.Cells {
			cells = append(cells, cell.Inline+cell.Value)
		}
		if strings.Contains(strings.Join(cells, "|"), p.TransactionID) {
			t.Errorf("workbook leaks the internal id: %v", cells)
		}
		if len(cells) > 3 && cells[3] == "RE-2026-17" {
			manualRow = cells
		}
	}
	if manualRow == nil || manualRow[2] != "manuell" || manualRow[len(manualRow)-1] != "Banküberweisung" {
		t.Errorf("workbook Payments row for the hand-entered payment: %v", manualRow)
	}
	bundle := unzipXLSX(t, invoicingDo(f.r, "GET", "/invoicing/export/jobcenter-bundle.zip", "").Body.Bytes())
	if summary := string(bundle["Zusammenfassung.csv"]); !strings.Contains(summary, "RE-2026-17") || strings.Contains(summary, p.TransactionID) {
		t.Errorf("bundle summary must print the reference, never the internal id: %s", summary)
	}

	// Paid work entered by hand is still paid work on an issued invoice:
	// it cannot be re-categorised or deleted.
	if w := invoicingDo(f.r, "PATCH", fmt.Sprintf("/invoicing/payments/%d", p.ID), `{"category":"gift"}`); w.Code != http.StatusConflict {
		t.Errorf("recategorise issued: %d", w.Code)
	}
	if w := invoicingDo(f.r, "DELETE", fmt.Sprintf("/invoicing/payments/%d", p.ID), ""); w.Code != http.StatusConflict {
		t.Errorf("delete issued: %d", w.Code)
	}
}

func TestManualPayment_WithoutReferencePrintsNoTransactionID(t *testing.T) {
	f := newInvoiceFixture(t)
	code, p, body := createManual(t, f, fmt.Sprintf(`{"paid_on":"2026-08-01","gross_cents":5000,"method":"cash","note":"Skizze","client_id":%d}`, f.clientID))
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	w := invoicingDo(f.r, "POST", "/invoicing/invoices", fmt.Sprintf(`{"payment_ids":[%d]}`, p.ID))
	id := decodeDetail(t, w.Body.Bytes()).Invoice.ID
	if c, _, b := f.issue(id); c != http.StatusOK {
		t.Fatalf("issue: %d %s", c, b)
	}
	csv := invoicingDo(f.r, "GET", "/invoicing/export/invoices.csv", "").Body.String()
	if strings.Contains(csv, p.TransactionID) {
		t.Errorf("internal id leaked into the CSV: %s", csv)
	}
}

func TestManualPayment_Refusals(t *testing.T) {
	f := newInvoiceFixture(t)
	ok := `"paid_on":"2026-07-14","gross_cents":1000,"method":"bank_transfer"`
	cases := []struct {
		name, body, code string
	}{
		{"future date", `{"paid_on":"2026-10-22","gross_cents":1000,"method":"cash"}`, "invalid_field"},
		{"not a date", `{"paid_on":"14.07.2026","gross_cents":1000,"method":"cash"}`, "invalid_field"},
		{"zero amount", `{"paid_on":"2026-07-14","gross_cents":0,"method":"cash"}`, "invalid_field"},
		{"negative amount", `{"paid_on":"2026-07-14","gross_cents":-5,"method":"cash"}`, "invalid_field"},
		{"missing amount", `{"paid_on":"2026-07-14","method":"cash"}`, "invalid_field"},
		{"unknown method", `{"paid_on":"2026-07-14","gross_cents":1000,"method":"crypto"}`, "invalid_field"},
		{"lower-case currency", `{` + ok + `,"currency":"eur"}`, "invalid_field"},
		{"reference with a newline", `{` + ok + `,"reference":"a\nb"}`, "invalid_field"},
		{"reference too long", `{` + ok + `,"reference":"` + strings.Repeat("x", 101) + `"}`, "invalid_field"},
		{"unknown client", `{` + ok + `,"client_id":999}`, "invalid_field"},
		{"category is server-owned", `{` + ok + `,"category":"gift"}`, "unknown_field"},
		{"issue date is not a payment field", `{` + ok + `,"issue_date":"2026-01-01"}`, "unknown_field"},
		{"transaction id is server-owned", `{` + ok + `,"transaction_id":"X"}`, "unknown_field"},
		// Security F3/F4 (#8409): invisible format characters and a
		// mistyped year are refused at entry.
		{"right-to-left override in the reference", `{` + ok + `,"reference":"RE\u202e71"}`, "invalid_field"},
		{"zero-width space in the payer", `{` + ok + `,"sender_name":"Galerie\u200bNord"}`, "invalid_field"},
		{"before 2000", `{"paid_on":"1999-12-31","gross_cents":1000,"method":"cash"}`, "invalid_field"},
	}
	for _, tc := range cases {
		w := invoicingDo(f.r, "POST", "/invoicing/payments/manual", tc.body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.code) {
			t.Errorf("%s: %d %s", tc.name, w.Code, w.Body.String())
		}
	}
	var n int
	f.db.Model(&data_models.InvoicingPayment{}).Where("entry = ?", "manual").Count(&n)
	if n != 0 {
		t.Errorf("refusals stored %d manual payments", n)
	}
}

// The same PayPal payment must not exist twice, whichever way in came first.
func TestManualPayment_PayPalReferenceDeduplicatesBothWays(t *testing.T) {
	f := newInvoiceFixture(t) // seeds imported PayPal payments TX1..TX4
	if code, _, body := createManual(t, f, `{"paid_on":"2026-07-10","gross_cents":10000,"method":"paypal","reference":"TX1"}`); code != http.StatusConflict || !strings.Contains(body, "duplicate_payment") {
		t.Errorf("manual copy of an imported PayPal payment: %d %s", code, body)
	}
	// PayPal ids are upper-case, so case does not make a second payment
	// (security F2): "tx1" is the imported TX1, and a lower-case entry is
	// stored upper-case.
	if code, _, body := createManual(t, f, `{"paid_on":"2026-07-10","gross_cents":10000,"method":"paypal","reference":"tx1"}`); code != http.StatusConflict {
		t.Errorf("lower-case copy of an imported PayPal id: %d %s", code, body)
	}
	code, stored, body := createManual(t, f, `{"paid_on":"2026-07-10","gross_cents":10000,"method":"paypal","reference":"9ab12345cd6789012"}`)
	if code != http.StatusCreated || stored.Reference != "9AB12345CD6789012" {
		t.Fatalf("manual PayPal payment: %d %s", code, body)
	}
	if code, _, body := createManual(t, f, `{"paid_on":"2026-07-10","gross_cents":10000,"method":"paypal","reference":"9AB12345CD6789012"}`); code != http.StatusConflict {
		t.Errorf("second manual entry of the same PayPal id: %d %s", code, body)
	}
	// A bank reference may repeat: only PayPal ids are unique keys.
	for i := 0; i < 2; i++ {
		if code, _, body := createManual(t, f, `{"paid_on":"2026-07-10","gross_cents":10000,"method":"bank_transfer","reference":"Miete"}`); code != http.StatusCreated {
			t.Errorf("bank reference %d: %d %s", i, code, body)
		}
	}
	csv := "Date,Name,Type,Status,Currency,Gross,Transaction ID\n" +
		"10.07.2026,A,Payment,Completed,EUR,100.00,9AB12345CD6789012\n" +
		"11.07.2026,B,Payment,Completed,EUR,20.00,NEW1\n"
	s := decodeSummary(t, uploadCSV(t, f.r, csv, ""))
	if s.Imported != 1 || s.Duplicates != 1 {
		t.Errorf("import over a manual PayPal payment: %+v", s)
	}
}

func TestManualPayment_DeleteOnlyWhenManualAndOnNoInvoice(t *testing.T) {
	f := newInvoiceFixture(t)
	_, loose, _ := createManual(t, f, `{"paid_on":"2026-07-14","gross_cents":1000,"method":"cash"}`)
	_, drafted, _ := createManual(t, f, `{"paid_on":"2026-07-14","gross_cents":2000,"method":"cash"}`)
	invoicingDo(f.r, "POST", "/invoicing/invoices", fmt.Sprintf(`{"payment_ids":[%d]}`, drafted.ID))

	// The list says which payments an invoice covers, so the client offers
	// Delete only where the server would allow it.
	onInvoice := map[uint]bool{}
	for _, p := range listPayments(t, f.r, "") {
		onInvoice[p.ID] = p.OnInvoice
	}
	if onInvoice[loose.ID] || !onInvoice[drafted.ID] {
		t.Errorf("on_invoice: loose %v, drafted %v", onInvoice[loose.ID], onInvoice[drafted.ID])
	}

	if w := invoicingDo(f.r, "DELETE", fmt.Sprintf("/invoicing/payments/%d", drafted.ID), ""); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "payment_on_invoice") {
		t.Errorf("delete a payment on a draft: %d %s", w.Code, w.Body.String())
	}
	if w := invoicingDo(f.r, "DELETE", fmt.Sprintf("/invoicing/payments/%d", f.payments[0].ID), ""); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "imported_payment") {
		t.Errorf("delete an imported payment: %d %s", w.Code, w.Body.String())
	}
	if w := invoicingDo(f.r, "DELETE", "/invoicing/payments/99999", ""); w.Code != http.StatusNotFound {
		t.Errorf("delete unknown: %d", w.Code)
	}
	if w := invoicingDo(f.r, "DELETE", fmt.Sprintf("/invoicing/payments/%d", loose.ID), ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete a loose manual payment: %d %s", w.Code, w.Body.String())
	}
	if got, _ := data_models.GetInvoicingPayment(f.db, loose.ID); got != nil {
		t.Errorf("payment still stored after delete")
	}
	events, _ := data_models.ListInvoicingAuditEvents(f.db, "payment", loose.ID)
	if len(events) != 2 || events[0].Action != "delete" {
		t.Errorf("delete audit: %+v", events)
	}
}

// Security F1 (#8409): a delete and a draft that links the same payment must
// not interleave into a link whose payment is gone, which issuing would then
// silently drop. Both orders, then both at once.
func TestManualPayment_DeleteAndDraftNeverLeaveALinkWithoutItsPayment(t *testing.T) {
	f := newInvoiceFixture(t)
	orphanLinks := func() int {
		var n int
		f.db.Table("invoicing_invoice_payments").
			Joins("LEFT JOIN invoicing_payments p ON p.id = invoicing_invoice_payments.payment_id").
			Where("p.id IS NULL").Count(&n)
		return n
	}

	_, gone, _ := createManual(t, f, `{"paid_on":"2026-07-14","gross_cents":1000,"method":"cash","note":"x"}`)
	if w := invoicingDo(f.r, "DELETE", fmt.Sprintf("/invoicing/payments/%d", gone.ID), ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}
	if w := invoicingDo(f.r, "POST", "/invoicing/invoices", fmt.Sprintf(`{"payment_ids":[%d]}`, gone.ID)); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "payment_not_found") {
		t.Errorf("draft after delete: %d %s", w.Code, w.Body.String())
	}

	_, kept, _ := createManual(t, f, `{"paid_on":"2026-07-14","gross_cents":1000,"method":"cash","note":"x"}`)
	invoicingDo(f.r, "POST", "/invoicing/invoices", fmt.Sprintf(`{"payment_ids":[%d]}`, kept.ID))
	if w := invoicingDo(f.r, "DELETE", fmt.Sprintf("/invoicing/payments/%d", kept.ID), ""); w.Code != http.StatusConflict {
		t.Errorf("delete after draft: %d", w.Code)
	}

	// Both at once, repeatedly. sqlite :memory: needs one shared connection,
	// so this serialises on it; on Postgres the payment-row and issuing
	// locks do the ordering. Either way exactly one side may win.
	f.db.DB().SetMaxOpenConns(1)
	for i := 0; i < 20; i++ {
		_, p, _ := createManual(t, f, `{"paid_on":"2026-07-14","gross_cents":1000,"method":"cash","note":"x"}`)
		var wg sync.WaitGroup
		codes := make([]int, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			codes[0] = invoicingDo(f.r, "DELETE", fmt.Sprintf("/invoicing/payments/%d", p.ID), "").Code
		}()
		go func() {
			defer wg.Done()
			codes[1] = invoicingDo(f.r, "POST", "/invoicing/invoices", fmt.Sprintf(`{"payment_ids":[%d]}`, p.ID)).Code
		}()
		wg.Wait()
		deleted, drafted := codes[0] == http.StatusNoContent, codes[1] == http.StatusCreated
		if deleted == drafted {
			t.Errorf("round %d: delete %d, draft %d; exactly one must win", i, codes[0], codes[1])
		}
	}
	if n := orphanLinks(); n != 0 {
		t.Errorf("%d invoice links point at a deleted payment", n)
	}
}
