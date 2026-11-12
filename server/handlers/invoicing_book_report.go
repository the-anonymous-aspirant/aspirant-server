package handlers

import (
	"sort"
	"strings"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// ExportBookReportHandler exports the Book tab's income and expenses as a
// standard bookkeeping grid (system_3 #10414-E; design_agent ruling, report
// 886 §2: "a grid with categories down the side, months across the top,
// totals, and the result"; operator c63757: treat it as standard bookkeeping,
// not a Jobcenter-specific form). Rows: one Income row, one row per counted
// expense category (including any the owner added), a total-expenses row, and
// a result row. Columns: one per calendar month that actually has a counted
// entry, oldest first, plus a Total column. No validation gate
// (ExportClientsCSVHandler's own precedent): a bookkeeping report has no
// invoice-numbering integrity to check.
//
// It counts the same rows as the Book, under her current selection (system_3
// #10555-B2, design §3.143 Decision 7, operator c65431): an incoming payment
// counts when its category is in the selection's money in, an expense when
// its category is in its money out, and Private never. Removed rows never
// count (the list reads leave them out). So under Work only, income no longer
// includes payments marked Gift, Loan, Refund, Other or Uncategorised: a
// stated change to the report (c65404). The file is named after the
// selection, e.g. book-report-work-only-2026-11-11-1430.csv.
func ExportBookReportHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	owner := invoicingUserID(c)

	selection, presets, err := readBookSelection(db, owner)
	if err != nil {
		respondInvoicingInternal(c, nil, "exporting the book report", err)
		return
	}
	counting := selection.BookCounting

	allPayments, err := data_models.ListInvoicingPayments(db, owner, "incoming", "")
	if err != nil {
		respondInvoicingInternal(c, nil, "exporting the book report", err)
		return
	}
	payments := make([]data_models.InvoicingPayment, 0, len(allPayments))
	for _, p := range allPayments {
		if bookCountsIncome(counting, p.Category) {
			payments = append(payments, p)
		}
	}
	allExpenses, err := data_models.ListInvoicingExpenses(db, owner, "")
	if err != nil {
		respondInvoicingInternal(c, nil, "exporting the book report", err)
		return
	}
	expenses := make([]data_models.InvoicingExpense, 0, len(allExpenses))
	for _, e := range allExpenses {
		if bookCountsExpense(counting, e.Category) {
			expenses = append(expenses, e)
		}
	}
	categories, err := data_models.ListInvoicingExpenseCategories(db, owner)
	if err != nil {
		respondInvoicingInternal(c, nil, "exporting the book report", err)
		return
	}

	months := bookReportMonths(payments, expenses)

	incomeByMonth := map[string]int64{}
	for _, p := range payments {
		incomeByMonth[p.PaidOn.Format("2006-01")] += p.GrossCents
	}
	expenseByCategoryMonth := map[string]map[string]int64{}
	for _, e := range expenses {
		m := e.PaidOn.Format("2006-01")
		if expenseByCategoryMonth[e.Category] == nil {
			expenseByCategoryMonth[e.Category] = map[string]int64{}
		}
		expenseByCategoryMonth[e.Category][m] += e.AmountCents
	}

	header := append([]string{"Kategorie"}, months...)
	header = append(header, "Gesamt")

	rows := [][]string{bookReportRow("Einnahmen", months, incomeByMonth)}

	totalExpenseByMonth := map[string]int64{}
	for _, cat := range categories {
		if !bookCountsExpense(counting, cat.Name) {
			continue
		}
		byMonth := expenseByCategoryMonth[cat.Name]
		rows = append(rows, bookReportRow(cat.Name, months, byMonth))
		for _, m := range months {
			totalExpenseByMonth[m] += byMonth[m]
		}
	}
	rows = append(rows, bookReportRow("Ausgaben gesamt", months, totalExpenseByMonth))

	resultByMonth := map[string]int64{}
	for _, m := range months {
		resultByMonth[m] = incomeByMonth[m] - totalExpenseByMonth[m]
	}
	rows = append(rows, bookReportRow("Ergebnis", months, resultByMonth))

	content, err := writeCSV(header, rows)
	if err != nil {
		respondInvoicingInternal(c, nil, "exporting the book report", err)
		return
	}
	name := bookSelectionFileName(selection, presets)
	serveCSV(c, db, exportFilename("book-report-"+name, "csv"), content,
		gin.H{"kind": "book_report_csv", "months": len(months), "selection": name})
}

// bookCountsIncome reports whether an incoming payment of this category
// counts under the selection.
func bookCountsIncome(b data_models.BookCounting, category string) bool {
	for _, c := range b.Income {
		if c == category {
			return true
		}
	}
	return false
}

// bookCountsExpense reports whether an expense of this category counts under
// the selection. Private never counts, whatever the selection says.
func bookCountsExpense(b data_models.BookCounting, category string) bool {
	if strings.EqualFold(category, data_models.ExpenseCategoryPrivate) {
		return false
	}
	if b.ExpenseAll {
		return true
	}
	for _, c := range b.Expense {
		if c == category {
			return true
		}
	}
	return false
}

// bookSelectionFileName names the selection for the report's file name, the
// way the Book's select names it (§3.143 Decisions 7 and 8): "work-only", her
// preset's name ("jobcenter"), either with "-changed" when the ticks differ
// from it, or "custom" when the selection names no preset (she deleted the
// one it came from). The result is lowercase ASCII letters, digits and '-'
// only, so it is safe inside the Content-Disposition header's quotes.
func bookSelectionFileName(s bookSelectionView, presets []bookPresetView) string {
	name := ""
	for _, p := range presets {
		if p.ID == s.Preset {
			name = fileNameSlug(p.Name)
			if name == "" {
				name = "preset-" + p.ID
			}
		}
	}
	if name == "" {
		return "custom"
	}
	if s.Changed {
		name += "-changed"
	}
	return name
}

var fileNameTransliterations = strings.NewReplacer("ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss")

// fileNameSlug lowercases name, spells out German umlauts and ß, and turns
// every run of anything else that is not an ASCII letter or digit into one
// '-', trimmed at both ends. It returns "" when nothing is left.
func fileNameSlug(name string) string {
	s := fileNameTransliterations.Replace(strings.ToLower(name))
	var b strings.Builder
	dash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
			continue
		}
		dash = true
	}
	return b.String()
}

// bookReportMonths is every calendar month ("2006-01") that has an incoming
// payment or an expense, oldest first — the same set of months the Book
// tab's own grouping shows (BookPanel.vue's months computed), not a
// continuous zero-filled range: a month with nothing in it is left out
// rather than printed as a column of zeroes.
func bookReportMonths(payments []data_models.InvoicingPayment, expenses []data_models.InvoicingExpense) []string {
	seen := map[string]bool{}
	for _, p := range payments {
		seen[p.PaidOn.Format("2006-01")] = true
	}
	for _, e := range expenses {
		seen[e.PaidOn.Format("2006-01")] = true
	}
	months := make([]string, 0, len(seen))
	for m := range seen {
		months = append(months, m)
	}
	sort.Strings(months)
	return months
}

func bookReportRow(label string, months []string, byMonth map[string]int64) []string {
	row := make([]string, 0, len(months)+2)
	row = append(row, spreadsheetSafe(label))
	var total int64
	for _, m := range months {
		cents := byMonth[m]
		total += cents
		row = append(row, csvMoney(cents))
	}
	row = append(row, csvMoney(total))
	return row
}
