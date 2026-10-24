package data_models

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/jinzhu/gorm"
)

// Cards and card versions (system_3 #8634, subtask #8646). A card is one
// printed card of a type: it picks its face (the type's front template, or an
// override) and overlays its own element values on the resolved chain. Values
// are versioned, append-only, like template specs.

// Card is one card of the deck.
type Card struct {
	gorm.Model
	CardTypeID         uint   `json:"card_type_id" gorm:"not null;index"`
	TemplateID         *uint  `json:"template_id" gorm:"index"`
	Slug               string `json:"slug" gorm:"type:varchar(40);not null;unique_index:idx_cards_slug"`
	Name               string `json:"name" gorm:"type:varchar(80);not null"`
	GoalCardID         *uint  `json:"goal_card_id"`
	RelationshipTypeID *uint  `json:"relationship_type_id"`
	CurrentVersionID   *uint  `json:"current_version_id"`
	DisplayOrder       int    `json:"display_order" gorm:"not null;index"`
}

func (Card) TableName() string { return "cards" }

// CardVersion is one saved set of values of a card.
type CardVersion struct {
	gorm.Model
	CardID          uint            `json:"card_id" gorm:"not null;unique_index:idx_card_versions_card_no"`
	VersionNo       int             `json:"version_no" gorm:"not null;unique_index:idx_card_versions_card_no"`
	Data            json.RawMessage `json:"data" gorm:"type:jsonb;not null"`
	Note            string          `json:"note" gorm:"type:varchar(200)"`
	CreatedByUserID uint            `json:"created_by_user_id" gorm:"not null"`
}

func (CardVersion) TableName() string { return "card_versions" }

// ErrCardVersionNotFound is returned for a version number a card never had.
var ErrCardVersionNotFound = fmt.Errorf("card version not found")

// ErrCardHasNoFront is returned when neither the card nor its type names a
// front template.
var ErrCardHasNoFront = fmt.Errorf("card has no front template")

// AddCardVersion appends the next version (data already validated) and moves
// the card's current pointer, in one transaction.
func AddCardVersion(db *gorm.DB, card *Card, data json.RawMessage, note string, userID uint) (*CardVersion, error) {
	tx := db.Begin()
	if tx.Error != nil {
		return nil, tx.Error
	}
	var last struct{ N int }
	if err := tx.Table("card_versions").Select("COALESCE(MAX(version_no), 0) AS n").Where("card_id = ?", card.ID).Scan(&last).Error; err != nil {
		tx.Rollback()
		return nil, err
	}
	v := CardVersion{CardID: card.ID, VersionNo: last.N + 1, Data: data, Note: note, CreatedByUserID: userID}
	if err := tx.Create(&v).Error; err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := tx.Model(card).Update("current_version_id", v.ID).Error; err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := tx.Commit().Error; err != nil {
		return nil, err
	}
	card.CurrentVersionID = &v.ID
	return &v, nil
}

// RestoreCardVersion appends a copy of version n as the newest version.
func RestoreCardVersion(db *gorm.DB, card *Card, n int, userID uint) (*CardVersion, error) {
	var old CardVersion
	err := db.Where("card_id = ? AND version_no = ?", card.ID, n).First(&old).Error
	if err == gorm.ErrRecordNotFound {
		return nil, ErrCardVersionNotFound
	}
	if err != nil {
		return nil, err
	}
	return AddCardVersion(db, card, old.Data, fmt.Sprintf("restored from v%d", n), userID)
}

// CurrentCardVersion loads the version the card points at.
func CurrentCardVersion(db *gorm.DB, card *Card) (*CardVersion, error) {
	if card.CurrentVersionID == nil {
		return nil, fmt.Errorf("card %d has no current version", card.ID)
	}
	var v CardVersion
	if err := db.Where("id = ?", *card.CurrentVersionID).First(&v).Error; err != nil {
		return nil, err
	}
	return &v, nil
}

// CardVersions lists a card's versions oldest first.
func CardVersions(db *gorm.DB, cardID uint) ([]CardVersion, error) {
	var out []CardVersion
	err := db.Where("card_id = ?", cardID).Order("version_no ASC").Find(&out).Error
	return out, err
}

// ResolvedCard is a card's two faces after the chain merge and the value
// overlay. Back is nil when the type has no back template.
type ResolvedCard struct {
	Front *ResolvedTemplate `json:"front"`
	Back  *ResolvedTemplate `json:"back"`
}

// ResolveCard resolves the card's front chain (its override or the type's
// front), overlays the current values with provenance "card", and resolves the
// type's back without the overlay.
func ResolveCard(db *gorm.DB, card *Card) (*ResolvedCard, error) {
	var ct CardType
	if err := db.Where("id = ?", card.CardTypeID).First(&ct).Error; err != nil {
		return nil, fmt.Errorf("card type %d: %w", card.CardTypeID, err)
	}
	frontID := card.TemplateID
	if frontID == nil {
		frontID = ct.FrontTemplateID
	}
	if frontID == nil {
		return nil, ErrCardHasNoFront
	}
	front, err := ResolveTemplate(db, *frontID)
	if err != nil {
		return nil, err
	}
	v, err := CurrentCardVersion(db, card)
	if err != nil {
		return nil, err
	}
	data, err := ParseCardData(v.Data, nil)
	if err != nil {
		return nil, err
	}
	elements := map[string]*ResolvedElement{}
	for i := range front.Elements {
		el := front.Elements[i]
		elements[el.Name] = &ResolvedElement{Name: el.Name, Props: el.Props, Provenance: el.Provenance}
	}
	MergeSpecLayer(front, elements, "card", &TemplateSpec{Elements: data.Values})
	front.Elements = sortedElements(elements)
	front.Chain = append(front.Chain, "card")

	out := &ResolvedCard{Front: front}
	if ct.BackTemplateID != nil {
		back, err := ResolveTemplate(db, *ct.BackTemplateID)
		if err != nil {
			return nil, err
		}
		out.Back = back
	}
	return out, nil
}

// ---- bootstrap ---------------------------------------------------------------

// connectionSlug maps a relationship-type code to a slug segment ("F+" is not
// a slug character).
func connectionSlug(code string) string {
	s := strings.ToLower(code)
	s = strings.ReplaceAll(s, "+", "plus")
	return "conn_" + s
}

// BootstrapCardgenCards creates the 16 relationship cards from goal_cards and
// the 6 connection cards from relationship_types when they do not exist,
// keyed by slug, linked by goal_card_id / relationship_type_id. It never
// updates an existing card: the editor owns them from then on, and the game's
// two tables stay read-only to the editor (their seeds reset them on boot).
func BootstrapCardgenCards(db *gorm.DB) {
	var relType, connType CardType
	if err := db.Where("slug = ?", "relationship").First(&relType).Error; err != nil {
		log.Printf("cardgen bootstrap: relationship type: %v", err)
		return
	}
	if err := db.Where("slug = ?", "connection").First(&connType).Error; err != nil {
		log.Printf("cardgen bootstrap: connection type: %v", err)
		return
	}
	var goals []GoalCard
	if err := db.Order("display_order ASC").Find(&goals).Error; err != nil {
		log.Printf("cardgen bootstrap: goal cards: %v", err)
	}
	for _, g := range goals {
		gid := g.ID
		data := mustCardDataJSON(map[string]map[string]any{
			"title": {"text": g.Name},
			"body":  {"text": g.VictoryCondition},
		})
		bootstrapCard(db, Card{CardTypeID: relType.ID, Slug: "rel_" + g.Code, Name: g.Name, GoalCardID: &gid, DisplayOrder: g.DisplayOrder}, data)
	}
	var rels []RelationshipType
	if err := db.Order("display_order ASC").Find(&rels).Error; err != nil {
		log.Printf("cardgen bootstrap: relationship types: %v", err)
	}
	for _, r := range rels {
		rid := r.ID
		data := mustCardDataJSON(map[string]map[string]any{
			"title": {"text": r.Label},
			"body":  {"text": r.Code},
		})
		bootstrapCard(db, Card{CardTypeID: connType.ID, Slug: connectionSlug(r.Code), Name: r.Label, RelationshipTypeID: &rid, DisplayOrder: r.DisplayOrder}, data)
	}
}

func mustCardDataJSON(values map[string]map[string]any) json.RawMessage {
	raw, err := json.Marshal(CardData{Values: values})
	if err != nil {
		panic(err)
	}
	if _, err := ParseCardData(raw, nil); err != nil {
		panic(err)
	}
	return raw
}

func bootstrapCard(db *gorm.DB, card Card, data json.RawMessage) {
	var n int
	if err := db.Unscoped().Model(&Card{}).Where("slug = ?", card.Slug).Count(&n).Error; err != nil {
		log.Printf("cardgen bootstrap: %s: %v", card.Slug, err)
		return
	}
	if n > 0 {
		return
	}
	if err := db.Create(&card).Error; err != nil {
		log.Printf("cardgen bootstrap: %s: %v", card.Slug, err)
		return
	}
	if _, err := AddCardVersion(db, &card, data, "bootstrap", 0); err != nil {
		log.Printf("cardgen bootstrap: %s v1: %v", card.Slug, err)
	}
}
