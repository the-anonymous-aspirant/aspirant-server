package data_models

import (
	"encoding/json"
	"testing"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

func TestAssetUsage_CurrentVersionsOfLiveRowsOnly(t *testing.T) {
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.AutoMigrate(&CardType{}, &CardTemplate{}, &CardTemplateVersion{}, &Card{}, &CardVersion{})

	// Unreferenced: empty lists, not nil.
	r, err := AssetUsage(db, 7)
	if err != nil || r.InUse() || r.Templates == nil || r.Cards == nil {
		t.Fatalf("empty: %+v %v", r, err)
	}

	// A template whose v1 references asset 7 on two elements; its v2 drops it.
	tmpl := &CardTemplate{Slug: "t", Name: "T"}
	db.Create(tmpl)
	AddTemplateVersion(db, tmpl, json.RawMessage(`{"elements": {"art": {"kind": "image", "asset_id": 7}, "title": {"kind": "text", "font_asset_id": 7}, "body": {"kind": "text", "asset_id": 8}}}`), "v1", 1)
	r, _ = AssetUsage(db, 7)
	if len(r.Templates) != 1 || r.Templates[0].VersionNo != 1 || len(r.Templates[0].Elements) != 2 || r.Templates[0].Elements[0] != "art" || r.Templates[0].Elements[1] != "title" {
		t.Errorf("template usage = %+v", r.Templates)
	}
	AddTemplateVersion(db, tmpl, json.RawMessage(`{"elements": {"art": {"kind": "image", "asset_id": 8}}}`), "v2", 1)
	r, _ = AssetUsage(db, 7)
	if r.InUse() {
		t.Errorf("an older version still counts: %+v", r)
	}
	r, _ = AssetUsage(db, 8)
	if len(r.Templates) != 1 || r.Templates[0].VersionNo != 2 {
		t.Errorf("asset 8 usage = %+v", r.Templates)
	}

	// A card's values reference asset 9; a soft-deleted card does not count.
	ct := CardType{Slug: "x", Name: "X"}
	db.Create(&ct)
	card := Card{CardTypeID: ct.ID, Slug: "c", Name: "C"}
	db.Create(&card)
	AddCardVersion(db, &card, json.RawMessage(`{"values": {"art": {"asset_id": 9}}}`), "v1", 1)
	r, _ = AssetUsage(db, 9)
	if len(r.Cards) != 1 || r.Cards[0].Slug != "c" || r.Cards[0].Elements[0] != "art" || len(r.Templates) != 0 {
		t.Errorf("card usage = %+v", r)
	}
	db.Delete(&card)
	r, _ = AssetUsage(db, 9)
	if r.InUse() {
		t.Errorf("a deleted card still counts: %+v", r)
	}
	db.Delete(tmpl)
	r, _ = AssetUsage(db, 8)
	if r.InUse() {
		t.Errorf("a deleted template still counts: %+v", r)
	}
}
