package handlers

import (
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
// and re-links nothing. Delete for good ("purge") erases a row only once it
// has been removed, and leaves a tombstone of its source and transaction id.
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
		refusal, err := applyPaymentRowAction(tx, owner, ids, action)
		if err != nil {
			tx.Rollback()
			log.Printf("payment %s: %v", action, err)
			RespondWithError(c, http.StatusInternalServerError, "Error updating payments")
			return
		}
		if refusal != nil {
			tx.Rollback()
			respondError(c, refusal.status, refusal.code, refusal.message)
			return
		}
		if err := tx.Commit().Error; err != nil {
			log.Printf("payment %s: commit: %v", action, err)
			RespondWithError(c, http.StatusInternalServerError, "Error updating payments")
			return
		}
		c.JSON(http.StatusOK, gin.H{"action": action, "ids": ids})
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

func applyPaymentRowAction(tx *gorm.DB, owner uint, ids []uint, action string) (*rowRefusal, error) {
	// The issuing lock orders this against a draft create, a payment_ids
	// edit or an issue that would link or freeze one of these payments, as
	// the legacy delete does (#8409 security F1).
	if _, err := lockIssuing(tx, owner); err != nil {
		return nil, fmt.Errorf("issuing lock: %w", err)
	}
	for _, id := range ascending(ids) {
		if err := lockPaymentRow(tx, owner, id); err != nil {
			return nil, fmt.Errorf("row lock %d: %w", id, err)
		}
	}
	// Read and check every row before writing any.
	payments := make([]data_models.InvoicingPayment, 0, len(ids))
	for _, id := range ids {
		p, err := data_models.GetInvoicingPayment(tx, owner, id)
		if err != nil {
			return nil, fmt.Errorf("read %d: %w", id, err)
		}
		if p == nil {
			return refuseRow(http.StatusNotFound, "not_found", "payment %d not found", id), nil
		}
		switch action {
		case rowRemove:
			if p.RemovedAt != nil {
				return refuseRow(http.StatusConflict, "already_removed", "payment %s is already removed", paymentLabel(*p)), nil
			}
			if refusal, err := issuedInvoiceRefusal(tx, owner, *p); refusal != nil || err != nil {
				return refusal, err
			}
		case rowRestore, rowPurge:
			if p.RemovedAt == nil {
				return refuseRow(http.StatusConflict, "not_removed",
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
					return refusal, err
				}
			}
		}
		payments = append(payments, *p)
	}

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
			if err := tx.Model(&data_models.InvoicingPayment{}).Where("id = ?", p.ID).
				Update("removed_at", now).Error; err != nil {
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
			details := auditDetails(gin.H{"before": gin.H{"removed_at": p.RemovedAt}, "after": gin.H{"removed_at": nil}})
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
	return nil, nil
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
		refusal, err := applyExpenseRowAction(tx, owner, ids, action)
		if err != nil {
			tx.Rollback()
			log.Printf("expense %s: %v", action, err)
			RespondWithError(c, http.StatusInternalServerError, "Error updating expenses")
			return
		}
		if refusal != nil {
			tx.Rollback()
			respondError(c, refusal.status, refusal.code, refusal.message)
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
