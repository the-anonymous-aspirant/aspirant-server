package handlers

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"aspirant-online/server/data_models"
)

// system_3 #8561 (#8368 item 9 default b): the Excel workbook's Payments
// sheet must not carry a payer's name, email or note for a payment that is
// not paid work.

func TestWorkbookPayments_BlanksPayerDataExceptPaidWork(t *testing.T) {
	db := setupPaymentsDB(t)
	r := setupPaymentsRouter(db, 9)
	r.GET("/invoicing/export/workbook.xlsx", ExportWorkbookHandler)

	for i, cat := range data_models.PaymentCategories {
		p := data_models.InvoicingPayment{
			Source: "paypal", TransactionID: fmt.Sprintf("TX%d", i+1), Kind: data_models.PaymentKindIncoming,
			PaidOn: time.Date(2026, 7, 1+i, 0, 0, 0, 0, time.UTC), Currency: "EUR",
			GrossCents: int64(1000 * (i + 1)),
			SenderName: "Sender-" + cat, SenderEmail: "sender-" + cat + "@example.test", Note: "Note-" + cat,
			Category: cat,
		}
		if err := db.Create(&p).Error; err != nil {
			t.Fatalf("seed %s: %v", cat, err)
		}
	}

	w := invoicingDo(r, "GET", "/invoicing/export/workbook.xlsx", "")
	if w.Code != http.StatusOK {
		t.Fatalf("export: %d %s", w.Code, w.Body.String())
	}
	files := unzipXLSX(t, w.Body.Bytes())
	sheet := readSheet(t, files, "3") // Invoices, Line items, Payments, Clients.
	if len(sheet.Rows) != len(data_models.PaymentCategories)+1 {
		t.Fatalf("payment rows: %d, want %d data rows + header", len(sheet.Rows), len(data_models.PaymentCategories))
	}

	seen := map[string]bool{}
	for _, row := range sheet.Rows[1:] {
		cells := make([]string, len(row.Cells))
		for i, c := range row.Cells {
			cells[i] = c.Inline
		}
		if len(cells) < 12 {
			t.Fatalf("row too short: %v", cells)
		}
		name, email, note, category := cells[4], cells[5], cells[6], cells[11]
		seen[category] = true
		if category == data_models.PaymentCategoryPaidWork {
			if name != "Sender-"+category || email != "sender-"+category+"@example.test" || note != "Note-"+category {
				t.Errorf("paid_work row must keep payer data: name=%q email=%q note=%q", name, email, note)
			}
			continue
		}
		if name != "" || email != "" || note != "" {
			t.Errorf("category %q must blank payer data, got name=%q email=%q note=%q", category, name, email, note)
		}
	}
	for _, cat := range data_models.PaymentCategories {
		if !seen[cat] {
			t.Errorf("no row exported for category %q", cat)
		}
	}
}
