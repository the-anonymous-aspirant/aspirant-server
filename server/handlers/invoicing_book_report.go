package handlers

import (
	"sort"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// ExportBookReportHandler exports the Book tab's income and expenses as a
// standard bookkeeping grid (system_3 #10414-E; design_agent ruling, report
// 886 §2: "a grid with categories down the side, months across the top,
// totals, and the result"; operator c63757: treat it as standard bookkeeping,
// not a Jobcenter-specific form). Rows: one Income row (every kind=incoming
// payment, matching the Book tab's own definition, #10414-C), one row per
// expense category (including any the owner added), a total-expenses row,
// and a result row. Columns: one per calendar month that actually has an
// entry, oldest first, plus a Total column. No validation gate
// (ExportClientsCSVHandler's own precedent): a bookkeeping report has no
// invoice-numbering integrity to check.
func ExportBookReportHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	owner := invoicingUserID(c)

	payments, err := data_models.ListInvoicingPayments(db, owner, "incoming", "")
	if err != nil {
		respondInvoicingInternal(c, nil, "exporting the book report", err)
		return
	}
	all, err := data_models.ListInvoicingExpenses(db, owner, "")
	if err != nil {
		respondInvoicingInternal(c, nil, "exporting the book report", err)
		return
	}
	// A Private expense never counts (§3.143 Decision 8): not in a row, a
	// total or a month column. Applying the rest of the Book's selection to
	// this report is #10555-B2.
	expenses := make([]data_models.InvoicingExpense, 0, len(all))
	for _, e := range all {
		if e.Category != data_models.ExpenseCategoryPrivate {
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
		if cat.Name == data_models.ExpenseCategoryPrivate {
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
	serveCSV(c, db, exportFilename("Buchhaltung", "csv"), content, gin.H{"kind": "book_report_csv", "months": len(months)})
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
