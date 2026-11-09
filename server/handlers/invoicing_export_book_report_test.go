package handlers

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// system_3 #10414-E: the Book tab's income and expenses as a standard
// bookkeeping CSV grid (design_agent ruling, report 886 §2; operator c63757).

const bookReportUser uint = 11

func setupBookReportDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := setupInvoicingDB(t)
	db.AutoMigrate(&data_models.InvoicingPayment{}, &data_models.InvoicingPaymentTombstone{},
		&data_models.InvoicingExpense{}, &data_models.InvoicingExpenseCategory{})
	return db
}

func bookReportRouter(db *gorm.DB) *gin.Engine {
	r := setupInvoicingRouter(db, bookReportUser)
	r.GET("/invoicing/export/book-report.csv", ExportBookReportHandler)
	return r
}

func seedPayment(t *testing.T, db *gorm.DB, owner uint, kind, txn, paidOn string, grossCents int64) {
	t.Helper()
	p := data_models.InvoicingPayment{
		OwnerUserID: owner, Source: "manual", TransactionID: txn, Kind: kind,
		PaidOn: mustParseDay(t, paidOn), Currency: "EUR", GrossCents: grossCents, Entry: "manual",
	}
	if err := db.Create(&p).Error; err != nil {
		t.Fatalf("seed payment %s: %v", txn, err)
	}
}

func seedExpense(t *testing.T, db *gorm.DB, owner uint, txn, paidOn, category string, amountCents int64) {
	t.Helper()
	e := data_models.InvoicingExpense{
		OwnerUserID: owner, Source: "manual", TransactionID: txn, PaidOn: mustParseDay(t, paidOn),
		Currency: "EUR", AmountCents: amountCents, Category: category, Entry: "manual",
	}
	if err := db.Create(&e).Error; err != nil {
		t.Fatalf("seed expense %s: %v", txn, err)
	}
}

func mustParseDay(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("parse day %q: %v", s, err)
	}
	return d
}

func readBookReportRow(rows [][]string, label string) []string {
	for _, r := range rows {
		if len(r) > 0 && r[0] == label {
			return r
		}
	}
	return nil
}

func TestExportBookReport_GridShapeGroupedByMonthWithTotals(t *testing.T) {
	db := setupBookReportDB(t)
	seedPayment(t, db, bookReportUser, "incoming", "P1", "2026-07-14", 35000)
	seedExpense(t, db, bookReportUser, "E1", "2026-07-20", "materials and supplies", 4599)
	seedExpense(t, db, bookReportUser, "E2", "2026-08-02", "travel", 1000)

	withInvoicingClock(t, exportClock)
	w := invoicingDo(bookReportRouter(db), "GET", "/invoicing/export/book-report.csv", "")
	if w.Code != http.StatusOK {
		t.Fatalf("export: %d %s", w.Code, w.Body.String())
	}
	if cd := w.Header().Get("Content-Disposition"); cd != `attachment; filename="Buchhaltung-2026-11-06-1049.csv"` {
		t.Errorf("disposition %q", cd)
	}
	rows := readClientsCSV(t, w.Body.String()) // same BOM+';' CSV shape, reused reader

	if got, want := rows[0], []string{"Kategorie", "2026-07", "2026-08", "Gesamt"}; joinRow(got) != joinRow(want) {
		t.Fatalf("header: got %q want %q", got, want)
	}
	if got, want := readBookReportRow(rows, "Einnahmen"), []string{"Einnahmen", "350,00", "0,00", "350,00"}; joinRow(got) != joinRow(want) {
		t.Errorf("Einnahmen: got %q want %q", got, want)
	}
	if got, want := readBookReportRow(rows, "materials and supplies"), []string{"materials and supplies", "45,99", "0,00", "45,99"}; joinRow(got) != joinRow(want) {
		t.Errorf("materials row: got %q want %q", got, want)
	}
	if got, want := readBookReportRow(rows, "travel"), []string{"travel", "0,00", "10,00", "10,00"}; joinRow(got) != joinRow(want) {
		t.Errorf("travel row: got %q want %q", got, want)
	}
	// A starter category with no entries at all still gets its own all-zero row.
	if got, want := readBookReportRow(rows, "advice"), []string{"advice", "0,00", "0,00", "0,00"}; joinRow(got) != joinRow(want) {
		t.Errorf("advice row: got %q want %q", got, want)
	}
	if got, want := readBookReportRow(rows, "Ausgaben gesamt"), []string{"Ausgaben gesamt", "45,99", "10,00", "55,99"}; joinRow(got) != joinRow(want) {
		t.Errorf("Ausgaben gesamt: got %q want %q", got, want)
	}
	if got, want := readBookReportRow(rows, "Ergebnis"), []string{"Ergebnis", "304,01", "-10,00", "294,01"}; joinRow(got) != joinRow(want) {
		t.Errorf("Ergebnis: got %q want %q", got, want)
	}
}

func joinRow(r []string) string {
	s := ""
	for i, c := range r {
		if i > 0 {
			s += "|"
		}
		s += c
	}
	return s
}

// Matches the Book tab's own definition (#10414-C, BookPanel.vue): only
// kind=incoming counts as income, so a withdrawal never shows as money in.
func TestExportBookReport_WithdrawalsAreNotIncome(t *testing.T) {
	db := setupBookReportDB(t)
	seedPayment(t, db, bookReportUser, "incoming", "P1", "2026-07-14", 35000)
	seedPayment(t, db, bookReportUser, "withdrawal", "P2", "2026-07-15", 100000)

	rows := readClientsCSV(t, invoicingDo(bookReportRouter(db), "GET", "/invoicing/export/book-report.csv", "").Body.String())
	if got, want := readBookReportRow(rows, "Einnahmen"), []string{"Einnahmen", "350,00", "350,00"}; joinRow(got) != joinRow(want) {
		t.Errorf("Einnahmen: got %q want %q", got, want)
	}
}

// @security: a user-added category name is neutralised the same way every
// other export cell is (spreadsheetSafe, c51455 on #8370).
func TestExportBookReport_FormulaLookingCategoryIsNeutralised(t *testing.T) {
	db := setupBookReportDB(t)
	if err := db.Create(&data_models.InvoicingExpenseCategory{
		OwnerUserID: bookReportUser, Name: `=HYPERLINK("http://evil.example","x")`, CountsAs: "other",
	}).Error; err != nil {
		t.Fatalf("seed category: %v", err)
	}
	seedExpense(t, db, bookReportUser, "E1", "2026-07-20", `=HYPERLINK("http://evil.example","x")`, 500)

	rows := readClientsCSV(t, invoicingDo(bookReportRouter(db), "GET", "/invoicing/export/book-report.csv", "").Body.String())
	row := readBookReportRow(rows, `'=HYPERLINK("http://evil.example","x")`)
	if row == nil {
		t.Fatalf("neutralised category row not found: %q", rows)
	}
}

func TestExportBookReport_AuditsTheExportWithMonthCount(t *testing.T) {
	db := setupBookReportDB(t)
	seedPayment(t, db, bookReportUser, "incoming", "P1", "2026-07-14", 35000)
	seedExpense(t, db, bookReportUser, "E1", "2026-08-02", "travel", 1000)
	before := countAuditEvents(t, db)

	if w := invoicingDo(bookReportRouter(db), "GET", "/invoicing/export/book-report.csv", ""); w.Code != http.StatusOK {
		t.Fatalf("export: %d", w.Code)
	}
	if n := countAuditEvents(t, db); n != before+1 {
		t.Fatalf("audit rows: %d, want %d", n, before+1)
	}
	var ev data_models.InvoicingAuditEvent
	db.Order("id desc").First(&ev)
	var details map[string]interface{}
	if err := json.Unmarshal([]byte(ev.Details), &details); err != nil {
		t.Fatalf("details: %v (%q)", err, ev.Details)
	}
	if ev.Action != invoicingActionExport || ev.EntityType != invoicingEntityExport || details["kind"] != "book_report_csv" || details["months"] != float64(2) {
		t.Errorf("audit row: action %q entity %q details %v", ev.Action, ev.EntityType, details)
	}
}

// With nothing in the book, the grid still has one row per starter category
// (and Einnahmen / Ausgaben gesamt / Ergebnis), all zero, no month columns.
func TestExportBookReport_EmptyBookIsAllZeroNoMonths(t *testing.T) {
	db := setupBookReportDB(t)
	rows := readClientsCSV(t, invoicingDo(bookReportRouter(db), "GET", "/invoicing/export/book-report.csv", "").Body.String())
	if got, want := rows[0], []string{"Kategorie", "Gesamt"}; joinRow(got) != joinRow(want) {
		t.Fatalf("header: got %q want %q", got, want)
	}
	if got, want := readBookReportRow(rows, "Einnahmen"), []string{"Einnahmen", "0,00"}; joinRow(got) != joinRow(want) {
		t.Errorf("Einnahmen: got %q want %q", got, want)
	}
	if got, want := readBookReportRow(rows, "Ergebnis"), []string{"Ergebnis", "0,00"}; joinRow(got) != joinRow(want) {
		t.Errorf("Ergebnis: got %q want %q", got, want)
	}
}

// Another owner's rows never reach this owner's report.
func TestExportBookReport_OwnerIsolation(t *testing.T) {
	db := setupBookReportDB(t)
	const other uint = 99
	seedPayment(t, db, bookReportUser, "incoming", "P1", "2026-07-14", 35000)
	seedPayment(t, db, other, "incoming", "P2", "2026-07-14", 999900)
	seedExpense(t, db, bookReportUser, "E1", "2026-07-20", "travel", 1000)
	seedExpense(t, db, other, "E2", "2026-07-20", "travel", 888800)

	rows := readClientsCSV(t, invoicingDo(bookReportRouter(db), "GET", "/invoicing/export/book-report.csv", "").Body.String())
	if got, want := readBookReportRow(rows, "Einnahmen"), []string{"Einnahmen", "350,00", "350,00"}; joinRow(got) != joinRow(want) {
		t.Errorf("Einnahmen leaked another owner's payment: got %q want %q", got, want)
	}
	if got, want := readBookReportRow(rows, "travel"), []string{"travel", "10,00", "10,00"}; joinRow(got) != joinRow(want) {
		t.Errorf("travel leaked another owner's expense: got %q want %q", got, want)
	}
}
