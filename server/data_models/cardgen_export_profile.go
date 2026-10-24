package data_models

import (
	"encoding/json"
	"fmt"
	"log"
	"math"

	"github.com/jinzhu/gorm"
)

// Export profiles (system_3 #8634, subtask #8644): the `export/print.yaml` of
// the operator's sketch as rows. A profile says how cards are laid on sheets
// for print: card size, bleed, DPI, crop marks, the sheet grid, and whether
// back pages are generated mirrored for duplex. The client's PDF export (#8652)
// reads one; the server stores and validates it.

// CardExportProfile is one named print configuration.
type CardExportProfile struct {
	gorm.Model
	Name string          `json:"name" gorm:"type:varchar(60);not null;unique_index:idx_card_export_profiles_name"`
	Spec json.RawMessage `json:"spec" gorm:"type:jsonb;not null"`
}

func (CardExportProfile) TableName() string { return "card_export_profiles" }

// ExportSheet is the page and grid a profile prints on.
type ExportSheet struct {
	Page        string  `json:"page"`
	PageWMM     float64 `json:"page_w_mm,omitempty"`
	PageHMM     float64 `json:"page_h_mm,omitempty"`
	Cols        int     `json:"cols"`
	Rows        int     `json:"rows"`
	GapMM       float64 `json:"gap_mm"`
	MarginMM    float64 `json:"margin_mm"`
	Orientation string  `json:"orientation"`
}

// ExportProfileSpec is a parsed, validated profile body.
type ExportProfileSpec struct {
	CardWMM   float64     `json:"card_w_mm"`
	CardHMM   float64     `json:"card_h_mm"`
	RadiusMM  float64     `json:"radius_mm"`
	BleedMM   float64     `json:"bleed_mm"`
	DPI       int         `json:"dpi"`
	CropMarks bool        `json:"crop_marks"`
	Sheet     ExportSheet `json:"sheet"`
	Backs     string      `json:"backs"`
}

// ExportSpecError names the key a profile was refused for.
type ExportSpecError struct {
	Key string
	Msg string
}

func (e *ExportSpecError) Error() string {
	if e.Key == "" {
		return "export profile: " + e.Msg
	}
	return fmt.Sprintf("export profile: %s: %s", e.Key, e.Msg)
}

var exportPages = map[string][2]float64{
	"A4":     {210, 297},
	"A3":     {297, 420},
	"Letter": {215.9, 279.4},
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

func num(m map[string]any, key string, required bool, min, max float64) (float64, error) {
	v, ok := m[key]
	if !ok {
		if required {
			return 0, &ExportSpecError{Key: key, Msg: "is required"}
		}
		return 0, nil
	}
	f, isNum := v.(float64)
	if !isNum || !finite(f) || f < min || f > max {
		return 0, &ExportSpecError{Key: key, Msg: fmt.Sprintf("must be a number between %g and %g", min, max)}
	}
	return f, nil
}

// ParseExportProfileSpec decodes and validates a profile body. The shape is
// closed: unknown keys at either level are refused naming the key.
func ParseExportProfileSpec(raw []byte) (*ExportProfileSpec, error) {
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, &ExportSpecError{Msg: "must be a JSON object"}
	}
	allowed := map[string]bool{"card_w_mm": true, "card_h_mm": true, "radius_mm": true, "bleed_mm": true, "dpi": true, "crop_marks": true, "sheet": true, "backs": true}
	for k := range top {
		if !allowed[k] {
			return nil, &ExportSpecError{Key: k, Msg: "unknown key"}
		}
	}
	spec := &ExportProfileSpec{Backs: "none", Sheet: ExportSheet{Page: "A4", Cols: 1, Rows: 1, Orientation: "portrait"}}
	var err error
	if spec.CardWMM, err = num(top, "card_w_mm", true, 10, 500); err != nil {
		return nil, err
	}
	if spec.CardHMM, err = num(top, "card_h_mm", true, 10, 500); err != nil {
		return nil, err
	}
	if spec.RadiusMM, err = num(top, "radius_mm", false, 0, 50); err != nil {
		return nil, err
	}
	if spec.BleedMM, err = num(top, "bleed_mm", false, 0, 20); err != nil {
		return nil, err
	}
	dpi, err := num(top, "dpi", false, 72, 1200)
	if err != nil {
		return nil, err
	}
	if dpi == 0 {
		dpi = 300
	}
	if dpi != math.Trunc(dpi) {
		return nil, &ExportSpecError{Key: "dpi", Msg: "must be an integer"}
	}
	spec.DPI = int(dpi)
	if v, ok := top["crop_marks"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return nil, &ExportSpecError{Key: "crop_marks", Msg: "must be true or false"}
		}
		spec.CropMarks = b
	}
	if v, ok := top["backs"]; ok {
		s, isStr := v.(string)
		if !isStr || (s != "mirror" && s != "none") {
			return nil, &ExportSpecError{Key: "backs", Msg: "must be mirror or none"}
		}
		spec.Backs = s
	}
	if v, ok := top["sheet"]; ok {
		sheet, isMap := v.(map[string]any)
		if !isMap {
			return nil, &ExportSpecError{Key: "sheet", Msg: "must be an object"}
		}
		sheetAllowed := map[string]bool{"page": true, "page_w_mm": true, "page_h_mm": true, "cols": true, "rows": true, "gap_mm": true, "margin_mm": true, "orientation": true}
		for k := range sheet {
			if !sheetAllowed[k] {
				return nil, &ExportSpecError{Key: "sheet." + k, Msg: "unknown key"}
			}
		}
		if pv, ok := sheet["page"]; ok {
			s, isStr := pv.(string)
			if !isStr || (s != "custom" && exportPages[s] == [2]float64{}) {
				return nil, &ExportSpecError{Key: "sheet.page", Msg: "must be A4, A3, Letter or custom"}
			}
			spec.Sheet.Page = s
		}
		if spec.Sheet.PageWMM, err = num(sheet, "page_w_mm", spec.Sheet.Page == "custom", 50, 2000); err != nil {
			return nil, &ExportSpecError{Key: "sheet.page_w_mm", Msg: err.(*ExportSpecError).Msg}
		}
		if spec.Sheet.PageHMM, err = num(sheet, "page_h_mm", spec.Sheet.Page == "custom", 50, 2000); err != nil {
			return nil, &ExportSpecError{Key: "sheet.page_h_mm", Msg: err.(*ExportSpecError).Msg}
		}
		cols, err := num(sheet, "cols", false, 1, 20)
		if err != nil {
			return nil, &ExportSpecError{Key: "sheet.cols", Msg: err.(*ExportSpecError).Msg}
		}
		rows, err := num(sheet, "rows", false, 1, 20)
		if err != nil {
			return nil, &ExportSpecError{Key: "sheet.rows", Msg: err.(*ExportSpecError).Msg}
		}
		if cols != 0 {
			spec.Sheet.Cols = int(cols)
		}
		if rows != 0 {
			spec.Sheet.Rows = int(rows)
		}
		if spec.Sheet.GapMM, err = num(sheet, "gap_mm", false, 0, 50); err != nil {
			return nil, &ExportSpecError{Key: "sheet.gap_mm", Msg: err.(*ExportSpecError).Msg}
		}
		if spec.Sheet.MarginMM, err = num(sheet, "margin_mm", false, 0, 50); err != nil {
			return nil, &ExportSpecError{Key: "sheet.margin_mm", Msg: err.(*ExportSpecError).Msg}
		}
		if ov, ok := sheet["orientation"]; ok {
			s, isStr := ov.(string)
			if !isStr || (s != "portrait" && s != "landscape") {
				return nil, &ExportSpecError{Key: "sheet.orientation", Msg: "must be portrait or landscape"}
			}
			spec.Sheet.Orientation = s
		}
	}
	if spec.Sheet.Page != "custom" {
		p := exportPages[spec.Sheet.Page]
		spec.Sheet.PageWMM, spec.Sheet.PageHMM = p[0], p[1]
	}
	return spec, nil
}

// MustExportSpecJSON marshals a seed spec; it panics on one that would not
// validate, a programming error in the seed.
func MustExportSpecJSON(spec ExportProfileSpec) json.RawMessage {
	raw, err := json.Marshal(spec)
	if err != nil {
		panic(err)
	}
	if _, err := ParseExportProfileSpec(raw); err != nil {
		panic(err)
	}
	return raw
}

// Presets, from the operator's materials (#8634 c53338 section 7): the
// prototype sheets are A4 with eight 105 x 74.25 mm cards, and the two vendor
// quotes price 75 x 105 mm cards (Friedmann with a 5 mm corner radius).
var cardExportProfileSeed = []CardExportProfile{
	{Name: "Prototype A4 2x4 (pixartprinting)", Spec: MustExportSpecJSON(ExportProfileSpec{CardWMM: 105, CardHMM: 74.25, RadiusMM: 5, BleedMM: 3, DPI: 300, CropMarks: true, Backs: "mirror", Sheet: ExportSheet{Page: "A4", Cols: 2, Rows: 4, GapMM: 0, MarginMM: 0, Orientation: "portrait"}})},
	{Name: "Friedmann 75x105 +3 mm bleed", Spec: MustExportSpecJSON(ExportProfileSpec{CardWMM: 75, CardHMM: 105, RadiusMM: 5, BleedMM: 3, DPI: 300, CropMarks: true, Backs: "mirror", Sheet: ExportSheet{Page: "A4", Cols: 2, Rows: 2, GapMM: 6, MarginMM: 10, Orientation: "portrait"}})},
	{Name: "Pinguin 75x105 +3 mm bleed", Spec: MustExportSpecJSON(ExportProfileSpec{CardWMM: 75, CardHMM: 105, RadiusMM: 0, BleedMM: 3, DPI: 300, CropMarks: true, Backs: "mirror", Sheet: ExportSheet{Page: "A4", Cols: 2, Rows: 2, GapMM: 6, MarginMM: 10, Orientation: "portrait"}})},
}

// SeedCardExportProfiles inserts the presets that do not exist. The lookup is
// Unscoped, so a preset the operator deleted stays deleted and an edited one
// is left as edited.
func SeedCardExportProfiles(db *gorm.DB) {
	for _, p := range cardExportProfileSeed {
		var n int
		if err := db.Unscoped().Model(&CardExportProfile{}).Where("name = ?", p.Name).Count(&n).Error; err != nil {
			log.Printf("cardgen seed: export profile %q: %v", p.Name, err)
			continue
		}
		if n > 0 {
			continue
		}
		row := p
		if err := db.Create(&row).Error; err != nil {
			log.Printf("cardgen seed: export profile %q: %v", p.Name, err)
		}
	}
}
