package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
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
// client, with the logo set at issue (nil for none), and stores the bytes. It
// returns the document's sha256.
func storeInvoicePDF(tx *gorm.DB, inv *data_models.InvoicingInvoice, profile *data_models.InvoicingProfile, client *data_models.InvoicingClient, items []data_models.InvoicingLineItem, logo *invoicing_pdf.Logo) (string, error) {
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
		Logo:     logo,
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

// GetInvoicePDFHandler serves the stored PDF of an issued invoice. A draft
// has no PDF: nothing is printed until the server has assigned the number and
// the date.
//
// A cancelled invoice is served marked (system_3 #12267): a copy of the
// stored PDF with "STORNIERT am <Berlin day>" across every page, named
// Rechnung-<number>-storniert.pdf. The stored bytes are not changed. When the
// copy cannot be made the route refuses (500, cancelled_mark_failed) and never
// falls back to the unmarked file, and the marked answer carries no
// X-Content-SHA256: that hash names the stored bytes, which were not sent.
func GetInvoicePDFHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	id, ok := parseInvoiceID(c)
	if !ok {
		return
	}
	inv, err := data_models.GetInvoicingInvoice(db, invoicingUserID(c), id)
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
	if inv.Status == data_models.InvoiceStatusCancelled {
		marked, err := markCancelledPDF(inv, stored.Content)
		if err != nil {
			log.Printf("Invoicing: marking cancelled invoice %d: %v", inv.ID, err)
			respondError(c, http.StatusInternalServerError, "cancelled_mark_failed", "The marked copy could not be made.")
			return
		}
		c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="Rechnung-%s-storniert.pdf"`, number))
		c.Data(http.StatusOK, "application/pdf", marked)
		return
	}
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="Rechnung-%s.pdf"`, number))
	c.Header("X-Content-SHA256", stored.SHA256)
	c.Data(http.StatusOK, "application/pdf", stored.Content)
}

// markCancelledPDF marks a cancelled invoice's stored PDF with the Berlin day
// of its Storno, written as the invoice writes its dates.
func markCancelledPDF(inv *data_models.InvoicingInvoice, stored []byte) ([]byte, error) {
	if inv.CancelledAt == nil {
		return nil, fmt.Errorf("cancelled invoice %d has no cancellation time", inv.ID)
	}
	return invoicing_pdf.MarkCancelled(stored, berlinDay(*inv.CancelledAt).Format("02.01.2006"))
}

// asUnprintable unwraps a font-coverage refusal from the renderer.
func asUnprintable(err error) *invoicing_pdf.UnprintableError {
	var u *invoicing_pdf.UnprintableError
	if errors.As(err, &u) {
		return u
	}
	return nil
}
