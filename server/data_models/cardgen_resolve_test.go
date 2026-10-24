package data_models

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

func cardgenTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.AutoMigrate(&CardType{}, &CardTemplate{}, &CardTemplateVersion{})
	t.Cleanup(func() { db.Close() })
	return db
}

func mkTemplate(t *testing.T, db *gorm.DB, slug string, parent *uint, spec string) *CardTemplate {
	t.Helper()
	tmpl := &CardTemplate{Slug: slug, Name: slug, ParentTemplateID: parent}
	if err := db.Create(tmpl).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := ParseTemplateSpec([]byte(spec), nil); err != nil {
		t.Fatalf("test spec %s: %v", slug, err)
	}
	if _, err := AddTemplateVersion(db, tmpl, json.RawMessage(spec), "v1", 1); err != nil {
		t.Fatal(err)
	}
	return tmpl
}

func elementByName(res *ResolvedTemplate, name string) *ResolvedElement {
	for i := range res.Elements {
		if res.Elements[i].Name == name {
			return &res.Elements[i]
		}
	}
	return nil
}

func TestResolveTemplate_ThreeLevelChainMergesWithProvenance(t *testing.T) {
	db := cardgenTestDB(t)
	base := mkTemplate(t, db, "base", nil, `{"size": {"w_mm": 105, "h_mm": 74.25, "radius_mm": 5},
	  "elements": {"title": {"kind": "text", "x_mm": 5, "y_mm": 6, "size_pt": 12, "color": "#111111", "z": 10},
	               "art": {"kind": "image", "x_mm": 0, "y_mm": 0, "w_mm": 105, "h_mm": 74.25, "z": 0, "hidden": true}}}`)
	mid := mkTemplate(t, db, "constellation", &base.ID, `{"size": {"radius_mm": 3},
	  "elements": {"title": {"color": "#ffffff", "size_pt": 14}, "art": {"hidden": false}}}`)
	leaf := mkTemplate(t, db, "orion", &mid.ID, `{"elements": {"title": {"text": "ORION"}, "badge": {"kind": "rect", "x_mm": 1, "y_mm": 1, "w_mm": 5, "h_mm": 5, "fill": "#ff0000", "z": 5}}}`)

	res, err := ResolveTemplate(db, leaf.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Chain; len(got) != 3 || got[0] != "base" || got[1] != "constellation" || got[2] != "orion" {
		t.Errorf("chain = %v", got)
	}
	if res.Size.WMM != 105 || res.Size.HMM != 74.25 || res.Size.RadiusMM != 3 {
		t.Errorf("size = %+v, want 105 x 74.25 radius 3", res.Size)
	}
	if res.SizeProvenance["w_mm"] != "base" || res.SizeProvenance["radius_mm"] != "constellation" {
		t.Errorf("size provenance = %v", res.SizeProvenance)
	}
	title := elementByName(res, "title")
	if title == nil {
		t.Fatal("title missing")
	}
	if title.Props["x_mm"] != 5.0 || title.Provenance["x_mm"] != "base" {
		t.Errorf("inherited x_mm = %v from %q", title.Props["x_mm"], title.Provenance["x_mm"])
	}
	if title.Props["color"] != "#ffffff" || title.Provenance["color"] != "constellation" {
		t.Errorf("overridden color = %v from %q", title.Props["color"], title.Provenance["color"])
	}
	if title.Props["text"] != "ORION" || title.Provenance["text"] != "orion" {
		t.Errorf("leaf text = %v from %q", title.Props["text"], title.Provenance["text"])
	}
	art := elementByName(res, "art")
	if art == nil || art.Props["hidden"] != false || art.Provenance["hidden"] != "constellation" {
		t.Errorf("art hidden = %v", art)
	}
	// Order: z 0 (art), z 5 (badge), z 10 (title).
	if len(res.Elements) != 3 || res.Elements[0].Name != "art" || res.Elements[1].Name != "badge" || res.Elements[2].Name != "title" {
		names := []string{}
		for _, e := range res.Elements {
			names = append(names, e.Name)
		}
		t.Errorf("order = %v, want art, badge, title", names)
	}
}

func TestResolveTemplate_HiddenIsPreservedNotDropped(t *testing.T) {
	db := cardgenTestDB(t)
	base := mkTemplate(t, db, "base", nil, `{"elements": {"quote": {"kind": "text", "text": "q"}}}`)
	leaf := mkTemplate(t, db, "leaf", &base.ID, `{"elements": {"quote": {"hidden": true}}}`)
	res, err := ResolveTemplate(db, leaf.ID)
	if err != nil {
		t.Fatal(err)
	}
	q := elementByName(res, "quote")
	if q == nil || q.Props["hidden"] != true || q.Props["text"] != "q" {
		t.Errorf("quote = %+v", q)
	}
}

func TestTemplateChain_RefusesCycleMissingAndDepth(t *testing.T) {
	db := cardgenTestDB(t)
	a := mkTemplate(t, db, "a", nil, `{}`)
	b := mkTemplate(t, db, "b", &a.ID, `{}`)
	// Make a extend b: a cycle.
	db.Model(a).Update("parent_template_id", b.ID)
	if _, err := TemplateChain(db, b.ID); !errors.Is(err, ErrTemplateCycle) {
		t.Errorf("cycle: %v", err)
	}
	if _, err := ResolveTemplate(db, b.ID); !errors.Is(err, ErrTemplateCycle) {
		t.Errorf("resolve cycle: %v", err)
	}
	cyc, err := WouldCycle(db, a.ID, &a.ID)
	if err != nil || !cyc {
		t.Errorf("self parent: cyc=%v err=%v", cyc, err)
	}

	missing := uint(999)
	c := mkTemplate(t, db, "c", &missing, `{}`)
	if _, err := TemplateChain(db, c.ID); !errors.Is(err, ErrTemplateMissing) {
		t.Errorf("missing parent: %v", err)
	}

	var prev *uint
	var last *CardTemplate
	for i := 0; i <= MaxTemplateChainDepth; i++ {
		last = mkTemplate(t, db, "d"+string(rune('a'+i)), prev, `{}`)
		id := last.ID
		prev = &id
	}
	if _, err := TemplateChain(db, last.ID); !errors.Is(err, ErrTemplateChainTooDeep) {
		t.Errorf("depth %d: %v", MaxTemplateChainDepth+1, err)
	}
}

func TestWouldCycle_DetectsIndirectAncestor(t *testing.T) {
	db := cardgenTestDB(t)
	a := mkTemplate(t, db, "a", nil, `{}`)
	b := mkTemplate(t, db, "b", &a.ID, `{}`)
	c := mkTemplate(t, db, "c", &b.ID, `{}`)
	cyc, err := WouldCycle(db, a.ID, &c.ID) // a would extend c, which extends b, which extends a
	if err != nil || !cyc {
		t.Errorf("indirect: cyc=%v err=%v", cyc, err)
	}
	other := mkTemplate(t, db, "other", nil, `{}`)
	cyc, err = WouldCycle(db, a.ID, &other.ID)
	if err != nil || cyc {
		t.Errorf("unrelated parent: cyc=%v err=%v", cyc, err)
	}
}

func TestTemplateVersions_AppendAndRestore(t *testing.T) {
	db := cardgenTestDB(t)
	tmpl := mkTemplate(t, db, "t", nil, `{"elements": {"title": {"text": "one"}}}`)
	v2, err := AddTemplateVersion(db, tmpl, json.RawMessage(`{"elements": {"title": {"text": "two"}}}`), "second", 7)
	if err != nil || v2.VersionNo != 2 {
		t.Fatalf("v2: %+v %v", v2, err)
	}
	if tmpl.CurrentVersionID == nil || *tmpl.CurrentVersionID != v2.ID {
		t.Errorf("current not moved to v2")
	}
	v3, err := RestoreTemplateVersion(db, tmpl, 1, 7)
	if err != nil || v3.VersionNo != 3 || v3.Note != "restored from v1" || string(v3.Spec) != `{"elements": {"title": {"text": "one"}}}` {
		t.Fatalf("v3: %+v %v", v3, err)
	}
	if _, err := RestoreTemplateVersion(db, tmpl, 9, 7); !errors.Is(err, ErrTemplateVersionNotFound) {
		t.Errorf("restore missing: %v", err)
	}
	versions, err := TemplateVersions(db, tmpl.ID)
	if err != nil || len(versions) != 3 || versions[0].VersionNo != 1 || versions[2].VersionNo != 3 {
		t.Errorf("versions = %+v %v", versions, err)
	}
	cur, err := CurrentTemplateVersion(db, tmpl)
	if err != nil || cur.VersionNo != 3 {
		t.Errorf("current = %+v %v", cur, err)
	}
}

func TestSeedCardgenTemplates_IdempotentAndEditable(t *testing.T) {
	db := cardgenTestDB(t)
	SeedCardgenTemplates(db)
	var templates, versions, types int
	db.Model(&CardTemplate{}).Count(&templates)
	db.Model(&CardTemplateVersion{}).Count(&versions)
	db.Model(&CardType{}).Count(&types)
	if templates != 9 || versions != 9 || types != 4 {
		t.Fatalf("after seed: %d templates, %d versions, %d types; want 9, 9, 4", templates, versions, types)
	}
	// Every type resolves through its faces.
	var ts []CardType
	db.Find(&ts)
	for _, ct := range ts {
		for _, id := range []*uint{ct.FrontTemplateID, ct.BackTemplateID} {
			if id == nil {
				t.Errorf("type %s has a nil face", ct.Slug)
				continue
			}
			res, err := ResolveTemplate(db, *id)
			if err != nil || res.Size.WMM != 105 || elementByName(res, "art") == nil {
				t.Errorf("type %s face %d: %+v %v", ct.Slug, *id, res, err)
			}
		}
	}
	// An operator edit survives a re-seed.
	var base CardTemplate
	db.Where("slug = ?", "base").First(&base)
	if _, err := AddTemplateVersion(db, &base, json.RawMessage(`{"elements": {"title": {"size_pt": 20}}}`), "edit", 1); err != nil {
		t.Fatal(err)
	}
	SeedCardgenTemplates(db)
	db.Model(&CardTemplate{}).Count(&templates)
	db.Model(&CardTemplateVersion{}).Count(&versions)
	db.Model(&CardType{}).Count(&types)
	if templates != 9 || versions != 10 || types != 4 {
		t.Fatalf("after re-seed: %d templates, %d versions, %d types; want 9, 10, 4", templates, versions, types)
	}
	cur, _ := CurrentTemplateVersion(db, &base)
	if cur.VersionNo != 2 {
		t.Errorf("re-seed moved base's current version to v%d", cur.VersionNo)
	}
	referenced, _ := TemplateIsReferenced(db, base.ID)
	if !referenced {
		t.Errorf("base is extended by the faces but reads unreferenced")
	}
}
