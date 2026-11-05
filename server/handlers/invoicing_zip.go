package handlers

import (
	"archive/zip"
	"bytes"
	"net/http"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Download all invoices: one ZIP holding the stored PDF of every issued
// invoice, named by its number, and nothing else. A cancelled invoice is left
// out: its stored PDF is the original, which carries no cancellation mark, so
// in the ZIP it would read as valid. The invoice list CSV still lists it.

const invoicesZipFilename = "Rechnungen.zip"

// ExportInvoicesZipHandler serves Rechnungen.zip.
func ExportInvoicesZipHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	if !runExportValidation(c, db) {
		return
	}
	var issued []data_models.InvoicingInvoice
	if err := db.Where("status = ?", data_models.InvoiceStatusIssued).Order("prefix asc, number asc").Find(&issued).Error; err != nil {
		respondInvoicingInternal(c, nil, "building the invoices ZIP", err)
		return
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, inv := range issued {
		pdf, err := data_models.GetInvoicePDF(db, inv.ID)
		if err != nil {
			respondInvoicingInternal(c, nil, "building the invoices ZIP", err)
			return
		}
		// The validation above refuses an issued invoice without a PDF, so a
		// nil here means the data changed in between; fail rather than ship
		// a ZIP with a hole in it.
		if pdf == nil || inv.InvoiceNumber == nil {
			respondInvoicingInternal(c, nil, "building the invoices ZIP", errMissingPDF(inv.ID))
			return
		}
		name := "Rechnung-" + unsafeFilenameChars.ReplaceAllString(*inv.InvoiceNumber, "_") + ".pdf"
		w, err := zw.Create(name)
		if err == nil {
			_, err = w.Write(pdf.Content)
		}
		if err != nil {
			respondInvoicingInternal(c, nil, "building the invoices ZIP", err)
			return
		}
	}
	if err := zw.Close(); err != nil {
		respondInvoicingInternal(c, nil, "building the invoices ZIP", err)
		return
	}
	if err := data_models.RecordInvoicingAuditEvent(db, invoicingUserID(c), invoicingActionExport, invoicingEntityExport, 0,
		auditDetails(gin.H{"kind": "invoices_zip", "invoices": len(issued)})); err != nil {
		respondInvoicingInternal(c, nil, "recording the export", err)
		return
	}
	c.Header("Content-Disposition", `attachment; filename="`+invoicesZipFilename+`"`)
	c.Data(http.StatusOK, "application/zip", buf.Bytes())
}

type missingPDFError uint

func (e missingPDFError) Error() string { return "issued invoice has no stored PDF" }

func errMissingPDF(id uint) error { return missingPDFError(id) }
