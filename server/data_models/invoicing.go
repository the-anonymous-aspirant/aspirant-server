package data_models

import (
	"time"
	_ "time/tzdata" // BerlinLocation must resolve even without a system zoneinfo

	"github.com/jinzhu/gorm"
)

// The invoicing app (system_3 #8367) issues retroactive German invoices for a
// single freelance user. Its tables are prefixed invoicing_ and share nothing
// with the retired finance service (operator decision 1, c51421): this is a
// third party's personal and financial data, kept apart by design. Like the
// Pappas logs it carries no owner column — the log is one user's, and that is
// enforced by the route gate in routes.go, not by the schema.

// invoicingProfileID is the fixed primary key of the single issuer-profile row.
const invoicingProfileID = 1

// InvoicingProfile is the issuer header printed on every invoice. One row.
// NextInvoiceNumber is the counter issuing draws from; it stays editable only
// until the first invoice under the current prefix is issued (enforced by the
// issue path), so a sequence started elsewhere can be continued.
type InvoicingProfile struct {
	ID                  uint      `json:"-" gorm:"primary_key"`
	FullName            string    `json:"full_name" gorm:"type:text;not null;default:''"`
	Address             string    `json:"address" gorm:"type:text;not null;default:''"`
	Steuernummer        string    `json:"steuernummer" gorm:"type:text;not null;default:''"`
	Email               string    `json:"email" gorm:"type:text;not null;default:''"`
	IsKleinunternehmer  bool      `json:"is_kleinunternehmer" gorm:"not null;default:true"`
	InvoiceNumberPrefix string    `json:"invoice_number_prefix" gorm:"type:text;not null;default:''"`
	NextInvoiceNumber   int       `json:"next_invoice_number" gorm:"not null;default:1"`
	UpdatedAt           time.Time `json:"updated_at"`
	UpdatedBy           uint      `json:"updated_by"`
}

func (InvoicingProfile) TableName() string { return "invoicing_profiles" }

// DefaultInvoicingProfile is what GET returns before anyone has saved a profile.
func DefaultInvoicingProfile() InvoicingProfile {
	return InvoicingProfile{
		ID:                  invoicingProfileID,
		IsKleinunternehmer:  true,
		InvoiceNumberPrefix: time.Now().In(BerlinLocation()).Format("2006"),
		NextInvoiceNumber:   1,
	}
}

// GetInvoicingProfile returns the saved profile, or nil when none exists yet.
func GetInvoicingProfile(db *gorm.DB) (*InvoicingProfile, error) {
	var p InvoicingProfile
	err := db.Where("id = ?", invoicingProfileID).First(&p).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// SaveInvoicingProfile upserts the single profile row.
func SaveInvoicingProfile(db *gorm.DB, p *InvoicingProfile, userID uint) error {
	p.ID = invoicingProfileID
	p.UpdatedAt = time.Now().UTC()
	p.UpdatedBy = userID
	return db.Save(p).Error
}

// InvoicingClient is someone Flower invoices. PayPalEmail matches imported
// payments to the client.
type InvoicingClient struct {
	ID          uint      `json:"id" gorm:"primary_key"`
	Name        string    `json:"name" gorm:"type:text;not null"`
	Address     string    `json:"address" gorm:"type:text;not null;default:''"`
	Email       string    `json:"email" gorm:"type:text;not null;default:''"`
	PayPalEmail string    `json:"paypal_email" gorm:"column:paypal_email;type:text;not null;default:''"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (InvoicingClient) TableName() string { return "invoicing_clients" }

// IsInvoiceable reports whether the client carries what §14 UStG needs on an
// invoice: a name and a postal address.
func (cl InvoicingClient) IsInvoiceable() bool {
	return cl.Name != "" && cl.Address != ""
}

func ListInvoicingClients(db *gorm.DB) ([]InvoicingClient, error) {
	var clients []InvoicingClient
	err := db.Order("name asc, id asc").Find(&clients).Error
	return clients, err
}

// GetInvoicingClient returns the client, or nil when the id is unknown.
func GetInvoicingClient(db *gorm.DB, id uint) (*InvoicingClient, error) {
	var cl InvoicingClient
	err := db.Where("id = ?", id).First(&cl).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &cl, nil
}

// InvoicingAuditEvent is one append-only record of a create, edit, issue,
// import or export in the invoicing app (spec §3: every action is logged with
// user and timestamp). Nothing updates or deletes these rows.
type InvoicingAuditEvent struct {
	ID         uint      `json:"id" gorm:"primary_key"`
	CreatedAt  time.Time `json:"created_at" gorm:"index"`
	UserID     uint      `json:"user_id"`
	Action     string    `json:"action" gorm:"type:text;not null"`
	EntityType string    `json:"entity_type" gorm:"type:text;not null;index:idx_invoicing_audit_entity"`
	EntityID   uint      `json:"entity_id" gorm:"index:idx_invoicing_audit_entity"`
	Details    string    `json:"details" gorm:"type:text;not null;default:''"`
}

func (InvoicingAuditEvent) TableName() string { return "invoicing_audit_events" }

// RecordInvoicingAuditEvent appends one audit row. Callers pass the same tx
// as the mutation it records, so the two commit or roll back together.
func RecordInvoicingAuditEvent(tx *gorm.DB, userID uint, action, entityType string, entityID uint, details string) error {
	return tx.Create(&InvoicingAuditEvent{
		CreatedAt:  time.Now().UTC(),
		UserID:     userID,
		Action:     action,
		EntityType: entityType,
		EntityID:   entityID,
		Details:    details,
	}).Error
}

// ListInvoicingAuditEvents returns events newest first, optionally narrowed to
// one entity type and (when entityID > 0) one entity.
func ListInvoicingAuditEvents(db *gorm.DB, entityType string, entityID uint) ([]InvoicingAuditEvent, error) {
	q := db.Order("created_at desc, id desc")
	if entityType != "" {
		q = q.Where("entity_type = ?", entityType)
	}
	if entityID > 0 {
		q = q.Where("entity_id = ?", entityID)
	}
	var events []InvoicingAuditEvent
	err := q.Find(&events).Error
	return events, err
}

// BerlinLocation is the invoicing app's calendar (spec §6: timezone
// Europe/Berlin). The time/tzdata import above embeds the zone database, so
// this resolves whether or not the runtime image ships zoneinfo.
func BerlinLocation() *time.Location {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		panic("invoicing: Europe/Berlin zone unavailable: " + err.Error())
	}
	return loc
}
