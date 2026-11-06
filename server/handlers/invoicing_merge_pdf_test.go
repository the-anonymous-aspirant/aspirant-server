package handlers

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"aspirant-online/server/data_models"
)

// #9940-A2 / #9956-A1 (operator c61402): one PDF holding several invoices.

func setupMergeRouter(f *invoiceFixture) {
	f.r.GET("/invoicing/export/invoices-merged.pdf", ExportInvoicesMergedPDFHandler)
}

func TestExportInvoicesMergedPDF_TwoIssuedInvoicesConcatenate(t *testing.T) {
	f := newInvoiceFixture(t)
	setupMergeRouter(f)

	id1 := f.draft(t, 0)
	if code, _, body := f.issue(id1); code != http.StatusOK {
		t.Fatalf("issue 1: %d %s", code, body)
	}
	id2 := f.draft(t, 1)
	if code, _, body := f.issue(id2); code != http.StatusOK {
		t.Fatalf("issue 2: %d %s", code, body)
	}

	withInvoicingClock(t, exportClock)
	w := invoicingDo(f.r, "GET", fmt.Sprintf("/invoicing/export/invoices-merged.pdf?ids=%d,%d", id1, id2), "")
	if w.Code != http.StatusOK {
		t.Fatalf("merge: %d %s", w.Code, w.Body.String())
	}
	if cd := w.Header().Get("Content-Disposition"); cd != `attachment; filename="Rechnungen-2026-11-06-1049.pdf"` {
		t.Errorf("disposition %q", cd)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/pdf" {
		t.Errorf("content-type: %s", ct)
	}
	body := w.Body.Bytes()
	if !strings.HasPrefix(string(body), "%PDF-") {
		t.Errorf("not a PDF: first bytes %q", body[:min(20, len(body))])
	}

	// Merging two single-page invoices gives a two-page document, bigger
	// than either invoice's own stored PDF.
	single := invoicingDo(f.r, "GET", fmt.Sprintf("/invoicing/invoices/%d/pdf", id1), "")
	if len(body) <= len(single.Body.Bytes()) {
		t.Errorf("merged PDF (%d bytes) is not bigger than one invoice's PDF (%d bytes)", len(body), len(single.Body.Bytes()))
	}
}

func TestExportInvoicesMergedPDF_Refusals(t *testing.T) {
	f := newInvoiceFixture(t)
	setupMergeRouter(f)
	draftID := f.draft(t, 0)

	cancelID := f.draft(t, 1)
	if code, _, body := f.issue(cancelID); code != http.StatusOK {
		t.Fatalf("issue to cancel: %d %s", code, body)
	}
	if w := invoicingDo(f.r, "POST", fmt.Sprintf("/invoicing/invoices/%d/cancel", cancelID), `{"reason":"wrong amount"}`); w.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}

	cases := map[string]struct {
		url       string
		wantCode  int
		wantInStr string
	}{
		"no ids":              {"/invoicing/export/invoices-merged.pdf", http.StatusBadRequest, "no_ids"},
		"non-numeric id":      {"/invoicing/export/invoices-merged.pdf?ids=abc", http.StatusBadRequest, "bad_id"},
		"nonexistent id":      {"/invoicing/export/invoices-merged.pdf?ids=999999", http.StatusUnprocessableEntity, "invoice_not_found"},
		"draft, not issued":   {fmt.Sprintf("/invoicing/export/invoices-merged.pdf?ids=%d", draftID), http.StatusUnprocessableEntity, "invoice_draft"},
		"cancelled, not kept": {fmt.Sprintf("/invoicing/export/invoices-merged.pdf?ids=%d", cancelID), http.StatusUnprocessableEntity, "invoice_cancelled"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			w := invoicingDo(f.r, "GET", c.url, "")
			if w.Code != c.wantCode || !strings.Contains(w.Body.String(), c.wantInStr) {
				t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
			}
		})
	}
}

// Order is by invoice number ascending, not the order ids were given in
// (design ack c61504: "She can predict it and check it against the list.").
// Exercised directly on invoicesForMerge, not through the HTTP response
// bytes: gopdf embeds a non-deterministic element (two back-to-back calls
// with IDENTICAL input already produce different bytes at the same length),
// so byte comparison cannot tell order apart from that noise.
func TestInvoicesForMerge_OrdersByInvoiceNumberNotRequestOrder(t *testing.T) {
	f := newInvoiceFixture(t)

	id1 := f.draft(t, 0)
	if code, _, body := f.issue(id1); code != http.StatusOK {
		t.Fatalf("issue 1: %d %s", code, body)
	}
	id2 := f.draft(t, 1)
	if code, _, body := f.issue(id2); code != http.StatusOK {
		t.Fatalf("issue 2: %d %s", code, body)
	}

	forward, gerr := invoicesForMerge(f.db, invoiceFixtureUser, []uint{id1, id2})
	if gerr != nil {
		t.Fatalf("forward: %v", gerr)
	}
	reverse, gerr := invoicesForMerge(f.db, invoiceFixtureUser, []uint{id2, id1})
	if gerr != nil {
		t.Fatalf("reverse: %v", gerr)
	}
	if len(forward) != 2 || len(reverse) != 2 || forward[0].ID != reverse[0].ID || forward[1].ID != reverse[1].ID {
		t.Fatalf("request order changed the result order: forward %v, reverse %v", idsOf(forward), idsOf(reverse))
	}
	if forward[0].ID != id1 || forward[1].ID != id2 {
		t.Errorf("not ordered by invoice number: got %v, want [%d %d]", idsOf(forward), id1, id2)
	}
}

func idsOf(invoices []data_models.InvoicingInvoice) []uint {
	ids := make([]uint, len(invoices))
	for i, inv := range invoices {
		ids[i] = inv.ID
	}
	return ids
}
