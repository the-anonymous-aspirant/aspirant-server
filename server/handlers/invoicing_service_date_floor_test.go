package handlers

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"aspirant-online/server/data_models"
)

// system_3 #12312 ruling 3: a draft's service dates take a payment date's
// floor, 2000-01-01, on save. A date after today stays allowed on a draft
// (the review's point 1: the ceiling is the issue's).
func TestDraftSave_RefusesAServiceDateBefore2000(t *testing.T) {
	f := newInvoiceFixture(t)
	id := f.draft(t, 0)
	path := fmt.Sprintf("/invoicing/invoices/%d", id)
	stored := func() data_models.InvoicingInvoice {
		var inv data_models.InvoicingInvoice
		if err := f.db.First(&inv, id).Error; err != nil {
			t.Fatalf("read the draft: %v", err)
		}
		return inv
	}
	before := stored()

	for _, tc := range []struct{ name, body, field string }{
		{"from", `{"service_date_from":"0026-11-03"}`, "service_date_from"},
		{"until", `{"service_date_to":"1999-12-31"}`, "service_date_to"},
	} {
		w := invoicingDo(f.r, "PUT", path, tc.body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "service_date_invalid") ||
			!strings.Contains(w.Body.String(), tc.field+" must be 2000-01-01 or later") {
			t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body.String())
		}
		after := stored()
		if !after.ServiceDateFrom.Equal(*before.ServiceDateFrom) || !after.ServiceDateTo.Equal(*before.ServiceDateTo) {
			t.Fatalf("%s: the refused save changed the stored dates: %v %v", tc.name, after.ServiceDateFrom, after.ServiceDateTo)
		}
	}

	// The floor itself, and a day after today, are saved.
	for _, body := range []string{
		`{"service_date_from":"2000-01-01","service_date_to":null}`,
		`{"service_date_from":"2099-03-01","service_date_to":null}`,
	} {
		if w := invoicingDo(f.r, "PUT", path, body); w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
}
