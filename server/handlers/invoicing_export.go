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
// export is what leaves the app, so it must not carry broken data.

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
	rep, err := validateInvoicing(db, invoicingUserID(c))
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

// exportFilename names an export with the time it was made, in Europe/Berlin
// (the app's calendar, spec §6), as <base>-YYYY-MM-DD-HHMM.<ext>. Every export
// used to carry one fixed name, so an older download and a newer one could not
// be told apart in the Downloads folder (system_3 #9990, from #9964). There
// is no colon, so the name is valid on every OS.
func exportFilename(base, ext string) string {
	return base + "-" + invoicingNow().In(data_models.BerlinLocation()).Format("2006-01-02-1504") + "." + ext
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
func clientNameFor(db *gorm.DB, owner uint, inv data_models.InvoicingInvoice) (string, string, error) {
	if snap, ok := decodeClientSnapshot(inv.ClientSnapshot); ok {
		return snap.Name, snap.Address, nil
	}
	if inv.ClientID == nil {
		return "", "", nil
	}
	cl, err := data_models.GetInvoicingClient(db, owner, *inv.ClientID)
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
	if err := db.Where("owner_user_id = ? AND number IS NOT NULL", invoicingUserID(c)).Order("prefix asc, number asc").Find(&invs).Error; err != nil {
		respondInvoicingInternal(c, nil, "exporting invoices", err)
		return
	}
	rows := make([][]string, 0, len(invs))
	for _, inv := range invs {
		row, err := invoiceCSVRow(db, invoicingUserID(c), inv)
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
	serveCSV(c, db, exportFilename("Rechnungen", "csv"), content, gin.H{"kind": "invoices_csv", "invoices": len(rows)})
}

func invoiceCSVRow(db *gorm.DB, owner uint, inv data_models.InvoicingInvoice) ([]string, error) {
	name, address, err := clientNameFor(db, owner, inv)
	if err != nil {
		return nil, err
	}
	payments, err := data_models.ListInvoicePayments(db, inv.ID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(payments))
	for _, p := range payments {
		// The printed reference: a hand-entered payment's internal id is
		// never shown, and one entered without a reference adds nothing.
		if ref := p.PrintedReference(); ref != "" {
			ids = append(ids, ref)
		}
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

// decodeClientSnapshot reads the client frozen at issue.
func decodeClientSnapshot(s string) (data_models.InvoicingClient, bool) {
	var cl data_models.InvoicingClient
	if s == "" || json.Unmarshal([]byte(s), &cl) != nil || cl.Name == "" {
		return cl, false
	}
	return cl, true
}

var clientsCSVHeader = []string{
	"Name", "Adresse", "E-Mail", "PayPal-E-Mail", "Telefon", "Instagram", "Telegram", "Angelegt am", "Geändert am",
}

// clientsCSVStampLayout prints a moment as the export's ISO day plus the
// time, in Berlin (the invoicing app's calendar, spec §6).
const clientsCSVStampLayout = "2006-01-02 15:04"

func clientsCSVStamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(data_models.BerlinLocation()).Format(clientsCSVStampLayout)
}

// ExportClientsCSVHandler exports her client list, one row per client in
// name order (system_3 #10012, from #9978, operator c61628). Her notes are
// never a column: they are hers alone, never on an invoice or an export
// (#9978 decision 1, #9962). Every text cell goes through spreadsheetSafe,
// so a phone like "+49 …" is written as "'+49 …" and stays text. There is
// no validation gate: a client list has no integrity rules to check.
func ExportClientsCSVHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	clients, err := data_models.ListInvoicingClients(db, invoicingUserID(c))
	if err != nil {
		respondInvoicingInternal(c, nil, "exporting clients", err)
		return
	}
	rows := make([][]string, 0, len(clients))
	for _, cl := range clients {
		rows = append(rows, []string{
			spreadsheetSafe(cl.Name),
			spreadsheetSafe(strings.ReplaceAll(cl.Address, "\n", ", ")),
			spreadsheetSafe(cl.Email),
			spreadsheetSafe(cl.PayPalEmail),
			spreadsheetSafe(cl.Phone),
			spreadsheetSafe(cl.InstagramURL),
			spreadsheetSafe(cl.TelegramURL),
			clientsCSVStamp(cl.CreatedAt),
			clientsCSVStamp(cl.UpdatedAt),
		})
	}
	content, err := writeCSV(clientsCSVHeader, rows)
	if err != nil {
		respondInvoicingInternal(c, nil, "exporting clients", err)
		return
	}
	serveCSV(c, db, exportFilename("Kunden", "csv"), content, gin.H{"kind": "clients_csv", "clients": len(rows)})
}
