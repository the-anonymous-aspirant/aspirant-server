package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Remove, restore and delete for good, for payments and expenses (system_3
// #10555-A2, design §3.143 Decision 6, operator c65419).
//
// Remove takes a row out of everything that counts: the list, the Book and
// its CSV (both read ListInvoicing*), Create invoice and payer matching. The
// row is kept with the time it was removed. A payment also comes off any
// draft it was on; its links to cancelled invoices stay, since a cancelled
// invoice keeps its record and the row still exists. Restore clears the time
// and puts a payment back on the drafts its latest removal took it off, each
// that is still a draft, unsaved since, and one an invoice of it could be
// (system_3 #12318); the answer says, for each draft, whether it went back
// and why not. Delete for good ("purge") erases a row only once it has been
// removed, and leaves a tombstone of its source and transaction id.
// It erases the row's links too, a cancelled invoice's included; a numbered
// invoice keeps its own copy of the payment, which it reads instead of the
// row (system_3 #12266).
//
// Every form takes a list of ids and is all or nothing: one id that is
// unknown, another owner's, on an issued invoice or in the wrong state
// refuses the whole request, and nothing is written. The single forms
// (/:id/remove and so on) are the same core with one id.
//
// Purge is the only delete. The legacy DELETE /payments/:id and
// /expenses/:id, which erased any non-issued row at once, are retired
// (system_3 #10647-A2): no client has called them since the Remove shipped
// (#10555-C1, C2).

const (
	invoicingActionRemove  = "remove"
	invoicingActionRestore = "restore"

	rowRemove  = "remove"
	rowRestore = "restore"
	rowPurge   = "purge"

	// maxRowActionIDs bounds one request: "select all shown" on the largest
	// book today is a few hundred rows.
	maxRowActionIDs = 1000
)

// rowActionIDs reads the ids a request acts on: the :id path parameter for a
// single form, else a JSON body {"ids": [...]}. Writes the 400 itself and
// returns false on a bad request.
func rowActionIDs(c *gin.Context) ([]uint, bool) {
	if raw := c.Param("id"); raw != "" {
		id, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || id == 0 {
			RespondWithError(c, http.StatusBadRequest, "Invalid id")
			return nil, false
		}
		return []uint{uint(id)}, true
	}
	var body struct {
		IDs []uint `json:"ids"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Body must be {\"ids\": [...]}")
		return nil, false
	}
	if len(body.IDs) == 0 {
		respondError(c, http.StatusBadRequest, "invalid_field", "ids must name at least one row")
		return nil, false
	}
	if len(body.IDs) > maxRowActionIDs {
		respondError(c, http.StatusBadRequest, "invalid_field", fmt.Sprintf("ids may name at most %d rows", maxRowActionIDs))
		return nil, false
	}
	seen := make(map[uint]bool, len(body.IDs))
	for _, id := range body.IDs {
		if id == 0 {
			respondError(c, http.StatusBadRequest, "invalid_field", "ids must be positive")
			return nil, false
		}
		if seen[id] {
			respondError(c, http.StatusBadRequest, "invalid_field", fmt.Sprintf("id %d is listed twice", id))
			return nil, false
		}
		seen[id] = true
	}
	return body.IDs, true
}

// rowRefusal is a 4xx that ends a row action before anything is written.
type rowRefusal struct {
	status        int
	code, message string
	// field and limit, where the refusal is about one request field
	// (system_3 #12300 ruling 1).
	field string
	limit int64
}

// respondRow answers a rowRefusal with its status, code, field and limit.
func respondRow(c *gin.Context, r *rowRefusal) {
	log.Printf("Error response: %d - %s - %s", r.status, r.code, r.message)
	c.JSON(r.status, ErrorResponse{Error: ErrorDetail{Code: r.code, Message: r.message, Field: r.field, Limit: r.limit}})
}

func refuseRow(status int, code, format string, args ...interface{}) *rowRefusal {
	return &rowRefusal{status: status, code: code, message: fmt.Sprintf(format, args...)}
}

// ascending returns a sorted copy, the order every row lock is taken in, so
// two requests over overlapping rows cannot deadlock.
func ascending(ids []uint) []uint {
	out := append([]uint(nil), ids...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// draftInvoiceIDsForPayment returns the drafts that list the payment.
func draftInvoiceIDsForPayment(tx *gorm.DB, paymentID uint) ([]uint, error) {
	var rows []struct{ InvoiceID uint }
	err := tx.Table("invoicing_invoice_payments").Select("invoicing_invoice_payments.invoice_id").
		Joins("JOIN invoicing_invoices i ON i.id = invoicing_invoice_payments.invoice_id").
		Where("invoicing_invoice_payments.payment_id = ? AND i.status = ?", paymentID, data_models.InvoiceStatusDraft).
		Order("invoicing_invoice_payments.invoice_id asc").Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]uint, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.InvoiceID)
	}
	return out, nil
}

// Why a restore did not put a payment back on a draft its latest removal
// took it off (system_3 #12318, rulings 1 and 2 with the review's 1 to 3).
const (
	relinkDraftGone      = "draft_gone"      // deleted since
	relinkDraftNotDraft  = "draft_issued"    // issued (or cancelled) since
	relinkDraftChanged   = "draft_changed"   // saved since the removal
	relinkNotInvoiceable = "not_invoiceable" // no longer a payment an invoice can cover
)

// restoredDraft is one draft of a restored payment's latest removal, and
// whether the restore put the payment back on it.
type restoredDraft struct {
	InvoiceID uint   `json:"invoice_id"`
	Relinked  bool   `json:"relinked"`
	Reason    string `json:"reason,omitempty"`
}

// restoredPayment is a restore's answer for one payment. Drafts is empty when
// its latest removal took it off no draft.
type restoredPayment struct {
	ID     uint            `json:"id"`
	Drafts []restoredDraft `json:"drafts"`
}

// latestRemoval returns the id of the payment's latest remove event and the
// drafts that removal took it off. Only the latest is read: an older
// removal's drafts are never put back (review point 3). No event, or one
// whose details were erased with an account, names no draft.
func latestRemoval(tx *gorm.DB, owner, paymentID uint) (uint, []uint, error) {
	var ev data_models.InvoicingAuditEvent
	err := tx.Where("user_id = ? AND entity_type = ? AND entity_id = ? AND action = ?",
		owner, invoicingEntityPayment, paymentID, invoicingActionRemove).Last(&ev).Error
	if gorm.IsRecordNotFoundError(err) {
		return 0, nil, nil
	}
	if err != nil {
		return 0, nil, err
	}
	var d struct {
		Drafts []uint `json:"unlinked_draft_ids"`
	}
	if ev.Details != "" {
		if err := json.Unmarshal([]byte(ev.Details), &d); err != nil {
			log.Printf("payment %d: removal %d details: %v", paymentID, ev.ID, err)
			return ev.ID, nil, nil
		}
	}
	return ev.ID, d.Drafts, nil
}

// relinkRefusal says why payment p cannot go back on draft draftID, which its
// removal (event removalID) took it off, or "" when it can. The payment's own
// removal is not looked at: restore asks after clearing it, the empty
// draft's line before.
func relinkRefusal(tx *gorm.DB, owner uint, p data_models.InvoicingPayment, removalID, draftID uint) (string, error) {
	inv, err := data_models.GetInvoicingInvoice(tx, owner, draftID)
	if err != nil {
		return "", err
	}
	if inv == nil {
		return relinkDraftGone, nil
	}
	if inv.Status != data_models.InvoiceStatusDraft {
		return relinkDraftNotDraft, nil
	}
	// Saved since: ze may have taken the payment's line out, and the draft
	// would then list a payment it no longer charges (review point 1).
	var saves int
	if err := tx.Model(&data_models.InvoicingAuditEvent{}).
		Where("user_id = ? AND entity_type = ? AND entity_id = ? AND action = ? AND id > ?",
			owner, invoicingEntityInvoice, draftID, invoicingActionUpdate, removalID).Count(&saves).Error; err != nil {
		return "", err
	}
	if saves > 0 {
		return relinkDraftChanged, nil
	}
	// Still a payment the draft could list (review point 2): a removed
	// payment's category can change.
	if p.Kind != data_models.PaymentKindIncoming || p.Category != data_models.PaymentCategoryPaidWork || isPayPalTransferIntoN26(p) {
		return relinkNotInvoiceable, nil
	}
	if mixedCurrencies([]data_models.InvoicingPayment{p}, inv.Currency) != nil {
		return relinkNotInvoiceable, nil
	}
	issued, err := data_models.IssuedInvoiceForPayment(tx, p.ID, draftID)
	if err != nil {
		return "", err
	}
	if issued != 0 {
		return relinkNotInvoiceable, nil
	}
	return "", nil
}

// restorePaymentDrafts puts a just-restored payment back on its latest
// removal's drafts, each that relinkRefusal passes, and answers for each.
func restorePaymentDrafts(tx *gorm.DB, owner uint, p data_models.InvoicingPayment) (restoredPayment, error) {
	out := restoredPayment{ID: p.ID, Drafts: []restoredDraft{}}
	removalID, drafts, err := latestRemoval(tx, owner, p.ID)
	if err != nil {
		return out, err
	}
	for _, draftID := range ascending(drafts) {
		// Wait for a save or an issue of the draft in flight. The issuing
		// lock, held already, orders this against a payment_ids edit.
		if _, err := lockInvoiceRow(tx, owner, draftID); err != nil {
			return out, err
		}
		reason, err := relinkRefusal(tx, owner, p, removalID, draftID)
		if err != nil {
			return out, err
		}
		if reason == "" {
			if err := tx.Create(&data_models.InvoicingInvoicePayment{InvoiceID: draftID, PaymentID: p.ID}).Error; err != nil {
				return out, fmt.Errorf("relink %d to %d: %w", p.ID, draftID, err)
			}
		}
		out.Drafts = append(out.Drafts, restoredDraft{InvoiceID: draftID, Relinked: reason == "", Reason: reason})
	}
	return out, nil
}

// restorablePaymentIDs lists the removed payments that a restore would put
// back on an empty draft: those whose latest removal names it and that pass
// relinkRefusal. The empty draft's line names the way back only when there
// is one (review point 4).
func restorablePaymentIDs(db *gorm.DB, owner uint, inv data_models.InvoicingInvoice) ([]uint, error) {
	var removed []data_models.InvoicingPayment
	if err := db.Where("owner_user_id = ? AND removed_at IS NOT NULL", owner).
		Order("id asc").Find(&removed).Error; err != nil {
		return nil, err
	}
	out := []uint{}
	for _, p := range removed {
		removalID, drafts, err := latestRemoval(db, owner, p.ID)
		if err != nil {
			return nil, err
		}
		for _, d := range drafts {
			if d != inv.ID {
				continue
			}
			reason, err := relinkRefusal(db, owner, p, removalID, d)
			if err != nil {
				return nil, err
			}
			if reason == "" {
				out = append(out, p.ID)
			}
		}
	}
	return out, nil
}

// PaymentsRowActionHandler returns the handler for one action on payments.
func PaymentsRowActionHandler(action string) gin.HandlerFunc {
	return func(c *gin.Context) {
		ids, ok := rowActionIDs(c)
		if !ok {
			return
		}
		db := c.MustGet("db").(*gorm.DB)
		owner := invoicingUserID(c)
		tx := db.Begin()
		defer tx.RollbackUnlessCommitted()
		restored, refusal, err := applyPaymentRowAction(tx, owner, ids, action)
		if err != nil {
			tx.Rollback()
			log.Printf("payment %s: %v", action, err)
			RespondWithError(c, http.StatusInternalServerError, "Error updating payments")
			return
		}
		if refusal != nil {
			tx.Rollback()
			respondRow(c, refusal)
			return
		}
		if err := tx.Commit().Error; err != nil {
			log.Printf("payment %s: commit: %v", action, err)
			RespondWithError(c, http.StatusInternalServerError, "Error updating payments")
			return
		}
		body := gin.H{"action": action, "ids": ids}
		if action == rowRestore {
			body["restored"] = restored
		}
		c.JSON(http.StatusOK, body)
	}
}

// issuedInvoiceRefusal refuses a payment that an issued (not cancelled)
// invoice lists: 409 payment_on_invoice, naming the invoice. Remove and purge
// share it, so the two refusals read the same.
func issuedInvoiceRefusal(tx *gorm.DB, owner uint, p data_models.InvoicingPayment) (*rowRefusal, error) {
	issuedID, err := data_models.IssuedInvoiceForPayment(tx, p.ID, 0)
	if err != nil {
		return nil, fmt.Errorf("issued check %d: %w", p.ID, err)
	}
	if issuedID == 0 {
		return nil, nil
	}
	label := fmt.Sprintf("%d", issuedID)
	if inv, err := data_models.GetInvoicingInvoice(tx, owner, issuedID); err == nil && inv != nil {
		label = invoiceLabel(*inv)
	}
	return refuseRow(http.StatusConflict, "payment_on_invoice",
		"payment %s is on issued invoice %s; cancel that invoice first", paymentLabel(p), label), nil
}

func applyPaymentRowAction(tx *gorm.DB, owner uint, ids []uint, action string) ([]restoredPayment, *rowRefusal, error) {
	payments, refusal, err := checkPaymentRowAction(tx, owner, ids, action)
	if refusal != nil || err != nil {
		return nil, refusal, err
	}
	restored, err := writePaymentRowAction(tx, owner, payments, action)
	return restored, nil, err
}

// checkPaymentRowAction takes the locks and reads and checks every row before
// any is written.
func checkPaymentRowAction(tx *gorm.DB, owner uint, ids []uint, action string) ([]data_models.InvoicingPayment, *rowRefusal, error) {
	// The issuing lock orders this against a draft create, a payment_ids
	// edit or an issue that would link or freeze one of these payments, as
	// the legacy delete does (#8409 security F1).
	if _, err := lockIssuing(tx, owner); err != nil {
		return nil, nil, fmt.Errorf("issuing lock: %w", err)
	}
	for _, id := range ascending(ids) {
		if err := lockPaymentRow(tx, owner, id); err != nil {
			return nil, nil, fmt.Errorf("row lock %d: %w", id, err)
		}
	}
	// Read and check every row before writing any.
	payments := make([]data_models.InvoicingPayment, 0, len(ids))
	for _, id := range ids {
		p, err := data_models.GetInvoicingPayment(tx, owner, id)
		if err != nil {
			return nil, nil, fmt.Errorf("read %d: %w", id, err)
		}
		if p == nil {
			return nil, refuseRow(http.StatusNotFound, "not_found", "payment %d not found", id), nil
		}
		switch action {
		case rowRemove:
			if p.RemovedAt != nil {
				return nil, refuseRow(http.StatusConflict, "already_removed", "payment %s is already removed", paymentLabel(*p)), nil
			}
			if refusal, err := issuedInvoiceRefusal(tx, owner, *p); refusal != nil || err != nil {
				return nil, refusal, err
			}
		case rowRestore, rowPurge:
			if p.RemovedAt == nil {
				return nil, refuseRow(http.StatusConflict, "not_removed",
					"payment %s is not removed; only a removed payment can be %s", paymentLabel(*p), pastTense(action)), nil
			}
			// Purge is the one step that cannot be undone, and it deletes
			// link rows. A removed payment is never on an issued invoice
			// today: draft create, draft edit and issue all refuse
			// payment_removed (checkInvoiceablePayments). Purge checks
			// anyway, as the legacy delete did, so a future path that forgets
			// that guard cannot make it erase a payment an issued invoice
			// still lists (system_3 #10647-A1, security c66061).
			if action == rowPurge {
				if refusal, err := issuedInvoiceRefusal(tx, owner, *p); refusal != nil || err != nil {
					return nil, refusal, err
				}
			}
		}
		payments = append(payments, *p)
	}
	return payments, nil, nil
}

// writePaymentRowAction writes an action checkPaymentRowAction passed.
func writePaymentRowAction(tx *gorm.DB, owner uint, payments []data_models.InvoicingPayment, action string) ([]restoredPayment, error) {
	restored := []restoredPayment{}
	now := invoicingNow().UTC()
	for _, p := range payments {
		switch action {
		case rowRemove:
			drafts, err := draftInvoiceIDsForPayment(tx, p.ID)
			if err != nil {
				return nil, fmt.Errorf("drafts of %d: %w", p.ID, err)
			}
			if len(drafts) > 0 {
				if err := tx.Where("payment_id = ? AND invoice_id IN (?)", p.ID, drafts).
					Delete(&data_models.InvoicingInvoicePayment{}).Error; err != nil {
					return nil, fmt.Errorf("unlink %d: %w", p.ID, err)
				}
			}
			// The payer's IBAN goes with the payment and does not come back
			// on a restore (security C4, c75108 on system_3 #11551).
			if err := tx.Model(&data_models.InvoicingPayment{}).Where("id = ?", p.ID).
				Updates(map[string]interface{}{"removed_at": now, "sender_iban": ""}).Error; err != nil {
				return nil, fmt.Errorf("remove %d: %w", p.ID, err)
			}
			details := auditDetails(gin.H{"before": gin.H{"removed_at": nil}, "after": gin.H{"removed_at": now}, "unlinked_draft_ids": drafts})
			if err := data_models.RecordInvoicingAuditEvent(tx, owner, invoicingActionRemove, invoicingEntityPayment, p.ID, details); err != nil {
				return nil, fmt.Errorf("audit %d: %w", p.ID, err)
			}
		case rowRestore:
			if err := tx.Model(&data_models.InvoicingPayment{}).Where("id = ?", p.ID).
				Update("removed_at", gorm.Expr("NULL")).Error; err != nil {
				return nil, fmt.Errorf("restore %d: %w", p.ID, err)
			}
			back, err := restorePaymentDrafts(tx, owner, p)
			if err != nil {
				return nil, fmt.Errorf("drafts of %d: %w", p.ID, err)
			}
			restored = append(restored, back)
			relinked := []uint{}
			for _, d := range back.Drafts {
				if d.Relinked {
					relinked = append(relinked, d.InvoiceID)
				}
			}
			details := auditDetails(gin.H{"before": gin.H{"removed_at": p.RemovedAt}, "after": gin.H{"removed_at": nil}, "relinked_draft_ids": relinked})
			if err := data_models.RecordInvoicingAuditEvent(tx, owner, invoicingActionRestore, invoicingEntityPayment, p.ID, details); err != nil {
				return nil, fmt.Errorf("audit %d: %w", p.ID, err)
			}
		case rowPurge:
			unlinked, err := data_models.InvoiceIDsForPayment(tx, p.ID)
			if err != nil {
				return nil, fmt.Errorf("links of %d: %w", p.ID, err)
			}
			if err := tx.Where("payment_id = ?", p.ID).Delete(&data_models.InvoicingInvoicePayment{}).Error; err != nil {
				return nil, fmt.Errorf("unlink %d: %w", p.ID, err)
			}
			row := p
			if err := tx.Delete(&row).Error; err != nil {
				return nil, fmt.Errorf("delete %d: %w", p.ID, err)
			}
			if err := data_models.TombstoneInvoicingPayment(tx, p, owner, now); err != nil {
				return nil, fmt.Errorf("tombstone %d: %w", p.ID, err)
			}
			// The whole row as it was, as the legacy delete records it.
			details := auditDetails(gin.H{"before": p, "unlinked_invoice_ids": unlinked})
			if err := data_models.RecordInvoicingAuditEvent(tx, owner, invoicingActionDelete, invoicingEntityPayment, p.ID, details); err != nil {
				return nil, fmt.Errorf("audit %d: %w", p.ID, err)
			}
		}
	}
	return restored, nil
}

// ExpensesRowActionHandler returns the handler for one action on expenses.
func ExpensesRowActionHandler(action string) gin.HandlerFunc {
	return func(c *gin.Context) {
		ids, ok := rowActionIDs(c)
		if !ok {
			return
		}
		db := c.MustGet("db").(*gorm.DB)
		owner := invoicingUserID(c)
		tx := db.Begin()
		defer tx.RollbackUnlessCommitted()
		refusal, err := applyExpenseRowAction(tx, owner, ids, action)
		if err != nil {
			tx.Rollback()
			log.Printf("expense %s: %v", action, err)
			RespondWithError(c, http.StatusInternalServerError, "Error updating expenses")
			return
		}
		if refusal != nil {
			tx.Rollback()
			respondRow(c, refusal)
			return
		}
		if err := tx.Commit().Error; err != nil {
			log.Printf("expense %s: commit: %v", action, err)
			RespondWithError(c, http.StatusInternalServerError, "Error updating expenses")
			return
		}
		c.JSON(http.StatusOK, gin.H{"action": action, "ids": ids})
	}
}

func applyExpenseRowAction(tx *gorm.DB, owner uint, ids []uint, action string) (*rowRefusal, error) {
	if err := data_models.LockInvoicingBook(tx, owner); err != nil {
		return nil, fmt.Errorf("book lock: %w", err)
	}
	for _, id := range ascending(ids) {
		if err := lockExpenseRow(tx, owner, id); err != nil {
			return nil, fmt.Errorf("row lock %d: %w", id, err)
		}
	}
	expenses := make([]data_models.InvoicingExpense, 0, len(ids))
	for _, id := range ids {
		e, err := data_models.GetInvoicingExpense(tx, owner, id)
		if err != nil {
			return nil, fmt.Errorf("read %d: %w", id, err)
		}
		if e == nil {
			return refuseRow(http.StatusNotFound, "not_found", "expense %d not found", id), nil
		}
		if action == rowRemove && e.RemovedAt != nil {
			return refuseRow(http.StatusConflict, "already_removed", "expense %d is already removed", id), nil
		}
		if (action == rowRestore || action == rowPurge) && e.RemovedAt == nil {
			return refuseRow(http.StatusConflict, "not_removed",
				"expense %d is not removed; only a removed expense can be %s", id, pastTense(action)), nil
		}
		expenses = append(expenses, *e)
	}

	now := invoicingNow().UTC()
	for _, e := range expenses {
		switch action {
		case rowRemove:
			if err := tx.Model(&data_models.InvoicingExpense{}).Where("id = ?", e.ID).
				Update("removed_at", now).Error; err != nil {
				return nil, fmt.Errorf("remove %d: %w", e.ID, err)
			}
			details := auditDetails(gin.H{"before": gin.H{"removed_at": nil}, "after": gin.H{"removed_at": now}})
			if err := data_models.RecordInvoicingAuditEvent(tx, owner, invoicingActionRemove, invoicingEntityExpense, e.ID, details); err != nil {
				return nil, fmt.Errorf("audit %d: %w", e.ID, err)
			}
		case rowRestore:
			if err := tx.Model(&data_models.InvoicingExpense{}).Where("id = ?", e.ID).
				Update("removed_at", gorm.Expr("NULL")).Error; err != nil {
				return nil, fmt.Errorf("restore %d: %w", e.ID, err)
			}
			details := auditDetails(gin.H{"before": gin.H{"removed_at": e.RemovedAt}, "after": gin.H{"removed_at": nil}})
			if err := data_models.RecordInvoicingAuditEvent(tx, owner, invoicingActionRestore, invoicingEntityExpense, e.ID, details); err != nil {
				return nil, fmt.Errorf("audit %d: %w", e.ID, err)
			}
		case rowPurge:
			// A receipt goes with its expense, as in the legacy delete
			// (system_3 #10537-A1).
			receipt, err := data_models.GetInvoicingExpenseReceipt(tx, owner, e.ID)
			if err != nil {
				return nil, fmt.Errorf("receipt of %d: %w", e.ID, err)
			}
			if receipt != nil {
				if _, err := data_models.DeleteInvoicingExpenseReceipt(tx, owner, e.ID); err != nil {
					return nil, fmt.Errorf("delete receipt of %d: %w", e.ID, err)
				}
			}
			row := e
			if err := tx.Delete(&row).Error; err != nil {
				return nil, fmt.Errorf("delete %d: %w", e.ID, err)
			}
			if err := data_models.TombstoneInvoicingExpense(tx, e, owner, now); err != nil {
				return nil, fmt.Errorf("tombstone %d: %w", e.ID, err)
			}
			if err := data_models.RecordInvoicingAuditEvent(tx, owner, invoicingActionDelete, invoicingEntityExpense, e.ID, auditDetails(gin.H{"before": e})); err != nil {
				return nil, fmt.Errorf("audit %d: %w", e.ID, err)
			}
			if receipt != nil {
				receiptDetails := auditDetails(gin.H{"before": gin.H{"sha256": receipt.SHA256, "content_type": receipt.ContentType, "bytes": receipt.SizeBytes}})
				if err := data_models.RecordInvoicingAuditEvent(tx, owner, invoicingActionDelete, invoicingEntityExpenseReceipt, e.ID, receiptDetails); err != nil {
					return nil, fmt.Errorf("audit receipt of %d: %w", e.ID, err)
				}
			}
		}
	}
	return nil, nil
}

func pastTense(action string) string {
	switch action {
	case rowRestore:
		return "restored"
	case rowPurge:
		return "deleted for good"
	}
	return action + "d"
}
