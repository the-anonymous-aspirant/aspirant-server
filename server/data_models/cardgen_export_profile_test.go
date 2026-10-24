package data_models

import (
	"strings"
	"testing"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

func TestParseExportProfileSpec_DefaultsAndBounds(t *testing.T) {
	spec, err := ParseExportProfileSpec([]byte(`{"card_w_mm": 105, "card_h_mm": 74.25}`))
	if err != nil {
		t.Fatal(err)
	}
	if spec.DPI != 300 || spec.Backs != "none" || spec.Sheet.Page != "A4" || spec.Sheet.PageWMM != 210 || spec.Sheet.PageHMM != 297 || spec.Sheet.Cols != 1 || spec.Sheet.Rows != 1 || spec.Sheet.Orientation != "portrait" {
		t.Errorf("defaults = %+v", spec)
	}
	full := `{"card_w_mm": 75, "card_h_mm": 105, "radius_mm": 5, "bleed_mm": 3, "dpi": 1200, "crop_marks": true, "backs": "mirror",
	  "sheet": {"page": "custom", "page_w_mm": 330, "page_h_mm": 483, "cols": 20, "rows": 20, "gap_mm": 50, "margin_mm": 50, "orientation": "landscape"}}`
	spec, err = ParseExportProfileSpec([]byte(full))
	if err != nil {
		t.Fatal(err)
	}
	if spec.Sheet.PageWMM != 330 || spec.Sheet.Cols != 20 || !spec.CropMarks || spec.Backs != "mirror" {
		t.Errorf("full = %+v", spec)
	}
}

func TestParseExportProfileSpec_Refusals(t *testing.T) {
	cases := map[string]struct{ raw, key string }{
		"not object":     {`[]`, ""},
		"unknown key":    {`{"card_w_mm": 105, "card_h_mm": 74, "units": "in"}`, "units"},
		"missing h":      {`{"card_w_mm": 105}`, "card_h_mm"},
		"dpi low":        {`{"card_w_mm": 105, "card_h_mm": 74, "dpi": 50}`, "dpi"},
		"dpi fraction":   {`{"card_w_mm": 105, "card_h_mm": 74, "dpi": 300.5}`, "dpi"},
		"cols zero":      {`{"card_w_mm": 105, "card_h_mm": 74, "sheet": {"cols": 0}}`, "sheet.cols"},
		"bad page":       {`{"card_w_mm": 105, "card_h_mm": 74, "sheet": {"page": "B5"}}`, "sheet.page"},
		"custom no size": {`{"card_w_mm": 105, "card_h_mm": 74, "sheet": {"page": "custom"}}`, "sheet.page_w_mm"},
		"neg bleed":      {`{"card_w_mm": 105, "card_h_mm": 74, "bleed_mm": -1}`, "bleed_mm"},
		"backs enum":     {`{"card_w_mm": 105, "card_h_mm": 74, "backs": "flip"}`, "backs"},
		"sheet unknown":  {`{"card_w_mm": 105, "card_h_mm": 74, "sheet": {"duplex": true}}`, "sheet.duplex"},
		"crop string":    {`{"card_w_mm": 105, "card_h_mm": 74, "crop_marks": "yes"}`, "crop_marks"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseExportProfileSpec([]byte(tc.raw))
			if err == nil {
				t.Fatal("accepted")
			}
			se, ok := err.(*ExportSpecError)
			if !ok || se.Key != tc.key {
				t.Errorf("err = %v, want key %q", err, tc.key)
			}
		})
	}
}

func TestSeedCardExportProfiles_IdempotentAndRespectsDeletes(t *testing.T) {
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.AutoMigrate(&CardExportProfile{})
	SeedCardExportProfiles(db)
	var n int
	db.Model(&CardExportProfile{}).Count(&n)
	if n != 3 {
		t.Fatalf("seeded %d, want 3", n)
	}
	var rows []CardExportProfile
	db.Find(&rows)
	for _, r := range rows {
		spec, err := ParseExportProfileSpec(r.Spec)
		if err != nil || spec.BleedMM != 3 || spec.DPI != 300 || !spec.CropMarks {
			t.Errorf("preset %q: %+v %v", r.Name, spec, err)
		}
		if strings.HasPrefix(r.Name, "Prototype") && (spec.CardWMM != 105 || spec.Sheet.Cols != 2 || spec.Sheet.Rows != 4) {
			t.Errorf("prototype preset = %+v", spec)
		}
	}
	// Edit one, delete one, re-seed: the edit stays, the deleted one stays gone.
	db.Model(&rows[0]).Update("spec", []byte(`{"card_w_mm": 50, "card_h_mm": 50}`))
	db.Delete(&rows[1])
	SeedCardExportProfiles(db)
	db.Model(&CardExportProfile{}).Count(&n)
	if n != 2 {
		t.Errorf("after re-seed: %d live rows, want 2", n)
	}
	var edited CardExportProfile
	db.Where("id = ?", rows[0].ID).First(&edited)
	if !strings.Contains(string(edited.Spec), `"card_w_mm": 50`) {
		t.Errorf("re-seed overwrote the edited preset: %s", edited.Spec)
	}
}
