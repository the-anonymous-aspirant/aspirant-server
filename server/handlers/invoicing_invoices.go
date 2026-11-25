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
// (invoicing_issue.go). Behind the owner+Admin gate on the /invoicing group.

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
	Details map[string]interface{}
}

func (e *invoicingGuardError) Error() string { return e.Message }

func guard(status int, code, format string, args ...interface{}) *invoicingGuardError {
	return &invoicingGuardError{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// with adds a detail the client words its sentence from (system_3 #12274),
// in key, value pairs.
func (e *invoicingGuardError) with(kv ...interface{}) *invoicingGuardError {
	if e.Details == nil {
		e.Details = map[string]interface{}{}
	}
	for i := 0; i+1 < len(kv); i += 2 {
		e.Details[kv[i].(string)] = kv[i+1]
	}
	return e
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
func checkInvoiceablePayments(tx *gorm.DB, owner uint, ids []uint, invoiceID uint) ([]data_models.InvoicingPayment, *invoicingGuardError, error) {
	if len(ids) == 0 {
		return nil, guard(http.StatusUnprocessableEntity, "payment_not_paid_work", "an invoice needs at least one paid-work payment").
			with("reason", "no_payments"), nil
	}
	// Lock every listed payment row before reading it, in ascending id order
	// so two drafts cannot deadlock: a concurrent delete of a hand-entered
	// payment then either waits for this link or has already removed the
	// row, which this check then refuses (#8409 security F1). This holds
	// even before the issuer profile, whose row the issuing lock uses, exists.
	ordered := append([]uint(nil), ids...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	for _, id := range ordered {
		if err := lockPaymentRow(tx, owner, id); err != nil {
			return nil, nil, err
		}
	}
	seen := map[uint]bool{}
	var out []data_models.InvoicingPayment
	for _, id := range ids {
		if seen[id] {
			return nil, guard(http.StatusBadRequest, codePaymentListedTwice, "payment %d is listed twice", id), nil
		}
		seen[id] = true
		p, err := data_models.GetInvoicingPayment(tx, owner, id)
		if err != nil {
			return nil, nil, err
		}
		if p == nil {
			return nil, guard(http.StatusUnprocessableEntity, "payment_not_found", "payment %d does not exist", id).
				with("payment_id", id), nil
		}
		// A removed payment counts nowhere until restored, Create invoice
		// included (§3.143 Decision 6).
		if p.RemovedAt != nil {
			return nil, guard(http.StatusUnprocessableEntity, "payment_removed",
				"payment %s is removed; restore it before invoicing it", paymentLabel(*p)).with("payment_id", p.ID), nil
		}
		if p.Kind != data_models.PaymentKindIncoming || p.Category != data_models.PaymentCategoryPaidWork {
			return nil, guard(http.StatusUnprocessableEntity, "payment_not_paid_work",
				"payment %s is %q, not paid work; only paid-work payments can be invoiced", paymentLabel(*p), p.Category).
				with("reason", "not_paid_work", "payment_id", p.ID, "category", p.Category), nil
		}
		// A row stored as paid work before the import rule (system_3 #9881)
		// still cannot be invoiced: the PayPal payments it sums are.
		if isPayPalTransferIntoN26(*p) {
			return nil, guard(http.StatusUnprocessableEntity, "payment_paypal_transfer",
				"payment %s is a transfer from PayPal into N26; invoice the PayPal payments it sums instead", paymentLabel(*p)).
				with("payment_id", p.ID), nil
		}
		other, err := data_models.IssuedInvoiceForPayment(tx, p.ID, invoiceID)
		if err != nil {
			return nil, nil, err
		}
		if other != 0 {
			g := guard(http.StatusUnprocessableEntity, "payment_already_invoiced",
				"payment %s is already on issued invoice %d", paymentLabel(*p), other).with("payment_id", p.ID, "invoice_id", other)
			onIt, err := data_models.GetInvoicingInvoice(tx, owner, other)
			if err != nil {
				return nil, nil, err
			}
			if onIt != nil && onIt.InvoiceNumber != nil {
				g.with("invoice_number", *onIt.InvoiceNumber)
			}
			return nil, g, nil
		}
		out = append(out, *p)
	}
	return out, nil, nil
}

// mixedCurrencies refuses payments that are not all in one currency, the
// draft's own (draftCurrency, empty for a new draft) included (system_3
// #12258). An invoice holds one currency, because a total never adds two. A
// draft in one currency other than euros stays allowed: its PDF prints its
// own currency. Create, the draft edit and issue call it; Generate does not,
// since it makes one draft per payment.
func mixedCurrencies(payments []data_models.InvoicingPayment, draftCurrency string) *invoicingGuardError {
	seen := map[string]bool{}
	if draftCurrency != "" {
		seen[draftCurrency] = true
	}
	for _, p := range payments {
		seen[p.Currency] = true
	}
	if len(seen) < 2 {
		return nil
	}
	codes := make([]string, 0, len(seen))
	for c := range seen {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	return guard(http.StatusUnprocessableEntity, "mixed_currencies",
		"the payments are in different currencies (%s); an invoice holds one currency, so invoice them separately", strings.Join(codes, ", ")).
		with("currencies", codes)
}

// refuseDraftedPayments refuses a payment that a draft other than invoiceID
// covers (system_3 #12263): a payment is on at most one live draft,
// whichever button made it. It runs where a draft is made or gains a
// payment, after checkInvoiceablePayments, and never at issue: pairs of
// drafts over one payment made before this rule must each stay issuable,
// so one of a pair can still be issued and the other then refused there.
// keep holds the ids already on invoiceID, which an edit may send again.
func refuseDraftedPayments(tx *gorm.DB, owner uint, ids []uint, invoiceID uint, keep map[uint]bool) (*invoicingGuardError, error) {
	var check []uint
	for _, id := range ids {
		if !keep[id] {
			check = append(check, id)
		}
	}
	if len(check) == 0 {
		return nil, nil
	}
	var rows []struct {
		PaymentID uint
		InvoiceID uint
	}
	// The owner is matched on the invoice, so another book's draft is never
	// named (as LiveInvoicesForPayments, security c62302).
	err := tx.Table("invoicing_invoice_payments").
		Select("invoicing_invoice_payments.payment_id, i.id AS invoice_id").
		Joins("JOIN invoicing_invoices i ON i.id = invoicing_invoice_payments.invoice_id").
		Where("i.owner_user_id = ? AND i.status = ? AND i.id <> ? AND invoicing_invoice_payments.payment_id IN (?)",
			owner, data_models.InvoiceStatusDraft, invoiceID, check).
		Order("i.id asc").Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	draftOf := map[uint]uint{}
	for _, r := range rows {
		if _, ok := draftOf[r.PaymentID]; !ok {
			draftOf[r.PaymentID] = r.InvoiceID
		}
	}
	for _, id := range check {
		if d, ok := draftOf[id]; ok {
			p, err := data_models.GetInvoicingPayment(tx, owner, id)
			if err != nil {
				return nil, err
			}
			label := fmt.Sprintf("#%d", id)
			if p != nil {
				label = paymentLabel(*p)
			}
			return guard(http.StatusUnprocessableEntity, "payment_on_draft",
				"payment %s is already on draft %d", label, d), nil
		}
	}
	return nil, nil
}

// recomputeTotals sets net, VAT and total from the line items.
func recomputeTotals(inv *data_models.InvoicingInvoice, items []data_models.InvoicingLineItem, kleinunternehmer bool) {
	var sum int64
	for _, it := range items {
		sum += it.LineTotalCents
	}
	totalsFromLines(inv, sum, kleinunternehmer)
}

// totalsFromLines prices an invoice from what was received (system_3 #12261,
// ruling 2): the lines are gross, so their sum is the total. With VAT on, the
// VAT is the share of it at the invoice's rate, total × bp / (10000 + bp)
// rounded half away from zero, and the net is the rest. A
// Kleinunternehmerin charges none. Adding VAT on top of a net would ask for
// more than was paid, and some totals (100,01 at 19 %) no whole-cent net
// reaches at all.
func totalsFromLines(inv *data_models.InvoicingInvoice, sum int64, kleinunternehmer bool) {
	inv.NetCents, inv.VatCents, inv.TotalCents = sum, 0, sum
	if !kleinunternehmer {
		inv.VatCents = vatShareOfGross(sum, int64(inv.VatRateBP))
		inv.NetCents = sum - inv.VatCents
	}
}

// vatShareOfGross is gross × bp / (10000 + bp), rounded half away from zero.
func vatShareOfGross(gross, bp int64) int64 {
	d := 10000 + bp
	p := gross * bp
	if p >= 0 {
		return (2*p + d) / (2 * d)
	}
	return -((-2*p + d) / (2 * d))
}

// draftTotalsNow works a draft's net, VAT and total out again from its lines
// and the profile's current setting (system_3 #12261, ruling 1). A draft
// shows what issuing would print, never a total stored under an earlier
// setting. An issued or cancelled invoice keeps what it was issued with. It
// writes nothing.
func draftTotalsNow(db *gorm.DB, owner uint, inv *data_models.InvoicingInvoice, items []data_models.InvoicingLineItem) error {
	if inv.Status != data_models.InvoiceStatusDraft {
		return nil
	}
	klein, err := profileIsKleinunternehmer(db, owner)
	if err != nil {
		return err
	}
	recomputeTotals(inv, items, klein)
	return nil
}

func profileIsKleinunternehmer(tx *gorm.DB, owner uint) (bool, error) {
	p, err := data_models.GetInvoicingProfile(tx, owner)
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
	// A draft whose last payment was deleted (system_3 #10009) keeps its line
	// items, so it must say its money is no longer covered: the mismatch
	// counts a draft with no payments too.
	if (len(payments) > 0 || inv.Status == data_models.InvoiceStatusDraft) && sum != inv.TotalCents {
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
	// RestorablePaymentIDs, on a draft that covers no payment, are the
	// removed payments a restore would put back on it (system_3 #12318
	// review point 4): its line names "Restore" only when one exists.
	// Empty on every other invoice.
	RestorablePaymentIDs []uint `json:"restorable_payment_ids"`
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

func loadInvoiceDetail(db *gorm.DB, owner uint, inv data_models.InvoicingInvoice) (*invoiceDetail, error) {
	items, err := data_models.ListInvoiceLineItems(db, inv.ID)
	if err != nil {
		return nil, err
	}
	// A numbered invoice lists its own copy of the payments, which stays
	// after a payment is edited or deleted for good (system_3 #12266).
	payments, err := data_models.ListInvoiceRecordPayments(db, inv)
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []data_models.InvoicingLineItem{}
	}
	if err := draftTotalsNow(db, owner, &inv, items); err != nil {
		return nil, err
	}
	if payments == nil {
		payments = []data_models.InvoicingPayment{}
	}
	live := ""
	if inv.ClientID != nil {
		cl, err := data_models.GetInvoicingClient(db, owner, *inv.ClientID)
		if err != nil {
			return nil, err
		}
		if cl != nil {
			live = cl.Name
		}
	}
	restorable := []uint{}
	if inv.Status == data_models.InvoiceStatusDraft && len(payments) == 0 {
		if restorable, err = restorablePaymentIDs(db, owner, inv); err != nil {
			return nil, err
		}
	}
	return &invoiceDetail{Invoice: inv, LineItems: items, Payments: payments, Warnings: invoiceWarnings(inv, payments),
		ClientName: invoiceClientName(inv, live), RestorablePaymentIDs: restorable}, nil
}

func respondGuard(c *gin.Context, tx *gorm.DB, g *invoicingGuardError) {
	if tx != nil {
		tx.Rollback()
	}
	log.Printf("Error response: %d - %s - %s", g.Status, g.Code, g.Message)
	c.JSON(g.Status, ErrorResponse{Error: ErrorDetail{Code: g.Code, Message: g.Message, Details: g.Details}})
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

// createDraftFromPayments writes one draft over the given payments, inside the
// caller's transaction: one line per payment (its note as the description,
// quantity 1, the amount as the price), the client when every payment names
// the same one, the payment dates as the service dates, the links, and the
// audit event. The payments must already have passed checkInvoiceablePayments.
// Shared by Create invoice and Generate invoices (system_3 #11504), so a
// generated draft is prefilled exactly as a hand-started one is.
func createDraftFromPayments(tx *gorm.DB, owner uint, payments []data_models.InvoicingPayment, kleinunternehmer bool, audit gin.H) (data_models.InvoicingInvoice, error) {
	inv := data_models.InvoicingInvoice{OwnerUserID: owner, Status: data_models.InvoiceStatusDraft, VatRateBP: 1900, Currency: payments[0].Currency}
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
	recomputeTotals(&inv, items, kleinunternehmer)
	if err := tx.Create(&inv).Error; err != nil {
		return inv, err
	}
	for i := range items {
		items[i].InvoiceID = inv.ID
		if err := tx.Create(&items[i]).Error; err != nil {
			return inv, err
		}
	}
	for _, p := range payments {
		if err := tx.Create(&data_models.InvoicingInvoicePayment{InvoiceID: inv.ID, PaymentID: p.ID}).Error; err != nil {
			return inv, err
		}
	}
	err := data_models.RecordInvoicingAuditEvent(tx, owner, invoicingActionCreate, invoicingEntityInvoice, inv.ID, auditDetails(audit))
	return inv, err
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
	defer tx.RollbackUnlessCommitted()
	// Linking payments takes the issuing lock, the same one a payment delete
	// takes (#8409 security F1): a delete and a draft that links the same
	// payment run one at a time, so no link can outlive its payment.
	if _, err := lockIssuing(tx, invoicingUserID(c)); err != nil {
		respondInvoicingInternal(c, tx, "creating invoice", err)
		return
	}
	payments, g, err := checkInvoiceablePayments(tx, invoicingUserID(c), req.PaymentIDs, 0)
	if err != nil {
		respondInvoicingInternal(c, tx, "creating invoice", err)
		return
	}
	if g == nil {
		g = mixedCurrencies(payments, "")
	}
	if g == nil {
		g, err = refuseDraftedPayments(tx, invoicingUserID(c), req.PaymentIDs, 0, nil)
		if err != nil {
			respondInvoicingInternal(c, tx, "creating invoice", err)
			return
		}
	}
	if g != nil {
		respondGuard(c, tx, g)
		return
	}
	klein, err := profileIsKleinunternehmer(tx, invoicingUserID(c))
	if err != nil {
		respondInvoicingInternal(c, tx, "creating invoice", err)
		return
	}

	inv, err := createDraftFromPayments(tx, invoicingUserID(c), payments, klein, gin.H{"payment_ids": req.PaymentIDs})
	if err != nil {
		respondInvoicingInternal(c, tx, "creating invoice", err)
		return
	}
	if err := tx.Commit().Error; err != nil {
		respondInvoicingInternal(c, nil, "creating invoice", err)
		return
	}
	detail, err := loadInvoiceDetail(db, invoicingUserID(c), inv)
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

// lineRefusal is a refusal of one draft line, with its field.
type lineRefusal struct {
	refusal
	field string
}

// lineItemRefusal checks one draft line; item is its 1-based position.
// Bounded so quantity × price stays far inside int64 (@security N1,
// c51467): 10,000 units, 100 million euros.
func lineItemRefusal(li lineItemInput, item int) *lineRefusal {
	at := func(field, code string, limit int64, format string) *lineRefusal {
		// The line's position is also in details, where #12274's client
		// reads it ("Line 2: the quantity is too large.").
		return &lineRefusal{refusal{code: code, limit: limit, item: item, whole: true, message: "line_items " + fmt.Sprintf(format, item),
			details: map[string]interface{}{"position": item}}, field}
	}
	switch {
	case li.QuantityMilli < 0:
		return at("quantity_milli", codeBelowZero, 0, "item %d has a negative quantity")
	case li.UnitPriceCents < 0:
		return at("unit_price_cents", codeBelowZero, 0, "item %d has a negative price")
	case li.QuantityMilli > maxQuantityMilli:
		return at("quantity_milli", codeQuantityTooLarge, maxQuantityMilli/1000, "item %d exceeds 10000 units")
	case li.UnitPriceCents > maxUnitPriceCents:
		return at("unit_price_cents", codeAmountTooLarge, maxUnitPriceCents, "item %d exceeds 100000000 euros")
	}
	return nil
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
	defer tx.RollbackUnlessCommitted()
	// A payment_ids edit links payments, so it takes the issuing lock like a
	// draft create (#8409 security F1), and before the row lock, in the same
	// order the issue handler takes them.
	if _, linksPayments := body["payment_ids"]; linksPayments {
		if _, err := lockIssuing(tx, invoicingUserID(c)); err != nil {
			respondInvoicingInternal(c, tx, "updating invoice", err)
			return
		}
	}
	// Wait for any issue of this invoice in flight, then read its
	// committed status: a draft check must not race the issue.
	if _, err := lockInvoiceRow(tx, invoicingUserID(c), id); err != nil {
		respondInvoicingInternal(c, tx, "updating invoice", err)
		return
	}
	inv, err := data_models.GetInvoicingInvoice(tx, invoicingUserID(c), id)
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
	// One code per cause and the field it is about (system_3 #12300
	// ruling 1); the message is the one these refusals have always had.
	bad := func(field string, r *refusal) {
		tx.Rollback()
		respondFieldRefusal(c, field, r)
	}

	if raw, ok := body["client_id"]; ok {
		cid, err := optionalID(raw)
		if err != nil {
			bad("client_id", refuse(codeInvalidID, err.Error()))
			return
		}
		if cid != nil {
			cl, err := data_models.GetInvoicingClient(tx, invoicingUserID(c), *cid)
			if err != nil {
				respondInvoicingInternal(c, tx, "updating invoice", err)
				return
			}
			if cl == nil {
				bad("client_id", noSuchClient)
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
			bad(f, refuse(codeInvalidDate, err.Error()))
			return
		}
		// A service date before 2000 is a mistyped year ("26" for 2026), as
		// it is for a payment's date; it is refused before it can be issued
		// (system_3 #12312 ruling 3). A date after today stays allowed on a
		// draft: the ceiling is the issue's. It answers as a payment's date
		// does, with the field and the year (#12300).
		if d != nil && d.Before(earliestPaymentDate) {
			bad(f, refuseOver(codeDateTooEarly, int64(earliestPaymentDate.Year()), "must be 2000-01-01 or later"))
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
			bad("vat_rate_bp", refuse(codeInvalidVATRate, "must be basis points between 0 and 10000, in steps of 10"))
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
			bad("line_items", refuse(codeInvalidLineItems, "must be a list of {description, quantity_milli, unit_price_cents}"))
			return
		}
		items = items[:0]
		for i, li := range in {
			// The field is the line's own, so the editor can print the
			// sentence at that line's input (c77188 on #12300).
			if r := lineItemRefusal(li, i+1); r != nil {
				tx.Rollback()
				respondFieldRefusal(c, r.field, &r.refusal)
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
			bad("payment_ids", refuse(codeInvalidID, "must be a list of ids"))
			return
		}
		linked, g, err := checkInvoiceablePayments(tx, invoicingUserID(c), ids, inv.ID)
		if err != nil {
			respondInvoicingInternal(c, tx, "updating invoice", err)
			return
		}
		if g == nil {
			g = mixedCurrencies(linked, inv.Currency)
		}
		if g != nil {
			respondGuard(c, tx, g)
			return
		}
		// A payment this draft already lists may be sent again; one it gains
		// may not be on another draft (system_3 #12263).
		onThis, err := data_models.ListInvoicePayments(tx, inv.ID)
		if err != nil {
			respondInvoicingInternal(c, tx, "updating invoice", err)
			return
		}
		keep := map[uint]bool{}
		for _, p := range onThis {
			keep[p.ID] = true
		}
		if g, err := refuseDraftedPayments(tx, invoicingUserID(c), ids, inv.ID, keep); err != nil {
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

	klein, err := profileIsKleinunternehmer(tx, invoicingUserID(c))
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
	detail, err := loadInvoiceDetail(db, invoicingUserID(c), *inv)
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
	defer tx.RollbackUnlessCommitted()
	// Wait for any issue of this invoice in flight, then read its
	// committed status: a draft check must not race the issue.
	if _, err := lockInvoiceRow(tx, invoicingUserID(c), id); err != nil {
		respondInvoicingInternal(c, tx, "deleting invoice", err)
		return
	}
	inv, err := data_models.GetInvoicingInvoice(tx, invoicingUserID(c), id)
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
	inv, err := data_models.GetInvoicingInvoice(db, invoicingUserID(c), id)
	if err != nil {
		respondInvoicingInternal(c, nil, "loading invoice", err)
		return
	}
	if inv == nil {
		RespondWithError(c, http.StatusNotFound, "Invoice not found")
		return
	}
	detail, err := loadInvoiceDetail(db, invoicingUserID(c), *inv)
	if err != nil {
		respondInvoicingInternal(c, nil, "loading invoice", err)
		return
	}
	c.JSON(http.StatusOK, detail)
}

// listedDraftTotalsNow is draftTotalsNow for the drafts of a list, with one
// read of their line sums.
func listedDraftTotalsNow(db *gorm.DB, owner uint, rows []invoiceListRow) error {
	var drafts []uint
	for _, r := range rows {
		if r.Status == data_models.InvoiceStatusDraft {
			drafts = append(drafts, r.ID)
		}
	}
	if len(drafts) == 0 {
		return nil
	}
	klein, err := profileIsKleinunternehmer(db, owner)
	if err != nil {
		return err
	}
	var sums []struct {
		InvoiceID uint
		Sum       int64
	}
	if err := db.Table("invoicing_line_items").Select("invoice_id, COALESCE(SUM(line_total_cents), 0) AS sum").
		Where("invoice_id IN (?)", drafts).Group("invoice_id").Scan(&sums).Error; err != nil {
		return err
	}
	byID := make(map[uint]int64, len(sums))
	for _, s := range sums {
		byID[s.InvoiceID] = s.Sum
	}
	for i := range rows {
		if rows[i].Status == data_models.InvoiceStatusDraft {
			totalsFromLines(&rows[i].InvoicingInvoice, byID[rows[i].ID], klein)
		}
	}
	return nil
}

type invoiceListRow struct {
	data_models.InvoicingInvoice
	ClientName string `json:"client_name"`
	// PaymentCount is how many payments the invoice lists, so that a month's
	// review can leave out a draft that covers none (system_3 #12318 review
	// point 5). A cancelled invoice keeps its links, and its count.
	PaymentCount int `json:"payment_count"`
}

// ListInvoicesHandler lists invoices newest first, filtered by ?status=,
// ?issued_from= / ?issued_to= (issue date, inclusive), ?client_id= and ?q=
// (the shown client name contains, case-insensitive). The shown name is the
// one frozen at issue for a non-draft invoice, so ?q= is matched after it is
// chosen rather than against the live client row.
func ListInvoicesHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	// The join carries the owner too: a draft pointing at another book's client
	// id must not show that client's name or match ?q= (security c62302).
	q := db.Table("invoicing_invoices").
		Select("invoicing_invoices.*, COALESCE(invoicing_clients.name, '') AS client_name, " +
			"(SELECT COUNT(*) FROM invoicing_invoice_payments WHERE invoicing_invoice_payments.invoice_id = invoicing_invoices.id) AS payment_count").
		Joins("LEFT JOIN invoicing_clients ON invoicing_clients.id = invoicing_invoices.client_id AND invoicing_clients.owner_user_id = invoicing_invoices.owner_user_id").
		Where("invoicing_invoices.owner_user_id = ?", invoicingUserID(c)).
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
		if !data_models.InvoicingIDInRange(uint(cid)) {
			// Names no client: the empty list an unknown id answers.
			q = q.Where("1 = 0")
		} else {
			q = q.Where("invoicing_invoices.client_id = ?", cid)
		}
	}
	var scanned []invoiceListRow
	if err := q.Scan(&scanned).Error; err != nil {
		respondInvoicingInternal(c, nil, "listing invoices", err)
		return
	}
	if err := listedDraftTotalsNow(db, invoicingUserID(c), scanned); err != nil {
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
