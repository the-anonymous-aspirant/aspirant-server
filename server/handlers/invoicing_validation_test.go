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

// #8367-E1: the Excel workbook export.
func TestExport_WorkbookHasFourSheetsAndIsGatedOnValidation(t *testing.T) {
	f, ids := issuedFixture(t)
	f.r.GET("/invoicing/export/workbook.xlsx", ExportWorkbookHandler)

	w := invoicingDo(f.r, "GET", "/invoicing/export/workbook.xlsx", "")
	if w.Code != http.StatusOK || w.Header().Get("Content-Disposition") != `attachment; filename="Rechnungen.xlsx"` {
		t.Fatalf("export: %d %v", w.Code, w.Header())
	}
	files := unzipXLSX(t, w.Body.Bytes())
	if got := strings.Join(sheetNames(t, files), "|"); got != "Invoices|Line items|Payments|Clients" {
		t.Fatalf("sheets: %s", got)
	}
	invoices := readSheet(t, files, "1")
	if len(invoices.Rows) != 1+3 {
		t.Errorf("Invoices rows: %d, want header + 3 numbered invoices", len(invoices.Rows))
	}
	first := invoices.Rows[1].Cells
	if first[0].Inline != "2026-001" || first[1].Inline != "issued" || first[9].Value != "100" || first[9].Type != "" {
		t.Errorf("first invoice row: %+v", first)
	}
	if invoices.Rows[2].Cells[1].Inline != "cancelled" {
		t.Errorf("cancelled invoice row: %+v", invoices.Rows[2].Cells[1])
	}
	if items := readSheet(t, files, "2"); len(items.Rows) != 1+3 {
		t.Errorf("Line items rows: %d", len(items.Rows))
	}
	payments := readSheet(t, files, "3")
	if len(payments.Rows) != 1+len(f.payments) {
		t.Errorf("Payments rows: %d, want header + %d", len(payments.Rows), len(f.payments))
	}
	cats := map[string]bool{}
	for _, r := range payments.Rows[1:] {
		cats[r.Cells[11].Inline] = true
	}
	if !cats["paid_work"] || !cats["gift"] {
		t.Errorf("Payments sheet categories: %v", cats)
	}
	if clients := readSheet(t, files, "4"); len(clients.Rows) != 2 || clients.Rows[1].Cells[0].Inline != "Galerie Nord" {
		t.Errorf("Clients sheet: %+v", clients.Rows)
	}
	events, _ := data_models.ListInvoicingAuditEvents(f.db, "export", 0)
	if len(events) != 1 || !strings.Contains(events[0].Details, "workbook_xlsx") {
		t.Errorf("export audit: %+v", events)
	}

	f.db.Model(&data_models.InvoicingInvoice{}).Where("id = ?", ids[0]).Update("total_cents", 1)
	if w := invoicingDo(f.r, "GET", "/invoicing/export/workbook.xlsx", ""); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "validation_failed") {
		t.Errorf("workbook with a violation: %d", w.Code)
	}
}

// #8367-E2: the Jobcenter bundle.
func TestExport_JobcenterBundle(t *testing.T) {
	f, ids := issuedFixture(t)
	f.r.GET("/invoicing/export/jobcenter-bundle.zip", ExportJobcenterBundleHandler)

	// A bank deposit, two paid-work payments under it, a gift outside it,
	// and paid work not invoiced yet.
	dep := data_models.InvoicingPayment{Source: "paypal", TransactionID: "W1", Kind: "withdrawal", PaidOn: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Currency: "EUR", GrossCents: 30000, Category: "uncategorised"}
	f.db.Create(&dep)
	f.db.Model(&data_models.InvoicingPayment{}).Where("id IN (?)", []uint{f.payments[0].ID, f.payments[2].ID}).Update("bank_deposit_id", dep.ID)
	f.db.Model(&data_models.InvoicingPayment{}).Where("id = ?", f.payments[3].ID).Update("note", `=HYPERLINK("http://x","y")`)
	late := data_models.InvoicingPayment{Source: "paypal", TransactionID: "TX9", Kind: "incoming", PaidOn: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Currency: "EUR", GrossCents: 700, Category: "paid_work", SenderName: "+cmd"}
	f.db.Create(&late)

	w := invoicingDo(f.r, "GET", "/invoicing/export/jobcenter-bundle.zip", "")
	if w.Code != http.StatusOK || w.Header().Get("Content-Disposition") != `attachment; filename="Jobcenter-Nachweise.zip"` {
		t.Fatalf("bundle: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	files := unzipXLSX(t, w.Body.Bytes())

	// Issued invoices 1 and 3 (2 was cancelled), byte for byte as stored.
	for i, id := range []uint{ids[0], ids[2]} {
		inv, _ := data_models.GetInvoicingInvoice(f.db, id)
		stored, _ := data_models.GetInvoicePDF(f.db, id)
		got, ok := files["Rechnung-"+*inv.InvoiceNumber+".pdf"]
		if !ok || !bytes.Equal(got, stored.Content) {
			t.Errorf("PDF %d (%s) missing or altered", i, *inv.InvoiceNumber)
		}
	}
	if _, ok := files["Rechnung-2026-002.pdf"]; ok {
		t.Errorf("the cancelled invoice's PDF is in the bundle")
	}
	if len(files) != 3 {
		t.Errorf("bundle holds %d files, want 2 PDFs + summary", len(files))
	}

	rows := readCSV(t, files["Zusammenfassung.csv"])
	if strings.Join(rows[0], ";") != strings.Join(bundleSummaryHeader, ";") {
		t.Fatalf("summary header: %v", rows[0])
	}
	body := rows[1:]
	if len(body) != 5 {
		t.Fatalf("summary rows: %d, want 5 incoming payments: %v", len(body), body)
	}
	// Under the deposit first: TX1 (2026-001) and TX3 (2026-003).
	if body[0][0] != "2026-08-01" || body[0][1] != "300,00" || body[0][6] != "TX1" || body[0][8] != "2026-001" || body[0][7] != "Bezahlte Arbeit" {
		t.Errorf("first grouped row: %v", body[0])
	}
	if body[1][6] != "TX3" || body[1][8] != "2026-003" {
		t.Errorf("second grouped row: %v", body[1])
	}
	byTxn := map[string][]string{}
	for _, r := range body[2:] {
		if r[0] != "Ohne Bankeinzahlung" {
			t.Errorf("an unassigned payment is not marked: %v", r)
		}
		byTxn[r[6]] = r
	}
	// TX2's invoice was cancelled, so it is paid work without an issued invoice.
	if byTxn["TX2"][8] != bundleNoInvoiceYet || byTxn["TX9"][8] != bundleNoInvoiceYet {
		t.Errorf("uninvoiced paid work: %v / %v", byTxn["TX2"], byTxn["TX9"])
	}
	if byTxn["TX4"][7] != "Geschenk" || byTxn["TX4"][8] != "" {
		t.Errorf("gift row: %v", byTxn["TX4"])
	}
	if byTxn["TX9"][5] != "'+cmd" {
		t.Errorf("sender not neutralised: %q", byTxn["TX9"][5])
	}
	events, _ := data_models.ListInvoicingAuditEvents(f.db, "export", 0)
	if len(events) != 1 || !strings.Contains(events[0].Details, "jobcenter_bundle") {
		t.Errorf("audit: %+v", events)
	}

	f.db.Model(&data_models.InvoicingInvoice{}).Where("id = ?", ids[0]).Update("total_cents", 1)
	if w := invoicingDo(f.r, "GET", "/invoicing/export/jobcenter-bundle.zip", ""); w.Code != http.StatusConflict {
		t.Errorf("bundle with a violation: %d", w.Code)
	}
}
