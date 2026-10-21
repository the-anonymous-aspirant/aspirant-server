package handlers

import (
	"fmt"
	"net/http"
	"strings"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// The Excel workbook export (system_3 #8367-E1, spec §4.8): one file with
// Invoices, Line items, Payments (with category) and Clients sheets. Like the
// CSV exports it runs the §5 validation first and refuses on violations.

const xlsxContentType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"

// ExportWorkbookHandler serves Rechnungen.xlsx.
func ExportWorkbookHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	if !runExportValidation(c, db) {
		return
	}
	sheets, err := buildInvoicingWorkbook(db)
	if err != nil {
		respondInvoicingInternal(c, nil, "exporting the workbook", err)
		return
	}
	content, err := writeXLSX(sheets)
	if err != nil {
		respondInvoicingInternal(c, nil, "exporting the workbook", err)
		return
	}
	if err := data_models.RecordInvoicingAuditEvent(db, invoicingUserID(c), invoicingActionExport, invoicingEntityExport, 0,
		auditDetails(gin.H{"kind": "workbook_xlsx", "invoices": len(sheets[0].Rows), "payments": len(sheets[2].Rows)})); err != nil {
		respondInvoicingInternal(c, nil, "recording the export", err)
		return
	}
	c.Header("Content-Disposition", `attachment; filename="Rechnungen.xlsx"`)
	c.Data(http.StatusOK, xlsxContentType, content)
}

func buildInvoicingWorkbook(db *gorm.DB) ([]xlsxSheet, error) {
	var invs []data_models.InvoicingInvoice
	if err := db.Where("number IS NOT NULL").Order("prefix asc, number asc").Find(&invs).Error; err != nil {
		return nil, err
	}

	invoices := xlsxSheet{Name: "Invoices", Header: []string{
		"Rechnungsnummer", "Status", "Rechnungsdatum", "Leistung von", "Leistung bis", "Kunde", "Kundenadresse",
		"Netto", "USt", "Gesamt", "Währung", "Transaktions-IDs", "Storniert am", "Stornogrund",
	}}
	items := xlsxSheet{Name: "Line items", Header: []string{
		"Rechnungsnummer", "Pos.", "Beschreibung", "Menge", "Einzelpreis", "Positionsbetrag",
	}}
	for _, inv := range invs {
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
			if ref := p.PrintedReference(); ref != "" {
				ids = append(ids, ref)
			}
		}
		number := ""
		if inv.InvoiceNumber != nil {
			number = *inv.InvoiceNumber
		}
		invoices.Rows = append(invoices.Rows, []xlsxCell{
			xlsxText(number), xlsxText(inv.Status),
			xlsxText(csvDate(inv.IssueDate)), xlsxText(csvDate(inv.ServiceDateFrom)), xlsxText(csvDate(inv.ServiceDateTo)),
			xlsxText(name), xlsxText(strings.ReplaceAll(address, "\n", ", ")),
			xlsxMoney(inv.NetCents), xlsxMoney(inv.VatCents), xlsxMoney(inv.TotalCents), xlsxText(inv.Currency),
			xlsxText(strings.Join(ids, " ")), xlsxText(csvDate(inv.CancelledAt)), xlsxText(inv.CancelReason),
		})
		lines, err := data_models.ListInvoiceLineItems(db, inv.ID)
		if err != nil {
			return nil, err
		}
		for i, it := range lines {
			items.Rows = append(items.Rows, []xlsxCell{
				xlsxText(number), xlsxNumber(float64(i + 1)), xlsxText(it.Description),
				xlsxNumber(float64(it.QuantityMilli) / 1000), xlsxMoney(it.UnitPriceCents), xlsxMoney(it.LineTotalCents),
			})
		}
	}

	clientRows, err := data_models.ListInvoicingClients(db)
	if err != nil {
		return nil, err
	}
	clientName := map[uint]string{}
	clients := xlsxSheet{Name: "Clients", Header: []string{"Name", "Adresse", "E-Mail", "PayPal-E-Mail"}}
	for _, cl := range clientRows {
		clientName[cl.ID] = cl.Name
		clients.Rows = append(clients.Rows, []xlsxCell{
			xlsxText(cl.Name), xlsxText(strings.ReplaceAll(cl.Address, "\n", ", ")), xlsxText(cl.Email), xlsxText(cl.PayPalEmail),
		})
	}

	paymentRows, err := data_models.ListInvoicingPayments(db, "", "")
	if err != nil {
		return nil, err
	}
	depositLabel := map[uint]string{}
	for _, p := range paymentRows {
		if p.Kind == data_models.PaymentKindWithdrawal {
			depositLabel[p.ID] = fmt.Sprintf("%s %s", p.PaidOn.Format(invoicingDateLayout), p.TransactionID)
		}
	}
	payments := xlsxSheet{Name: "Payments", Header: []string{
		"Datum", "Art", "Quelle", "Transaktions-ID", "Name", "E-Mail", "Hinweis", "Brutto", "Gebühr", "Netto",
		"Währung", "Kategorie", "Kunde", "Bankeinzahlung", "Zahlungsart",
	}}
	for _, p := range paymentRows {
		client, deposit := "", ""
		if p.ClientID != nil {
			client = clientName[*p.ClientID]
		}
		if p.BankDepositID != nil {
			deposit = depositLabel[*p.BankDepositID]
		}
		// A hand-entered payment (#8409) reads "manuell", with the owner's
		// reference rather than its internal id.
		source := p.Source
		if p.IsManual() {
			source = "manuell"
		}
		payments.Rows = append(payments.Rows, []xlsxCell{
			xlsxText(p.PaidOn.Format(invoicingDateLayout)), xlsxText(p.Kind), xlsxText(source), xlsxText(p.PrintedReference()),
			xlsxText(p.SenderName), xlsxText(p.SenderEmail), xlsxText(p.Note),
			xlsxMoney(p.GrossCents), xlsxMoney(p.FeeCents), xlsxMoney(p.NetCents), xlsxText(p.Currency),
			xlsxText(p.Category), xlsxText(client), xlsxText(deposit), xlsxText(p.MethodLabel()),
		})
	}
	return []xlsxSheet{invoices, items, payments, clients}, nil
}
