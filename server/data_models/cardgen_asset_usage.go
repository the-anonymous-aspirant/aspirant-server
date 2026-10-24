package data_models

import (
	"sort"

	"github.com/jinzhu/gorm"
)

// Asset usage (system_3 #8634, subtask #8647): which templates and cards
// reference an asset, read from the CURRENT version of every live row. The
// viewer shows it, and a referenced asset cannot be deleted.

// AssetUser is one template or card that references an asset, with the
// elements that do.
type AssetUser struct {
	ID        uint     `json:"id"`
	Slug      string   `json:"slug"`
	Name      string   `json:"name"`
	VersionNo int      `json:"version_no"`
	Elements  []string `json:"elements"`
}

// AssetUsageReport lists every current reference to one asset.
type AssetUsageReport struct {
	Templates []AssetUser `json:"templates"`
	Cards     []AssetUser `json:"cards"`
}

// InUse reports whether anything references the asset.
func (r *AssetUsageReport) InUse() bool { return len(r.Templates) > 0 || len(r.Cards) > 0 }

// referencingElements names the elements whose asset_id or font_asset_id is
// id, sorted.
func referencingElements(elements map[string]map[string]any, id uint) []string {
	var out []string
	for name, props := range elements {
		for _, prop := range assetProps {
			if v, ok := props[prop].(float64); ok && uint(v) == id {
				out = append(out, name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// AssetUsage walks the current version of every live template and card. One
// pass in Go rather than a jsonb containment query, so the sqlite-backed
// tests cover it and the shape stays database-neutral; the deck is small and
// the viewer asks once per open.
func AssetUsage(db *gorm.DB, assetID uint) (*AssetUsageReport, error) {
	report := &AssetUsageReport{Templates: []AssetUser{}, Cards: []AssetUser{}}

	var templates []CardTemplate
	if err := db.Where("current_version_id IS NOT NULL").Order("id ASC").Find(&templates).Error; err != nil {
		return nil, err
	}
	for i := range templates {
		v, err := CurrentTemplateVersion(db, &templates[i])
		if err != nil {
			return nil, err
		}
		spec, err := ParseTemplateSpec(v.Spec, nil)
		if err != nil {
			return nil, err
		}
		if names := referencingElements(spec.Elements, assetID); len(names) > 0 {
			report.Templates = append(report.Templates, AssetUser{ID: templates[i].ID, Slug: templates[i].Slug, Name: templates[i].Name, VersionNo: v.VersionNo, Elements: names})
		}
	}

	if db.HasTable("cards") {
		var cards []Card
		if err := db.Where("current_version_id IS NOT NULL").Order("id ASC").Find(&cards).Error; err != nil {
			return nil, err
		}
		for i := range cards {
			v, err := CurrentCardVersion(db, &cards[i])
			if err != nil {
				return nil, err
			}
			data, err := ParseCardData(v.Data, nil)
			if err != nil {
				return nil, err
			}
			if names := referencingElements(data.Values, assetID); len(names) > 0 {
				report.Cards = append(report.Cards, AssetUser{ID: cards[i].ID, Slug: cards[i].Slug, Name: cards[i].Name, VersionNo: v.VersionNo, Elements: names})
			}
		}
	}
	return report, nil
}
