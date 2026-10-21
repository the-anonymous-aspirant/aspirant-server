package handlers

import (
	"archive/zip"
	"bytes"
	"net/http"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// The Jobcenter bundle (system_3 #8367-E2, spec §4.8): every issued invoice
// PDF plus a summary that maps each bank deposit to the payments behind it
// and what proves each one. Cancelled invoices are left out — the bundle
// proves current income; a Storno stays in the CSV and Excel exports.

// bundleCategoryLabels are the German names of the payment categories for
// the Jobcenter summary. Labels only; no statement is attached to them.
var bundleCategoryLabels = map[string]string{
	data_models.PaymentCategoryPaidWork:      "Bezahlte Arbeit",
	data_models.PaymentCategoryGift:          "Geschenk",
	data_models.PaymentCategoryLoan:          "Darlehen",
	data_models.PaymentCategoryRefund:        "Erstattung",
	data_models.PaymentCategoryOther:         "Sonstiges",
	data_models.PaymentCategoryUncategorised: "Nicht zugeordnet",
}

const bundleNoInvoiceYet = "keine Rechnung (bezahlte Arbeit ohne ausgestellte Rechnung)"

var bundleSummaryHeader = []string{
	"Bankeinzahlung Datum", "Bankeinzahlung Betrag", "Bankeinzahlung Transaktions-ID",
	"Zahlung Datum", "Zahlung Betrag", "Absender", "Zahlung Transaktions-ID",
	"Kategorie", "Nachweis",
}

// buildBundleSummary returns the summary rows: payments grouped under their
// bank deposit, deposits in date order, unassigned payments last.
func buildBundleSummary(db *gorm.DB) ([][]string, error) {
	all, err := data_models.ListInvoicingPayments(db, "", "")
	if err != nil {
		return nil, err
	}
	var deposits, incoming []data_models.InvoicingPayment
	for _, p := range all {
		if p.Kind == data_models.PaymentKindWithdrawal {
			deposits = append(deposits, p)
		} else {
			incoming = append(incoming, p)
		}
	}

	proof := func(p data_models.InvoicingPayment) (string, error) {
		if p.Category != data_models.PaymentCategoryPaidWork {
			return "", nil
		}
		invID, err := data_models.IssuedInvoiceForPayment(db, p.ID, 0)
		if err != nil || invID == 0 {
			return bundleNoInvoiceYet, err
		}
		inv, err := data_models.GetInvoicingInvoice(db, invID)
		if err != nil || inv == nil || inv.InvoiceNumber == nil {
			return bundleNoInvoiceYet, err
		}
		return *inv.InvoiceNumber, nil
	}
	row := func(dep *data_models.InvoicingPayment, p data_models.InvoicingPayment) ([]string, error) {
		nachweis, err := proof(p)
		if err != nil {
			return nil, err
		}
		depDate, depAmount, depID := "Ohne Bankeinzahlung", "", ""
		if dep != nil {
			depDate, depAmount, depID = dep.PaidOn.Format(invoicingDateLayout), csvMoney(dep.GrossCents), dep.TransactionID
		}
		label := bundleCategoryLabels[p.Category]
		if label == "" {
			label = p.Category
		}
		return []string{
			depDate, depAmount, spreadsheetSafe(depID),
			p.PaidOn.Format(invoicingDateLayout), csvMoney(p.GrossCents), spreadsheetSafe(p.SenderName), spreadsheetSafe(p.PrintedReference()),
			label, spreadsheetSafe(nachweis),
		}, nil
	}

	var rows [][]string
	assigned := map[uint]bool{}
	for i := range deposits {
		dep := deposits[i]
		for _, p := range incoming {
			if p.BankDepositID == nil || *p.BankDepositID != dep.ID {
				continue
			}
			r, err := row(&dep, p)
			if err != nil {
				return nil, err
			}
			rows = append(rows, r)
			assigned[p.ID] = true
		}
	}
	for _, p := range incoming {
		if assigned[p.ID] {
			continue
		}
		r, err := row(nil, p)
		if err != nil {
			return nil, err
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// ExportJobcenterBundleHandler serves Jobcenter-Nachweise.zip.
func ExportJobcenterBundleHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	if !runExportValidation(c, db) {
		return
	}
	var issued []data_models.InvoicingInvoice
	if err := db.Where("status = ?", data_models.InvoiceStatusIssued).Order("prefix asc, number asc").Find(&issued).Error; err != nil {
		respondInvoicingInternal(c, nil, "building the Jobcenter bundle", err)
		return
	}
	summary, err := buildBundleSummary(db)
	if err != nil {
		respondInvoicingInternal(c, nil, "building the Jobcenter bundle", err)
		return
	}
	summaryCSV, err := writeCSV(bundleSummaryHeader, summary)
	if err != nil {
		respondInvoicingInternal(c, nil, "building the Jobcenter bundle", err)
		return
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name string, content []byte) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write(content)
		return err
	}
	for _, inv := range issued {
		pdf, err := data_models.GetInvoicePDF(db, inv.ID)
		if err != nil {
			respondInvoicingInternal(c, nil, "building the Jobcenter bundle", err)
			return
		}
		// The validation above refuses an issued invoice without a PDF, so a
		// nil here means the data changed in between; fail rather than ship
		// a bundle with a hole in it.
		if pdf == nil || inv.InvoiceNumber == nil {
			respondInvoicingInternal(c, nil, "building the Jobcenter bundle", errMissingPDF(inv.ID))
			return
		}
		name := "Rechnung-" + unsafeFilenameChars.ReplaceAllString(*inv.InvoiceNumber, "_") + ".pdf"
		if err := add(name, pdf.Content); err != nil {
			respondInvoicingInternal(c, nil, "building the Jobcenter bundle", err)
			return
		}
	}
	if err := add("Zusammenfassung.csv", summaryCSV); err != nil {
		respondInvoicingInternal(c, nil, "building the Jobcenter bundle", err)
		return
	}
	if err := zw.Close(); err != nil {
		respondInvoicingInternal(c, nil, "building the Jobcenter bundle", err)
		return
	}
	if err := data_models.RecordInvoicingAuditEvent(db, invoicingUserID(c), invoicingActionExport, invoicingEntityExport, 0,
		auditDetails(gin.H{"kind": "jobcenter_bundle", "invoices": len(issued), "summary_rows": len(summary)})); err != nil {
		respondInvoicingInternal(c, nil, "recording the export", err)
		return
	}
	c.Header("Content-Disposition", `attachment; filename="Jobcenter-Nachweise.zip"`)
	c.Data(http.StatusOK, "application/zip", buf.Bytes())
}

type missingPDFError uint

func (e missingPDFError) Error() string { return "issued invoice has no stored PDF" }

func errMissingPDF(id uint) error { return missingPDFError(id) }
