package data_models

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

// #8643: the spec is a closed shape. Every refusal names the element and the
// prop, so the editor can point at the field.

func TestParseTemplateSpec_AcceptsEveryPropAtItsBounds(t *testing.T) {
	raw := `{"size": {"w_mm": 105, "h_mm": 74.25, "radius_mm": 5},
	 "elements": {"title": {"kind": "text", "x_mm": -1000, "y_mm": 1000, "w_mm": 0, "h_mm": 1000, "rotate_deg": -360, "z": 1000,
	   "text": "AFFAIR", "size_pt": 200, "line_height": 0.5, "letter_spacing": 20, "align": "right", "valign": "bottom", "weight": "bold",
	   "italic": true, "transform": "uppercase", "color": "#FFffFF", "fill": "#000000", "stroke": "#123abc", "stroke_mm": 20, "opacity": 1,
	   "asset_id": 1, "font_asset_id": 2, "fit": "stretch", "hidden": false}}}`
	spec, err := ParseTemplateSpec([]byte(raw), nil)
	if err != nil {
		t.Fatalf("valid spec refused: %v", err)
	}
	if spec.Size == nil || spec.Size.WMM != 105 || spec.Size.RadiusMM != 5 {
		t.Errorf("size = %+v", spec.Size)
	}
	if len(spec.Elements["title"]) != len(AllowedElementProps()) {
		t.Errorf("title carries %d props, want all %d", len(spec.Elements["title"]), len(AllowedElementProps()))
	}
}

func TestParseTemplateSpec_Refusals(t *testing.T) {
	cases := map[string]struct {
		raw          string
		wantElement  string
		wantProp     string
		wantContains string
	}{
		"not an object":    {`[]`, "", "", "JSON object"},
		"unknown top key":  {`{"extends": "base"}`, "", "", "unknown key"},
		"bad element name": {`{"elements": {"Title": {}}}`, "Title", "", "element names"},
		"unknown prop":     {`{"elements": {"title": {"onclick": "x"}}}`, "title", "onclick", "unknown prop"},
		"css in color":     {`{"elements": {"title": {"color": "red; background:url(x)"}}}`, "title", "color", "#rrggbb"},
		"text too long":    {`{"elements": {"title": {"text": "` + strings.Repeat("a", 2001) + `"}}}`, "title", "text", "2000"},
		"size_pt zero":     {`{"elements": {"title": {"size_pt": 0}}}`, "title", "size_pt", "between"},
		"z fraction":       {`{"elements": {"title": {"z": 1.5}}}`, "title", "z", "integer"},
		"asset id zero":    {`{"elements": {"art": {"asset_id": 0}}}`, "art", "asset_id", "between"},
		"bad enum":         {`{"elements": {"title": {"align": "justify"}}}`, "title", "align", "one of"},
		"bool as string":   {`{"elements": {"title": {"hidden": "yes"}}}`, "title", "hidden", "true or false"},
		"size unknown key": {`{"size": {"w_mm": 10, "h_mm": 10, "unit": "in"}}`, "", "", "size: unknown key"},
		"size half":        {`{"size": {"w_mm": 10}}`, "", "", "both"},
		"size negative":    {`{"size": {"w_mm": -1, "h_mm": 10}}`, "", "", "size.w_mm"},
		"too many":         {tooManyElements(), "", "", "at most 64"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseTemplateSpec([]byte(tc.raw), nil)
			if err == nil {
				t.Fatalf("accepted")
			}
			se, ok := err.(*SpecError)
			if !ok {
				t.Fatalf("error type %T: %v", err, err)
			}
			if se.Element != tc.wantElement || se.Prop != tc.wantProp || !strings.Contains(se.Msg, tc.wantContains) {
				t.Errorf("got element=%q prop=%q msg=%q; want element=%q prop=%q containing %q", se.Element, se.Prop, se.Msg, tc.wantElement, tc.wantProp, tc.wantContains)
			}
		})
	}
}

func tooManyElements(t ...string) string {
	parts := make([]string, 0, 65)
	for i := 0; i < 65; i++ {
		parts = append(parts, `"e`+strings.Repeat("x", i%3)+string(rune('a'+i%26))+string(rune('a'+i/26))+`": {}`)
	}
	return `{"elements": {` + strings.Join(parts, ",") + `}}`
}

func TestParseTemplateSpec_AssetReferencesMustBeLive(t *testing.T) {
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Without the asset table, a reference is refused.
	if _, err := ParseTemplateSpec([]byte(`{"elements": {"art": {"asset_id": 1}}}`), db); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("without card_assets: %v", err)
	}
	// With the table: a live row passes, a missing or soft-deleted one is refused.
	db.Exec(`CREATE TABLE card_assets (id INTEGER PRIMARY KEY, deleted_at DATETIME)`)
	db.Exec(`INSERT INTO card_assets (id, deleted_at) VALUES (1, NULL), (2, '2026-10-24 00:00:00')`)
	if _, err := ParseTemplateSpec([]byte(`{"elements": {"art": {"asset_id": 1}}}`), db); err != nil {
		t.Errorf("live asset refused: %v", err)
	}
	for _, raw := range []string{`{"elements": {"art": {"asset_id": 2}}}`, `{"elements": {"title": {"font_asset_id": 9}}}`} {
		_, err := ParseTemplateSpec([]byte(raw), db)
		if err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Errorf("%s: %v", raw, err)
		}
	}
}

func TestMustSpecJSON_SeedsValidate(t *testing.T) {
	for name, raw := range map[string]json.RawMessage{"base": seedBaseSpec(), "art": seedArtFaceSpec(), "back": seedBackSpec(), "plain": seedPlainFaceSpec()} {
		if _, err := ParseTemplateSpec(raw, nil); err != nil {
			t.Errorf("seed %s: %v", name, err)
		}
	}
}
