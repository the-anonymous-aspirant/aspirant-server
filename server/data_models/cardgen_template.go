package data_models

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/jinzhu/gorm"
)

// Card types and templates (system_3 #8634, subtask #8643). The YAML sketch's
// `extends` is ParentTemplateID; a template's "defaults" are the element props
// of its current version; a card (subtask #8646) overlays its own values on the
// resolved chain. Versions are append-only, so "go back" is always a new
// version that copies an old one, never a rewrite.

// CardType is one of the deck's card kinds (relationship, action, life,
// connection). Its front and back templates are what the review page and the
// export draw for a card of this type unless the card overrides the front.
type CardType struct {
	gorm.Model
	Slug            string `json:"slug" gorm:"type:varchar(40);not null;unique_index:idx_card_types_slug"`
	Name            string `json:"name" gorm:"type:varchar(60);not null"`
	Description     string `json:"description" gorm:"type:text"`
	FrontTemplateID *uint  `json:"front_template_id"`
	BackTemplateID  *uint  `json:"back_template_id"`
	DisplayOrder    int    `json:"display_order" gorm:"not null;index"`
}

func (CardType) TableName() string { return "card_types" }

// CardTemplate is a named, versioned layout. ParentTemplateID is `extends`.
type CardTemplate struct {
	gorm.Model
	Slug             string `json:"slug" gorm:"type:varchar(40);not null;unique_index:idx_card_templates_slug"`
	Name             string `json:"name" gorm:"type:varchar(60);not null"`
	ParentTemplateID *uint  `json:"parent_template_id" gorm:"index"`
	CurrentVersionID *uint  `json:"current_version_id"`
}

func (CardTemplate) TableName() string { return "card_templates" }

// CardTemplateVersion is one saved spec of a template. VersionNo counts from 1
// per template and never repeats; a restore inserts a new number.
type CardTemplateVersion struct {
	gorm.Model
	TemplateID      uint            `json:"template_id" gorm:"not null;unique_index:idx_card_template_versions_template_no"`
	VersionNo       int             `json:"version_no" gorm:"not null;unique_index:idx_card_template_versions_template_no"`
	Spec            json.RawMessage `json:"spec" gorm:"type:jsonb;not null"`
	Note            string          `json:"note" gorm:"type:varchar(200)"`
	CreatedByUserID uint            `json:"created_by_user_id" gorm:"not null"`
}

func (CardTemplateVersion) TableName() string { return "card_template_versions" }

// ErrTemplateVersionNotFound is returned for a version number a template never had.
var ErrTemplateVersionNotFound = fmt.Errorf("template version not found")

// AddTemplateVersion appends the next version with spec (already validated by
// the caller) and moves the template's current pointer to it, in one
// transaction.
func AddTemplateVersion(db *gorm.DB, tmpl *CardTemplate, spec json.RawMessage, note string, userID uint) (*CardTemplateVersion, error) {
	tx := db.Begin()
	if tx.Error != nil {
		return nil, tx.Error
	}
	var last struct{ N int }
	if err := tx.Table("card_template_versions").Select("COALESCE(MAX(version_no), 0) AS n").Where("template_id = ?", tmpl.ID).Scan(&last).Error; err != nil {
		tx.Rollback()
		return nil, err
	}
	v := CardTemplateVersion{TemplateID: tmpl.ID, VersionNo: last.N + 1, Spec: spec, Note: note, CreatedByUserID: userID}
	if err := tx.Create(&v).Error; err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := tx.Model(tmpl).Update("current_version_id", v.ID).Error; err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := tx.Commit().Error; err != nil {
		return nil, err
	}
	tmpl.CurrentVersionID = &v.ID
	return &v, nil
}

// RestoreTemplateVersion appends a copy of version n as the newest version.
func RestoreTemplateVersion(db *gorm.DB, tmpl *CardTemplate, n int, userID uint) (*CardTemplateVersion, error) {
	var old CardTemplateVersion
	err := db.Where("template_id = ? AND version_no = ?", tmpl.ID, n).First(&old).Error
	if err == gorm.ErrRecordNotFound {
		return nil, ErrTemplateVersionNotFound
	}
	if err != nil {
		return nil, err
	}
	return AddTemplateVersion(db, tmpl, old.Spec, fmt.Sprintf("restored from v%d", n), userID)
}

// CurrentTemplateVersion loads the version the template points at. A template
// always has one after creation; a nil pointer is a data error.
func CurrentTemplateVersion(db *gorm.DB, tmpl *CardTemplate) (*CardTemplateVersion, error) {
	if tmpl.CurrentVersionID == nil {
		return nil, fmt.Errorf("template %d has no current version", tmpl.ID)
	}
	var v CardTemplateVersion
	if err := db.Where("id = ?", *tmpl.CurrentVersionID).First(&v).Error; err != nil {
		return nil, err
	}
	return &v, nil
}

// TemplateVersions lists a template's versions oldest first.
func TemplateVersions(db *gorm.DB, templateID uint) ([]CardTemplateVersion, error) {
	var out []CardTemplateVersion
	err := db.Where("template_id = ?", templateID).Order("version_no ASC").Find(&out).Error
	return out, err
}

// TemplateIsReferenced reports whether a type uses the template as a face,
// another live template extends it, or a live card overrides its front with
// it; such a template cannot be deleted.
func TemplateIsReferenced(db *gorm.DB, id uint) (bool, error) {
	var n int
	if err := db.Model(&CardType{}).Where("front_template_id = ? OR back_template_id = ?", id, id).Count(&n).Error; err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}
	if err := db.Model(&CardTemplate{}).Where("parent_template_id = ?", id).Count(&n).Error; err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}
	// A card that overrides its type's front with this template (#8646). The
	// cards table may not exist on a database that has not migrated Step 20
	// yet; HasTable keeps this read safe either way.
	if db.HasTable("cards") {
		if err := db.Table("cards").Where("template_id = ? AND deleted_at IS NULL", id).Count(&n).Error; err != nil {
			return false, err
		}
	}
	return n > 0, nil
}

// ---- seeds -----------------------------------------------------------------

// Seed specs, transcribed from the operator's materials (#8634 c53338 "What
// the materials say the cards are"): 105 x 74.25 mm landscape (A4 halved and
// quartered), 5 mm radius; a display title, an uppercase body, a small italic
// quote line, an optional silent-card icon top left, and full-bleed art on the
// relationship and connection faces and on every back.
func seedBaseSpec() json.RawMessage {
	return MustSpecJSON(TemplateSpec{
		Size: &CardSize{WMM: 105, HMM: 74.25, RadiusMM: 5},
		Elements: map[string]map[string]any{
			"art":         {"kind": "image", "x_mm": 0.0, "y_mm": 0.0, "w_mm": 105.0, "h_mm": 74.25, "fit": "cover", "z": 0.0, "hidden": true},
			"title":       {"kind": "text", "x_mm": 6.0, "y_mm": 6.0, "w_mm": 93.0, "h_mm": 12.0, "size_pt": 16.0, "align": "center", "valign": "middle", "weight": "bold", "transform": "uppercase", "color": "#111111", "z": 10.0, "text": ""},
			"body":        {"kind": "text", "x_mm": 8.0, "y_mm": 22.0, "w_mm": 89.0, "h_mm": 34.0, "size_pt": 8.5, "line_height": 1.35, "align": "center", "valign": "top", "transform": "uppercase", "color": "#111111", "z": 10.0, "text": ""},
			"quote":       {"kind": "text", "x_mm": 8.0, "y_mm": 58.0, "w_mm": 89.0, "h_mm": 10.0, "size_pt": 6.5, "align": "center", "valign": "middle", "italic": true, "color": "#333333", "z": 10.0, "text": ""},
			"silent_icon": {"kind": "image", "x_mm": 3.0, "y_mm": 3.0, "w_mm": 6.0, "h_mm": 6.0, "fit": "contain", "z": 20.0, "hidden": true},
		},
	})
}

func seedArtFaceSpec() json.RawMessage {
	return MustSpecJSON(TemplateSpec{Elements: map[string]map[string]any{
		"art":   {"hidden": false},
		"title": {"color": "#ffffff", "size_pt": 18.0},
		"body":  {"color": "#ffffff"},
		"quote": {"color": "#f3f3f3"},
	}})
}

func seedBackSpec() json.RawMessage {
	return MustSpecJSON(TemplateSpec{Elements: map[string]map[string]any{
		"art":         {"hidden": false},
		"title":       {"hidden": true},
		"body":        {"hidden": true},
		"quote":       {"hidden": true},
		"silent_icon": {"hidden": true},
	}})
}

func seedPlainFaceSpec() json.RawMessage {
	return MustSpecJSON(TemplateSpec{Elements: map[string]map[string]any{}})
}

type seedType struct {
	slug, name, description string
	front                   func() json.RawMessage
}

var cardTypeSeed = []seedType{
	{"relationship", "Relationship", "The 16 goal cards: a relationship style and the condition that wins it.", seedArtFaceSpec},
	{"action", "Action", "Played on your turn to create, change or break connections.", seedPlainFaceSpec},
	{"life", "Life", "Drawn on a 1 or a 6: chaos that happens to you.", seedPlainFaceSpec},
	{"connection", "Connection", "One of the six connection types, offered to another player.", seedArtFaceSpec},
}

// SeedCardgenTemplates creates the base template, one front and one back
// template per type, and the four types, when they do not exist. Unlike
// SeedGoalCards it never updates an existing row: these are the operator's
// editable starting points, and a re-run must not undo edits.
func SeedCardgenTemplates(db *gorm.DB) {
	base, err := seedTemplate(db, "base", "Base card", nil, seedBaseSpec())
	if err != nil {
		log.Printf("cardgen seed: base template: %v", err)
		return
	}
	for i, st := range cardTypeSeed {
		front, err := seedTemplate(db, st.slug, st.name+" front", &base.ID, st.front())
		if err != nil {
			log.Printf("cardgen seed: %s front: %v", st.slug, err)
			continue
		}
		back, err := seedTemplate(db, st.slug+"_back", st.name+" back", &base.ID, seedBackSpec())
		if err != nil {
			log.Printf("cardgen seed: %s back: %v", st.slug, err)
			continue
		}
		var existing CardType
		err = db.Where("slug = ?", st.slug).First(&existing).Error
		if err == gorm.ErrRecordNotFound {
			ct := CardType{Slug: st.slug, Name: st.name, Description: st.description, FrontTemplateID: &front.ID, BackTemplateID: &back.ID, DisplayOrder: i + 1}
			if err := db.Create(&ct).Error; err != nil {
				log.Printf("cardgen seed: type %s: %v", st.slug, err)
			}
		} else if err != nil {
			log.Printf("cardgen seed: type %s: %v", st.slug, err)
		}
	}
}

// seedTemplate returns the live template with slug, creating it with v1 = spec
// when absent. System seeds carry CreatedByUserID 0.
func seedTemplate(db *gorm.DB, slug, name string, parent *uint, spec json.RawMessage) (*CardTemplate, error) {
	var t CardTemplate
	err := db.Where("slug = ?", slug).First(&t).Error
	if err == nil {
		return &t, nil
	}
	if err != gorm.ErrRecordNotFound {
		return nil, err
	}
	t = CardTemplate{Slug: slug, Name: name, ParentTemplateID: parent}
	if err := db.Create(&t).Error; err != nil {
		return nil, err
	}
	if _, err := AddTemplateVersion(db, &t, spec, "seed", 0); err != nil {
		return nil, err
	}
	return &t, nil
}
