package data_models

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // BerlinLocation must resolve even without a system zoneinfo

	"github.com/jinzhu/gorm"
)

// The invoicing app (system_3 #8367) issues retroactive German invoices for a
// single freelance user. Its tables are prefixed invoicing_ and share nothing
// with the retired finance service (operator decision 1, c51421): this is a
// third party's personal and financial data, kept apart by design.
//
// Each signed-in user keeps their own book (system_3 #10054, operator c62279:
// "a separate book per user"): every root row carries OwnerUserID, the user
// who made it, and every read is scoped to the signed-in user, so an Admin
// testing the app never shows up in the owner's book. Rows from before the
// column existed have owner 0, which no login has. The route gate in
// routes.go still decides who may open the app at all.

// InvoicingProfile is the issuer header printed on every invoice. One row per
// owner.
// NextInvoiceNumber is the counter issuing draws from; it stays editable only
// until the first invoice under the current prefix is issued (enforced by the
// issue path), so a sequence started elsewhere can be continued.
type InvoicingProfile struct {
	ID           uint   `json:"-" gorm:"primary_key"`
	OwnerUserID  uint   `json:"-" gorm:"column:owner_user_id;not null;default:0;unique_index:idx_invoicing_profiles_owner"`
	FullName     string `json:"full_name" gorm:"type:text;not null;default:''"`
	Address      string `json:"address" gorm:"type:text;not null;default:''"`
	Steuernummer string `json:"steuernummer" gorm:"type:text;not null;default:''"`
	Email        string `json:"email" gorm:"type:text;not null;default:''"`
	// Phone is optional; printed on the invoice under the email only when set
	// (operator c61727, system_3 #9967).
	Phone string `json:"phone" gorm:"type:text;not null;default:''"`
	// InstagramURL and TelegramURL are the issuer's own profile links, stored
	// canonical (https://www.instagram.com/<name>/, https://t.me/<name>) or
	// empty (system_3 #9971). Not printed: the PDF prints named fields only.
	InstagramURL        string    `json:"instagram_url" gorm:"column:instagram_url;type:text;not null;default:''"`
	TelegramURL         string    `json:"telegram_url" gorm:"column:telegram_url;type:text;not null;default:''"`
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
		IsKleinunternehmer:  true,
		InvoiceNumberPrefix: time.Now().In(BerlinLocation()).Format("2006"),
		NextInvoiceNumber:   1,
	}
}

// GetInvoicingProfile returns the owner's saved profile, or nil when they have
// none yet.
func GetInvoicingProfile(db *gorm.DB, owner uint) (*InvoicingProfile, error) {
	var p InvoicingProfile
	err := db.Where("owner_user_id = ?", owner).First(&p).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// SaveInvoicingProfile upserts the owner's profile row. The signed-in user is
// both the owner and the editor.
func SaveInvoicingProfile(db *gorm.DB, p *InvoicingProfile, userID uint) error {
	existing, err := GetInvoicingProfile(db, userID)
	if err != nil {
		return err
	}
	p.ID = 0
	if existing != nil {
		p.ID = existing.ID
	}
	p.OwnerUserID = userID
	p.UpdatedAt = time.Now().UTC()
	p.UpdatedBy = userID
	return db.Save(p).Error
}

// InvoicingClient is someone Flower invoices. PayPalEmail matches imported
// payments to the client.
type InvoicingClient struct {
	ID          uint   `json:"id" gorm:"primary_key"`
	OwnerUserID uint   `json:"-" gorm:"column:owner_user_id;not null;default:0;index"`
	Name        string `json:"name" gorm:"type:text;not null"`
	Address     string `json:"address" gorm:"type:text;not null;default:''"`
	Email       string `json:"email" gorm:"type:text;not null;default:''"`
	PayPalEmail string `json:"paypal_email" gorm:"column:paypal_email;type:text;not null;default:''"`
	// Notes are Flower's own notes on the client (system_3 #9962). They are
	// zirs alone: never on an invoice, its PDF, its frozen client snapshot or
	// an export (see ForInvoice).
	Notes string `json:"notes" gorm:"type:text;not null;default:''"`
	// Phone and the profile links are how Flower reaches the client (system_3
	// #9971). Like the notes they are zirs, not the invoice's: ForInvoice
	// leaves them out. Links are stored canonical or empty.
	Phone        string `json:"phone" gorm:"type:text;not null;default:''"`
	InstagramURL string `json:"instagram_url" gorm:"column:instagram_url;type:text;not null;default:''"`
	TelegramURL  string `json:"telegram_url" gorm:"column:telegram_url;type:text;not null;default:''"`
	// IBAN is the client's bank account, kept so later N26 payments from it
	// link to the client (system_3 #11551; security c75108). Stored in
	// NormaliseIBAN's form or empty. It is Flower's own record, not the
	// invoice's: ForInvoice leaves it out, client audit rows record only
	// whether it changed, and no export carries it.
	IBAN      string    `json:"iban" gorm:"column:iban;type:text;not null;default:''"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (InvoicingClient) TableName() string { return "invoicing_clients" }

// ForInvoice is the client as an invoice may carry it. The issue-time
// ClientSnapshot and the PDF are built from this, so nothing outside the list
// below reaches a stored invoice or anything rendered from one. It is a list
// of the fields an invoice keeps, not a list of the ones it drops, so a new
// client field stays off invoices until someone adds it here (security C1,
// system_3 #11551). Left out on purpose: zir private notes (#9962), zir ways
// of reaching the client (phone, Instagram, Telegram; #9971) and the IBAN.
func (cl InvoicingClient) ForInvoice() InvoicingClient {
	return InvoicingClient{
		ID:          cl.ID,
		OwnerUserID: cl.OwnerUserID,
		Name:        cl.Name,
		Address:     cl.Address,
		Email:       cl.Email,
		PayPalEmail: cl.PayPalEmail,
		CreatedAt:   cl.CreatedAt,
		UpdatedAt:   cl.UpdatedAt,
	}
}

// KleinbetragLimitCents is the §33 UStDV ceiling: an invoice up to 250.00 EUR
// gross is a Kleinbetragsrechnung, which need not name the recipient's
// address (system_3 #9835, report 795 F3). §34a UStDV leaves §33 untouched,
// so the ceiling holds for a Kleinunternehmerin too.
const KleinbetragLimitCents = 25000

// HasPostalAddress reports whether the client has an address to print.
func (cl InvoicingClient) HasPostalAddress() bool {
	return strings.TrimSpace(cl.Address) != ""
}

// InvoiceNeedsRecipientAddress reports whether an invoice with this gross
// total must carry the recipient's full address. Above the §33 UStDV ceiling
// it must, under §14 Abs. 4 Nr. 1 UStG or, for a Kleinunternehmerin, §34a
// Nr. 1 UStDV (report 795 F1). The ceiling is in euros, so an invoice in any
// other currency always needs the address: the rule fails closed.
func InvoiceNeedsRecipientAddress(totalCents int64, currency string) bool {
	if currency != "" && currency != "EUR" {
		return true
	}
	return totalCents > KleinbetragLimitCents
}

// ListInvoicingClients is the owner's clients.
func ListInvoicingClients(db *gorm.DB, owner uint) ([]InvoicingClient, error) {
	var clients []InvoicingClient
	err := db.Where("owner_user_id = ?", owner).Order("name asc, id asc").Find(&clients).Error
	return clients, err
}

// GetInvoicingClient returns the owner's client, or nil when the id is unknown
// or belongs to someone else: another book's client does not exist here.
func GetInvoicingClient(db *gorm.DB, owner, id uint) (*InvoicingClient, error) {
	var cl InvoicingClient
	err := db.Where("id = ? AND owner_user_id = ?", id, owner).First(&cl).Error
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
// user and timestamp). Nothing deletes these rows. One write ever changes
// them: thirty days after an account is deleted, the identifier erasure sweep
// empties its rows' Details (EmptyInvoicingAuditDetails).
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

// ErasedAuditDetails is what an erased account's audit rows keep as Details:
// an empty object. The action, entity, entity id, user and time stay, so the
// trail of who did what, and when, outlives the names and addresses the
// details had copied (the operator's answer c74952 on system_3 #11380).
const ErasedAuditDetails = "{}"

// The one audit row about a user that someone else writes: an admin's
// download of the user's failed import, under the admin's id, with details
// {"owner_user_id": <the user>, "filename": ...} (handlers'
// DownloadImportFailureHandler). A new writer whose actor is not the book's
// owner has to be taught to the erase here too.
const (
	ImportFailureAuditEntity   = "import_failure"
	ImportFailureAuditDownload = "download"
)

// EmptyInvoicingAuditDetails sets Details to ErasedAuditDetails on every
// audit row of userID that still holds something, and on every admin's
// download row about userID's failed imports, and returns how many it
// changed. A row already "{}" or "" (some writers record none) is left as it
// is, so a second call changes nothing (system_3 #12081).
func EmptyInvoicingAuditDetails(tx *gorm.DB, userID uint) (int64, error) {
	res := tx.Model(&InvoicingAuditEvent{}).
		Where("user_id = ? AND details NOT IN (?)", userID, []string{"", ErasedAuditDetails}).
		UpdateColumn("details", ErasedAuditDetails)
	if res.Error != nil {
		return 0, res.Error
	}
	n, err := emptyImportFailureDownloadDetails(tx, map[uint]bool{userID: true})
	return res.RowsAffected + n, err
}

// EmptyErasedAccountsAuditDetails is EmptyInvoicingAuditDetails for every
// account the sweep has already erased: those erased before the audit rows
// were part of the erase (system_3 #12081: the boot pass of 2026-11-18 erased
// at least one), or any whose rows were somehow left. Returns how many rows
// it changed, 0 once all are done.
func EmptyErasedAccountsAuditDetails(db *gorm.DB) (int64, error) {
	erased := db.Unscoped().Model(&User{}).Select("id").Where("identifiers_erased_at IS NOT NULL").QueryExpr()
	res := db.Model(&InvoicingAuditEvent{}).
		Where("details NOT IN (?) AND user_id IN (?)", []string{"", ErasedAuditDetails}, erased).
		UpdateColumn("details", ErasedAuditDetails)
	if res.Error != nil {
		return 0, res.Error
	}
	var ids []uint
	if err := db.Unscoped().Model(&User{}).Where("identifiers_erased_at IS NOT NULL").Pluck("id", &ids).Error; err != nil {
		return res.RowsAffected, err
	}
	owners := make(map[uint]bool, len(ids))
	for _, id := range ids {
		owners[id] = true
	}
	n, err := emptyImportFailureDownloadDetails(db, owners)
	return res.RowsAffected + n, err
}

// emptyImportFailureDownloadDetails empties the download rows whose
// owner_user_id is one of owners. Rows are chosen by entity and action, then
// matched in Go: details is text, and a row that is "" or does not parse is
// skipped, never cast, so it cannot fail the erase it is part of (security's
// E1, c75300 on system_3 #12081). The match is the account id, never the file
// name (E2).
func emptyImportFailureDownloadDetails(tx *gorm.DB, owners map[uint]bool) (int64, error) {
	if len(owners) == 0 {
		return 0, nil
	}
	var rows []InvoicingAuditEvent
	if err := tx.Select("id, details").
		Where("entity_type = ? AND action = ? AND details NOT IN (?)",
			ImportFailureAuditEntity, ImportFailureAuditDownload, []string{"", ErasedAuditDetails}).
		Find(&rows).Error; err != nil {
		return 0, err
	}
	var ids []uint
	for _, r := range rows {
		var d struct {
			OwnerUserID json.Number `json:"owner_user_id"`
		}
		dec := json.NewDecoder(strings.NewReader(r.Details))
		dec.UseNumber()
		if dec.Decode(&d) != nil {
			continue
		}
		owner, err := strconv.ParseUint(d.OwnerUserID.String(), 10, 64)
		if err != nil || !owners[uint(owner)] {
			continue
		}
		ids = append(ids, r.ID)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	res := tx.Model(&InvoicingAuditEvent{}).Where("id IN (?)", ids).UpdateColumn("details", ErasedAuditDetails)
	return res.RowsAffected, res.Error
}

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

// ListInvoicingAuditEvents returns the owner's events newest first, optionally
// narrowed to one entity type and (when entityID > 0) one entity. Every write
// records the signed-in user, who owns the book written to, so a book's trail
// is the events its owner recorded (system_3 #10054).
func ListInvoicingAuditEvents(db *gorm.DB, owner uint, entityType string, entityID uint) ([]InvoicingAuditEvent, error) {
	q := db.Where("user_id = ?", owner).Order("created_at desc, id desc")
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
