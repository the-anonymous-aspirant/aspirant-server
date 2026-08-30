package data_models

import "github.com/jinzhu/gorm"

// RelationshipType is the enumerated Constellations connection vocabulary
// (rulebook §3: P, D, F+, F, A, R). Colour is stored on the row so the
// frontend renders each term bold in its own colour rather than hard-coding it
// (epic #4587, subtask #4594-A2). The exact hex values are data and may be
// tuned; final contrast validation against the rendered dark room surface is
// the graph-render child's job (§3.100 / §3.60 — measure the rendered pixel).
type RelationshipType struct {
	gorm.Model
	Code         string `json:"code" gorm:"type:varchar(4);unique;not null"`
	Label        string `json:"label" gorm:"type:varchar(40);not null"`
	Colour       string `json:"colour" gorm:"type:varchar(9);not null"`
	DisplayOrder int    `json:"display_order" gorm:"not null;index"`
}

// relationshipTypeSeed is the source of truth for the six connection types.
// Colours are distinct hues chosen to read on a dark surface.
var relationshipTypeSeed = []RelationshipType{
	{Code: "P", Label: "Partner", Colour: "#FF6B6B", DisplayOrder: 1},
	{Code: "D", Label: "Date", Colour: "#FFA94D", DisplayOrder: 2},
	{Code: "F+", Label: "Friends with benefits", Colour: "#C792EA", DisplayOrder: 3},
	{Code: "F", Label: "Friend", Colour: "#6BCB77", DisplayOrder: 4},
	{Code: "A", Label: "Affair", Colour: "#4D96FF", DisplayOrder: 5},
	{Code: "R", Label: "Rejection", Colour: "#ADB5BD", DisplayOrder: 6},
}

// SeedRelationshipTypes inserts the six connection types idempotently, keyed on
// Code, and keeps Label/Colour/DisplayOrder in sync on re-run. Mirrors
// SeedPushupMilestones so re-running AutoMigrate is safe.
func SeedRelationshipTypes(db *gorm.DB) {
	for _, rt := range relationshipTypeSeed {
		var existing RelationshipType
		err := db.Where("code = ?", rt.Code).First(&existing).Error
		if err == gorm.ErrRecordNotFound {
			db.Create(&rt)
			continue
		}
		db.Model(&existing).Updates(map[string]interface{}{
			"label":         rt.Label,
			"colour":        rt.Colour,
			"display_order": rt.DisplayOrder,
		})
	}
}

// GetRelationshipTypes returns the vocabulary ordered for display.
func GetRelationshipTypes(db *gorm.DB) ([]RelationshipType, error) {
	var types []RelationshipType
	err := db.Order("display_order ASC").Find(&types).Error
	return types, err
}
