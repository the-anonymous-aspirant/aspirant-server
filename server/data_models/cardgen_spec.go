package data_models

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/jinzhu/gorm"
)

// Card template specs (system_3 #8634, subtask #8643). A spec is the jsonb
// body of one template version: a card size and a map of named elements, each
// a flat bag of props. The shape is CLOSED: every top-level key, every element
// name and every prop is validated here before it is stored, and the client
// renders the values as data (text interpolation, numeric mm and pt, hex
// colours, integer asset ids resolved by the server), never as markup or CSS.
// That is what keeps a crafted spec from injecting anything (security review,
// #8634 c53354 item 5).

// CardSize is the physical card in millimetres.
type CardSize struct {
	WMM      float64 `json:"w_mm"`
	HMM      float64 `json:"h_mm"`
	RadiusMM float64 `json:"radius_mm"`
}

// TemplateSpec is a parsed, validated template version body.
type TemplateSpec struct {
	Size     *CardSize                 `json:"size,omitempty"`
	Elements map[string]map[string]any `json:"elements"`
}

// SpecError names the element and prop a spec was refused for.
type SpecError struct {
	Element string
	Prop    string
	Msg     string
}

func (e *SpecError) Error() string {
	switch {
	case e.Element == "" && e.Prop == "":
		return "spec: " + e.Msg
	case e.Prop == "":
		return fmt.Sprintf("spec: element %q: %s", e.Element, e.Msg)
	default:
		return fmt.Sprintf("spec: element %q prop %q: %s", e.Element, e.Prop, e.Msg)
	}
}

const (
	maxSpecElements   = 64
	maxSpecTextLength = 2000
	maxCardSideMM     = 1000
)

var (
	elementNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	hexColourPattern   = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

type propRule func(v any) error

func enumRule(allowed ...string) propRule {
	set := map[string]bool{}
	for _, a := range allowed {
		set[a] = true
	}
	return func(v any) error {
		s, ok := v.(string)
		if !ok || !set[s] {
			return fmt.Errorf("must be one of %s", strings.Join(allowed, "|"))
		}
		return nil
	}
}

func numberRule(min, max float64) propRule {
	return func(v any) error {
		f, ok := v.(float64)
		if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Errorf("must be a finite number")
		}
		if f < min || f > max {
			return fmt.Errorf("must be between %g and %g", min, max)
		}
		return nil
	}
}

func integerRule(min, max float64) propRule {
	inner := numberRule(min, max)
	return func(v any) error {
		if err := inner(v); err != nil {
			return err
		}
		if f := v.(float64); f != math.Trunc(f) {
			return fmt.Errorf("must be an integer")
		}
		return nil
	}
}

func boolRule(v any) error {
	if _, ok := v.(bool); !ok {
		return fmt.Errorf("must be true or false")
	}
	return nil
}

func colourRule(v any) error {
	s, ok := v.(string)
	if !ok || !hexColourPattern.MatchString(s) {
		return fmt.Errorf("must be a #rrggbb colour")
	}
	return nil
}

func textRule(v any) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("must be a string")
	}
	if len(s) > maxSpecTextLength {
		return fmt.Errorf("must be at most %d characters", maxSpecTextLength)
	}
	return nil
}

// elementPropRules is the closed prop set. A prop outside it is refused.
var elementPropRules = map[string]propRule{
	"kind":           enumRule("text", "image", "rect"),
	"x_mm":           numberRule(-maxCardSideMM, maxCardSideMM),
	"y_mm":           numberRule(-maxCardSideMM, maxCardSideMM),
	"w_mm":           numberRule(0, maxCardSideMM),
	"h_mm":           numberRule(0, maxCardSideMM),
	"rotate_deg":     numberRule(-360, 360),
	"z":              integerRule(-1000, 1000),
	"text":           textRule,
	"size_pt":        numberRule(1, 200),
	"line_height":    numberRule(0.5, 3),
	"letter_spacing": numberRule(-5, 20),
	"align":          enumRule("left", "center", "right"),
	"valign":         enumRule("top", "middle", "bottom"),
	"weight":         enumRule("normal", "bold"),
	"italic":         boolRule,
	"transform":      enumRule("none", "uppercase"),
	"color":          colourRule,
	"fill":           colourRule,
	"stroke":         colourRule,
	"stroke_mm":      numberRule(0, 20),
	"opacity":        numberRule(0, 1),
	"asset_id":       integerRule(1, math.MaxInt32),
	"font_asset_id":  integerRule(1, math.MaxInt32),
	"fit":            enumRule("cover", "contain", "stretch"),
	"hidden":         boolRule,
}

// assetProps are the props whose value must name a live card_assets row.
var assetProps = []string{"asset_id", "font_asset_id"}

// AllowedElementProps lists the closed prop set, sorted, for error messages
// and for the client's inspector.
func AllowedElementProps() []string {
	out := make([]string, 0, len(elementPropRules))
	for k := range elementPropRules {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ParseTemplateSpec decodes and validates a spec. db may be nil to skip the
// asset-existence check (the resolver re-validates nothing; a stored spec was
// validated when it was saved). With a db, every asset_id / font_asset_id must
// name a live card_assets row when that table exists.
func ParseTemplateSpec(raw []byte, db *gorm.DB) (*TemplateSpec, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, &SpecError{Msg: "must be a JSON object"}
	}
	for k := range top {
		if k != "size" && k != "elements" {
			return nil, &SpecError{Msg: fmt.Sprintf("unknown key %q (allowed: size, elements)", k)}
		}
	}
	spec := &TemplateSpec{Elements: map[string]map[string]any{}}

	if rawSize, ok := top["size"]; ok && string(rawSize) != "null" {
		var sizeMap map[string]any
		if err := json.Unmarshal(rawSize, &sizeMap); err != nil {
			return nil, &SpecError{Msg: "size must be an object"}
		}
		size := &CardSize{}
		for k, v := range sizeMap {
			f, isNum := v.(float64)
			switch k {
			case "w_mm", "h_mm":
				if !isNum || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 || f > maxCardSideMM {
					return nil, &SpecError{Msg: fmt.Sprintf("size.%s must be a number between 0 and %d", k, maxCardSideMM)}
				}
			case "radius_mm":
				if !isNum || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 50 {
					return nil, &SpecError{Msg: "size.radius_mm must be a number between 0 and 50"}
				}
			default:
				return nil, &SpecError{Msg: fmt.Sprintf("size: unknown key %q (allowed: w_mm, h_mm, radius_mm)", k)}
			}
			switch k {
			case "w_mm":
				size.WMM = f
			case "h_mm":
				size.HMM = f
			case "radius_mm":
				size.RadiusMM = f
			}
		}
		if (size.WMM == 0) != (size.HMM == 0) {
			return nil, &SpecError{Msg: "size needs both w_mm and h_mm"}
		}
		spec.Size = size
	}

	rawElements, ok := top["elements"]
	if !ok || string(rawElements) == "null" {
		return spec, nil
	}
	var elements map[string]map[string]any
	if err := json.Unmarshal(rawElements, &elements); err != nil {
		return nil, &SpecError{Msg: "elements must be an object of element objects"}
	}
	if len(elements) > maxSpecElements {
		return nil, &SpecError{Msg: fmt.Sprintf("at most %d elements", maxSpecElements)}
	}
	for name, props := range elements {
		if err := validateElementProps(name, props); err != nil {
			return nil, err
		}
		spec.Elements[name] = props
	}

	if db != nil {
		if err := checkSpecAssets(db, spec); err != nil {
			return nil, err
		}
	}
	return spec, nil
}

// validateElementProps checks one element's name and every prop against the
// closed rule table. Shared by the template spec and the card data (#8646).
func validateElementProps(name string, props map[string]any) error {
	if !elementNamePattern.MatchString(name) {
		return &SpecError{Element: name, Msg: "element names are lowercase letters, digits and underscores, starting with a letter"}
	}
	if props == nil {
		return &SpecError{Element: name, Msg: "must be an object"}
	}
	for prop, v := range props {
		rule, known := elementPropRules[prop]
		if !known {
			return &SpecError{Element: name, Prop: prop, Msg: "unknown prop (allowed: " + strings.Join(AllowedElementProps(), ", ") + ")"}
		}
		if err := rule(v); err != nil {
			return &SpecError{Element: name, Prop: prop, Msg: err.Error()}
		}
	}
	return nil
}

// CardData is a parsed, validated card version body: per-element prop values
// overlaid on the resolved template chain (#8646).
type CardData struct {
	Values map[string]map[string]any `json:"values"`
}

// ParseCardData decodes and validates card values with the same closed shape
// as a template spec's elements: `{"values": {"<element>": {<prop>: value}}}`.
func ParseCardData(raw []byte, db *gorm.DB) (*CardData, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, &SpecError{Msg: "must be a JSON object"}
	}
	for k := range top {
		if k != "values" {
			return nil, &SpecError{Msg: fmt.Sprintf("unknown key %q (allowed: values)", k)}
		}
	}
	data := &CardData{Values: map[string]map[string]any{}}
	rawValues, ok := top["values"]
	if !ok || string(rawValues) == "null" {
		return data, nil
	}
	var values map[string]map[string]any
	if err := json.Unmarshal(rawValues, &values); err != nil {
		return nil, &SpecError{Msg: "values must be an object of element objects"}
	}
	if len(values) > maxSpecElements {
		return nil, &SpecError{Msg: fmt.Sprintf("at most %d elements", maxSpecElements)}
	}
	for name, props := range values {
		if err := validateElementProps(name, props); err != nil {
			return nil, err
		}
		data.Values[name] = props
	}
	if db != nil {
		if err := checkSpecAssets(db, &TemplateSpec{Elements: data.Values}); err != nil {
			return nil, err
		}
	}
	return data, nil
}

// checkSpecAssets refuses an asset reference that is not a live row. When the
// asset table does not exist yet (a database migrated before #8642 landed),
// references cannot be checked and are refused outright.
func checkSpecAssets(db *gorm.DB, spec *TemplateSpec) error {
	hasTable := db.HasTable("card_assets")
	for name, props := range spec.Elements {
		for _, prop := range assetProps {
			v, ok := props[prop]
			if !ok {
				continue
			}
			id := uint(v.(float64))
			if !hasTable {
				return &SpecError{Element: name, Prop: prop, Msg: "the asset store is not available"}
			}
			var n int
			if err := db.Table("card_assets").Where("id = ? AND deleted_at IS NULL", id).Count(&n).Error; err != nil {
				return &SpecError{Element: name, Prop: prop, Msg: "could not check the asset: " + err.Error()}
			}
			if n == 0 {
				return &SpecError{Element: name, Prop: prop, Msg: fmt.Sprintf("asset %d does not exist", id)}
			}
		}
	}
	return nil
}

// MustSpecJSON marshals a spec for seeding; it panics on a spec that would
// not validate, which is a programming error in the seed.
func MustSpecJSON(spec TemplateSpec) json.RawMessage {
	raw, err := json.Marshal(spec)
	if err != nil {
		panic(err)
	}
	if _, err := ParseTemplateSpec(raw, nil); err != nil {
		panic(err)
	}
	return raw
}
