package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	"github.com/signintech/gopdf"
)

// Download several invoices as one PDF (#9940-A2 / #9956-A1, operator
// c61402): the owner ticks rows on the Invoices tab and gets one file
// holding each ticked invoice's stored PDF, ordered by invoice number
// ascending (the ZIP's own order), not tick order, so she can predict it and
// check it against the list (design_agent Design ack, c61504 on #9960).
//
// Only an issued invoice is accepted (design ack D2, binding this route too):
// a cancelled invoice's stored PDF is never re-stamped (CancelInvoiceHandler
// only flips status/metadata, the bytes from issuing are kept as-is -- the
// same fact ExportInvoicesZipHandler already excludes cancelled invoices
// for), so merging one in would produce a document that reads as a valid,
// uncancelled invoice. A refused id fails the whole request with a stated
// reason rather than being silently skipped, so a stale client can never get
// a shorter file than it asked for (ack D13).

// invoicesMergedPDFBase is the merged PDF's name before exportFilename adds
// the export time.
const invoicesMergedPDFBase = "Rechnungen"

// ExportInvoicesMergedPDFHandler serves Rechnungen-<export time>.pdf: the comma-separated
// invoice ids in ?ids= are fetched, ordered by (prefix, number) ascending,
// and their stored PDF pages are concatenated into one document.
func ExportInvoicesMergedPDFHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	ids, ok := parseInvoiceIDList(c)
	if !ok {
		return
	}
	if !runExportValidation(c, db) {
		return
	}

	invoices, gerr := invoicesForMerge(db, invoicingUserID(c), ids)
	if gerr != nil {
		respondGuard(c, nil, gerr)
		return
	}

	pdf := &gopdf.GoPdf{}
	pdf.Start(gopdf.Config{PageSize: *gopdf.PageSizeA4})

	for _, inv := range invoices {
		stored, err := data_models.GetInvoicePDF(db, inv.ID)
		if err != nil {
			respondInvoicingInternal(c, nil, "building the merged PDF", err)
			return
		}
		// The status check above already requires issued, which always has a
		// stored PDF; a nil here means the data changed in between, same
		// defensive shape as ExportInvoicesZipHandler.
		if stored == nil {
			respondInvoicingInternal(c, nil, "building the merged PDF", errMissingPDF(inv.ID))
			return
		}
		if err := pdf.ImportPagesFromSource(stored.Content, "/MediaBox"); err != nil {
			respondInvoicingInternal(c, nil, "building the merged PDF", err)
			return
		}
	}

	if err := data_models.RecordInvoicingAuditEvent(db, invoicingUserID(c), invoicingActionExport, invoicingEntityExport, 0,
		auditDetails(gin.H{"kind": "invoices_merged_pdf", "invoices": len(ids)})); err != nil {
		respondInvoicingInternal(c, nil, "recording the export", err)
		return
	}

	c.Header("Content-Disposition", `attachment; filename="`+exportFilename(invoicesMergedPDFBase, "pdf")+`"`)
	c.Data(http.StatusOK, "application/pdf", pdf.GetBytesPdf())
}

// invoicesForMerge fetches the given invoice ids, ordered by (prefix,
// number) ascending -- the ZIP's own order, independent of the order ids
// were requested in (design ack c61504: "She can predict it and check it
// against the list"). It refuses the whole request, rather than silently
// dropping a row, on the first id that does not exist or is not issued, so
// a stale client can never get a shorter file than it asked for (ack D13).
func invoicesForMerge(db *gorm.DB, owner uint, ids []uint) ([]data_models.InvoicingInvoice, *invoicingGuardError) {
	var invoices []data_models.InvoicingInvoice
	if err := db.Where("owner_user_id = ? AND id IN (?)", owner, ids).Order("prefix asc, number asc").Find(&invoices).Error; err != nil {
		return nil, guard(http.StatusInternalServerError, "lookup_failed", "%v", err)
	}
	byID := make(map[uint]data_models.InvoicingInvoice, len(invoices))
	for _, inv := range invoices {
		byID[inv.ID] = inv
	}
	for _, id := range ids {
		inv, found := byID[id]
		if !found {
			return nil, guard(http.StatusUnprocessableEntity, "invoice_not_found", "invoice %d does not exist", id)
		}
		switch inv.Status {
		case data_models.InvoiceStatusCancelled:
			return nil, guard(http.StatusUnprocessableEntity, "invoice_cancelled",
				"invoice %s is cancelled; its stored PDF does not show that, so it is left out of a merged download", invoiceLabel(inv))
		case data_models.InvoiceStatusDraft:
			return nil, guard(http.StatusUnprocessableEntity, "invoice_draft",
				"invoice %s is a draft; it has no stored PDF yet", invoiceLabel(inv))
		}
	}
	return invoices, nil
}

// parseInvoiceIDList reads the comma-separated ?ids= query param. It writes
// its own 400 response and returns ok=false on any malformed input.
func parseInvoiceIDList(c *gin.Context) (ids []uint, ok bool) {
	raw := strings.TrimSpace(c.Query("ids"))
	if raw == "" {
		respondError(c, http.StatusBadRequest, "no_ids", "pass at least one invoice id in ?ids=")
		return nil, false
	}
	parts := strings.Split(raw, ",")
	ids = make([]uint, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.ParseUint(strings.TrimSpace(p), 10, 64)
		if err != nil {
			respondError(c, http.StatusBadRequest, "bad_id", "'"+p+"' is not a valid invoice id")
			return nil, false
		}
		ids = append(ids, uint(n))
	}
	return ids, true
}
