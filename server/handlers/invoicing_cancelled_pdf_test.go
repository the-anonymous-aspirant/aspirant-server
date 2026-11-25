package handlers

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"aspirant-online/server/data_models"
)

// A cancelled invoice never leaves the app looking valid (system_3 #12267,
// the #11498 review's C3-12): its single download is the stored PDF with
// "STORNIERT am <Berlin day>" across every page, and the stored bytes stay as
// they were issued.

// cancelledLateEvening issues an invoice and cancels it at 23:30 UTC, which is
// 01:30 the next day in Berlin (summer time).
func cancelledLateEvening(t *testing.T) (*invoiceFixture, uint, data_models.InvoicingInvoicePDF) {
	t.Helper()
	f := newInvoiceFixture(t)
	id := f.draft(t, 0)
	if code, _, body := f.issue(id); code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, body)
	}
	var stored data_models.InvoicingInvoicePDF
	if err := f.db.Where("invoice_id = ?", id).First(&stored).Error; err != nil {
		t.Fatalf("stored PDF: %v", err)
	}
	issued := invoicingDo(f.r, "GET", fmt.Sprintf("/invoicing/invoices/%d/pdf", id), "")
	if issued.Code != http.StatusOK || !bytes.Equal(issued.Body.Bytes(), stored.Content) ||
		issued.Header().Get("X-Content-SHA256") != stored.SHA256 ||
		issued.Header().Get("Content-Disposition") != `attachment; filename="Rechnung-2026-001.pdf"` {
		t.Fatalf("an issued invoice's download is not its stored PDF: %d %v", issued.Code, issued.Header())
	}
	withInvoicingClock(t, time.Date(2026, 10, 21, 23, 30, 0, 0, time.UTC))
	if w := invoicingDo(f.r, "POST", fmt.Sprintf("/invoicing/invoices/%d/cancel", id), `{"reason":"wrong amount"}`); w.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	return f, id, stored
}

func rawPDFText(t *testing.T, pdf []byte) string {
	t.Helper()
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Fatalf("pdftotext is not installed; this test reads the marked PDF (the verify surface installs poppler-utils)")
	}
	// -raw keeps the order the page draws in; the slanted mark's letters come
	// out spaced apart, so spaces are taken out.
	cmd := exec.Command("pdftotext", "-raw", "-", "-")
	cmd.Stdin = bytes.NewReader(pdf)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("pdftotext: %v", err)
	}
	return strings.Join(strings.Fields(string(out)), "")
}

func TestCancelledInvoicePDF_IsMarkedWithTheBerlinDayAndNamedSo(t *testing.T) {
	f, id, stored := cancelledLateEvening(t)

	w := invoicingDo(f.r, "GET", fmt.Sprintf("/invoicing/invoices/%d/pdf", id), "")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/pdf" {
		t.Fatalf("cancelled download: %d %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Disposition"); got != `attachment; filename="Rechnung-2026-001-storniert.pdf"` {
		t.Errorf("file name: %s", got)
	}
	if got := w.Header().Get("X-Content-SHA256"); got != "" {
		t.Errorf("the marked answer names a hash: %s", got)
	}
	if bytes.Equal(w.Body.Bytes(), stored.Content) {
		t.Fatalf("the cancelled download is the unmarked stored PDF")
	}
	text := rawPDFText(t, w.Body.Bytes())
	if !strings.Contains(text, "STORNIERTam22.10.2026") {
		t.Errorf("no mark with the Berlin day 22.10.2026: %s", text)
	}
	if !strings.Contains(text, "Rechnungsnummer:2026-001") {
		t.Errorf("the invoice under the mark is gone: %s", text)
	}

	var after data_models.InvoicingInvoicePDF
	f.db.Where("invoice_id = ?", id).First(&after)
	sum := sha256.Sum256(after.Content)
	if !bytes.Equal(after.Content, stored.Content) || after.SHA256 != stored.SHA256 || hex.EncodeToString(sum[:]) != stored.SHA256 {
		t.Errorf("the stored PDF changed")
	}
}

func TestCancelledInvoicePDF_RefusesWhenTheMarkCannotBeAdded(t *testing.T) {
	f, id, _ := cancelledLateEvening(t)
	// A stored file the mark cannot be drawn on.
	f.db.Model(&data_models.InvoicingInvoicePDF{}).Where("invoice_id = ?", id).Update("content", []byte("not a pdf"))

	w := invoicingDo(f.r, "GET", fmt.Sprintf("/invoicing/invoices/%d/pdf", id), "")
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), `"cancelled_mark_failed"`) {
		t.Fatalf("want 500 cancelled_mark_failed, got %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "not a pdf") || w.Header().Get("Content-Disposition") != "" {
		t.Errorf("the refusal served the stored file: %v %s", w.Header(), w.Body.String())
	}
}

func TestCancelledInvoiceCSV_StorniertAmIsTheBerlinDay(t *testing.T) {
	f, _, _ := cancelledLateEvening(t)
	csv := invoicingDo(f.r, "GET", "/invoicing/export/invoices.csv", "").Body.String()
	var row string
	for _, line := range strings.Split(csv, "\n") {
		if strings.HasPrefix(line, "2026-001;") {
			row = line
		}
	}
	if !strings.Contains(row, ";2026-10-22;wrong amount") {
		t.Errorf("Storniert am is not the Berlin day 2026-10-22: %s", row)
	}
}
