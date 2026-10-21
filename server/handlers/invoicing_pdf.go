package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"

	"aspirant-online/server/data_models"
	"aspirant-online/server/invoicing_pdf"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Invoice PDFs (system_3 #8367-C1): rendered once, inside the issuing
// transaction, and served from storage ever after.

// storeInvoicePDF renders the issued invoice from its frozen profile and
// client and stores the bytes. It returns the document's sha256.
func storeInvoicePDF(tx *gorm.DB, inv *data_models.InvoicingInvoice, profile *data_models.InvoicingProfile, client *data_models.InvoicingClient, items []data_models.InvoicingLineItem) (string, error) {
	payments, err := data_models.ListInvoicePayments(tx, inv.ID)
	if err != nil {
		return "", err
	}
	doc, err := invoicing_pdf.Build(invoicing_pdf.Input{
		Profile:  *profile,
		Client:   *client,
		Invoice:  *inv,
		Items:    items,
		Payments: payments,
	})
	if err != nil {
		return "", err
	}
	content, err := invoicing_pdf.Render(doc)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	if err := tx.Create(&data_models.InvoicingInvoicePDF{InvoiceID: inv.ID, Content: content, SHA256: digest}).Error; err != nil {
		return "", err
	}
	return digest, nil
}

var unsafeFilenameChars = regexp.MustCompile(`[^0-9A-Za-z-]`)

// GetInvoicePDFHandler serves the stored PDF of an issued (or cancelled)
// invoice. A draft has no PDF: nothing is printed until the server has
// assigned the number and the date.
func GetInvoicePDFHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	id, ok := parseInvoiceID(c)
	if !ok {
		return
	}
	inv, err := data_models.GetInvoicingInvoice(db, id)
	if err != nil {
		respondInvoicingInternal(c, nil, "loading invoice", err)
		return
	}
	if inv == nil {
		RespondWithError(c, http.StatusNotFound, "Invoice not found")
		return
	}
	if inv.Status == data_models.InvoiceStatusDraft {
		respondError(c, http.StatusConflict, "not_issued", "a draft has no PDF; issue the invoice first")
		return
	}
	stored, err := data_models.GetInvoicePDF(db, inv.ID)
	if err != nil {
		respondInvoicingInternal(c, nil, "loading invoice PDF", err)
		return
	}
	if stored == nil {
		respondInvoicingInternal(c, nil, "loading invoice PDF", fmt.Errorf("issued invoice %d has no stored PDF", inv.ID))
		return
	}
	number := ""
	if inv.InvoiceNumber != nil {
		number = unsafeFilenameChars.ReplaceAllString(*inv.InvoiceNumber, "_")
	}
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="Rechnung-%s.pdf"`, number))
	c.Header("X-Content-SHA256", stored.SHA256)
	c.Data(http.StatusOK, "application/pdf", stored.Content)
}
