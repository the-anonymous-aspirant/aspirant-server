package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Hand-entered payments (system_3 #8409): an invoice must not depend on a
// PayPal import. The owner enters a payment she received; it is stored like
// an imported one, as incoming paid work, and audited. The §10 guardrails are
// unchanged: drafts, issuing and immutability go through the same handlers.

const (
	invoicingActionCreateManual = "create_manual"

	maxManualReferenceRunes = 100
	maxManualSenderRunes    = 200
	maxManualNoteRunes      = 1000
)

var manualPaymentFields = map[string]bool{
	"paid_on": true, "gross_cents": true, "currency": true, "method": true,
	"reference": true, "sender_name": true, "note": true, "client_id": true,
}

// newManualTransactionID is the database key of a hand-entered payment. It is
// never printed; it only keeps (source, transaction_id) unique.
func newManualTransactionID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "M-" + hex.EncodeToString(b), nil
}

func hasControlChars(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) && r != '\n' {
			return true
		}
	}
	return false
}

// hasFormatChars reports invisible format characters (Unicode Cf: the
// right-to-left override, zero-width spaces, …), which would print into the
// exports and could make a reference read differently from what is stored
// (#8409 security F3). A reference cannot be edited, so refuse them up front.
func hasFormatChars(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Cf, r) {
			return true
		}
	}
	return false
}

// lockPaymentRow takes the payment row's lock with a no-op UPDATE. A delete
// and a draft that links the payment both take it, so they run one at a
// time (#8409 security F1).
func lockPaymentRow(tx *gorm.DB, id uint) error {
	return tx.Model(&data_models.InvoicingPayment{}).Where("id = ?", id).
		UpdateColumn("category", gorm.Expr("category")).Error
}

// paymentLabel names a payment in a message: its printed reference, or its
// id when it has none (a hand-entered payment's internal key is not shown).
func paymentLabel(p data_models.InvoicingPayment) string {
	if ref := p.PrintedReference(); ref != "" {
		return ref
	}
	return fmt.Sprintf("#%d", p.ID)
}

// earliestPaymentDate bounds paid_on from below, to catch a mistyped year
// (#8409 security F4).
var earliestPaymentDate = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// CreateManualPaymentHandler stores a payment the owner received and enters
// by hand: POST /invoicing/payments/manual with paid_on (YYYY-MM-DD, not after
// today in Berlin), gross_cents (> 0), method (bank_transfer, paypal, stripe,
// cash, other), and optional currency (default EUR), reference, sender_name,
// note and client_id. It is always incoming paid work.
func CreateManualPaymentHandler(c *gin.Context) {
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
		if !manualPaymentFields[k] {
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
		bad("paid_on", "cannot be in the future: enter the day the money was received")
		return
	}
	if paidOn.Before(earliestPaymentDate) {
		bad("paid_on", "must be 2000-01-01 or later")
		return
	}
	var gross int64
	if err := json.Unmarshal(body["gross_cents"], &gross); err != nil || gross <= 0 || gross > maxUnitPriceCents {
		bad("gross_cents", "must be a whole number of cents above zero")
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
	method, ok := str("method")
	if !ok {
		return
	}
	if _, known := data_models.PaymentMethodLabels[method]; !known {
		bad("method", "must be one of "+strings.Join(data_models.PaymentMethods, ", "))
		return
	}
	reference, ok := str("reference")
	if !ok {
		return
	}
	if utf8.RuneCountInString(reference) > maxManualReferenceRunes || strings.Contains(reference, "\n") || hasControlChars(reference) || hasFormatChars(reference) {
		bad("reference", fmt.Sprintf("must be one line of at most %d characters", maxManualReferenceRunes))
		return
	}
	sender, ok := str("sender_name")
	if !ok {
		return
	}
	if utf8.RuneCountInString(sender) > maxManualSenderRunes || strings.Contains(sender, "\n") || hasControlChars(sender) || hasFormatChars(sender) {
		bad("sender_name", fmt.Sprintf("must be one line of at most %d characters", maxManualSenderRunes))
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

	// PayPal transaction ids are upper-case letters and digits; store a
	// PayPal reference that way so "tx1" and an imported "TX1" are one id
	// (#8409 security F2).
	if method == "paypal" {
		reference = strings.ToUpper(reference)
	}

	tx := db.Begin()
	// The duplicate check and the insert run under the issuing lock, so two
	// hand entries of the same PayPal id cannot both pass the check.
	if _, err := lockIssuing(tx); err != nil {
		tx.Rollback()
		log.Printf("Error locking for a manual payment: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error storing payment")
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
			cl, err := data_models.GetInvoicingClient(tx, *cid)
			if err != nil {
				tx.Rollback()
				log.Printf("Error reading client %d: %v", *cid, err)
				RespondWithError(c, http.StatusInternalServerError, "Error storing payment")
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
	// A PayPal payment entered by hand and later imported (or the other way
	// round) must not become two payments.
	if method == "paypal" && reference != "" {
		exists, err := data_models.InvoicingPaymentExists(tx, invoicingPaymentSourcePayPal, reference)
		if err == nil && !exists {
			exists, err = data_models.ManualPayPalPaymentWithReference(tx, reference)
		}
		if err != nil {
			tx.Rollback()
			log.Printf("Error checking for a duplicate PayPal payment: %v", err)
			RespondWithError(c, http.StatusInternalServerError, "Error storing payment")
			return
		}
		if exists {
			tx.Rollback()
			respondError(c, http.StatusConflict, "duplicate_payment",
				fmt.Sprintf("a PayPal payment with transaction id %s is already stored", reference))
			return
		}
	}
	txnID, err := newManualTransactionID()
	if err != nil {
		tx.Rollback()
		log.Printf("Error generating a payment id: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error storing payment")
		return
	}
	p := data_models.InvoicingPayment{
		Source:        data_models.PaymentSourceManual,
		TransactionID: txnID,
		Kind:          data_models.PaymentKindIncoming,
		PaidOn:        paidOn,
		Currency:      currency,
		GrossCents:    gross,
		NetCents:      gross,
		SenderName:    sender,
		Note:          note,
		Category:      data_models.PaymentCategoryPaidWork,
		ClientID:      clientID,
		Entry:         data_models.PaymentEntryManual,
		Method:        method,
		Reference:     reference,
	}
	if err := tx.Create(&p).Error; err != nil {
		tx.Rollback()
		log.Printf("Error storing manual payment: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error storing payment")
		return
	}
	details := auditDetails(gin.H{
		"paid_on": paidOnRaw, "gross_cents": gross, "currency": currency, "method": method,
		"reference": reference, "sender_name": sender, "note": note, "client_id": clientID,
	})
	if err := data_models.RecordInvoicingAuditEvent(tx, invoicingUserID(c), invoicingActionCreateManual, invoicingEntityPayment, p.ID, details); err != nil {
		tx.Rollback()
		log.Printf("Error auditing manual payment %d: %v", p.ID, err)
		RespondWithError(c, http.StatusInternalServerError, "Error storing payment")
		return
	}
	if err := tx.Commit().Error; err != nil {
		log.Printf("Error committing manual payment: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error storing payment")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"payment": p})
}

// DeleteManualPaymentHandler removes a hand-entered payment that is on no
// invoice (draft, issued or cancelled): DELETE /invoicing/payments/:id. An
// imported payment is never deleted, because the file is its record.
func DeleteManualPaymentHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		RespondWithError(c, http.StatusBadRequest, "Invalid payment id")
		return
	}
	tx := db.Begin()
	// The issuing lock orders this against a draft create or a payment_ids
	// edit that would link the payment while it is deleted: both take the
	// same lock before reading the payment (#8409 security F1).
	if _, err := lockIssuing(tx); err != nil {
		tx.Rollback()
		log.Printf("Error locking for payment %d: %v", id, err)
		RespondWithError(c, http.StatusInternalServerError, "Error deleting payment")
		return
	}
	if err := lockPaymentRow(tx, uint(id)); err != nil {
		tx.Rollback()
		log.Printf("Error locking payment %d: %v", id, err)
		RespondWithError(c, http.StatusInternalServerError, "Error deleting payment")
		return
	}
	p, err := data_models.GetInvoicingPayment(tx, uint(id))
	if err != nil {
		tx.Rollback()
		log.Printf("Error reading payment %d: %v", id, err)
		RespondWithError(c, http.StatusInternalServerError, "Error deleting payment")
		return
	}
	if p == nil {
		tx.Rollback()
		RespondWithError(c, http.StatusNotFound, "Payment not found")
		return
	}
	if !p.IsManual() {
		tx.Rollback()
		respondError(c, http.StatusConflict, "imported_payment",
			"an imported payment cannot be deleted; its PayPal file is the record")
		return
	}
	invoiceID, err := data_models.PaymentOnAnyInvoice(tx, p.ID)
	if err != nil {
		tx.Rollback()
		log.Printf("Error checking invoices for payment %d: %v", id, err)
		RespondWithError(c, http.StatusInternalServerError, "Error deleting payment")
		return
	}
	if invoiceID != 0 {
		tx.Rollback()
		respondError(c, http.StatusConflict, "payment_on_invoice",
			fmt.Sprintf("payment is on invoice %d; delete that draft (or cancel that invoice) first", invoiceID))
		return
	}
	if err := tx.Delete(p).Error; err != nil {
		tx.Rollback()
		log.Printf("Error deleting payment %d: %v", id, err)
		RespondWithError(c, http.StatusInternalServerError, "Error deleting payment")
		return
	}
	details := auditDetails(gin.H{
		"paid_on": p.PaidOn.Format(invoicingDateLayout), "gross_cents": p.GrossCents, "currency": p.Currency,
		"method": p.Method, "reference": p.Reference, "sender_name": p.SenderName, "note": p.Note,
	})
	if err := data_models.RecordInvoicingAuditEvent(tx, invoicingUserID(c), invoicingActionDelete, invoicingEntityPayment, p.ID, details); err != nil {
		tx.Rollback()
		log.Printf("Error auditing payment delete %d: %v", id, err)
		RespondWithError(c, http.StatusInternalServerError, "Error deleting payment")
		return
	}
	if err := tx.Commit().Error; err != nil {
		log.Printf("Error committing payment delete %d: %v", id, err)
		RespondWithError(c, http.StatusInternalServerError, "Error deleting payment")
		return
	}
	c.Status(http.StatusNoContent)
}
