package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Invoicing app (system_3 #8367): issuer profile, clients and the audit trail.
// Every route here sits behind the owner+Admin group gate in routes.go; these
// handlers assume the caller already passed it.

// Audit vocabulary. Entity types name the table a row is about; actions are
// the verbs the spec's audit requirement lists (create, edit, issue, export).
const (
	invoicingEntityProfile = "profile"
	invoicingEntityClient  = "client"

	invoicingActionCreate = "create"
	invoicingActionUpdate = "update"
)

var invoicePrefixPattern = regexp.MustCompile(`^[0-9A-Za-z-]{1,20}$`)

// invoicePhonePattern is the optional issuer phone (system_3 #9967): what a
// German number is written with, at most 40 characters, or empty.
var invoicePhonePattern = regexp.MustCompile(`^[0-9+()/. -]{0,40}$`)

// The refusals the editors key on to mark the field (system_3 #9993, #9971):
// a dedicated code each, not the generic bad_request.
const (
	invalidPhoneMessage = "phone may hold digits, spaces and + - ( ) / . only, at most 40 characters"
	invalidLinkMessage  = "%s takes a handle (@name) or a link to a profile on %s"
)

// bindSocialLinks normalises the two profile links in place; it writes the
// 400 itself and returns false on a refusal.
func bindSocialLinks(c *gin.Context, instagram, telegram *string) bool {
	ig, err := instagramPlatform.normalize(*instagram)
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid_instagram", fmt.Sprintf(invalidLinkMessage, "instagram_url", "instagram.com"))
		return false
	}
	tg, err := telegramPlatform.normalize(*telegram)
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid_telegram", fmt.Sprintf(invalidLinkMessage, "telegram_url", "t.me"))
		return false
	}
	*instagram, *telegram = ig, tg
	return true
}

// RequireInvoicingUser refuses a request with no signed-in user (system_3
// #10054). Every invoicing read and write is scoped to invoicingUserID, and
// rows from before the owner column have owner 0, so a request that reached
// a handler as user 0 would read them. The route gate always sets a user;
// this keeps that true for every route in the group, whatever is added later.
func RequireInvoicingUser() gin.HandlerFunc {
	return func(c *gin.Context) {
		if invoicingUserID(c) == 0 {
			RespondWithError(c, http.StatusUnauthorized, "Sign in to use the invoicing app")
			c.Abort()
			return
		}
		c.Next()
	}
}

// invoicingUserID is the signed-in user, who is also the owner of the book
// every handler reads and writes (system_3 #10054).
func invoicingUserID(c *gin.Context) uint {
	if v, ok := c.Get("user_id"); ok {
		if id, ok := v.(uint); ok {
			return id
		}
	}
	return 0
}

// auditDetails renders a change record as JSON for the audit row. A marshal
// failure is impossible for these map shapes; an empty string keeps the
// mutation going rather than failing the user's save over its log line.
func auditDetails(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// validOptionalEmail accepts an empty string or one bare address.
func validOptionalEmail(s string) bool {
	if s == "" {
		return true
	}
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s
}

type invoicingProfileRequest struct {
	FullName            string `json:"full_name"`
	Address             string `json:"address"`
	Steuernummer        string `json:"steuernummer"`
	Email               string `json:"email"`
	Phone               string `json:"phone"`
	InstagramURL        string `json:"instagram_url"`
	TelegramURL         string `json:"telegram_url"`
	IsKleinunternehmer  *bool  `json:"is_kleinunternehmer"`
	InvoiceNumberPrefix string `json:"invoice_number_prefix"`
	NextInvoiceNumber   *int   `json:"next_invoice_number"`
}

// GetInvoicingProfileHandler returns the issuer profile, or the defaults when
// none has been saved yet (saved=false tells the UI it is unseeded).
func GetInvoicingProfileHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	owner := invoicingUserID(c)
	p, err := data_models.GetInvoicingProfile(db, owner)
	if err != nil {
		log.Printf("Error retrieving invoicing profile: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error retrieving profile")
		return
	}
	logo, err := data_models.GetInvoicingProfileLogo(db, owner)
	if err != nil {
		log.Printf("Error retrieving invoicing logo: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error retrieving profile")
		return
	}
	// has_logo tells the editor whether to fetch the preview; the bytes are
	// served by GET /invoicing/profile/logo, never in this payload.
	if p == nil {
		d := data_models.DefaultInvoicingProfile()
		c.JSON(http.StatusOK, gin.H{"profile": d, "saved": false, "has_logo": logo != nil})
		return
	}
	c.JSON(http.StatusOK, gin.H{"profile": p, "saved": true, "has_logo": logo != nil})
}

// PutInvoicingProfileHandler replaces the issuer profile. The admin seeds it
// and the owner edits it (spec §4.1); both reach here through the gate. An
// empty Steuernummer is allowed to save — it blocks issuing, not editing.
func PutInvoicingProfileHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	var req invoicingProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid request body")
		return
	}
	req.FullName = strings.TrimSpace(req.FullName)
	req.Address = strings.TrimSpace(req.Address)
	req.Steuernummer = strings.TrimSpace(req.Steuernummer)
	req.Email = strings.TrimSpace(req.Email)
	req.Phone = strings.TrimSpace(req.Phone)
	req.InvoiceNumberPrefix = strings.TrimSpace(req.InvoiceNumberPrefix)

	if req.FullName == "" || req.Address == "" {
		RespondWithError(c, http.StatusBadRequest, "full_name and address are required")
		return
	}
	if !validOptionalEmail(req.Email) {
		RespondWithError(c, http.StatusBadRequest, "email is not a valid address")
		return
	}
	if !invoicePhonePattern.MatchString(req.Phone) {
		respondError(c, http.StatusBadRequest, "invalid_phone", invalidPhoneMessage)
		return
	}
	if !bindSocialLinks(c, &req.InstagramURL, &req.TelegramURL) {
		return
	}
	if !invoicePrefixPattern.MatchString(req.InvoiceNumberPrefix) {
		RespondWithError(c, http.StatusBadRequest, "invoice_number_prefix must be 1-20 letters, digits or hyphens")
		return
	}
	if req.IsKleinunternehmer == nil || req.NextInvoiceNumber == nil {
		RespondWithError(c, http.StatusBadRequest, "is_kleinunternehmer and next_invoice_number are required")
		return
	}
	if *req.NextInvoiceNumber < 1 {
		RespondWithError(c, http.StatusBadRequest, "next_invoice_number must be at least 1")
		return
	}

	userID := invoicingUserID(c)
	p := data_models.InvoicingProfile{
		FullName:            req.FullName,
		Address:             req.Address,
		Steuernummer:        req.Steuernummer,
		Email:               req.Email,
		Phone:               req.Phone,
		InstagramURL:        req.InstagramURL,
		TelegramURL:         req.TelegramURL,
		IsKleinunternehmer:  *req.IsKleinunternehmer,
		InvoiceNumberPrefix: req.InvoiceNumberPrefix,
		NextInvoiceNumber:   *req.NextInvoiceNumber,
	}

	tx := db.Begin()
	defer tx.RollbackUnlessCommitted()
	// Wait for any issue in flight before reading the profile and the issued numbers, so a
	// save cannot write back a next number an issue has just moved past
	// (@security N4, c51472 on #8371).
	if _, err := lockIssuing(tx, userID); err != nil {
		tx.Rollback()
		log.Printf("Error locking the issuer profile: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error saving profile")
		return
	}
	existing, err := data_models.GetInvoicingProfile(tx, userID)
	if err != nil {
		tx.Rollback()
		log.Printf("Error reading invoicing profile: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error saving profile")
		return
	}
	action := invoicingActionUpdate
	if existing == nil {
		action = invoicingActionCreate
	}
	// Once a number has been issued under a prefix, the counter belongs to
	// the server: it must read one past the highest number issued, so the
	// sequence stays gap-free and unique (spec §5). A new prefix may start
	// wherever an earlier, outside sequence left off (spec §8 Q3).
	issuedUnderPrefix, err := data_models.CountInvoicesWithPrefix(tx, userID, p.InvoiceNumberPrefix)
	if err != nil {
		tx.Rollback()
		log.Printf("Error counting invoices for prefix %q: %v", p.InvoiceNumberPrefix, err)
		RespondWithError(c, http.StatusInternalServerError, "Error saving profile")
		return
	}
	if issuedUnderPrefix > 0 {
		maxNumber, err := data_models.MaxInvoiceNumberWithPrefix(tx, userID, p.InvoiceNumberPrefix)
		if err != nil {
			tx.Rollback()
			log.Printf("Error reading max invoice number for prefix %q: %v", p.InvoiceNumberPrefix, err)
			RespondWithError(c, http.StatusInternalServerError, "Error saving profile")
			return
		}
		if p.NextInvoiceNumber != maxNumber+1 {
			tx.Rollback()
			respondError(c, http.StatusConflict, "next_number_server_owned",
				fmt.Sprintf("invoices have been issued under %q, so the next number is %d and is set by the server", p.InvoiceNumberPrefix, maxNumber+1))
			return
		}
	}
	if err := data_models.SaveInvoicingProfile(tx, &p, userID); err != nil {
		tx.Rollback()
		log.Printf("Error saving invoicing profile: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error saving profile")
		return
	}
	details := auditDetails(gin.H{"before": existing, "after": p})
	if err := data_models.RecordInvoicingAuditEvent(tx, userID, action, invoicingEntityProfile, p.ID, details); err != nil {
		tx.Rollback()
		log.Printf("Error auditing invoicing profile save: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error saving profile")
		return
	}
	if err := tx.Commit().Error; err != nil {
		log.Printf("Error committing invoicing profile: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error saving profile")
		return
	}
	c.JSON(http.StatusOK, gin.H{"profile": p, "saved": true})
}

type invoicingClientRequest struct {
	Name         string `json:"name"`
	Address      string `json:"address"`
	Email        string `json:"email"`
	PayPalEmail  string `json:"paypal_email"`
	Notes        string `json:"notes"`
	Phone        string `json:"phone"`
	InstagramURL string `json:"instagram_url"`
	TelegramURL  string `json:"telegram_url"`
	// IBAN is optional in the body: absent keeps the stored value, so a form
	// that does not know the field yet cannot wipe one an import set; "" clears
	// it. Bound into NormaliseIBAN's form (system_3 #11551).
	IBAN *string `json:"iban"`
	// A second client with the name of one already in the book is asked
	// about, not refused (system_3 #12264): the create answers 409
	// client_exists naming that client unless this is true.
	ConfirmSecondClient bool `json:"confirm_second_client"`
}

// maxClientNotesRunes bounds the free-text notes on a client (system_3 #9962).
const maxClientNotesRunes = 5000

// The name prints as the invoice's recipient and the address under it: a
// name is one line, an address at most ten, each at most 200 characters
// (system_3 #12264).
const (
	maxClientNameRunes        = 200
	maxClientAddressLines     = 10
	maxClientAddressLineRunes = 200
)

// hasDirectionChars reports the Unicode bidirectional controls (the marks,
// embeddings, overrides and isolates), which make printed text read in an
// order other than the one stored. Joiners and other format characters that
// a name may need are left alone.
func hasDirectionChars(s string) bool {
	for _, r := range s {
		switch {
		case r == 0x061C, r == 0x200E, r == 0x200F,
			r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
			return true
		}
	}
	return false
}

// checkClientNameAndAddress applies the name's and the address's bounds,
// writing the 400 itself. before is the stored client on an edit, nil on a
// create: a field the edit did not change is not checked, so a client
// stored before the bounds stays editable in its other fields (system_3
// #12264, review point 4).
func checkClientNameAndAddress(c *gin.Context, req invoicingClientRequest, before *data_models.InvoicingClient) bool {
	if before == nil || req.Name != before.Name {
		switch {
		case !validClientName(req.Name):
			respondError(c, http.StatusBadRequest, "invalid_name", "A client needs a name.")
			return false
		case strings.ContainsAny(req.Name, "\r\n"):
			respondError(c, http.StatusBadRequest, "invalid_name", "The name is one line.")
			return false
		case hasControlChars(req.Name) || hasDirectionChars(req.Name):
			respondError(c, http.StatusBadRequest, "invalid_name", "The name has a tab, a control or a direction character. Remove it.")
			return false
		case utf8.RuneCountInString(req.Name) > maxClientNameRunes:
			respondError(c, http.StatusBadRequest, "invalid_name", fmt.Sprintf("The name is at most %d characters.", maxClientNameRunes))
			return false
		}
	}
	if before == nil || req.Address != before.Address {
		lines := strings.Split(req.Address, "\n")
		if hasControlChars(req.Address) || hasDirectionChars(req.Address) {
			respondError(c, http.StatusBadRequest, "invalid_address", "The address has a tab, a control or a direction character. Remove it.")
			return false
		}
		if len(lines) > maxClientAddressLines {
			respondError(c, http.StatusBadRequest, "invalid_address", fmt.Sprintf("The address is at most %d lines.", maxClientAddressLines))
			return false
		}
		for _, l := range lines {
			if utf8.RuneCountInString(l) > maxClientAddressLineRunes {
				respondError(c, http.StatusBadRequest, "invalid_address", fmt.Sprintf("Each line of the address is at most %d characters.", maxClientAddressLineRunes))
				return false
			}
		}
	}
	return true
}

// otherClientNamed is Decision 4's "the same" client of that name in the
// owner's book, other than the client except, or nil.
func otherClientNamed(tx *gorm.DB, owner uint, name string, except uint) (*data_models.InvoicingClient, error) {
	clients, err := data_models.ListInvoicingClients(tx, owner)
	if err != nil {
		return nil, err
	}
	for i := range clients {
		if clients[i].ID != except && sameClientName(clients[i].Name, name) {
			return &clients[i], nil
		}
	}
	return nil, nil
}

// bindInvoicingClient parses and validates a client body; it writes the 400
// itself and returns ok=false on any problem. The name is required. The
// address is optional: an invoice up to 250 EUR gross needs none (§33 UStDV),
// and issuing enforces it above that (system_3 #9835).
func bindInvoicingClient(c *gin.Context) (invoicingClientRequest, bool) {
	var req invoicingClientRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid request body")
		return req, false
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Address = strings.TrimSpace(req.Address)
	req.Email = strings.TrimSpace(req.Email)
	req.PayPalEmail = strings.ToLower(strings.TrimSpace(req.PayPalEmail))
	req.Notes = strings.TrimSpace(req.Notes)
	// A NUL cannot be stored: say so, never a 500 (system_3 #12264).
	if strings.ContainsRune(req.Notes, 0) {
		respondError(c, http.StatusBadRequest, "invalid_notes", "The notes have a character that cannot be stored. Remove it.")
		return req, false
	}
	if utf8.RuneCountInString(req.Notes) > maxClientNotesRunes {
		RespondWithError(c, http.StatusBadRequest, fmt.Sprintf("notes must be at most %d characters", maxClientNotesRunes))
		return req, false
	}
	if req.Name == "" {
		RespondWithError(c, http.StatusBadRequest, "name is required")
		return req, false
	}
	if !validOptionalEmail(req.Email) || !validOptionalEmail(req.PayPalEmail) {
		RespondWithError(c, http.StatusBadRequest, "email and paypal_email must be valid addresses when given")
		return req, false
	}
	// The client's phone takes the issuer phone's rule (system_3 #9971).
	req.Phone = strings.TrimSpace(req.Phone)
	if !invoicePhonePattern.MatchString(req.Phone) {
		respondError(c, http.StatusBadRequest, "invalid_phone", invalidPhoneMessage)
		return req, false
	}
	if !bindSocialLinks(c, &req.InstagramURL, &req.TelegramURL) {
		return req, false
	}
	if req.IBAN != nil {
		iban := data_models.NormaliseIBAN(*req.IBAN)
		// The message never repeats the value (security C5, #11551).
		if iban != "" && !data_models.ValidIBAN(iban) {
			respondError(c, http.StatusBadRequest, "invalid_iban", "iban must be a valid IBAN when given")
			return req, false
		}
		req.IBAN = &iban
	}
	return req, true
}

func ListInvoicingClientsHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	clients, err := data_models.ListInvoicingClients(db, invoicingUserID(c))
	if err != nil {
		log.Printf("Error listing invoicing clients: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error listing clients")
		return
	}
	c.JSON(http.StatusOK, gin.H{"clients": clients})
}

func CreateInvoicingClientHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	req, ok := bindInvoicingClient(c)
	if !ok {
		return
	}
	if !checkClientNameAndAddress(c, req, nil) {
		return
	}
	userID := invoicingUserID(c)
	cl := data_models.InvoicingClient{
		OwnerUserID:  userID,
		Name:         req.Name,
		Address:      req.Address,
		Email:        req.Email,
		PayPalEmail:  req.PayPalEmail,
		Notes:        req.Notes,
		Phone:        req.Phone,
		InstagramURL: req.InstagramURL,
		TelegramURL:  req.TelegramURL,
	}
	if req.IBAN != nil {
		cl.IBAN = *req.IBAN
	}

	tx := db.Begin()
	defer tx.RollbackUnlessCommitted()
	if !req.ConfirmSecondClient {
		other, err := otherClientNamed(tx, userID, req.Name, 0)
		if err != nil {
			log.Printf("Error reading the client book: %v", err)
			RespondWithError(c, http.StatusInternalServerError, "Error creating client")
			return
		}
		if other != nil {
			respondClientExists(c, other)
			return
		}
	}
	if err := tx.Create(&cl).Error; err != nil {
		tx.Rollback()
		log.Printf("Error creating invoicing client: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error creating client")
		return
	}
	if err := data_models.RecordInvoicingAuditEvent(tx, userID, invoicingActionCreate, invoicingEntityClient, cl.ID, auditDetails(gin.H{"after": clientAudit(cl), "iban_changed": cl.IBAN != ""})); err != nil {
		tx.Rollback()
		log.Printf("Error auditing invoicing client create: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error creating client")
		return
	}
	if err := tx.Commit().Error; err != nil {
		log.Printf("Error committing invoicing client: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error creating client")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"client": cl})
}

func UpdateInvoicingClientHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		RespondWithError(c, http.StatusBadRequest, "Invalid client id")
		return
	}
	req, ok := bindInvoicingClient(c)
	if !ok {
		return
	}
	userID := invoicingUserID(c)

	tx := db.Begin()
	defer tx.RollbackUnlessCommitted()
	existing, err := data_models.GetInvoicingClient(tx, userID, uint(id))
	if err != nil {
		tx.Rollback()
		log.Printf("Error reading invoicing client %d: %v", id, err)
		RespondWithError(c, http.StatusInternalServerError, "Error updating client")
		return
	}
	if existing == nil {
		tx.Rollback()
		RespondWithError(c, http.StatusNotFound, "Client not found")
		return
	}
	if !checkClientNameAndAddress(c, req, existing) {
		return
	}
	// A renamed client is asked about as a new one is; a kept name is not
	// checked, so a stored pair of namesakes stays editable.
	if req.Name != existing.Name && !req.ConfirmSecondClient {
		other, err := otherClientNamed(tx, userID, req.Name, existing.ID)
		if err != nil {
			log.Printf("Error reading the client book: %v", err)
			RespondWithError(c, http.StatusInternalServerError, "Error updating client")
			return
		}
		if other != nil {
			respondClientExists(c, other)
			return
		}
	}
	before := *existing
	existing.Name = req.Name
	existing.Address = req.Address
	existing.Email = req.Email
	existing.PayPalEmail = req.PayPalEmail
	existing.Notes = req.Notes
	existing.Phone = req.Phone
	existing.InstagramURL = req.InstagramURL
	existing.TelegramURL = req.TelegramURL
	if req.IBAN != nil {
		existing.IBAN = *req.IBAN
	}
	if err := tx.Save(existing).Error; err != nil {
		tx.Rollback()
		log.Printf("Error updating invoicing client %d: %v", id, err)
		RespondWithError(c, http.StatusInternalServerError, "Error updating client")
		return
	}
	if err := data_models.RecordInvoicingAuditEvent(tx, userID, invoicingActionUpdate, invoicingEntityClient, existing.ID, auditDetails(clientAuditChange(before, *existing))); err != nil {
		tx.Rollback()
		log.Printf("Error auditing invoicing client update: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error updating client")
		return
	}
	if err := tx.Commit().Error; err != nil {
		log.Printf("Error committing invoicing client update: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error updating client")
		return
	}
	c.JSON(http.StatusOK, gin.H{"client": existing})
}

// ListInvoicingAuditEventsHandler serves the audit trail, newest first,
// optionally narrowed with ?entity_type= and ?entity_id=.
func ListInvoicingAuditEventsHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	var entityID uint
	if raw := c.Query("entity_id"); raw != "" {
		v, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			RespondWithError(c, http.StatusBadRequest, "Invalid entity_id")
			return
		}
		entityID = uint(v)
	}
	events, err := data_models.ListInvoicingAuditEvents(db, invoicingUserID(c), c.Query("entity_type"), entityID)
	if err != nil {
		log.Printf("Error listing invoicing audit events: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error listing audit events")
		return
	}
	c.JSON(http.StatusOK, gin.H{"events": events})
}
