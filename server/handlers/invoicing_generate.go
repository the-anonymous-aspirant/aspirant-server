package handlers

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Generate invoices (system_3 #11504, epic #11497 items 1 and 2): one draft
// per paid-work payment, for a month or for a list of payments. Nothing here
// issues: a draft takes its number only when the owner accepts it through
// IssueInvoiceHandler, and a rejected one is deleted without ever holding one.

const (
	invoicingMonthLayout = "2006-01"

	// maxGeneratePayments bounds one call. A month of paid work is tens of
	// payments; this only stops a request from holding the book lock for an
	// unbounded list.
	maxGeneratePayments = 500

	generateSkipOnInvoice = "already_on_invoice"
)

// generateSkip names a payment that was left alone because an invoice (a
// draft or an issued one) already covers it, and which invoice that is.
type generateSkip struct {
	PaymentID uint                          `json:"payment_id"`
	Reason    string                        `json:"reason"`
	Invoice   data_models.PaymentInvoiceRef `json:"invoice"`
}

type generateResponse struct {
	Invoices []invoiceDetail `json:"invoices"`
	Skipped  []generateSkip  `json:"skipped"`
}

// parseGenerateRequest reads exactly one of month or payment_ids.
func parseGenerateRequest(body map[string]json.RawMessage) (month string, ids []uint, byMonth bool, g *invoicingGuardError) {
	if g := checkFieldAllowlist(body, map[string]bool{"payment_ids": true, "month": true}); g != nil {
		return "", nil, false, g
	}
	rawMonth, hasMonth := body["month"]
	rawIDs, hasIDs := body["payment_ids"]
	if hasMonth == hasIDs {
		return "", nil, false, guard(http.StatusBadRequest, "invalid_field", "send exactly one of month or payment_ids")
	}
	if hasMonth {
		if err := json.Unmarshal(rawMonth, &month); err != nil {
			return "", nil, false, guard(http.StatusBadRequest, "invalid_field", "month must be a YYYY-MM string")
		}
		if t, err := time.Parse(invoicingMonthLayout, month); err != nil || t.Format(invoicingMonthLayout) != month {
			return "", nil, false, guard(http.StatusBadRequest, "invalid_field", "month must be a YYYY-MM string")
		}
		return month, nil, true, nil
	}
	if err := json.Unmarshal(rawIDs, &ids); err != nil {
		return "", nil, false, guard(http.StatusBadRequest, "invalid_field", "payment_ids must be a list of ids")
	}
	if len(ids) == 0 {
		return "", nil, false, guard(http.StatusBadRequest, "invalid_field", "payment_ids must list at least one payment")
	}
	if len(ids) > maxGeneratePayments {
		return "", nil, false, guard(http.StatusBadRequest, "invalid_field", "payment_ids lists %d payments; at most %d in one call", len(ids), maxGeneratePayments)
	}
	seen := map[uint]bool{}
	for _, id := range ids {
		if seen[id] {
			return "", nil, false, guard(http.StatusBadRequest, "duplicate_payment", "payment %d is listed twice", id)
		}
		seen[id] = true
	}
	return "", ids, false, nil
}

// monthCandidateIDs lists the owner's payments a month's generation looks at:
// incoming paid work, not removed, paid in that month. A PayPal transfer into
// N26 is left out because it can never be invoiced (system_3 #9881); the
// PayPal payments it sums are the candidates.
func monthCandidateIDs(tx *gorm.DB, owner uint, month string) ([]uint, error) {
	payments, err := data_models.ListInvoicingPayments(tx, owner, data_models.PaymentKindIncoming, data_models.PaymentCategoryPaidWork)
	if err != nil {
		return nil, err
	}
	var ids []uint
	for _, p := range payments {
		if p.PaidOn.Format(invoicingMonthLayout) != month || isPayPalTransferIntoN26(p) {
			continue
		}
		ids = append(ids, p.ID)
	}
	return ids, nil
}

// GenerateInvoiceDraftsHandler creates one draft per payment, for
// {"month":"YYYY-MM"} or {"payment_ids":[…]}.
//
// A payment an invoice already covers (a draft or an issued one; a cancelled
// one does not count) is not drafted again: it comes back under "skipped"
// with that invoice, so pressing the button twice adds nothing. In ids mode a
// listed payment that cannot be invoiced at all refuses the whole call with
// the same code Create invoice gives, and nothing is created.
//
// Everything runs in one transaction under the book lock, in the caller's own
// book. It only adds rows; no existing payment, client or invoice is changed.
func GenerateInvoiceDraftsHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	owner := invoicingUserID(c)
	var body map[string]json.RawMessage
	if err := c.ShouldBindJSON(&body); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid request body")
		return
	}
	month, ids, byMonth, g := parseGenerateRequest(body)
	if g != nil {
		respondGuard(c, nil, g)
		return
	}

	tx := db.Begin()
	defer tx.RollbackUnlessCommitted()
	if _, err := lockIssuing(tx, owner); err != nil {
		respondInvoicingInternal(c, tx, "generating invoices", err)
		return
	}
	if byMonth {
		var err error
		if ids, err = monthCandidateIDs(tx, owner, month); err != nil {
			respondInvoicingInternal(c, tx, "generating invoices", err)
			return
		}
		if len(ids) > maxGeneratePayments {
			respondGuard(c, tx, guard(http.StatusUnprocessableEntity, "too_many_payments",
				"%s has %d paid-work payments; generate at most %d in one call by selecting rows", month, len(ids), maxGeneratePayments))
			return
		}
	}

	// Read after the lock, so a draft another request just committed is seen.
	live, err := data_models.LiveInvoicesForPayments(tx, owner)
	if err != nil {
		respondInvoicingInternal(c, tx, "generating invoices", err)
		return
	}
	resp := generateResponse{Invoices: []invoiceDetail{}, Skipped: []generateSkip{}}
	var todo []uint
	for _, id := range ids {
		if ref, ok := live[id]; ok {
			resp.Skipped = append(resp.Skipped, generateSkip{PaymentID: id, Reason: generateSkipOnInvoice, Invoice: ref})
			continue
		}
		todo = append(todo, id)
	}
	if len(todo) == 0 {
		tx.Rollback()
		c.JSON(http.StatusOK, resp)
		return
	}

	payments, g, err := checkInvoiceablePayments(tx, owner, todo, 0)
	if err != nil {
		respondInvoicingInternal(c, tx, "generating invoices", err)
		return
	}
	if g != nil {
		respondGuard(c, tx, g)
		return
	}
	// Oldest payment first, so the review walks the month in date order.
	sort.SliceStable(payments, func(i, j int) bool {
		if !payments[i].PaidOn.Equal(payments[j].PaidOn) {
			return payments[i].PaidOn.Before(payments[j].PaidOn)
		}
		return payments[i].ID < payments[j].ID
	})
	klein, err := profileIsKleinunternehmer(tx, owner)
	if err != nil {
		respondInvoicingInternal(c, tx, "generating invoices", err)
		return
	}
	created := make([]data_models.InvoicingInvoice, 0, len(payments))
	for _, p := range payments {
		inv, err := createDraftFromPayments(tx, owner, []data_models.InvoicingPayment{p}, klein,
			gin.H{"payment_ids": []uint{p.ID}, "generated": true})
		if err != nil {
			respondInvoicingInternal(c, tx, "generating invoices", err)
			return
		}
		created = append(created, inv)
	}
	if err := tx.Commit().Error; err != nil {
		respondInvoicingInternal(c, nil, "generating invoices", err)
		return
	}
	for _, inv := range created {
		detail, err := loadInvoiceDetail(db, owner, inv)
		if err != nil {
			respondInvoicingInternal(c, nil, "loading invoice", err)
			return
		}
		resp.Invoices = append(resp.Invoices, *detail)
	}
	c.JSON(http.StatusCreated, resp)
}
