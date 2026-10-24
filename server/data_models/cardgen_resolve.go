package data_models

import (
	"errors"
	"sort"

	"github.com/jinzhu/gorm"
)

// The inheritance resolver (system_3 #8634, subtask #8643). One implementation,
// in Go: the client never merges template chains, it renders what this returns.

var (
	// ErrTemplateCycle is returned when the extends chain loops.
	ErrTemplateCycle = errors.New("template chain contains a cycle")
	// ErrTemplateChainTooDeep bounds the chain walk.
	ErrTemplateChainTooDeep = errors.New("template chain is deeper than the limit")
	// ErrTemplateMissing is returned when a chain names a template that does not exist.
	ErrTemplateMissing = errors.New("template in the chain does not exist")
)

// MaxTemplateChainDepth bounds how many templates an extends chain may hold.
const MaxTemplateChainDepth = 16

// ResolvedElement is one element after the chain merge: its effective props
// and, per prop, the slug of the template that set it.
type ResolvedElement struct {
	Name       string            `json:"name"`
	Props      map[string]any    `json:"props"`
	Provenance map[string]string `json:"provenance"`
}

// ResolvedTemplate is the flat output of a chain merge.
type ResolvedTemplate struct {
	Chain          []string          `json:"chain"`
	Size           CardSize          `json:"size"`
	SizeProvenance map[string]string `json:"size_provenance"`
	Elements       []ResolvedElement `json:"elements"`
}

// TemplateChain returns the templates from the root to the one with id, each
// live. It refuses a cycle, a missing parent and a chain over the depth limit.
func TemplateChain(db *gorm.DB, id uint) ([]CardTemplate, error) {
	var chain []CardTemplate
	seen := map[uint]bool{}
	next := &id
	for next != nil {
		if seen[*next] {
			return nil, ErrTemplateCycle
		}
		if len(chain) >= MaxTemplateChainDepth {
			return nil, ErrTemplateChainTooDeep
		}
		seen[*next] = true
		var t CardTemplate
		err := db.Where("id = ?", *next).First(&t).Error
		if err == gorm.ErrRecordNotFound {
			return nil, ErrTemplateMissing
		}
		if err != nil {
			return nil, err
		}
		chain = append(chain, t)
		next = t.ParentTemplateID
	}
	// Reverse: root first.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, nil
}

// WouldCycle reports whether setting parent on the template with id would
// create a cycle (the parent's own chain already contains id).
func WouldCycle(db *gorm.DB, id uint, parent *uint) (bool, error) {
	if parent == nil {
		return false, nil
	}
	if *parent == id {
		return true, nil
	}
	chain, err := TemplateChain(db, *parent)
	if err != nil {
		return false, err
	}
	for _, t := range chain {
		if t.ID == id {
			return true, nil
		}
	}
	return false, nil
}

// ResolveTemplate merges the chain of the template with id, root to leaf.
func ResolveTemplate(db *gorm.DB, id uint) (*ResolvedTemplate, error) {
	chain, err := TemplateChain(db, id)
	if err != nil {
		return nil, err
	}
	type layer struct {
		slug string
		spec *TemplateSpec
	}
	layers := make([]layer, 0, len(chain))
	for i := range chain {
		v, err := CurrentTemplateVersion(db, &chain[i])
		if err != nil {
			return nil, err
		}
		spec, err := ParseTemplateSpec(v.Spec, nil)
		if err != nil {
			return nil, err
		}
		layers = append(layers, layer{chain[i].Slug, spec})
	}
	res := &ResolvedTemplate{SizeProvenance: map[string]string{}}
	elements := map[string]*ResolvedElement{}
	for _, l := range layers {
		res.Chain = append(res.Chain, l.slug)
		MergeSpecLayer(res, elements, l.slug, l.spec)
	}
	res.Elements = sortedElements(elements)
	return res, nil
}

// MergeSpecLayer overlays one spec on the accumulated result, recording slug
// as the provenance of every prop it sets. Exported so B1's card overlay can
// reuse it with slug "card".
func MergeSpecLayer(res *ResolvedTemplate, elements map[string]*ResolvedElement, slug string, spec *TemplateSpec) {
	if spec.Size != nil {
		if spec.Size.WMM > 0 {
			res.Size.WMM = spec.Size.WMM
			res.Size.HMM = spec.Size.HMM
			res.SizeProvenance["w_mm"] = slug
			res.SizeProvenance["h_mm"] = slug
		}
		if spec.Size.RadiusMM > 0 || spec.Size.WMM > 0 {
			res.Size.RadiusMM = spec.Size.RadiusMM
			res.SizeProvenance["radius_mm"] = slug
		}
	}
	for name, props := range spec.Elements {
		el, ok := elements[name]
		if !ok {
			el = &ResolvedElement{Name: name, Props: map[string]any{}, Provenance: map[string]string{}}
			elements[name] = el
		}
		for prop, v := range props {
			el.Props[prop] = v
			el.Provenance[prop] = slug
		}
	}
}

// sortedElements orders by z (missing z = 0) then name, so the client draws
// them bottom to top without sorting again.
func sortedElements(elements map[string]*ResolvedElement) []ResolvedElement {
	out := make([]ResolvedElement, 0, len(elements))
	for _, el := range elements {
		out = append(out, *el)
	}
	z := func(el ResolvedElement) float64 {
		if v, ok := el.Props["z"].(float64); ok {
			return v
		}
		return 0
	}
	sort.Slice(out, func(i, j int) bool {
		if z(out[i]) != z(out[j]) {
			return z(out[i]) < z(out[j])
		}
		return out[i].Name < out[j].Name
	})
	return out
}
