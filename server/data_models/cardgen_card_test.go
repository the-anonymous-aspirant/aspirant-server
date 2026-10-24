package data_models

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

func cardgenCardDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.AutoMigrate(&CardType{}, &CardTemplate{}, &CardTemplateVersion{}, &Card{}, &CardVersion{}, &GoalCard{}, &RelationshipType{})
	t.Cleanup(func() { db.Close() })
	return db
}

func TestParseCardData(t *testing.T) {
	d, err := ParseCardData([]byte(`{"values": {"title": {"text": "ORION", "color": "#ffffff"}, "art": {"asset_id": 3}}}`), nil)
	if err != nil || d.Values["title"]["text"] != "ORION" {
		t.Fatalf("valid data: %+v %v", d, err)
	}
	for name, raw := range map[string]string{
		"unknown top": `{"overrides": {}}`,
		"bad prop":    `{"values": {"title": {"html": "<b>"}}}`,
		"bad name":    `{"values": {"Title": {}}}`,
		"bad colour":  `{"values": {"title": {"color": "red"}}}`,
	} {
		if _, err := ParseCardData([]byte(raw), nil); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	db := cardgenCardDB(t)
	if _, err := ParseCardData([]byte(`{"values": {"art": {"asset_id": 1}}}`), db); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("asset without table: %v", err)
	}
}

func TestResolveCard_OverlayAndFaces(t *testing.T) {
	db := cardgenCardDB(t)
	SeedCardgenTemplates(db)
	var action, actionFront, actionBack CardType
	db.Where("slug = ?", "action").First(&action)
	_ = actionFront
	_ = actionBack
	card := Card{CardTypeID: action.ID, Slug: "smooth_operator", Name: "Smooth operator"}
	if err := db.Create(&card).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := AddCardVersion(db, &card, json.RawMessage(`{"values": {"title": {"text": "SMOOTH OPERATOR", "color": "#aa0000"}, "badge": {"kind": "rect", "x_mm": 1, "y_mm": 1, "w_mm": 4, "h_mm": 4, "fill": "#00ff00"}}}`), "v1", 1); err != nil {
		t.Fatal(err)
	}
	res, err := ResolveCard(db, &card)
	if err != nil {
		t.Fatal(err)
	}
	if res.Front == nil || res.Back == nil {
		t.Fatalf("faces: %+v", res)
	}
	if got := res.Front.Chain; len(got) != 3 || got[0] != "base" || got[1] != "action" || got[2] != "card" {
		t.Errorf("front chain = %v", got)
	}
	title := elementByName(res.Front, "title")
	if title.Props["text"] != "SMOOTH OPERATOR" || title.Provenance["text"] != "card" || title.Props["color"] != "#aa0000" || title.Provenance["color"] != "card" {
		t.Errorf("title = %+v", title)
	}
	if title.Props["x_mm"] != 6.0 || title.Provenance["x_mm"] != "base" {
		t.Errorf("inherited title x_mm = %v from %q", title.Props["x_mm"], title.Provenance["x_mm"])
	}
	if badge := elementByName(res.Front, "badge"); badge == nil || badge.Provenance["fill"] != "card" {
		t.Errorf("card-only element missing: %+v", badge)
	}
	if res.Front.Size.WMM != 105 {
		t.Errorf("front size = %+v", res.Front.Size)
	}
	// The back is the type's back chain without the overlay.
	if got := res.Back.Chain; len(got) != 2 || got[1] != "action_back" {
		t.Errorf("back chain = %v", got)
	}
	if bt := elementByName(res.Back, "title"); bt == nil || bt.Props["hidden"] != true || bt.Props["text"] == "SMOOTH OPERATOR" {
		t.Errorf("back title = %+v", bt)
	}

	// A template override replaces the type's front.
	var base CardTemplate
	db.Where("slug = ?", "base").First(&base)
	special := &CardTemplate{Slug: "special", Name: "special", ParentTemplateID: &base.ID}
	db.Create(special)
	AddTemplateVersion(db, special, json.RawMessage(`{"elements": {"title": {"size_pt": 30}}}`), "v1", 1)
	db.Model(&card).Update("template_id", special.ID)
	db.Where("id = ?", card.ID).First(&card)
	res, err = ResolveCard(db, &card)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Front.Chain; got[1] != "special" {
		t.Errorf("override chain = %v", got)
	}
	if elementByName(res.Front, "title").Props["size_pt"] != 30.0 {
		t.Errorf("override size_pt = %v", elementByName(res.Front, "title").Props["size_pt"])
	}
	referenced, _ := TemplateIsReferenced(db, special.ID)
	if !referenced {
		t.Errorf("a card's override template reads unreferenced")
	}

	// A type with no front cannot resolve.
	noFront := CardType{Slug: "blank", Name: "Blank"}
	db.Create(&noFront)
	orphan := Card{CardTypeID: noFront.ID, Slug: "orphan", Name: "orphan"}
	db.Create(&orphan)
	AddCardVersion(db, &orphan, json.RawMessage(`{"values": {}}`), "v1", 1)
	if _, err := ResolveCard(db, &orphan); !errors.Is(err, ErrCardHasNoFront) {
		t.Errorf("no front: %v", err)
	}
}

func TestCardVersions_AppendAndRestore(t *testing.T) {
	db := cardgenCardDB(t)
	card := Card{CardTypeID: 1, Slug: "c", Name: "c"}
	db.Create(&card)
	AddCardVersion(db, &card, json.RawMessage(`{"values": {"title": {"text": "one"}}}`), "v1", 1)
	v2, err := AddCardVersion(db, &card, json.RawMessage(`{"values": {"title": {"text": "two"}}}`), "second", 7)
	if err != nil || v2.VersionNo != 2 || *card.CurrentVersionID != v2.ID {
		t.Fatalf("v2: %+v %v", v2, err)
	}
	v3, err := RestoreCardVersion(db, &card, 1, 7)
	if err != nil || v3.VersionNo != 3 || v3.Note != "restored from v1" || !strings.Contains(string(v3.Data), `"one"`) {
		t.Fatalf("v3: %+v %v", v3, err)
	}
	if _, err := RestoreCardVersion(db, &card, 9, 7); !errors.Is(err, ErrCardVersionNotFound) {
		t.Errorf("restore missing: %v", err)
	}
	versions, _ := CardVersions(db, card.ID)
	if len(versions) != 3 {
		t.Errorf("versions = %d", len(versions))
	}
}

func TestBootstrapCardgenCards_IdempotentAndLinked(t *testing.T) {
	db := cardgenCardDB(t)
	SeedRelationshipTypes(db)
	SeedGoalCards(db)
	SeedCardgenTemplates(db)
	BootstrapCardgenCards(db)
	var n int
	db.Model(&Card{}).Count(&n)
	if n != 22 {
		t.Fatalf("bootstrapped %d cards, want 22", n)
	}
	var rel Card
	if err := db.Where("slug = ?", "rel_the_cheater").First(&rel).Error; err != nil {
		t.Fatal(err)
	}
	var goal GoalCard
	db.Where("code = ?", "the_cheater").First(&goal)
	if rel.GoalCardID == nil || *rel.GoalCardID != goal.ID || rel.Name != goal.Name {
		t.Errorf("relationship card link: %+v vs goal %d", rel, goal.ID)
	}
	v, _ := CurrentCardVersion(db, &rel)
	if !strings.Contains(string(v.Data), goal.VictoryCondition[:20]) {
		t.Errorf("relationship card values: %s", v.Data)
	}
	var conn Card
	if err := db.Where("slug = ?", "conn_fplus").First(&conn).Error; err != nil {
		t.Fatalf("F+ connection card: %v", err)
	}
	if conn.RelationshipTypeID == nil || conn.Name != "Friends with benefits" {
		t.Errorf("connection card: %+v", conn)
	}
	// Every bootstrapped card resolves both faces.
	var cards []Card
	db.Find(&cards)
	for _, c := range cards {
		if res, err := ResolveCard(db, &c); err != nil || res.Back == nil {
			t.Errorf("card %s: %v", c.Slug, err)
		}
	}
	// Re-run after an edit and a delete: nothing is reset or resurrected.
	AddCardVersion(db, &rel, json.RawMessage(`{"values": {"title": {"text": "EDITED"}}}`), "edit", 1)
	db.Delete(&conn)
	BootstrapCardgenCards(db)
	db.Model(&Card{}).Count(&n)
	if n != 21 {
		t.Errorf("after re-run: %d live cards, want 21", n)
	}
	v, _ = CurrentCardVersion(db, &rel)
	if v.VersionNo != 2 {
		t.Errorf("re-run moved the edited card to v%d", v.VersionNo)
	}
}
