package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Invoice drafts for the invoicing app (system_3 #8367-B2). A draft is built
// only from paid-work payments and is freely editable until it is issued
// (invoicing_issue.go). Behind the flowah+Admin gate on the /invoicing group.

const (
	invoicingEntityInvoice = "invoice"
	invoicingActionDelete  = "delete"

	invoicingDateLayout = "2006-01-02"

	maxQuantityMilli  = 10_000 * 1000
	maxUnitPriceCents = 100_000_000 * 100
)

// serverOwnedInvoiceFields can never come from a request: the number and the
// issue date are assigned at issue, server-side (spec §4.6, §10).
var serverOwnedInvoiceFields = map[string]bool{
	"issue_date": true, "invoice_number": true, "number": true, "prefix": true,
	"status": true, "issued_at": true, "issued_by": true, "total_cents": true,
	"net_cents": true, "vat_cents": true, "profile_snapshot": true, "client_snapshot": true,
}

var draftEditableFields = map[string]bool{
	"client_id": true, "service_date_from": true, "service_date_to": true,
	"vat_rate_bp": true, "line_items": true, "payment_ids": true,
}

// invoicingGuardError is a refusal with a stable reason code the client can
// show and the tests can assert.
type invoicingGuardError struct {
	Status  int
	Code    string
	Message string
}

func (e *invoicingGuardError) Error() string { return e.Message }

func guard(status int, code, format string, args ...interface{}) *invoicingGuardError {
	return &invoicingGuardError{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// checkFieldAllowlist refuses server-owned and unknown fields by name.
func checkFieldAllowlist(body map[string]json.RawMessage, allowed map[string]bool) *invoicingGuardError {
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if serverOwnedInvoiceFields[k] {
			return guard(http.StatusBadRequest, "server_owned_field", "%s is set by the server when the invoice is issued and cannot be sent", k)
		}
		if !allowed[k] {
			return guard(http.StatusBadRequest, "unknown_field", "unknown field %q", k)
		}
	}
	return nil
}

// checkInvoiceablePayments loads the payments and refuses any that is not an
// incoming paid-work payment or already sits on another issued invoice.
func checkInvoiceablePayments(tx *gorm.DB, ids []uint, invoiceID uint) ([]data_models.InvoicingPayment, *invoicingGuardError, error) {
	if len(ids) == 0 {
		return nil, guard(http.StatusUnprocessableEntity, "payment_not_paid_work", "an invoice needs at least one paid-work payment"), nil
	}
	// Lock every listed payment row before reading it, in ascending id order
	// so two drafts cannot deadlock: a concurrent delete of a hand-entered
	// payment then either waits for this link or has already removed the
	// row, which this check then refuses (#8409 security F1). This holds
	// even before the issuer profile, whose row the issuing lock uses, exists.
	ordered := append([]uint(nil), ids...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	for _, id := range ordered {
		if err := lockPaymentRow(tx, id); err != nil {
			return nil, nil, err
		}
	}
	seen := map[uint]bool{}
	var out []data_models.InvoicingPayment
	for _, id := range ids {
		if seen[id] {
			return nil, guard(http.StatusBadRequest, "duplicate_payment", "payment %d is listed twice", id), nil
		}
		seen[id] = true
		p, err := data_models.GetInvoicingPayment(tx, id)
		if err != nil {
			return nil, nil, err
		}
		if p == nil {
			return nil, guard(http.StatusUnprocessableEntity, "payment_not_found", "payment %d does not exist", id), nil
		}
		if p.Kind != data_models.PaymentKindIncoming || p.Category != data_models.PaymentCategoryPaidWork {
			return nil, guard(http.StatusUnprocessableEntity, "payment_not_paid_work",
				"payment %s is %q, not paid work; only paid-work payments can be invoiced", paymentLabel(*p), p.Category), nil
		}
		other, err := data_models.IssuedInvoiceForPayment(tx, p.ID, invoiceID)
		if err != nil {
			return nil, nil, err
		}
		if other != 0 {
			return nil, guard(http.StatusUnprocessableEntity, "payment_already_invoiced",
				"payment %s is already on issued invoice %d", paymentLabel(*p), other), nil
		}
		out = append(out, *p)
	}
	return out, nil, nil
}

// recomputeTotals sets net, VAT and total from the line items. VAT applies
// only when the issuer is not a Kleinunternehmerin.
func recomputeTotals(inv *data_models.InvoicingInvoice, items []data_models.InvoicingLineItem, kleinunternehmer bool) {
	var net int64
	for _, it := range items {
		net += it.LineTotalCents
	}
	inv.NetCents, inv.VatCents = net, 0
	if !kleinunternehmer {
		// net × bp / 10000, rounded half away from zero. LineTotalCents
		// rounds q×p/1000, and vat_rate_bp is kept a multiple of 10, so
		// bp/10 is exact and the same rounding applies.
		inv.VatCents = data_models.LineTotalCents(net, int64(inv.VatRateBP/10))
	}
	inv.TotalCents = inv.NetCents + inv.VatCents
}

func profileIsKleinunternehmer(tx *gorm.DB) (bool, error) {
	p, err := data_models.GetInvoicingProfile(tx)
	if err != nil {
		return false, err
	}
	if p == nil {
		return data_models.DefaultInvoicingProfile().IsKleinunternehmer, nil
	}
	return p.IsKleinunternehmer, nil
}

// invoiceWarnings reports the non-blocking §5 check: the invoice total should
// match what the linked payments brought in.
func invoiceWarnings(inv data_models.InvoicingInvoice, payments []data_models.InvoicingPayment) []string {
	var sum int64
	for _, p := range payments {
		sum += p.GrossCents
	}
	if len(payments) > 0 && sum != inv.TotalCents {
		return []string{fmt.Sprintf("total_mismatch: invoice total %d cents, linked payments %d cents", inv.TotalCents, sum)}
	}
	return []string{}
}

type invoiceDetail struct {
	Invoice   data_models.InvoicingInvoice    `json:"invoice"`
	LineItems []data_models.InvoicingLineItem `json:"line_items"`
	Payments  []data_models.InvoicingPayment  `json:"payments"`
	Warnings  []string                        `json:"warnings"`
	// ClientName is the name to show: frozen at issue once the invoice has
	// left draft, the live client for a draft (see invoiceClientName).
	ClientName string `json:"client_name"`
}

// invoiceClientName is the client name to show for an invoice. Once issued
// (or cancelled) it is the client as frozen at issue, which is what the PDF
// and the exports print; a later rename of the client must not change what
// the screen says the legal invoice was addressed to. A draft shows the live
// client.
func invoiceClientName(inv data_models.InvoicingInvoice, live string) string {
	if inv.Status != data_models.InvoiceStatusDraft {
		if snap, ok := decodeClientSnapshot(inv.ClientSnapshot); ok {
			return snap.Name
		}
	}
	return live
}

func loadInvoiceDetail(db *gorm.DB, inv data_models.InvoicingInvoice) (*invoiceDetail, error) {
	items, err := data_models.ListInvoiceLineItems(db, inv.ID)
	if err != nil {
		return nil, err
	}
	payments, err := data_models.ListInvoicePayments(db, inv.ID)
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []data_models.InvoicingLineItem{}
	}
	if payments == nil {
		payments = []data_models.InvoicingPayment{}
	}
	live := ""
	if inv.ClientID != nil {
		cl, err := data_models.GetInvoicingClient(db, *inv.ClientID)
		if err != nil {
			return nil, err
		}
		if cl != nil {
			live = cl.Name
		}
	}
	return &invoiceDetail{Invoice: inv, LineItems: items, Payments: payments, Warnings: invoiceWarnings(inv, payments),
		ClientName: invoiceClientName(inv, live)}, nil
}

func respondGuard(c *gin.Context, tx *gorm.DB, g *invoicingGuardError) {
	if tx != nil {
		tx.Rollback()
	}
	respondError(c, g.Status, g.Code, g.Message)
}

func respondInvoicingInternal(c *gin.Context, tx *gorm.DB, what string, err error) {
	if tx != nil {
		tx.Rollback()
	}
	log.Printf("Invoicing: %s: %v", what, err)
	RespondWithError(c, http.StatusInternalServerError, "Error "+what)
}

func parseInvoiceID(c *gin.Context) (uint, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		RespondWithError(c, http.StatusBadRequest, "Invalid invoice id")
		return 0, false
	}
	return uint(id), true
}

type createDraftRequest struct {
	PaymentIDs []uint `json:"payment_ids"`
}

// CreateInvoiceDraftHandler starts a draft from paid-work payments, prefilled
// from data only: one line per payment (its note as the description — empty
// when the payment has none — quantity 1, the amount as the price), the client
// when every payment names the same one, and the payment date as the service
// date. Whatever the data does not supply stays empty and blocks issuing.
func CreateInvoiceDraftHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	var body map[string]json.RawMessage
	if err := c.ShouldBindJSON(&body); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid request body")
		return
	}
	if g := checkFieldAllowlist(body, map[string]bool{"payment_ids": true}); g != nil {
		respondGuard(c, nil, g)
		return
	}
	var req createDraftRequest
	if raw, ok := body["payment_ids"]; ok {
		if err := json.Unmarshal(raw, &req.PaymentIDs); err != nil {
			RespondWithError(c, http.StatusBadRequest, "payment_ids must be a list of ids")
			return
		}
	}

	tx := db.Begin()
	// Linking payments takes the issuing lock, the same one a payment delete
	// takes (#8409 security F1): a delete and a draft that links the same
	// payment run one at a time, so no link can outlive its payment.
	if _, err := lockIssuing(tx); err != nil {
		respondInvoicingInternal(c, tx, "creating invoice", err)
		return
	}
	payments, g, err := checkInvoiceablePayments(tx, req.PaymentIDs, 0)
	if err != nil {
		respondInvoicingInternal(c, tx, "creating invoice", err)
		return
	}
	if g != nil {
		respondGuard(c, tx, g)
		return
	}
	klein, err := profileIsKleinunternehmer(tx)
	if err != nil {
		respondInvoicingInternal(c, tx, "creating invoice", err)
		return
	}

	inv := data_models.InvoicingInvoice{Status: data_models.InvoiceStatusDraft, VatRateBP: 1900, Currency: payments[0].Currency}
	sameClient := payments[0].ClientID
	earliest, latest := payments[0].PaidOn, payments[0].PaidOn
	for _, p := range payments[1:] {
		if sameClient != nil && (p.ClientID == nil || *p.ClientID != *sameClient) {
			sameClient = nil
		}
		if p.PaidOn.Before(earliest) {
			earliest = p.PaidOn
		}
		if p.PaidOn.After(latest) {
			latest = p.PaidOn
		}
	}
	inv.ClientID = sameClient
	inv.ServiceDateFrom, inv.ServiceDateTo = &earliest, &latest

	items := make([]data_models.InvoicingLineItem, 0, len(payments))
	for i, p := range payments {
		items = append(items, data_models.InvoicingLineItem{
			Position:       i + 1,
			Description:    p.Note,
			QuantityMilli:  1000,
			UnitPriceCents: p.GrossCents,
			LineTotalCents: p.GrossCents,
		})
	}
	recomputeTotals(&inv, items, klein)
	if err := tx.Create(&inv).Error; err != nil {
		respondInvoicingInternal(c, tx, "creating invoice", err)
		return
	}
	for i := range items {
		items[i].InvoiceID = inv.ID
		if err := tx.Create(&items[i]).Error; err != nil {
			respondInvoicingInternal(c, tx, "creating invoice", err)
			return
		}
	}
	for _, p := range payments {
		if err := tx.Create(&data_models.InvoicingInvoicePayment{InvoiceID: inv.ID, PaymentID: p.ID}).Error; err != nil {
			respondInvoicingInternal(c, tx, "creating invoice", err)
			return
		}
	}
	if err := data_models.RecordInvoicingAuditEvent(tx, invoicingUserID(c), invoicingActionCreate, invoicingEntityInvoice, inv.ID,
		auditDetails(gin.H{"payment_ids": req.PaymentIDs})); err != nil {
		respondInvoicingInternal(c, tx, "creating invoice", err)
		return
	}
	if err := tx.Commit().Error; err != nil {
		respondInvoicingInternal(c, nil, "creating invoice", err)
		return
	}
	detail, err := loadInvoiceDetail(db, inv)
	if err != nil {
		respondInvoicingInternal(c, nil, "loading invoice", err)
		return
	}
	c.JSON(http.StatusCreated, detail)
}

func parseOptionalDate(raw json.RawMessage) (*time.Time, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("must be a YYYY-MM-DD string or null")
	}
	t, err := time.Parse(invoicingDateLayout, s)
	if err != nil {
		return nil, fmt.Errorf("must be a YYYY-MM-DD date")
	}
	return &t, nil
}

type lineItemInput struct {
	Description    string `json:"description"`
	QuantityMilli  int64  `json:"quantity_milli"`
	UnitPriceCents int64  `json:"unit_price_cents"`
}

// UpdateInvoiceDraftHandler edits a draft. Fields left out are unchanged;
// line_items and payment_ids replace the whole list when given. Issued and
// cancelled invoices refuse every edit.
func UpdateInvoiceDraftHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	id, ok := parseInvoiceID(c)
	if !ok {
		return
	}
	var body map[string]json.RawMessage
	if err := c.ShouldBindJSON(&body); err != nil || len(body) == 0 {
		RespondWithError(c, http.StatusBadRequest, "Send at least one draft field")
		return
	}
	if g := checkFieldAllowlist(body, draftEditableFields); g != nil {
		respondGuard(c, nil, g)
		return
	}

	tx := db.Begin()
	// A payment_ids edit links payments, so it takes the issuing lock like a
	// draft create (#8409 security F1), and before the row lock, in the same
	// order the issue handler takes them.
	if _, linksPayments := body["payment_ids"]; linksPayments {
		if _, err := lockIssuing(tx); err != nil {
			respondInvoicingInternal(c, tx, "updating invoice", err)
			return
		}
	}
	// Wait for any issue of this invoice in flight, then read its
	// committed status: a draft check must not race the issue.
	if _, err := lockInvoiceRow(tx, id); err != nil {
		respondInvoicingInternal(c, tx, "updating invoice", err)
		return
	}
	inv, err := data_models.GetInvoicingInvoice(tx, id)
	if err != nil {
		respondInvoicingInternal(c, tx, "updating invoice", err)
		return
	}
	if inv == nil {
		tx.Rollback()
		RespondWithError(c, http.StatusNotFound, "Invoice not found")
		return
	}
	if inv.Status != data_models.InvoiceStatusDraft {
		respondGuard(c, tx, guard(http.StatusConflict, "not_a_draft", "invoice %d is %s and can no longer be changed", inv.ID, inv.Status))
		return
	}
	bad := func(field string, err error) {
		respondGuard(c, tx, guard(http.StatusBadRequest, "invalid_field", "%s %v", field, err))
	}

	if raw, ok := body["client_id"]; ok {
		cid, err := optionalID(raw)
		if err != nil {
			bad("client_id", err)
			return
		}
		if cid != nil {
			cl, err := data_models.GetInvoicingClient(tx, *cid)
			if err != nil {
				respondInvoicingInternal(c, tx, "updating invoice", err)
				return
			}
			if cl == nil {
				bad("client_id", fmt.Errorf("does not name a client"))
				return
			}
		}
		inv.ClientID = cid
	}
	for _, f := range []string{"service_date_from", "service_date_to"} {
		raw, ok := body[f]
		if !ok {
			continue
		}
		d, err := parseOptionalDate(raw)
		if err != nil {
			bad(f, err)
			return
		}
		if f == "service_date_from" {
			inv.ServiceDateFrom = d
		} else {
			inv.ServiceDateTo = d
		}
	}
	if raw, ok := body["vat_rate_bp"]; ok {
		var bp int
		if err := json.Unmarshal(raw, &bp); err != nil || bp < 0 || bp > 10000 || bp%10 != 0 {
			bad("vat_rate_bp", fmt.Errorf("must be basis points between 0 and 10000, in steps of 10"))
			return
		}
		inv.VatRateBP = bp
	}

	items, err := data_models.ListInvoiceLineItems(tx, inv.ID)
	if err != nil {
		respondInvoicingInternal(c, tx, "updating invoice", err)
		return
	}
	if raw, ok := body["line_items"]; ok {
		var in []lineItemInput
		if err := json.Unmarshal(raw, &in); err != nil {
			bad("line_items", fmt.Errorf("must be a list of {description, quantity_milli, unit_price_cents}"))
			return
		}
		items = items[:0]
		for i, li := range in {
			if li.QuantityMilli < 0 || li.UnitPriceCents < 0 {
				bad("line_items", fmt.Errorf("item %d has a negative quantity or price", i+1))
				return
			}
			// Bounded so quantity × price stays far inside int64
			// (@security N1, c51467): 10,000 units, 100 million euros.
			if li.QuantityMilli > maxQuantityMilli || li.UnitPriceCents > maxUnitPriceCents {
				bad("line_items", fmt.Errorf("item %d exceeds 10000 units or 100000000 euros", i+1))
				return
			}
			items = append(items, data_models.InvoicingLineItem{
				InvoiceID:      inv.ID,
				Position:       i + 1,
				Description:    strings.TrimSpace(li.Description),
				QuantityMilli:  li.QuantityMilli,
				UnitPriceCents: li.UnitPriceCents,
				LineTotalCents: data_models.LineTotalCents(li.QuantityMilli, li.UnitPriceCents),
			})
		}
		if err := tx.Where("invoice_id = ?", inv.ID).Delete(&data_models.InvoicingLineItem{}).Error; err != nil {
			respondInvoicingInternal(c, tx, "updating invoice", err)
			return
		}
		for i := range items {
			items[i].ID = 0
			if err := tx.Create(&items[i]).Error; err != nil {
				respondInvoicingInternal(c, tx, "updating invoice", err)
				return
			}
		}
	}
	if raw, ok := body["payment_ids"]; ok {
		var ids []uint
		if err := json.Unmarshal(raw, &ids); err != nil {
			bad("payment_ids", fmt.Errorf("must be a list of ids"))
			return
		}
		if _, g, err := checkInvoiceablePayments(tx, ids, inv.ID); err != nil {
			respondInvoicingInternal(c, tx, "updating invoice", err)
			return
		} else if g != nil {
			respondGuard(c, tx, g)
			return
		}
		if err := tx.Where("invoice_id = ?", inv.ID).Delete(&data_models.InvoicingInvoicePayment{}).Error; err != nil {
			respondInvoicingInternal(c, tx, "updating invoice", err)
			return
		}
		for _, pid := range ids {
			if err := tx.Create(&data_models.InvoicingInvoicePayment{InvoiceID: inv.ID, PaymentID: pid}).Error; err != nil {
				respondInvoicingInternal(c, tx, "updating invoice", err)
				return
			}
		}
	}

	klein, err := profileIsKleinunternehmer(tx)
	if err != nil {
		respondInvoicingInternal(c, tx, "updating invoice", err)
		return
	}
	recomputeTotals(inv, items, klein)
	if err := tx.Save(inv).Error; err != nil {
		respondInvoicingInternal(c, tx, "updating invoice", err)
		return
	}
	changed := make([]string, 0, len(body))
	for k := range body {
		changed = append(changed, k)
	}
	sort.Strings(changed)
	if err := data_models.RecordInvoicingAuditEvent(tx, invoicingUserID(c), invoicingActionUpdate, invoicingEntityInvoice, inv.ID,
		auditDetails(gin.H{"fields": changed, "body": body})); err != nil {
		respondInvoicingInternal(c, tx, "updating invoice", err)
		return
	}
	if err := tx.Commit().Error; err != nil {
		respondInvoicingInternal(c, nil, "updating invoice", err)
		return
	}
	detail, err := loadInvoiceDetail(db, *inv)
	if err != nil {
		respondInvoicingInternal(c, nil, "loading invoice", err)
		return
	}
	c.JSON(http.StatusOK, detail)
}

// DeleteInvoiceDraftHandler removes a draft. An issued or cancelled invoice is
// never deleted (spec §4.6); it is cancelled instead.
func DeleteInvoiceDraftHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	id, ok := parseInvoiceID(c)
	if !ok {
		return
	}
	tx := db.Begin()
	// Wait for any issue of this invoice in flight, then read its
	// committed status: a draft check must not race the issue.
	if _, err := lockInvoiceRow(tx, id); err != nil {
		respondInvoicingInternal(c, tx, "deleting invoice", err)
		return
	}
	inv, err := data_models.GetInvoicingInvoice(tx, id)
	if err != nil {
		respondInvoicingInternal(c, tx, "deleting invoice", err)
		return
	}
	if inv == nil {
		tx.Rollback()
		RespondWithError(c, http.StatusNotFound, "Invoice not found")
		return
	}
	if inv.Status != data_models.InvoiceStatusDraft {
		respondGuard(c, tx, guard(http.StatusConflict, "not_a_draft", "invoice %d is %s; issued invoices are never deleted, cancel it instead", inv.ID, inv.Status))
		return
	}
	for _, m := range []interface{}{&data_models.InvoicingLineItem{}, &data_models.InvoicingInvoicePayment{}} {
		if err := tx.Where("invoice_id = ?", inv.ID).Delete(m).Error; err != nil {
			respondInvoicingInternal(c, tx, "deleting invoice", err)
			return
		}
	}
	if err := tx.Delete(inv).Error; err != nil {
		respondInvoicingInternal(c, tx, "deleting invoice", err)
		return
	}
	if err := data_models.RecordInvoicingAuditEvent(tx, invoicingUserID(c), invoicingActionDelete, invoicingEntityInvoice, inv.ID, ""); err != nil {
		respondInvoicingInternal(c, tx, "deleting invoice", err)
		return
	}
	if err := tx.Commit().Error; err != nil {
		respondInvoicingInternal(c, nil, "deleting invoice", err)
		return
	}
	c.Status(http.StatusNoContent)
}

// GetInvoiceHandler returns one invoice with its line items, payments and
// warnings.
func GetInvoiceHandler(c *gin.Context) {
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
	detail, err := loadInvoiceDetail(db, *inv)
	if err != nil {
		respondInvoicingInternal(c, nil, "loading invoice", err)
		return
	}
	c.JSON(http.StatusOK, detail)
}

type invoiceListRow struct {
	data_models.InvoicingInvoice
	ClientName string `json:"client_name"`
}

// ListInvoicesHandler lists invoices newest first, filtered by ?status=,
// ?issued_from= / ?issued_to= (issue date, inclusive), ?client_id= and ?q=
// (the shown client name contains, case-insensitive). The shown name is the
// one frozen at issue for a non-draft invoice, so ?q= is matched after it is
// chosen rather than against the live client row.
func ListInvoicesHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	q := db.Table("invoicing_invoices").
		Select("invoicing_invoices.*, COALESCE(invoicing_clients.name, '') AS client_name").
		Joins("LEFT JOIN invoicing_clients ON invoicing_clients.id = invoicing_invoices.client_id").
		Order("invoicing_invoices.issue_date desc, invoicing_invoices.id desc")
	if s := c.Query("status"); s != "" {
		if s != data_models.InvoiceStatusDraft && s != data_models.InvoiceStatusIssued && s != data_models.InvoiceStatusCancelled {
			RespondWithError(c, http.StatusBadRequest, "status must be draft, issued or cancelled")
			return
		}
		q = q.Where("invoicing_invoices.status = ?", s)
	}
	for param, op := range map[string]string{"issued_from": ">=", "issued_to": "<="} {
		if v := c.Query(param); v != "" {
			d, err := time.Parse(invoicingDateLayout, v)
			if err != nil {
				RespondWithError(c, http.StatusBadRequest, param+" must be YYYY-MM-DD")
				return
			}
			q = q.Where("invoicing_invoices.issue_date "+op+" ?", d)
		}
	}
	if v := c.Query("client_id"); v != "" {
		cid, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			RespondWithError(c, http.StatusBadRequest, "Invalid client_id")
			return
		}
		q = q.Where("invoicing_invoices.client_id = ?", cid)
	}
	var scanned []invoiceListRow
	if err := q.Scan(&scanned).Error; err != nil {
		respondInvoicingInternal(c, nil, "listing invoices", err)
		return
	}
	search := strings.ToLower(strings.TrimSpace(c.Query("q")))
	rows := []invoiceListRow{}
	for _, row := range scanned {
		row.ClientName = invoiceClientName(row.InvoicingInvoice, row.ClientName)
		if search != "" && !strings.Contains(strings.ToLower(row.ClientName), search) {
			continue
		}
		rows = append(rows, row)
	}
	c.JSON(http.StatusOK, gin.H{"invoices": rows})
}
