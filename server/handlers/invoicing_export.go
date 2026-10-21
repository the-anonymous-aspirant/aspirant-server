package handlers

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// CSV exports for the invoicing app (system_3 #8367-C2). Every export runs
// the §5 validation first and is refused while it reports violations: an
// export is what goes to the Jobcenter and the lawyer.

const (
	invoicingEntityExport = "export"
	invoicingActionExport = "export"
)

// spreadsheetSafe neutralises formula injection (@security, c51455 on
// #8370): payer-written text such as a PayPal note reaches these files, and a
// cell starting with = + - @ tab or CR is run as a formula by Excel and
// LibreOffice. A leading apostrophe makes it plain text.
func spreadsheetSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

// csvMoney prints cents for a German spreadsheet: 1234,56. Exports carry
// only server-computed amounts, which never start with a formula character
// except a minus sign, and invoices hold no negative amounts.
func csvMoney(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s%d,%02d", sign, cents/100, cents%100)
}

func csvDate(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(invoicingDateLayout)
}

func csvQuantity(milli int64) string {
	s := fmt.Sprintf("%d,%03d", milli/1000, milli%1000)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ",")
}

// writeCSV renders rows as a German-Excel-friendly CSV: UTF-8 BOM, ';'
// separated.
func writeCSV(header []string, rows [][]string) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("\ufeff")
	w := csv.NewWriter(&buf)
	w.Comma = ';'
	if err := w.Write(header); err != nil {
		return nil, err
	}
	for _, r := range rows {
		if err := w.Write(r); err != nil {
			return nil, err
		}
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}

// runExportValidation answers false (and writes the 409 report) when the
// data has violations.
func runExportValidation(c *gin.Context, db *gorm.DB) bool {
	rep, err := validateInvoicing(db)
	if err != nil {
		respondInvoicingInternal(c, nil, "validating before export", err)
		return false
	}
	if !rep.OK {
		c.JSON(http.StatusConflict, gin.H{
			"error":      ErrorDetail{Code: "validation_failed", Message: "the data has integrity violations; fix them before exporting"},
			"validation": rep,
		})
		return false
	}
	return true
}

func serveCSV(c *gin.Context, db *gorm.DB, filename string, content []byte, details gin.H) {
	if err := data_models.RecordInvoicingAuditEvent(db, invoicingUserID(c), invoicingActionExport, invoicingEntityExport, 0, auditDetails(details)); err != nil {
		respondInvoicingInternal(c, nil, "recording the export", err)
		return
	}
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	c.Data(http.StatusOK, "text/csv; charset=utf-8", content)
}

// clientNameFor prefers the name frozen at issue, so an export shows what
// the invoice says, not what the client row says today.
func clientNameFor(db *gorm.DB, inv data_models.InvoicingInvoice) (string, string, error) {
	if snap, ok := decodeClientSnapshot(inv.ClientSnapshot); ok {
		return snap.Name, snap.Address, nil
	}
	if inv.ClientID == nil {
		return "", "", nil
	}
	cl, err := data_models.GetInvoicingClient(db, *inv.ClientID)
	if err != nil || cl == nil {
		return "", "", err
	}
	return cl.Name, cl.Address, nil
}

var invoicesCSVHeader = []string{
	"Rechnungsnummer", "Status", "Rechnungsdatum", "Leistung von", "Leistung bis",
	"Kunde", "Kundenadresse", "Netto", "USt", "Gesamt", "Währung",
	"Transaktions-IDs", "Storniert am", "Stornogrund",
}

// ExportAllInvoicesCSVHandler exports every numbered invoice, one row each.
func ExportAllInvoicesCSVHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	if !runExportValidation(c, db) {
		return
	}
	var invs []data_models.InvoicingInvoice
	if err := db.Where("number IS NOT NULL").Order("prefix asc, number asc").Find(&invs).Error; err != nil {
		respondInvoicingInternal(c, nil, "exporting invoices", err)
		return
	}
	rows := make([][]string, 0, len(invs))
	for _, inv := range invs {
		row, err := invoiceCSVRow(db, inv)
		if err != nil {
			respondInvoicingInternal(c, nil, "exporting invoices", err)
			return
		}
		rows = append(rows, row)
	}
	content, err := writeCSV(invoicesCSVHeader, rows)
	if err != nil {
		respondInvoicingInternal(c, nil, "exporting invoices", err)
		return
	}
	serveCSV(c, db, "Rechnungen.csv", content, gin.H{"kind": "invoices_csv", "invoices": len(rows)})
}

func invoiceCSVRow(db *gorm.DB, inv data_models.InvoicingInvoice) ([]string, error) {
	name, address, err := clientNameFor(db, inv)
	if err != nil {
		return nil, err
	}
	payments, err := data_models.ListInvoicePayments(db, inv.ID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(payments))
	for _, p := range payments {
		ids = append(ids, p.TransactionID)
	}
	number := ""
	if inv.InvoiceNumber != nil {
		number = *inv.InvoiceNumber
	}
	return []string{
		spreadsheetSafe(number), inv.Status, csvDate(inv.IssueDate), csvDate(inv.ServiceDateFrom), csvDate(inv.ServiceDateTo),
		spreadsheetSafe(name), spreadsheetSafe(strings.ReplaceAll(address, "\n", ", ")),
		csvMoney(inv.NetCents), csvMoney(inv.VatCents), csvMoney(inv.TotalCents), spreadsheetSafe(inv.Currency),
		spreadsheetSafe(strings.Join(ids, " ")), csvDate(inv.CancelledAt), spreadsheetSafe(inv.CancelReason),
	}, nil
}

var lineItemsCSVHeader = append(append([]string{}, invoicesCSVHeader[:7]...),
	"Pos.", "Beschreibung", "Menge", "Einzelpreis", "Positionsbetrag")

// ExportInvoiceCSVHandler exports one issued invoice, one row per line item.
func ExportInvoiceCSVHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	id, ok := parseInvoiceID(c)
	if !ok {
		return
	}
	inv, err := data_models.GetInvoicingInvoice(db, id)
	if err != nil {
		respondInvoicingInternal(c, nil, "exporting invoice", err)
		return
	}
	if inv == nil {
		RespondWithError(c, http.StatusNotFound, "Invoice not found")
		return
	}
	if inv.InvoiceNumber == nil {
		respondError(c, http.StatusConflict, "not_issued", "a draft is not exported; issue the invoice first")
		return
	}
	if !runExportValidation(c, db) {
		return
	}
	base, err := invoiceCSVRow(db, *inv)
	if err != nil {
		respondInvoicingInternal(c, nil, "exporting invoice", err)
		return
	}
	items, err := data_models.ListInvoiceLineItems(db, inv.ID)
	if err != nil {
		respondInvoicingInternal(c, nil, "exporting invoice", err)
		return
	}
	rows := make([][]string, 0, len(items))
	for i, it := range items {
		row := append(append([]string{}, base[:7]...),
			fmt.Sprint(i+1), spreadsheetSafe(it.Description), csvQuantity(it.QuantityMilli), csvMoney(it.UnitPriceCents), csvMoney(it.LineTotalCents))
		rows = append(rows, row)
	}
	content, err := writeCSV(lineItemsCSVHeader, rows)
	if err != nil {
		respondInvoicingInternal(c, nil, "exporting invoice", err)
		return
	}
	filename := "Rechnung-" + unsafeFilenameChars.ReplaceAllString(*inv.InvoiceNumber, "_") + ".csv"
	serveCSV(c, db, filename, content, gin.H{"kind": "invoice_csv", "invoice_id": inv.ID})
}

// decodeClientSnapshot reads the client frozen at issue.
func decodeClientSnapshot(s string) (data_models.InvoicingClient, bool) {
	var cl data_models.InvoicingClient
	if s == "" || json.Unmarshal([]byte(s), &cl) != nil || cl.Name == "" {
		return cl, false
	}
	return cl, true
}
