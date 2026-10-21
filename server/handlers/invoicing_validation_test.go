package handlers

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"aspirant-online/server/data_models"
)

// #8367-C2: the §5 validation report and the CSV exports gated on it. The
// API refuses to create any violation, so each check is exercised by writing
// one directly to the database after a clean issue.

func (f *invoiceFixture) validation(t *testing.T) validationReport {
	t.Helper()
	w := invoicingDo(f.r, "GET", "/invoicing/validation", "")
	if w.Code != http.StatusOK {
		t.Fatalf("validation: %d %s", w.Code, w.Body.String())
	}
	var rep validationReport
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return rep
}

func codes(items []validationItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Code)
	}
	return out
}

// issuedFixture issues three invoices and cancels the middle one: the clean
// baseline every check runs against.
func issuedFixture(t *testing.T) (*invoiceFixture, []uint) {
	t.Helper()
	f := newInvoiceFixture(t)
	var ids []uint
	for i := 0; i < 3; i++ {
		id := f.draft(t, i)
		if code, _, body := f.issue(id); code != http.StatusOK {
			t.Fatalf("issue %d: %d %s", i, code, body)
		}
		ids = append(ids, id)
	}
	invoicingDo(f.r, "POST", fmt.Sprintf("/invoicing/invoices/%d/cancel", ids[1]), `{"reason":"Storno"}`)
	return f, ids
}

func TestValidation_CleanDataIsOK(t *testing.T) {
	f, _ := issuedFixture(t)
	rep := f.validation(t)
	if !rep.OK || len(rep.Violations) != 0 || len(rep.Warnings) != 0 {
		t.Fatalf("a clean history must validate: %+v", rep)
	}
}

func TestValidation_EachCheckFires(t *testing.T) {
	cases := []struct {
		code  string
		seed  func(f *invoiceFixture, ids []uint)
		warns bool
	}{
		{"number_gap", func(f *invoiceFixture, ids []uint) {
			f.db.Model(&data_models.InvoicingInvoice{}).Where("id = ?", ids[2]).Updates(map[string]interface{}{"number": 5, "invoice_number": "2026-005"})
		}, false},
		{"number_order", func(f *invoiceFixture, ids []uint) {
			f.db.Model(&data_models.InvoicingInvoice{}).Where("id = ?", ids[2]).Update("issue_date", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
		}, false},
		{"issue_date_future", func(f *invoiceFixture, ids []uint) {
			for _, id := range ids {
				f.db.Model(&data_models.InvoicingInvoice{}).Where("id = ?", id).Update("issue_date", time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
			}
		}, false},
		{"service_after_issue", func(f *invoiceFixture, ids []uint) {
			f.db.Model(&data_models.InvoicingInvoice{}).Where("id = ?", ids[0]).Update("service_date_to", time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC))
		}, false},
		{"total_mismatch_items", func(f *invoiceFixture, ids []uint) {
			f.db.Model(&data_models.InvoicingInvoice{}).Where("id = ?", ids[0]).Update("total_cents", 1)
		}, false},
		{"payment_not_paid_work", func(f *invoiceFixture, ids []uint) {
			f.db.Model(&data_models.InvoicingPayment{}).Where("id = ?", f.payments[0].ID).Update("category", "gift")
		}, false},
		{"payment_multiple_invoices", func(f *invoiceFixture, ids []uint) {
			f.db.Create(&data_models.InvoicingInvoicePayment{InvoiceID: ids[2], PaymentID: f.payments[0].ID})
		}, false},
		{"pdf_missing", func(f *invoiceFixture, ids []uint) {
			f.db.Where("invoice_id = ?", ids[0]).Delete(&data_models.InvoicingInvoicePDF{})
		}, false},
		{"total_mismatch_payments", func(f *invoiceFixture, ids []uint) {
			f.db.Model(&data_models.InvoicingPayment{}).Where("id = ?", f.payments[0].ID).Update("gross_cents", 12345)
		}, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.code, func(t *testing.T) {
			f, ids := issuedFixture(t)
			tc.seed(f, ids)
			rep := f.validation(t)
			list, other := rep.Violations, rep.Warnings
			if tc.warns {
				list, other = rep.Warnings, rep.Violations
				if !rep.OK {
					t.Errorf("a warning alone must leave ok=true: %+v", rep)
				}
			} else if rep.OK {
				t.Errorf("ok=true with a seeded %s", tc.code)
			}
			found := false
			for _, c := range codes(list) {
				found = found || c == tc.code
			}
			if !found {
				t.Fatalf("%s did not fire: violations %v warnings %v", tc.code, codes(rep.Violations), codes(rep.Warnings))
			}
			if tc.code != "payment_multiple_invoices" && tc.code != "total_mismatch_items" && len(other) != 0 {
				t.Errorf("unrelated findings: %v", codes(other))
			}
		})
	}
}

func TestValidation_SequenceStartingAboveOneIsNotAGap(t *testing.T) {
	f := newInvoiceFixture(t)
	invoicingDo(f.r, "PUT", "/invoicing/profile", strings.Replace(validProfileBody, `"next_invoice_number":1`, `"next_invoice_number":7`, 1))
	for i := 0; i < 2; i++ {
		f.issue(f.draft(t, i))
	}
	if rep := f.validation(t); !rep.OK {
		t.Fatalf("a sequence continuing at 7 flagged: %+v", rep)
	}
}

func readCSV(t *testing.T, body []byte) [][]string {
	t.Helper()
	if !bytes.HasPrefix(body, []byte("\xef\xbb\xbf")) {
		t.Fatalf("no UTF-8 BOM")
	}
	r := csv.NewReader(bytes.NewReader(body[3:]))
	r.Comma = ';'
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatalf("csv: %v", err)
	}
	return rows
}

func TestExport_AllInvoicesCSV(t *testing.T) {
	f, _ := issuedFixture(t)
	w := invoicingDo(f.r, "GET", "/invoicing/export/invoices.csv", "")
	if w.Code != http.StatusOK || w.Header().Get("Content-Disposition") != `attachment; filename="Rechnungen.csv"` {
		t.Fatalf("export: %d %v", w.Code, w.Header())
	}
	rows := readCSV(t, w.Body.Bytes())
	if strings.Join(rows[0], ";") != strings.Join(invoicesCSVHeader, ";") {
		t.Errorf("header: %v", rows[0])
	}
	if len(rows) != 4 {
		t.Fatalf("rows: %d, want header + 3", len(rows))
	}
	first := rows[1]
	if first[0] != "2026-001" || first[1] != "issued" || first[2] != "2026-10-21" || first[5] != "Galerie Nord" ||
		first[6] != "Nordweg 2, 13353 Berlin" || first[9] != "100,00" || first[10] != "EUR" || first[11] != "TX1" {
		t.Errorf("first row: %v", first)
	}
	if rows[2][1] != "cancelled" || rows[2][13] != "Storno" || rows[2][12] == "" {
		t.Errorf("cancelled row: %v", rows[2])
	}
	events, _ := data_models.ListInvoicingAuditEvents(f.db, "export", 0)
	if len(events) != 1 || events[0].Action != "export" {
		t.Errorf("export audit: %+v", events)
	}
}

func TestExport_SingleInvoiceCSVHasOneRowPerItem(t *testing.T) {
	f := newInvoiceFixture(t)
	id := f.draft(t, 0)
	invoicingDo(f.r, "PUT", fmt.Sprintf("/invoicing/invoices/%d", id), `{"line_items":[{"description":"Wandbild","quantity_milli":2500,"unit_price_cents":2000},{"description":"Material","quantity_milli":1000,"unit_price_cents":5000}]}`)
	if w := invoicingDo(f.r, "GET", fmt.Sprintf("/invoicing/invoices/%d/export.csv", id), ""); w.Code != http.StatusConflict {
		t.Errorf("a draft exported: %d", w.Code)
	}
	f.issue(id)
	w := invoicingDo(f.r, "GET", fmt.Sprintf("/invoicing/invoices/%d/export.csv", id), "")
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Disposition"), "Rechnung-2026-001.csv") {
		t.Fatalf("export: %d %v", w.Code, w.Header())
	}
	rows := readCSV(t, w.Body.Bytes())
	if len(rows) != 3 || rows[1][8] != "Wandbild" || rows[1][9] != "2,5" || rows[1][10] != "20,00" || rows[1][11] != "50,00" || rows[2][8] != "Material" {
		t.Fatalf("line rows: %v", rows)
	}
}

func TestExport_ViolationsBlockTheExportWarningsDoNot(t *testing.T) {
	f, ids := issuedFixture(t)
	f.db.Model(&data_models.InvoicingPayment{}).Where("id = ?", f.payments[0].ID).Update("gross_cents", 12345)
	if w := invoicingDo(f.r, "GET", "/invoicing/export/invoices.csv", ""); w.Code != http.StatusOK {
		t.Fatalf("a warning blocked the export: %d", w.Code)
	}
	f.db.Model(&data_models.InvoicingInvoice{}).Where("id = ?", ids[0]).Update("total_cents", 1)
	auditBefore := countAuditEvents(t, f.db)
	for _, path := range []string{"/invoicing/export/invoices.csv", fmt.Sprintf("/invoicing/invoices/%d/export.csv", ids[2])} {
		w := invoicingDo(f.r, "GET", path, "")
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "validation_failed") || !strings.Contains(w.Body.String(), "total_mismatch_items") {
			t.Errorf("%s with a violation: %d %s", path, w.Code, w.Body.String())
		}
	}
	if countAuditEvents(t, f.db) != auditBefore {
		t.Errorf("a refused export was audited as an export")
	}
}

// @security condition (c51455 on #8370): payer-written text never reaches a
// spreadsheet as a formula.
func TestExport_FormulaInjectionIsNeutralised(t *testing.T) {
	f := newInvoiceFixture(t)
	f.db.Model(&data_models.InvoicingClient{}).Where("id = ?", f.clientID).Updates(map[string]interface{}{"name": "+cmd|' /C calc'!A0", "address": "@SUM(A1)\nBerlin"})
	f.db.Model(&data_models.InvoicingPayment{}).Where("id = ?", f.payments[0].ID).Update("note", `=HYPERLINK("http://x","y")`)
	id := f.draft(t, 0)
	invoicingDo(f.r, "PUT", fmt.Sprintf("/invoicing/invoices/%d", id), `{"line_items":[{"description":"=HYPERLINK(\"http://x\",\"y\")","quantity_milli":1000,"unit_price_cents":10000}]}`)
	if code, _, body := f.issue(id); code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, body)
	}
	w := invoicingDo(f.r, "GET", fmt.Sprintf("/invoicing/invoices/%d/export.csv", id), "")
	rows := readCSV(t, w.Body.Bytes())
	if rows[1][5] != "'+cmd|' /C calc'!A0" || !strings.HasPrefix(rows[1][6], "'@SUM(A1)") || rows[1][8] != `'=HYPERLINK("http://x","y")` {
		t.Fatalf("unneutralised cells: name %q address %q description %q", rows[1][5], rows[1][6], rows[1][8])
	}
}

func TestSpreadsheetSafe(t *testing.T) {
	cases := map[string]string{
		"=1+1": "'=1+1", "+49 30": "'+49 30", "-5 % Rabatt": "'-5 % Rabatt", "@SUM(A1)": "'@SUM(A1)",
		"\tx": "'\tx", "\rx": "'\rx", "Wandbild": "Wandbild", "": "", "a=b": "a=b", "2026-001": "2026-001",
	}
	for in, want := range cases {
		if got := spreadsheetSafe(in); got != want {
			t.Errorf("spreadsheetSafe(%q) = %q, want %q", in, got, want)
		}
	}
}

// @security N5: an issue whose printed text has a character the font lacks
// is refused with the characters named, and uses no number.
func TestIssue_UnprintableCharactersAreRefused(t *testing.T) {
	f := newInvoiceFixture(t)
	f.db.Model(&data_models.InvoicingClient{}).Where("id = ?", f.clientID).Update("name", "王小明 Studio")
	id := f.draft(t, 0)
	code, _, body := f.issue(id)
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, "unprintable_characters") || !strings.Contains(body, "王小明") {
		t.Fatalf("got %d %s", code, body)
	}
	if f.profileNext(t) != 1 {
		t.Errorf("a refused issue used a number")
	}
	var n int
	f.db.Model(&data_models.InvoicingInvoicePDF{}).Count(&n)
	if n != 0 {
		t.Errorf("a refused issue stored a PDF")
	}
	// The same client spelt in Latin script issues fine.
	f.db.Model(&data_models.InvoicingClient{}).Where("id = ?", f.clientID).Update("name", "Wang Xiaoming Studio")
	if code, _, body := f.issue(id); code != http.StatusOK {
		t.Fatalf("after the fix: %d %s", code, body)
	}
}
