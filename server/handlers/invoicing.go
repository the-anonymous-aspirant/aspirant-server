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

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Invoicing app (system_3 #8367): issuer profile, clients and the audit trail.
// Every route here sits behind the flowah+Admin group gate in routes.go; these
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
	IsKleinunternehmer  *bool  `json:"is_kleinunternehmer"`
	InvoiceNumberPrefix string `json:"invoice_number_prefix"`
	NextInvoiceNumber   *int   `json:"next_invoice_number"`
}

// GetInvoicingProfileHandler returns the issuer profile, or the defaults when
// none has been saved yet (saved=false tells the UI it is unseeded).
func GetInvoicingProfileHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	p, err := data_models.GetInvoicingProfile(db)
	if err != nil {
		log.Printf("Error retrieving invoicing profile: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error retrieving profile")
		return
	}
	if p == nil {
		d := data_models.DefaultInvoicingProfile()
		c.JSON(http.StatusOK, gin.H{"profile": d, "saved": false})
		return
	}
	c.JSON(http.StatusOK, gin.H{"profile": p, "saved": true})
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
	req.InvoiceNumberPrefix = strings.TrimSpace(req.InvoiceNumberPrefix)

	if req.FullName == "" || req.Address == "" {
		RespondWithError(c, http.StatusBadRequest, "full_name and address are required")
		return
	}
	if !validOptionalEmail(req.Email) {
		RespondWithError(c, http.StatusBadRequest, "email is not a valid address")
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
		IsKleinunternehmer:  *req.IsKleinunternehmer,
		InvoiceNumberPrefix: req.InvoiceNumberPrefix,
		NextInvoiceNumber:   *req.NextInvoiceNumber,
	}

	tx := db.Begin()
	existing, err := data_models.GetInvoicingProfile(tx)
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
	issuedUnderPrefix, err := data_models.CountInvoicesWithPrefix(tx, p.InvoiceNumberPrefix)
	if err != nil {
		tx.Rollback()
		log.Printf("Error counting invoices for prefix %q: %v", p.InvoiceNumberPrefix, err)
		RespondWithError(c, http.StatusInternalServerError, "Error saving profile")
		return
	}
	if issuedUnderPrefix > 0 {
		maxNumber, err := data_models.MaxInvoiceNumberWithPrefix(tx, p.InvoiceNumberPrefix)
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
	Name        string `json:"name"`
	Address     string `json:"address"`
	Email       string `json:"email"`
	PayPalEmail string `json:"paypal_email"`
}

// bindInvoicingClient parses and validates a client body; it writes the 400
// itself and returns ok=false on any problem. Name and address are required
// because an invoice needs both (spec §4.2).
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
	if req.Name == "" || req.Address == "" {
		RespondWithError(c, http.StatusBadRequest, "name and address are required")
		return req, false
	}
	if !validOptionalEmail(req.Email) || !validOptionalEmail(req.PayPalEmail) {
		RespondWithError(c, http.StatusBadRequest, "email and paypal_email must be valid addresses when given")
		return req, false
	}
	return req, true
}

func ListInvoicingClientsHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	clients, err := data_models.ListInvoicingClients(db)
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
	userID := invoicingUserID(c)
	cl := data_models.InvoicingClient{
		Name:        req.Name,
		Address:     req.Address,
		Email:       req.Email,
		PayPalEmail: req.PayPalEmail,
	}

	tx := db.Begin()
	if err := tx.Create(&cl).Error; err != nil {
		tx.Rollback()
		log.Printf("Error creating invoicing client: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error creating client")
		return
	}
	if err := data_models.RecordInvoicingAuditEvent(tx, userID, invoicingActionCreate, invoicingEntityClient, cl.ID, auditDetails(gin.H{"after": cl})); err != nil {
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
	existing, err := data_models.GetInvoicingClient(tx, uint(id))
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
	before := *existing
	existing.Name = req.Name
	existing.Address = req.Address
	existing.Email = req.Email
	existing.PayPalEmail = req.PayPalEmail
	if err := tx.Save(existing).Error; err != nil {
		tx.Rollback()
		log.Printf("Error updating invoicing client %d: %v", id, err)
		RespondWithError(c, http.StatusInternalServerError, "Error updating client")
		return
	}
	if err := data_models.RecordInvoicingAuditEvent(tx, userID, invoicingActionUpdate, invoicingEntityClient, existing.ID, auditDetails(gin.H{"before": before, "after": existing})); err != nil {
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
	events, err := data_models.ListInvoicingAuditEvents(db, c.Query("entity_type"), entityID)
	if err != nil {
		log.Printf("Error listing invoicing audit events: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error listing audit events")
		return
	}
	c.JSON(http.StatusOK, gin.H{"events": events})
}
