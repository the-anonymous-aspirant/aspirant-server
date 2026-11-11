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
	"new_client_name": true,
}

// validOneLineName is the rule a payer name typed by hand meets, whether it
// is the payment's sender_name or a new client's name (system_3 #10539): one
// line of at most maxManualSenderRunes, with no control or invisible format
// characters.
func validOneLineName(s string) bool {
	return utf8.RuneCountInString(s) <= maxManualSenderRunes && !strings.Contains(s, "\n") && !hasControlChars(s) && !hasFormatChars(s)
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
// note, and either client_id or new_client_name. new_client_name adds that
// payer to the client book and links the payment to it, in one transaction;
// a name already in the book (design §3.142 Decision 4) is refused with 409
// client_exists and that client. It is always incoming paid work, and it
// never makes a draft.
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
	if !validOneLineName(sender) {
		bad("sender_name", fmt.Sprintf("must be one line of at most %d characters", maxManualSenderRunes))
		return
	}
	// new_client_name adds the payer to the client book in the same
	// transaction as the payment (system_3 #10539, design §3.142 Decisions 2
	// and 3): both are saved, or neither.
	newClientName, ok := str("new_client_name")
	if !ok {
		return
	}
	if !validOneLineName(newClientName) {
		bad("new_client_name", fmt.Sprintf("must be one line of at most %d characters", maxManualSenderRunes))
		return
	}
	if newClientName != "" {
		if raw, present := body["client_id"]; present && string(raw) != "null" {
			bad("new_client_name", "cannot be given together with client_id: choose a client or add a new one")
			return
		}
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
	if _, err := lockIssuing(tx, invoicingUserID(c)); err != nil {
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
			cl, err := data_models.GetInvoicingClient(tx, invoicingUserID(c), *cid)
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
	// A new payer name: refuse one that is already in the book under
	// Decision 4's "the same", naming that client so the form can offer it,
	// rather than make a silent duplicate. The issuing lock taken above
	// serialises two hand entries of one new name, so the second one finds
	// the first one's client here.
	var newClient *data_models.InvoicingClient
	if newClientName != "" {
		existing, err := findSameClient(tx, invoicingUserID(c), newClientName)
		if err != nil {
			tx.Rollback()
			log.Printf("Error reading the client book: %v", err)
			RespondWithError(c, http.StatusInternalServerError, "Error storing payment")
			return
		}
		if existing != nil {
			tx.Rollback()
			c.JSON(http.StatusConflict, gin.H{
				"error": ErrorDetail{
					Code:    "client_exists",
					Message: fmt.Sprintf("%s is already in your client book", existing.Name),
				},
				"client": existing,
			})
			return
		}
		cl := data_models.InvoicingClient{OwnerUserID: invoicingUserID(c), Name: newClientName}
		if err := tx.Create(&cl).Error; err != nil {
			tx.Rollback()
			log.Printf("Error creating a client for a manual payment: %v", err)
			RespondWithError(c, http.StatusInternalServerError, "Error storing payment")
			return
		}
		if err := data_models.RecordInvoicingAuditEvent(tx, invoicingUserID(c), invoicingActionCreate, invoicingEntityClient, cl.ID, auditDetails(gin.H{"after": cl, "via": "manual_payment"})); err != nil {
			tx.Rollback()
			log.Printf("Error auditing client create %d: %v", cl.ID, err)
			RespondWithError(c, http.StatusInternalServerError, "Error storing payment")
			return
		}
		newClient = &cl
		clientID = &cl.ID
	}
	// A PayPal payment entered by hand and later imported (or the other way
	// round) must not become two payments.
	if method == "paypal" && reference != "" {
		exists, err := data_models.InvoicingPaymentExists(tx, invoicingUserID(c), invoicingPaymentSourcePayPal, reference)
		if err == nil && !exists {
			exists, err = data_models.ManualPayPalPaymentWithReference(tx, invoicingUserID(c), reference)
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
		OwnerUserID:   invoicingUserID(c),
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
		"new_client_name": newClientName,
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
	resp := gin.H{"payment": p}
	if newClient != nil {
		resp["client"] = newClient
	}
	c.JSON(http.StatusCreated, resp)
}

// DeletePaymentHandler removes a payment, imported or entered by hand:
// DELETE /invoicing/payments/:id (system_3 #10009, operator c61587/c61853).
// Only a payment on an issued invoice is refused: that invoice must be
// cancelled first. A payment on a draft or a cancelled invoice is deleted
// with its link. The draft keeps its line items and its total, and says the
// payments no longer cover it (invoiceWarnings); the cancelled invoice keeps
// its number and its stored PDF. A tombstone keeps a re-import of the same
// file from bringing an imported payment back, and the delete's audit event
// keeps the whole row.
func DeletePaymentHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		RespondWithError(c, http.StatusBadRequest, "Invalid payment id")
		return
	}
	tx := db.Begin()
	// The issuing lock orders this against a draft create, a payment_ids edit
	// or an issue that would link or freeze the payment while it is deleted:
	// all take the same lock before reading the payment (#8409 security F1).
	if _, err := lockIssuing(tx, invoicingUserID(c)); err != nil {
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
	p, err := data_models.GetInvoicingPayment(tx, invoicingUserID(c), uint(id))
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
	issuedID, err := data_models.IssuedInvoiceForPayment(tx, p.ID, 0)
	if err != nil {
		tx.Rollback()
		log.Printf("Error checking invoices for payment %d: %v", id, err)
		RespondWithError(c, http.StatusInternalServerError, "Error deleting payment")
		return
	}
	if issuedID != 0 {
		label := fmt.Sprintf("%d", issuedID)
		if inv, err := data_models.GetInvoicingInvoice(tx, invoicingUserID(c), issuedID); err == nil && inv != nil {
			label = invoiceLabel(*inv)
		}
		tx.Rollback()
		respondError(c, http.StatusConflict, "payment_on_invoice",
			fmt.Sprintf("payment is on issued invoice %s; cancel that invoice first", label))
		return
	}
	unlinked, err := data_models.InvoiceIDsForPayment(tx, p.ID)
	if err != nil {
		tx.Rollback()
		log.Printf("Error listing invoices for payment %d: %v", id, err)
		RespondWithError(c, http.StatusInternalServerError, "Error deleting payment")
		return
	}
	if err := tx.Where("payment_id = ?", p.ID).Delete(&data_models.InvoicingInvoicePayment{}).Error; err != nil {
		tx.Rollback()
		log.Printf("Error unlinking payment %d: %v", id, err)
		RespondWithError(c, http.StatusInternalServerError, "Error deleting payment")
		return
	}
	if err := tx.Delete(p).Error; err != nil {
		tx.Rollback()
		log.Printf("Error deleting payment %d: %v", id, err)
		RespondWithError(c, http.StatusInternalServerError, "Error deleting payment")
		return
	}
	userID := invoicingUserID(c)
	if err := data_models.TombstoneInvoicingPayment(tx, *p, userID, invoicingNow().UTC()); err != nil {
		tx.Rollback()
		log.Printf("Error tombstoning payment %d: %v", id, err)
		RespondWithError(c, http.StatusInternalServerError, "Error deleting payment")
		return
	}
	// The whole row as it was, so the delete loses nothing the audit trail
	// had, plus the invoices it was unlinked from.
	details := auditDetails(gin.H{"before": p, "unlinked_invoice_ids": unlinked})
	if err := data_models.RecordInvoicingAuditEvent(tx, userID, invoicingActionDelete, invoicingEntityPayment, p.ID, details); err != nil {
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
