package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Issuing and Storno for the invoicing app (system_3 #8367-B2). Issuing is
// the one place an invoice gets its number and its date, and both come from
// the server (spec §4.6). The §10 guardrails are checked here, inside the
// issuing transaction, so no client can skip them.

const (
	invoicingActionIssue  = "issue"
	invoicingActionCancel = "cancel"
)

// invoicingNow is the clock issuing reads. Tests swap it.
var invoicingNow = func() time.Time { return time.Now() }

// berlinToday is the issue date: the calendar day in Europe/Berlin (spec §6),
// as a UTC-midnight date like every other date column.
func berlinToday() time.Time {
	t := invoicingNow().In(data_models.BerlinLocation())
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// checkIssuable runs every guardrail that does not depend on the number. It
// returns the profile and client to snapshot.
func checkIssuable(tx *gorm.DB, inv *data_models.InvoicingInvoice, items []data_models.InvoicingLineItem, issueDate time.Time) (*data_models.InvoicingProfile, *data_models.InvoicingClient, *invoicingGuardError, error) {
	if inv.Status != data_models.InvoiceStatusDraft {
		return nil, nil, guard(http.StatusConflict, "not_a_draft", "invoice %d is already %s", inv.ID, inv.Status), nil
	}

	profile, err := data_models.GetInvoicingProfile(tx)
	if err != nil {
		return nil, nil, nil, err
	}
	if profile == nil || profile.FullName == "" || profile.Address == "" || strings.TrimSpace(profile.Steuernummer) == "" {
		return nil, nil, guard(http.StatusUnprocessableEntity, "profile_incomplete",
			"the issuer profile needs a name, an address and a Steuernummer before an invoice can be issued"), nil
	}

	if inv.ClientID == nil {
		return nil, nil, guard(http.StatusUnprocessableEntity, "client_incomplete", "the invoice has no client"), nil
	}
	client, err := data_models.GetInvoicingClient(tx, *inv.ClientID)
	if err != nil {
		return nil, nil, nil, err
	}
	if client == nil || !client.IsInvoiceable() {
		return nil, nil, guard(http.StatusUnprocessableEntity, "client_incomplete", "the client needs a name and a postal address"), nil
	}

	if len(items) == 0 {
		return nil, nil, guard(http.StatusUnprocessableEntity, "line_items_incomplete", "the invoice has no line items"), nil
	}
	for _, it := range items {
		if strings.TrimSpace(it.Description) == "" || it.QuantityMilli <= 0 || it.UnitPriceCents <= 0 {
			return nil, nil, guard(http.StatusUnprocessableEntity, "line_items_incomplete",
				"line item %d needs a description, a quantity above zero and a price above zero", it.Position), nil
		}
	}

	if inv.ServiceDateFrom == nil {
		return nil, nil, guard(http.StatusUnprocessableEntity, "service_date_invalid", "the invoice has no service date"), nil
	}
	end := dateOnly(*inv.ServiceDateFrom)
	if inv.ServiceDateTo != nil {
		to := dateOnly(*inv.ServiceDateTo)
		if to.Before(end) {
			return nil, nil, guard(http.StatusUnprocessableEntity, "service_date_invalid", "the service period ends before it starts"), nil
		}
		end = to
	}
	if end.After(issueDate) {
		return nil, nil, guard(http.StatusUnprocessableEntity, "service_date_invalid",
			"the service date %s is after today's issue date %s", end.Format(invoicingDateLayout), issueDate.Format(invoicingDateLayout)), nil
	}

	payments, err := data_models.ListInvoicePayments(tx, inv.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	ids := make([]uint, 0, len(payments))
	for _, p := range payments {
		ids = append(ids, p.ID)
	}
	if _, g, err := checkInvoiceablePayments(tx, ids, inv.ID); err != nil || g != nil {
		return nil, nil, g, err
	}

	latest, err := data_models.LatestIssueDate(tx)
	if err != nil {
		return nil, nil, nil, err
	}
	if latest != nil && issueDate.Before(dateOnly(*latest)) {
		return nil, nil, guard(http.StatusUnprocessableEntity, "issue_date_before_previous",
			"today (%s) is before the previous invoice's issue date (%s); check the server clock", issueDate.Format(invoicingDateLayout), latest.Format(invoicingDateLayout)), nil
	}
	return profile, client, nil, nil
}

// lockIssuing takes the issuer-profile row's lock with a no-op UPDATE, so
// every issue — and every write that could change what an issue checks —
// runs one at a time. Postgres runs at READ COMMITTED: without this lock two
// concurrent issues each read their checks before either commits, so one
// draft could be issued twice or one payment land on two invoices
// (@security F1, c51467 on #8371). The reads after the lock see every
// committed issue. It reports whether the profile row exists.
func lockIssuing(tx *gorm.DB) (bool, error) {
	res := tx.Model(&data_models.InvoicingProfile{}).Where("id = ?", 1).
		UpdateColumn("next_invoice_number", gorm.Expr("next_invoice_number"))
	return res.RowsAffected == 1, res.Error
}

// lockInvoiceRow takes the invoice row's lock with a no-op UPDATE, so an
// edit, delete, issue or cancel of the same invoice waits for any other in
// flight and then reads its committed status. It reports whether the row
// exists.
func lockInvoiceRow(tx *gorm.DB, id uint) (bool, error) {
	res := tx.Model(&data_models.InvoicingInvoice{}).Where("id = ?", id).
		UpdateColumn("status", gorm.Expr("status"))
	return res.RowsAffected == 1, res.Error
}

// nextInvoiceNumber draws the next number from the profile's counter inside
// the caller's transaction. The UPDATE takes the profile row's lock on
// Postgres, so a concurrent issue waits for this transaction and then reads
// the advanced counter: two issues cannot draw the same number. A rollback
// rolls the counter back too, so a refused issue leaves no gap.
func nextInvoiceNumber(tx *gorm.DB) (int, error) {
	res := tx.Model(&data_models.InvoicingProfile{}).Where("id = ?", 1).
		UpdateColumn("next_invoice_number", gorm.Expr("next_invoice_number + 1"))
	if res.Error != nil {
		return 0, res.Error
	}
	if res.RowsAffected != 1 {
		return 0, fmt.Errorf("issuer profile row missing")
	}
	var p data_models.InvoicingProfile
	if err := tx.Where("id = ?", 1).First(&p).Error; err != nil {
		return 0, err
	}
	return p.NextInvoiceNumber - 1, nil
}

// IssueInvoiceHandler issues a draft: guardrails, then the number and today's
// Berlin date, the profile and client snapshots, totals, and the audit row —
// all in one transaction.
func IssueInvoiceHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	id, ok := parseInvoiceID(c)
	if !ok {
		return
	}
	// Issuing takes no input: a body naming a date or a number is refused
	// rather than ignored, so nobody believes they set one.
	// ContentLength is -1 for a chunked body, so test != 0, not > 0.
	if c.Request.ContentLength != 0 {
		var body map[string]json.RawMessage
		if err := c.ShouldBindJSON(&body); err != nil {
			RespondWithError(c, http.StatusBadRequest, "Issuing takes no request body")
			return
		}
		if g := checkFieldAllowlist(body, map[string]bool{}); g != nil {
			respondGuard(c, nil, g)
			return
		}
	}

	issueDate := berlinToday()
	tx := db.Begin()
	// Locks first, reads after: profile row (serialises all issues), then
	// this invoice's row. Every check below reads committed state.
	hasProfile, err := lockIssuing(tx)
	if err != nil {
		respondInvoicingInternal(c, tx, "issuing invoice", err)
		return
	}
	exists, err := lockInvoiceRow(tx, id)
	if err != nil {
		respondInvoicingInternal(c, tx, "issuing invoice", err)
		return
	}
	if !exists {
		tx.Rollback()
		RespondWithError(c, http.StatusNotFound, "Invoice not found")
		return
	}
	if !hasProfile {
		respondGuard(c, tx, guard(http.StatusUnprocessableEntity, "profile_incomplete",
			"the issuer profile needs a name, an address and a Steuernummer before an invoice can be issued"))
		return
	}
	inv, err := data_models.GetInvoicingInvoice(tx, id)
	if err != nil {
		respondInvoicingInternal(c, tx, "issuing invoice", err)
		return
	}
	items, err := data_models.ListInvoiceLineItems(tx, inv.ID)
	if err != nil {
		respondInvoicingInternal(c, tx, "issuing invoice", err)
		return
	}
	profile, client, g, err := checkIssuable(tx, inv, items, issueDate)
	if err != nil {
		respondInvoicingInternal(c, tx, "issuing invoice", err)
		return
	}
	if g != nil {
		respondGuard(c, tx, g)
		return
	}

	number, err := nextInvoiceNumber(tx)
	if err != nil {
		respondInvoicingInternal(c, tx, "issuing invoice", err)
		return
	}
	invoiceNumber := fmt.Sprintf("%s-%03d", profile.InvoiceNumberPrefix, number)
	now := time.Now().UTC()
	userID := invoicingUserID(c)

	recomputeTotals(inv, items, profile.IsKleinunternehmer)
	inv.Status = data_models.InvoiceStatusIssued
	inv.Prefix = profile.InvoiceNumberPrefix
	inv.Number = &number
	inv.InvoiceNumber = &invoiceNumber
	inv.IssueDate = &issueDate
	inv.ProfileSnapshot = auditDetails(profile)
	inv.ClientSnapshot = auditDetails(client)
	inv.IssuedAt = &now
	inv.IssuedBy = userID
	inv.UpdatedAt = now
	// The transition is conditional on the row still being a draft, so even
	// a path that skipped the locks above could not issue an invoice twice.
	res := tx.Model(&data_models.InvoicingInvoice{}).
		Where("id = ? AND status = ?", inv.ID, data_models.InvoiceStatusDraft).
		Updates(map[string]interface{}{
			"status": inv.Status, "prefix": inv.Prefix, "number": number, "invoice_number": invoiceNumber,
			"issue_date": issueDate, "net_cents": inv.NetCents, "vat_cents": inv.VatCents, "total_cents": inv.TotalCents,
			"profile_snapshot": inv.ProfileSnapshot, "client_snapshot": inv.ClientSnapshot,
			"issued_at": now, "issued_by": userID, "updated_at": now,
		})
	if res.Error != nil {
		respondInvoicingInternal(c, tx, "issuing invoice", res.Error)
		return
	}
	if res.RowsAffected != 1 {
		respondGuard(c, tx, guard(http.StatusConflict, "not_a_draft", "invoice %d is no longer a draft", inv.ID))
		return
	}
	if err := data_models.RecordInvoicingAuditEvent(tx, userID, invoicingActionIssue, invoicingEntityInvoice, inv.ID,
		auditDetails(gin.H{"invoice_number": invoiceNumber, "issue_date": issueDate.Format(invoicingDateLayout), "total_cents": inv.TotalCents})); err != nil {
		respondInvoicingInternal(c, tx, "issuing invoice", err)
		return
	}
	if err := tx.Commit().Error; err != nil {
		respondInvoicingInternal(c, nil, "issuing invoice", err)
		return
	}
	detail, err := loadInvoiceDetail(db, *inv)
	if err != nil {
		respondInvoicingInternal(c, nil, "loading invoice", err)
		return
	}
	c.JSON(http.StatusOK, detail)
}

type cancelInvoiceRequest struct {
	Reason string `json:"reason"`
}

// CancelInvoiceHandler is the Storno: an issued invoice is marked cancelled
// and kept on record; its payments become free for a corrected invoice.
func CancelInvoiceHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	id, ok := parseInvoiceID(c)
	if !ok {
		return
	}
	var req cancelInvoiceRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Reason) == "" {
		RespondWithError(c, http.StatusBadRequest, "A cancellation needs a reason")
		return
	}
	tx := db.Begin()
	if _, err := lockInvoiceRow(tx, id); err != nil {
		respondInvoicingInternal(c, tx, "cancelling invoice", err)
		return
	}
	inv, err := data_models.GetInvoicingInvoice(tx, id)
	if err != nil {
		respondInvoicingInternal(c, tx, "cancelling invoice", err)
		return
	}
	if inv == nil {
		tx.Rollback()
		RespondWithError(c, http.StatusNotFound, "Invoice not found")
		return
	}
	if inv.Status != data_models.InvoiceStatusIssued {
		respondGuard(c, tx, guard(http.StatusConflict, "not_issued", "only an issued invoice can be cancelled; invoice %d is %s", inv.ID, inv.Status))
		return
	}
	now := time.Now().UTC()
	userID := invoicingUserID(c)
	inv.Status = data_models.InvoiceStatusCancelled
	inv.CancelledAt = &now
	inv.CancelledBy = userID
	inv.CancelReason = strings.TrimSpace(req.Reason)
	res := tx.Model(&data_models.InvoicingInvoice{}).
		Where("id = ? AND status = ?", inv.ID, data_models.InvoiceStatusIssued).
		Updates(map[string]interface{}{"status": inv.Status, "cancelled_at": now, "cancelled_by": userID, "cancel_reason": inv.CancelReason, "updated_at": now})
	if res.Error != nil {
		respondInvoicingInternal(c, tx, "cancelling invoice", res.Error)
		return
	}
	if res.RowsAffected != 1 {
		respondGuard(c, tx, guard(http.StatusConflict, "not_issued", "invoice %d is no longer issued", inv.ID))
		return
	}
	if err := data_models.RecordInvoicingAuditEvent(tx, userID, invoicingActionCancel, invoicingEntityInvoice, inv.ID,
		auditDetails(gin.H{"invoice_number": inv.InvoiceNumber, "reason": inv.CancelReason})); err != nil {
		respondInvoicingInternal(c, tx, "cancelling invoice", err)
		return
	}
	if err := tx.Commit().Error; err != nil {
		respondInvoicingInternal(c, nil, "cancelling invoice", err)
		return
	}
	detail, err := loadInvoiceDetail(db, *inv)
	if err != nil {
		respondInvoicingInternal(c, nil, "loading invoice", err)
		return
	}
	c.JSON(http.StatusOK, detail)
}
