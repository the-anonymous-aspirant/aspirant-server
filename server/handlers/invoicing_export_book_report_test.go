package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
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
		&data_models.InvoicingExpense{}, &data_models.InvoicingExpenseCategory{},
		&data_models.InvoicingBookSelection{}, &data_models.InvoicingBookPreset{})
	return db
}

func bookReportRouter(db *gorm.DB) *gin.Engine {
	r := setupInvoicingRouter(db, bookReportUser)
	r.GET("/invoicing/export/book-report.csv", ExportBookReportHandler)
	addBookSelectionRoutes(r)
	return r
}

// seedPayment seeds a payment marked Paid work, which Work only (the starting
// selection) counts as income.
func seedPayment(t *testing.T, db *gorm.DB, owner uint, kind, txn, paidOn string, grossCents int64) {
	t.Helper()
	seedPaymentIn(t, db, owner, kind, data_models.PaymentCategoryPaidWork, txn, paidOn, grossCents)
}

func seedPaymentIn(t *testing.T, db *gorm.DB, owner uint, kind, category, txn, paidOn string, grossCents int64) *data_models.InvoicingPayment {
	t.Helper()
	p := data_models.InvoicingPayment{
		OwnerUserID: owner, Source: "manual", TransactionID: txn, Kind: kind, Category: category,
		PaidOn: mustParseDay(t, paidOn), Currency: "EUR", GrossCents: grossCents, Entry: "manual",
	}
	if err := db.Create(&p).Error; err != nil {
		t.Fatalf("seed payment %s: %v", txn, err)
	}
	return &p
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
	if cd := w.Header().Get("Content-Disposition"); cd != `attachment; filename="book-report-work-only-2026-11-06-1049.csv"` {
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

// system_3 #10555-B2 (design §3.143 Decision 7, operator c65431): the report
// counts the same rows as the Book, under her current selection, and its
// file is named after that selection.

func bookReportCSV(t *testing.T, r *gin.Engine) ([][]string, string) {
	t.Helper()
	w := invoicingDo(r, "GET", "/invoicing/export/book-report.csv", "")
	if w.Code != http.StatusOK {
		t.Fatalf("export: %d %s", w.Code, w.Body.String())
	}
	return readClientsCSV(t, w.Body.String()), w.Header().Get("Content-Disposition")
}

func putBookSelection(t *testing.T, r *gin.Engine, body string) {
	t.Helper()
	if code, _, _, raw := bookCall(t, r, "PUT", "/invoicing/book/selection", body); code != http.StatusOK {
		t.Fatalf("put selection %s: %d %s", body, code, raw)
	}
}

func createBookPreset(t *testing.T, r *gin.Engine, body string) string {
	t.Helper()
	code, resp, _, raw := bookCall(t, r, "POST", "/invoicing/book/presets", body)
	if code != http.StatusCreated || resp.Preset.ID == "" {
		t.Fatalf("create preset %s: %d %s", body, code, raw)
	}
	return resp.Preset.ID
}

// seedEveryIncomeCategory seeds one incoming payment per payment category in
// July: Paid work 350, Gift 100, Loan 200, Refund 50, Other 25, Uncategorised 75.
func seedEveryIncomeCategory(t *testing.T, db *gorm.DB) {
	t.Helper()
	for i, p := range []struct {
		cat   string
		cents int64
	}{
		{data_models.PaymentCategoryPaidWork, 35000},
		{data_models.PaymentCategoryGift, 10000},
		{data_models.PaymentCategoryLoan, 20000},
		{data_models.PaymentCategoryRefund, 5000},
		{data_models.PaymentCategoryOther, 2500},
		{data_models.PaymentCategoryUncategorised, 7500},
	} {
		seedPaymentIn(t, db, bookReportUser, "incoming", p.cat, "P"+string(rune('A'+i)), "2026-07-14", p.cents)
	}
}

// The stated change (c65404): before #10555-B2 the report's income was every
// incoming payment (800,00 here). Under Work only it is Paid work alone, and
// ticking every money-in category gives the old figure back.
func TestExportBookReport_WorkOnlyIncomeIsPaidWorkAlone(t *testing.T) {
	db := setupBookReportDB(t)
	r := bookReportRouter(db)
	seedEveryIncomeCategory(t, db)

	rows, _ := bookReportCSV(t, r)
	if got, want := readBookReportRow(rows, "Einnahmen"), []string{"Einnahmen", "350,00", "350,00"}; joinRow(got) != joinRow(want) {
		t.Errorf("Work only Einnahmen: got %q want %q", got, want)
	}

	// Control: with every money-in category ticked, income is the old total.
	putBookSelection(t, r, `{"income":["paid_work","gift","loan","refund","other","uncategorised"],"expense_all":false,"expense":["travel"],"preset":""}`)
	rows, _ = bookReportCSV(t, r)
	if got, want := readBookReportRow(rows, "Einnahmen"), []string{"Einnahmen", "800,00", "800,00"}; joinRow(got) != joinRow(want) {
		t.Errorf("all-income Einnahmen: got %q want %q", got, want)
	}
}

// Ticking a category adds exactly its rows (Gift: +100,00), and the result
// row follows.
func TestExportBookReport_CountsOnlyTheSelectedCategories(t *testing.T) {
	db := setupBookReportDB(t)
	r := bookReportRouter(db)
	seedEveryIncomeCategory(t, db)
	seedExpense(t, db, bookReportUser, "E1", "2026-07-20", "travel", 1000)
	seedExpense(t, db, bookReportUser, "E2", "2026-07-21", "advice", 4000)
	seedExpense(t, db, bookReportUser, "E3", "2026-08-02", "advice", 500)
	seedExpense(t, db, bookReportUser, "E4", "2026-07-22", data_models.ExpenseCategoryPrivate, 777700)

	putBookSelection(t, r, `{"income":["paid_work","gift"],"expense_all":false,"expense":["travel"],"preset":""}`)
	rows, _ := bookReportCSV(t, r)

	// August holds only an uncounted advice expense, so it is not a column.
	if got, want := rows[0], []string{"Kategorie", "2026-07", "Gesamt"}; joinRow(got) != joinRow(want) {
		t.Fatalf("header: got %q want %q", got, want)
	}
	if got, want := readBookReportRow(rows, "Einnahmen"), []string{"Einnahmen", "450,00", "450,00"}; joinRow(got) != joinRow(want) {
		t.Errorf("Einnahmen: got %q want %q", got, want)
	}
	if got, want := readBookReportRow(rows, "travel"), []string{"travel", "10,00", "10,00"}; joinRow(got) != joinRow(want) {
		t.Errorf("travel: got %q want %q", got, want)
	}
	// An expense category she did not tick is not a row of the report.
	if got := readBookReportRow(rows, "advice"); got != nil {
		t.Errorf("an uncounted category is a row: %q", got)
	}
	if got, want := readBookReportRow(rows, "Ausgaben gesamt"), []string{"Ausgaben gesamt", "10,00", "10,00"}; joinRow(got) != joinRow(want) {
		t.Errorf("Ausgaben gesamt: got %q want %q", got, want)
	}
	if got, want := readBookReportRow(rows, "Ergebnis"), []string{"Ergebnis", "440,00", "440,00"}; joinRow(got) != joinRow(want) {
		t.Errorf("Ergebnis: got %q want %q", got, want)
	}
	for _, row := range rows {
		if strings.Contains(strings.ToLower(row[0]), "private") || strings.Contains(joinRow(row), "7777") {
			t.Errorf("a Private expense reached the report: %q", row)
		}
	}
}

// A removed row never counts, even one the selection would count: a removed
// Paid work payment and a removed travel expense are out of every figure.
func TestExportBookReport_RemovedRowsNeverCount(t *testing.T) {
	db := setupBookReportDB(t)
	r := bookReportRouter(db)
	seedPayment(t, db, bookReportUser, "incoming", "P1", "2026-07-14", 35000)
	gone := seedPaymentIn(t, db, bookReportUser, "incoming", data_models.PaymentCategoryPaidWork, "P2", "2026-07-15", 99900)
	seedExpense(t, db, bookReportUser, "E1", "2026-07-20", "travel", 1000)
	seedExpense(t, db, bookReportUser, "E2", "2026-09-01", "travel", 88800)

	// Control: before the removal both count, and September is a column.
	rows, _ := bookReportCSV(t, r)
	if got, want := readBookReportRow(rows, "Einnahmen"), []string{"Einnahmen", "1349,00", "0,00", "1349,00"}; joinRow(got) != joinRow(want) {
		t.Fatalf("control Einnahmen: got %q want %q", got, want)
	}

	now := time.Now()
	if err := db.Model(gone).Update("removed_at", &now).Error; err != nil {
		t.Fatalf("remove payment: %v", err)
	}
	if err := db.Model(&data_models.InvoicingExpense{}).Where("transaction_id = ?", "E2").Update("removed_at", &now).Error; err != nil {
		t.Fatalf("remove expense: %v", err)
	}

	rows, _ = bookReportCSV(t, r)
	if got, want := rows[0], []string{"Kategorie", "2026-07", "Gesamt"}; joinRow(got) != joinRow(want) {
		t.Fatalf("header: got %q want %q", got, want)
	}
	if got, want := readBookReportRow(rows, "Einnahmen"), []string{"Einnahmen", "350,00", "350,00"}; joinRow(got) != joinRow(want) {
		t.Errorf("Einnahmen: got %q want %q", got, want)
	}
	if got, want := readBookReportRow(rows, "travel"), []string{"travel", "10,00", "10,00"}; joinRow(got) != joinRow(want) {
		t.Errorf("travel: got %q want %q", got, want)
	}
}

// The file is named after the selection, as the Book's select names it.
func TestExportBookReport_FileIsNamedAfterTheSelection(t *testing.T) {
	db := setupBookReportDB(t)
	r := bookReportRouter(db)
	withInvoicingClock(t, exportClock)
	name := func() string {
		t.Helper()
		_, cd := bookReportCSV(t, r)
		return cd
	}
	const stamp = "-2026-11-06-1049.csv\""

	if got, want := name(), `attachment; filename="book-report-work-only`+stamp; got != want {
		t.Errorf("built-in: %q want %q", got, want)
	}

	id := createBookPreset(t, r, `{"name":"Jobcenter Süd","income":["paid_work","gift"],"expense_all":false,"expense":["travel"]}`)
	putBookSelection(t, r, `{"income":["paid_work","gift"],"expense_all":false,"expense":["travel"],"preset":"`+id+`"}`)
	if got, want := name(), `attachment; filename="book-report-jobcenter-sued`+stamp; got != want {
		t.Errorf("named preset: %q want %q", got, want)
	}

	// A changed tick: "Jobcenter Süd (changed)".
	putBookSelection(t, r, `{"income":["paid_work"],"expense_all":false,"expense":["travel"],"preset":"`+id+`"}`)
	if got, want := name(), `attachment; filename="book-report-jobcenter-sued-changed`+stamp; got != want {
		t.Errorf("changed preset: %q want %q", got, want)
	}
	putBookSelection(t, r, `{"income":["paid_work","gift"],"expense_all":false,"expense":[],"preset":"work_only"}`)
	if got, want := name(), `attachment; filename="book-report-work-only-changed`+stamp; got != want {
		t.Errorf("changed Work only: %q want %q", got, want)
	}

	// Deleting the preset the selection came from keeps the ticks, under no name.
	putBookSelection(t, r, `{"income":["paid_work","gift"],"expense_all":false,"expense":["travel"],"preset":"`+id+`"}`)
	if code, _, _, raw := bookCall(t, r, "DELETE", "/invoicing/book/presets/"+id, ""); code != http.StatusOK {
		t.Fatalf("delete preset: %d %s", code, raw)
	}
	if got, want := name(), `attachment; filename="book-report-custom`+stamp; got != want {
		t.Errorf("no preset: %q want %q", got, want)
	}

	// A name with no ASCII letter or digit falls back to the preset's id.
	id = createBookPreset(t, r, `{"name":"Работа","income":["paid_work"],"expense_all":false,"expense":["travel"]}`)
	putBookSelection(t, r, `{"income":["paid_work"],"expense_all":false,"expense":["travel"],"preset":"`+id+`"}`)
	if got, want := name(), `attachment; filename="book-report-preset-`+id+stamp; got != want {
		t.Errorf("non-ASCII name: %q want %q", got, want)
	}
}

func TestFileNameSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Work only":              "work-only",
		"Jobcenter":              "jobcenter",
		"  Größe & Ärger 2026! ": "groesse-aerger-2026",
		`a"b\\c;d/e`:             "a-b-c-d-e",
		"Работа":                 "",
		"":                       "",
	} {
		if got := fileNameSlug(in); got != want {
			t.Errorf("fileNameSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExportBookReport_AuditNamesTheSelection(t *testing.T) {
	db := setupBookReportDB(t)
	if _, cd := bookReportCSV(t, bookReportRouter(db)); cd == "" {
		t.Fatal("control: no Content-Disposition")
	}
	var ev data_models.InvoicingAuditEvent
	db.Order("id desc").First(&ev)
	var details map[string]interface{}
	if err := json.Unmarshal([]byte(ev.Details), &details); err != nil {
		t.Fatalf("details: %v (%q)", err, ev.Details)
	}
	if details["selection"] != "work-only" {
		t.Errorf("audit selection: %v", details)
	}
}
