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
	"unicode/utf8"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Editing a payment or an expense (system_3 #10555-A1, design §3.143
// Decision 5). What may change depends on how the row got into the book: a
// hand-entered row is hers, so every field she typed may change; an imported
// row's date, amount and payer are the bank's record of what happened, so
// only her own notes on it (note, category, client) may change. To fix a
// bank fact she removes the row and enters it by hand.
//
// The per-field rules restate the hand-entry rules in
// invoicing_manual_payments.go and invoicing_manual_expenses.go from the same
// constants; invoicing_edit_test.go pins the two to the same refusals.

const invoicingActionEdit = "edit"

var (
	paymentEditableByHand = map[string]bool{
		"paid_on": true, "gross_cents": true, "method": true, "reference": true,
		"sender_name": true, "note": true, "category": true, "client_id": true,
		// Adds the payer to the client book in the same transaction, as the
		// hand-entry create does (system_3 #10696, design §3.143 Decision 5:
		// "Paid by" keeps §3.142's behaviour in edit mode).
		"new_client_name": true,
	}
	paymentEditableImported = map[string]bool{"note": true, "category": true, "client_id": true}

	expenseEditableByHand = map[string]bool{
		"paid_on": true, "amount_cents": true, "category": true, "payee": true,
		"reference": true, "note": true,
	}
	expenseEditableImported = map[string]bool{"category": true, "note": true}
)

// editBody decodes a PATCH body and checks every key against the fields any
// row of that type may ever change. It writes the 400 itself.
func editBody(c *gin.Context, known map[string]bool) (map[string]json.RawMessage, []string, bool) {
	var body map[string]json.RawMessage
	if err := c.ShouldBindJSON(&body); err != nil || len(body) == 0 {
		respondError(c, http.StatusBadRequest, "bad_request", "send at least one field to change")
		return nil, nil, false
	}
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !known[k] {
			respondError(c, http.StatusBadRequest, "unknown_field", fmt.Sprintf("unknown field %q", k))
			return nil, nil, false
		}
	}
	return body, keys, true
}

// bankFactRefusal names the first key that is a bank fact on an imported row.
func bankFactRefusal(keys []string, allowed map[string]bool, what string) string {
	for _, k := range keys {
		if !allowed[k] {
			return fmt.Sprintf("%s is the bank's record on an imported %s; remove the %s and enter it by hand to change it", k, what, what)
		}
	}
	return ""
}

// editString decodes one string field, trimmed, as the hand-entry handlers do.
func editString(raw json.RawMessage) (string, bool) {
	if string(raw) == "null" {
		return "", true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return strings.TrimSpace(s), true
}

// checkEditPaidOn applies the hand-entry date rule. futureHint completes the
// "cannot be in the future" message the same way each create handler does.
func checkEditPaidOn(raw json.RawMessage, futureHint string) (time.Time, string) {
	s, ok := editString(raw)
	if !ok {
		return time.Time{}, "must be a string"
	}
	d, err := time.Parse(invoicingDateLayout, s)
	if err != nil {
		return time.Time{}, "must be a date, YYYY-MM-DD"
	}
	if d.After(berlinToday()) {
		return time.Time{}, "cannot be in the future: " + futureHint
	}
	if d.Before(earliestPaymentDate) {
		return time.Time{}, "must be 2000-01-01 or later"
	}
	return d, ""
}

func checkEditCents(raw json.RawMessage) (int64, string) {
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil || v <= 0 || v > maxUnitPriceCents {
		return 0, "must be a whole number of cents above zero"
	}
	return v, ""
}

func checkEditReference(raw json.RawMessage) (string, string) {
	s, ok := editString(raw)
	if !ok {
		return "", "must be a string"
	}
	if utf8.RuneCountInString(s) > maxManualReferenceRunes || strings.Contains(s, "\n") || hasControlChars(s) || hasFormatChars(s) {
		return "", fmt.Sprintf("must be one line of at most %d characters", maxManualReferenceRunes)
	}
	return s, ""
}

func checkEditOneLineName(raw json.RawMessage) (string, string) {
	s, ok := editString(raw)
	if !ok {
		return "", "must be a string"
	}
	if !validOneLineName(s) {
		return "", fmt.Sprintf("must be one line of at most %d characters", maxManualSenderRunes)
	}
	return s, ""
}

func checkEditNote(raw json.RawMessage) (string, string) {
	s, ok := editString(raw)
	if !ok {
		return "", "must be a string"
	}
	if utf8.RuneCountInString(s) > maxManualNoteRunes || hasControlChars(s) {
		return "", fmt.Sprintf("must be at most %d characters", maxManualNoteRunes)
	}
	return s, ""
}

// changedFields compares the before and after values of the keys sent and
// returns, for each one that changed, its old and new value for the audit.
func changedFields(keys []string, before, after map[string]interface{}) (gin.H, gin.H) {
	b, a := gin.H{}, gin.H{}
	for _, k := range keys {
		if fmt.Sprint(deref(before[k])) != fmt.Sprint(deref(after[k])) {
			b[k] = before[k]
			a[k] = after[k]
		}
	}
	return b, a
}

func deref(v interface{}) interface{} {
	if p, ok := v.(*uint); ok {
		if p == nil {
			return nil
		}
		return *p
	}
	return v
}

func paymentEditView(p data_models.InvoicingPayment) map[string]interface{} {
	return map[string]interface{}{
		"paid_on": p.PaidOn.Format(invoicingDateLayout), "gross_cents": p.GrossCents, "method": p.Method,
		"reference": p.Reference, "sender_name": p.SenderName, "note": p.Note,
		"category": p.Category, "client_id": p.ClientID,
	}
}

func expenseEditView(e data_models.InvoicingExpense) map[string]interface{} {
	return map[string]interface{}{
		"paid_on": e.PaidOn.Format(invoicingDateLayout), "amount_cents": e.AmountCents, "category": e.Category,
		"payee": e.Payee, "reference": e.Reference, "note": e.Note,
	}
}

// draftCoveringPayment returns a draft invoice that covers the payment, or nil.
func draftCoveringPayment(tx *gorm.DB, owner, paymentID uint) (*data_models.InvoicingInvoice, error) {
	ids, err := data_models.InvoiceIDsForPayment(tx, paymentID)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		inv, err := data_models.GetInvoicingInvoice(tx, owner, id)
		if err != nil {
			return nil, err
		}
		if inv != nil && inv.Status == data_models.InvoiceStatusDraft {
			return inv, nil
		}
	}
	return nil, nil
}

// PatchInvoicingPaymentHandler edits a payment: PATCH /invoicing/payments/:id.
// Fields left out of the body are unchanged; null clears a client link.
func PatchInvoicingPaymentHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		RespondWithError(c, http.StatusBadRequest, "Invalid payment id")
		return
	}
	body, keys, ok := editBody(c, paymentEditableByHand)
	if !ok {
		return
	}
	owner := invoicingUserID(c)
	fail := func(tx *gorm.DB, what string, err error) {
		tx.Rollback()
		log.Printf("Error editing payment %d (%s): %v", id, what, err)
		RespondWithError(c, http.StatusInternalServerError, "Error updating payment")
	}

	tx := db.Begin()
	// The issuing lock orders this edit against a draft create, a draft's
	// payment edit or an issue, all of which take it before reading the
	// payment, so the invoice checks below cannot go stale mid-edit.
	if _, err := lockIssuing(tx, owner); err != nil {
		fail(tx, "issuing lock", err)
		return
	}
	if err := lockPaymentRow(tx, owner, uint(id)); err != nil {
		fail(tx, "row lock", err)
		return
	}
	p, err := data_models.GetInvoicingPayment(tx, owner, uint(id))
	if err != nil {
		fail(tx, "read", err)
		return
	}
	if p == nil {
		tx.Rollback()
		RespondWithError(c, http.StatusNotFound, "Payment not found")
		return
	}
	if !p.IsManual() {
		// An imported payment's payer is the bank's record (§3.143 Decision
		// 5): she can link a client, not add one from here.
		if _, sent := body["new_client_name"]; sent {
			tx.Rollback()
			respondError(c, http.StatusBadRequest, "bank_fact_field",
				"new_client_name cannot be given on an imported payment: its payer is the bank's record; choose a client with client_id")
			return
		}
		if msg := bankFactRefusal(keys, paymentEditableImported, "payment"); msg != "" {
			tx.Rollback()
			respondError(c, http.StatusBadRequest, "bank_fact_field", msg)
			return
		}
	}
	bad := func(field, msg string) {
		tx.Rollback()
		respondError(c, http.StatusBadRequest, "invalid_field", field+" "+msg)
	}

	next := *p
	newClientName := ""
	for _, k := range keys {
		raw := body[k]
		switch k {
		case "new_client_name":
			s, ok := editString(raw)
			if !ok {
				bad(k, "must be a string")
				return
			}
			if msg := newClientNameRefusal(s); msg != "" {
				bad(k, msg)
				return
			}
			if s != "" {
				if cid, present := body["client_id"]; present && string(cid) != "null" {
					bad(k, "cannot be given together with client_id: choose a client or add a new one")
					return
				}
			}
			newClientName = s
		case "paid_on":
			d, msg := checkEditPaidOn(raw, "enter the day the money was received")
			if msg != "" {
				bad(k, msg)
				return
			}
			next.PaidOn = d
		case "gross_cents":
			v, msg := checkEditCents(raw)
			if msg != "" {
				bad(k, msg)
				return
			}
			next.GrossCents = v
			next.NetCents = v - next.FeeCents
		case "method":
			s, ok := editString(raw)
			if _, known := data_models.PaymentMethodLabels[s]; !ok || !known {
				bad(k, "must be one of "+strings.Join(data_models.PaymentMethods, ", "))
				return
			}
			next.Method = s
		case "reference":
			s, msg := checkEditReference(raw)
			if msg != "" {
				bad(k, msg)
				return
			}
			next.Reference = s
		case "sender_name":
			s, msg := checkEditOneLineName(raw)
			if msg != "" {
				bad(k, msg)
				return
			}
			next.SenderName = s
		case "note":
			s, msg := checkEditNote(raw)
			if msg != "" {
				bad(k, msg)
				return
			}
			next.Note = s
		case "category":
			var cat string
			if err := json.Unmarshal(raw, &cat); err != nil || !data_models.IsPaymentCategory(cat) {
				bad(k, "must be one of uncategorised, paid_work, gift, loan, refund, other")
				return
			}
			if p.Kind != data_models.PaymentKindIncoming {
				tx.Rollback()
				RespondWithError(c, http.StatusBadRequest, "Only incoming payments are categorised; this row is a bank withdrawal")
				return
			}
			// A transfer from PayPal into N26 is money the PayPal import
			// already holds, so it is never paid work (system_3 #9881).
			if cat == data_models.PaymentCategoryPaidWork && cat != p.Category && isPayPalTransferIntoN26(*p) {
				tx.Rollback()
				respondError(c, http.StatusUnprocessableEntity, "paypal_transfer_not_paid_work",
					"this is a transfer from PayPal into N26; the PayPal payments it sums are the paid work, so it cannot be paid work itself")
				return
			}
			next.Category = cat
		case "client_id":
			cid, err := optionalID(raw)
			if err != nil {
				bad(k, err.Error())
				return
			}
			if cid != nil {
				cl, err := data_models.GetInvoicingClient(tx, owner, *cid)
				if err != nil {
					fail(tx, "client read", err)
					return
				}
				if cl == nil {
					bad(k, "does not name a client")
					return
				}
			}
			next.ClientID = cid
		}
	}
	// PayPal ids are upper-case; store a hand-entered PayPal reference that
	// way, as the hand-entry handler does (#8409 security F2). Only when this
	// edit sends the reference or the method: an unrelated edit must not
	// rewrite a stored reference it never mentioned, unaudited.
	_, sentRef := body["reference"]
	_, sentMethod := body["method"]
	if next.Method == "paypal" && (sentRef || sentMethod) {
		next.Reference = strings.ToUpper(next.Reference)
	}

	// A new name adds the client in this transaction and links it, so the
	// edit's audit event records the new client_id (system_3 #10696). Any
	// refusal below rolls the client back with the edit.
	diffKeys := keys
	var newClient *data_models.InvoicingClient
	if newClientName != "" {
		cl, existing, err := addPayerClient(tx, owner, newClientName, "payment_edit")
		if err != nil {
			fail(tx, "new client", err)
			return
		}
		if existing != nil {
			tx.Rollback()
			respondClientExists(c, existing)
			return
		}
		newClient = cl
		next.ClientID = &cl.ID
		if _, sent := body["client_id"]; !sent {
			diffKeys = append(append([]string{}, keys...), "client_id")
		}
	}

	before, after := changedFields(diffKeys, paymentEditView(*p), paymentEditView(next))
	if len(after) == 0 {
		tx.Rollback()
		c.JSON(http.StatusOK, gin.H{"payment": p})
		return
	}

	// An issued invoice is immutable (spec §10), and so is every fact about
	// the payments it covers. Cancel the invoice first.
	issuedID, err := data_models.IssuedInvoiceForPayment(tx, p.ID, 0)
	if err != nil {
		fail(tx, "issued check", err)
		return
	}
	if issuedID != 0 {
		label := fmt.Sprintf("%d", issuedID)
		if inv, err := data_models.GetInvoicingInvoice(tx, owner, issuedID); err == nil && inv != nil {
			label = invoiceLabel(*inv)
		}
		tx.Rollback()
		respondError(c, http.StatusConflict, "payment_on_issued_invoice",
			fmt.Sprintf("payment is on issued invoice %s; cancel that invoice before changing it", label))
		return
	}
	// A draft invoices paid work only, so a payment on one keeps that
	// category. Its other edits are allowed: the draft keeps the amount and
	// description it was made with.
	if _, changed := after["category"]; changed && next.Category != data_models.PaymentCategoryPaidWork {
		draft, err := draftCoveringPayment(tx, owner, p.ID)
		if err != nil {
			fail(tx, "draft check", err)
			return
		}
		if draft != nil {
			tx.Rollback()
			respondError(c, http.StatusConflict, "payment_on_draft",
				fmt.Sprintf("payment is on draft D-%d; take it off the draft before changing its category", draft.ID))
			return
		}
	}

	// A hand-entered PayPal payment and an imported one must not become two
	// copies of one PayPal transaction, so a changed PayPal reference gets
	// the hand-entry handler's duplicate check.
	if next.Method == "paypal" && next.Reference != "" && (next.Reference != p.Reference || next.Method != p.Method) {
		exists, err := data_models.InvoicingPaymentExists(tx, owner, invoicingPaymentSourcePayPal, next.Reference)
		if err == nil && !exists {
			exists, err = data_models.ManualPayPalPaymentWithReference(tx, owner, next.Reference)
		}
		if err != nil {
			fail(tx, "duplicate check", err)
			return
		}
		if exists {
			tx.Rollback()
			respondError(c, http.StatusConflict, "duplicate_payment",
				fmt.Sprintf("a PayPal payment with transaction id %s is already stored", next.Reference))
			return
		}
	}

	if err := tx.Save(&next).Error; err != nil {
		fail(tx, "save", err)
		return
	}
	action := invoicingActionEdit
	if onlyKeys(after, "category", "client_id") {
		action = invoicingActionCategorise
	}
	if err := data_models.RecordInvoicingAuditEvent(tx, owner, action, invoicingEntityPayment, p.ID, auditDetails(gin.H{"before": before, "after": after})); err != nil {
		fail(tx, "audit", err)
		return
	}
	if err := tx.Commit().Error; err != nil {
		log.Printf("Error committing payment edit %d: %v", id, err)
		RespondWithError(c, http.StatusInternalServerError, "Error updating payment")
		return
	}
	resp := gin.H{"payment": next}
	if newClient != nil {
		resp["client"] = newClient
	}
	c.JSON(http.StatusOK, resp)
}

// onlyKeys reports whether every key of m is one of allowed.
func onlyKeys(m gin.H, allowed ...string) bool {
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	for k := range m {
		if !ok[k] {
			return false
		}
	}
	return true
}

// PatchInvoicingExpenseHandler edits a work expense: PATCH
// /invoicing/expenses/:id. An expense is never invoiced, so it carries no
// invoice locks.
func PatchInvoicingExpenseHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		RespondWithError(c, http.StatusBadRequest, "Invalid expense id")
		return
	}
	body, keys, ok := editBody(c, expenseEditableByHand)
	if !ok {
		return
	}
	owner := invoicingUserID(c)
	fail := func(tx *gorm.DB, what string, err error) {
		tx.Rollback()
		log.Printf("Error editing expense %d (%s): %v", id, what, err)
		RespondWithError(c, http.StatusInternalServerError, "Error updating expense")
	}

	tx := db.Begin()
	if err := data_models.LockInvoicingBook(tx, owner); err != nil {
		fail(tx, "book lock", err)
		return
	}
	if err := lockExpenseRow(tx, owner, uint(id)); err != nil {
		fail(tx, "row lock", err)
		return
	}
	e, err := data_models.GetInvoicingExpense(tx, owner, uint(id))
	if err != nil {
		fail(tx, "read", err)
		return
	}
	if e == nil {
		tx.Rollback()
		RespondWithError(c, http.StatusNotFound, "Expense not found")
		return
	}
	if !e.IsManual() {
		if msg := bankFactRefusal(keys, expenseEditableImported, "expense"); msg != "" {
			tx.Rollback()
			respondError(c, http.StatusBadRequest, "bank_fact_field", msg)
			return
		}
	}
	bad := func(field, msg string) {
		tx.Rollback()
		respondError(c, http.StatusBadRequest, "invalid_field", field+" "+msg)
	}

	next := *e
	for _, k := range keys {
		raw := body[k]
		switch k {
		case "paid_on":
			d, msg := checkEditPaidOn(raw, "enter the day the expense was paid")
			if msg != "" {
				bad(k, msg)
				return
			}
			next.PaidOn = d
		case "amount_cents":
			v, msg := checkEditCents(raw)
			if msg != "" {
				bad(k, msg)
				return
			}
			next.AmountCents = v
		case "category":
			s, ok := editString(raw)
			if !ok || s == "" {
				bad(k, "is not one of your expense categories")
				return
			}
			valid, err := data_models.IsExpenseCategoryForOwner(tx, owner, s)
			if err != nil {
				fail(tx, "category check", err)
				return
			}
			if !valid {
				bad(k, "is not one of your expense categories")
				return
			}
			next.Category = s
		case "payee":
			s, msg := checkEditOneLineName(raw)
			if msg != "" {
				bad(k, msg)
				return
			}
			next.Payee = s
		case "reference":
			s, msg := checkEditReference(raw)
			if msg != "" {
				bad(k, msg)
				return
			}
			next.Reference = s
		case "note":
			s, msg := checkEditNote(raw)
			if msg != "" {
				bad(k, msg)
				return
			}
			next.Note = s
		}
	}

	before, after := changedFields(keys, expenseEditView(*e), expenseEditView(next))
	if len(after) == 0 {
		tx.Rollback()
		c.JSON(http.StatusOK, gin.H{"expense": e})
		return
	}
	if err := tx.Save(&next).Error; err != nil {
		fail(tx, "save", err)
		return
	}
	if err := data_models.RecordInvoicingAuditEvent(tx, owner, invoicingActionEdit, invoicingEntityExpense, e.ID, auditDetails(gin.H{"before": before, "after": after})); err != nil {
		fail(tx, "audit", err)
		return
	}
	if err := tx.Commit().Error; err != nil {
		log.Printf("Error committing expense edit %d: %v", id, err)
		RespondWithError(c, http.StatusInternalServerError, "Error updating expense")
		return
	}
	c.JSON(http.StatusOK, gin.H{"expense": next})
}
