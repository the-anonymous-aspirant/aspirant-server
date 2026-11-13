package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Work expenses (system_3 #10414-A): money going out, entered by hand. A
// sibling of the manual-payment path (invoicing_manual_payments.go), with the
// same validation rigor, but an expense is never invoiced, so it skips the
// payment-on-invoice checks entirely.

const (
	invoicingEntityExpense         = "expense"
	invoicingEntityExpenseCategory = "expense_category"

	maxExpenseCategoryNameRunes = 100
)

var manualExpenseFields = map[string]bool{
	"paid_on": true, "amount_cents": true, "currency": true,
	"category": true, "reference": true, "note": true, "client_id": true,
	"payee": true,
}

var expenseCategoryFields = map[string]bool{"name": true, "counts_as": true}

// newManualExpenseTransactionID is the database key of a hand-entered
// expense, mirroring newManualTransactionID for payments.
func newManualExpenseTransactionID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "M-" + hex.EncodeToString(b), nil
}

// lockExpenseRow takes the expense row's lock with a no-op UPDATE, so a
// concurrent delete of the same expense waits rather than racing. Scoped to
// the owner (security review of PR 175 F2) so it never locks a row in
// another book.
func lockExpenseRow(tx *gorm.DB, owner, id uint) error {
	return tx.Model(&data_models.InvoicingExpense{}).Where("id = ? AND owner_user_id = ?", id, owner).
		UpdateColumn("category", gorm.Expr("category")).Error
}

// CreateManualExpenseHandler stores a work expense the owner enters by hand:
// POST /invoicing/expenses/manual with paid_on (YYYY-MM-DD, not after today
// in Berlin), amount_cents (> 0), optional currency (default EUR), category
// (one of data_models.ExpenseCategories, default "other"), reference, note
// and client_id.
func CreateManualExpenseHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	var body map[string]json.RawMessage
	if err := c.ShouldBindJSON(&body); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid request body")
		return
	}
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !manualExpenseFields[k] {
			respondError(c, http.StatusBadRequest, "unknown_field", fmt.Sprintf("unknown field %q", k))
			return
		}
	}
	bad := func(field, msg string) {
		respondError(c, http.StatusBadRequest, "invalid_field", field+" "+msg)
	}
	str := func(field string) (string, bool) {
		raw, ok := body[field]
		if !ok || string(raw) == "null" {
			return "", true
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			bad(field, "must be a string")
			return "", false
		}
		return strings.TrimSpace(s), true
	}

	paidOnRaw, ok := str("paid_on")
	if !ok {
		return
	}
	paidOn, err := time.Parse(invoicingDateLayout, paidOnRaw)
	if err != nil {
		bad("paid_on", "must be a date, YYYY-MM-DD")
		return
	}
	if paidOn.After(berlinToday()) {
		bad("paid_on", "cannot be in the future: enter the day the expense was paid")
		return
	}
	if paidOn.Before(earliestPaymentDate) {
		bad("paid_on", "must be 2000-01-01 or later")
		return
	}
	var amount int64
	if err := json.Unmarshal(body["amount_cents"], &amount); err != nil || amount <= 0 || amount > maxUnitPriceCents {
		bad("amount_cents", "must be a whole number of cents above zero")
		return
	}
	currency, ok := str("currency")
	if !ok {
		return
	}
	if currency == "" {
		currency = "EUR"
	}
	if !isoCurrency.MatchString(currency) {
		bad("currency", "must be a three-letter ISO code such as EUR")
		return
	}
	category, ok := str("category")
	if !ok {
		return
	}
	if category == "" {
		category = data_models.ExpenseCategoryOther
	}
	reference, ok := str("reference")
	if !ok {
		return
	}
	if utf8.RuneCountInString(reference) > maxManualReferenceRunes || strings.Contains(reference, "\n") || hasControlChars(reference) || hasFormatChars(reference) {
		bad("reference", fmt.Sprintf("must be one line of at most %d characters", maxManualReferenceRunes))
		return
	}
	note, ok := str("note")
	if !ok {
		return
	}
	if utf8.RuneCountInString(note) > maxManualNoteRunes || hasControlChars(note) {
		bad("note", fmt.Sprintf("must be at most %d characters", maxManualNoteRunes))
		return
	}
	// "Paid to (optional)" (system_3 #10555-A1, design §3.143 Decision 3):
	// the same one-line rule as a payment's payer.
	payee, ok := str("payee")
	if !ok {
		return
	}
	if !validOneLineName(payee) {
		bad("payee", fmt.Sprintf("must be one line of at most %d characters", maxManualSenderRunes))
		return
	}

	tx := db.Begin()
	if err := data_models.LockInvoicingBook(tx, invoicingUserID(c)); err != nil {
		tx.Rollback()
		log.Printf("Error locking for a manual expense: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error storing expense")
		return
	}
	valid, err := data_models.IsExpenseCategoryForOwner(tx, invoicingUserID(c), category)
	if err != nil {
		tx.Rollback()
		log.Printf("Error checking expense category %q: %v", category, err)
		RespondWithError(c, http.StatusInternalServerError, "Error storing expense")
		return
	}
	if !valid {
		tx.Rollback()
		bad("category", "is not one of your expense categories")
		return
	}
	var clientID *uint
	if raw, present := body["client_id"]; present {
		cid, err := optionalID(raw)
		if err != nil {
			tx.Rollback()
			bad("client_id", err.Error())
			return
		}
		if cid != nil {
			cl, err := data_models.GetInvoicingClient(tx, invoicingUserID(c), *cid)
			if err != nil {
				tx.Rollback()
				log.Printf("Error reading client %d: %v", *cid, err)
				RespondWithError(c, http.StatusInternalServerError, "Error storing expense")
				return
			}
			if cl == nil {
				tx.Rollback()
				bad("client_id", "does not name a client")
				return
			}
		}
		clientID = cid
	}
	txnID, err := newManualExpenseTransactionID()
	if err != nil {
		tx.Rollback()
		log.Printf("Error generating an expense id: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error storing expense")
		return
	}
	e := data_models.InvoicingExpense{
		OwnerUserID:   invoicingUserID(c),
		Source:        data_models.ExpenseSourceManual,
		TransactionID: txnID,
		PaidOn:        paidOn,
		Currency:      currency,
		AmountCents:   amount,
		Note:          note,
		Category:      category,
		ClientID:      clientID,
		Entry:         data_models.ExpenseEntryManual,
		Reference:     reference,
		Payee:         payee,
	}
	if err := tx.Create(&e).Error; err != nil {
		tx.Rollback()
		log.Printf("Error storing manual expense: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error storing expense")
		return
	}
	details := auditDetails(gin.H{
		"paid_on": paidOnRaw, "amount_cents": amount, "currency": currency, "category": category,
		"reference": reference, "note": note, "client_id": clientID, "payee": payee,
	})
	if err := data_models.RecordInvoicingAuditEvent(tx, invoicingUserID(c), invoicingActionCreateManual, invoicingEntityExpense, e.ID, details); err != nil {
		tx.Rollback()
		log.Printf("Error auditing manual expense %d: %v", e.ID, err)
		RespondWithError(c, http.StatusInternalServerError, "Error storing expense")
		return
	}
	if err := tx.Commit().Error; err != nil {
		log.Printf("Error committing manual expense: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error storing expense")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"expense": e})
}

// ListInvoicingExpensesHandler lists the owner's expenses, oldest first,
// optionally narrowed by category: GET /invoicing/expenses. The owner's
// category list has its own endpoint (ListInvoicingExpenseCategoriesHandler).
func ListInvoicingExpensesHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	category := c.Query("category")
	if category != "" {
		valid, err := isExpenseCategoryForOwnerLocked(db, invoicingUserID(c), category)
		if err != nil {
			log.Printf("Error checking expense category %q: %v", category, err)
			RespondWithError(c, http.StatusInternalServerError, "Error listing expenses")
			return
		}
		if !valid {
			RespondWithError(c, http.StatusBadRequest, "Unknown category")
			return
		}
	}
	// ?removed=true serves the removed view (§3.143 Decision 6).
	var expenses []data_models.InvoicingExpense
	var err error
	if c.Query("removed") == "true" {
		expenses, err = data_models.ListRemovedInvoicingExpenses(db, invoicingUserID(c))
	} else {
		expenses, err = data_models.ListInvoicingExpenses(db, invoicingUserID(c), category)
	}
	if err != nil {
		log.Printf("Error listing expenses: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error listing expenses")
		return
	}
	c.JSON(http.StatusOK, gin.H{"expenses": expenses})
}

// ListInvoicingExpenseCategoriesHandler lists the owner's expense
// categories, seeding the defaults first if she has none yet: GET
// /invoicing/expense-categories.
func ListInvoicingExpenseCategoriesHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	cats, err := listInvoicingExpenseCategoriesLocked(db, invoicingUserID(c))
	if err != nil {
		log.Printf("Error listing expense categories: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error listing expense categories")
		return
	}
	c.JSON(http.StatusOK, gin.H{"categories": cats})
}

// listInvoicingExpenseCategoriesLocked takes the book lock before seeding
// (security review of PR 175 F3): without it, two concurrent first requests
// can both see zero categories and both try to seed, and the loser's insert
// hits the (owner, name) unique index. A create or delete already locks the
// same way, so this only needs to join that existing serialisation.
func listInvoicingExpenseCategoriesLocked(db *gorm.DB, owner uint) ([]data_models.InvoicingExpenseCategory, error) {
	tx := db.Begin()
	if err := data_models.LockInvoicingBook(tx, owner); err != nil {
		tx.Rollback()
		return nil, err
	}
	cats, err := data_models.ListInvoicingExpenseCategories(tx, owner)
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	return cats, tx.Commit().Error
}

// isExpenseCategoryForOwnerLocked is listInvoicingExpenseCategoriesLocked's
// counterpart for a single-category existence check (same seeding race).
func isExpenseCategoryForOwnerLocked(db *gorm.DB, owner uint, name string) (bool, error) {
	tx := db.Begin()
	if err := data_models.LockInvoicingBook(tx, owner); err != nil {
		tx.Rollback()
		return false, err
	}
	valid, err := data_models.IsExpenseCategoryForOwner(tx, owner, name)
	if err != nil {
		tx.Rollback()
		return false, err
	}
	return valid, tx.Commit().Error
}

// CreateExpenseCategoryHandler adds one of the owner's own expense
// categories on top of the seeded defaults (system_3 #10424, operator's own
// ask c63803 on #9656): POST /invoicing/expense-categories with name and
// optional counts_as (one of data_models.DefaultExpenseCategories, default
// "other" — the report line this category's entries add into, design_agent
// c63823). Case-insensitively deduplicated against her existing categories,
// which also refuses a new category taking a starter type's name, since the
// starters are rows in the same list.
func CreateExpenseCategoryHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	var body map[string]json.RawMessage
	if err := c.ShouldBindJSON(&body); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid request body")
		return
	}
	for k := range body {
		if !expenseCategoryFields[k] {
			respondError(c, http.StatusBadRequest, "unknown_field", fmt.Sprintf("unknown field %q", k))
			return
		}
	}
	raw, present := body["name"]
	if !present {
		respondError(c, http.StatusBadRequest, "invalid_field", "name is required")
		return
	}
	var name string
	if err := json.Unmarshal(raw, &name); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_field", "name must be a string")
		return
	}
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxExpenseCategoryNameRunes || strings.Contains(name, "\n") || hasControlChars(name) || hasFormatChars(name) {
		respondError(c, http.StatusBadRequest, "invalid_field", fmt.Sprintf("name must be one line of 1-%d characters", maxExpenseCategoryNameRunes))
		return
	}
	// Private is built in and never counts (§3.143 Decision 8): she cannot
	// make a second one under another case, nor any category that counts as it.
	if strings.EqualFold(name, data_models.ExpenseCategoryPrivate) {
		respondError(c, http.StatusConflict, "reserved_category", "Private is a built-in category for expenses that never count")
		return
	}
	countsAs := data_models.ExpenseCategoryOther
	if raw, present := body["counts_as"]; present {
		if err := json.Unmarshal(raw, &countsAs); err != nil {
			respondError(c, http.StatusBadRequest, "invalid_field", "counts_as must be a string")
			return
		}
		countsAs = strings.TrimSpace(countsAs)
	}
	if !data_models.IsStarterExpenseCategory(countsAs) {
		respondError(c, http.StatusBadRequest, "invalid_field", "counts_as must be one of "+strings.Join(data_models.DefaultExpenseCategories, ", "))
		return
	}

	tx := db.Begin()
	if err := data_models.LockInvoicingBook(tx, invoicingUserID(c)); err != nil {
		tx.Rollback()
		log.Printf("Error locking for an expense category: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error storing category")
		return
	}
	cats, err := data_models.ListInvoicingExpenseCategories(tx, invoicingUserID(c))
	if err != nil {
		tx.Rollback()
		log.Printf("Error listing expense categories: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error storing category")
		return
	}
	for _, existing := range cats {
		if strings.EqualFold(existing.Name, name) {
			tx.Rollback()
			respondError(c, http.StatusConflict, "duplicate_category", fmt.Sprintf("category %q already exists", existing.Name))
			return
		}
	}
	cat := data_models.InvoicingExpenseCategory{OwnerUserID: invoicingUserID(c), Name: name, CountsAs: countsAs}
	if err := tx.Create(&cat).Error; err != nil {
		tx.Rollback()
		log.Printf("Error storing expense category: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error storing category")
		return
	}
	details := auditDetails(gin.H{"name": name, "counts_as": countsAs})
	if err := data_models.RecordInvoicingAuditEvent(tx, invoicingUserID(c), invoicingActionCreateManual, invoicingEntityExpenseCategory, cat.ID, details); err != nil {
		tx.Rollback()
		log.Printf("Error auditing expense category %d: %v", cat.ID, err)
		RespondWithError(c, http.StatusInternalServerError, "Error storing category")
		return
	}
	if err := tx.Commit().Error; err != nil {
		log.Printf("Error committing expense category: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error storing category")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"category": cat})
}
