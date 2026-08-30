package data_models

import (
	"testing"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

func newRelationshipTypeTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	db.AutoMigrate(&RelationshipType{})
	return db
}

// SeedRelationshipTypes must insert exactly the six rulebook connection types,
// ordered, each with a stored colour.
func TestSeedRelationshipTypesInsertsSix(t *testing.T) {
	db := newRelationshipTypeTestDB(t)
	defer db.Close()

	SeedRelationshipTypes(db)

	types, err := GetRelationshipTypes(db)
	if err != nil {
		t.Fatalf("GetRelationshipTypes: %v", err)
	}
	if len(types) != 6 {
		t.Fatalf("got %d types, want 6", len(types))
	}

	wantCodes := []string{"P", "D", "F+", "F", "A", "R"}
	for i, want := range wantCodes {
		if types[i].Code != want {
			t.Errorf("types[%d].Code = %q, want %q (order must follow display_order)", i, types[i].Code, want)
		}
		if types[i].Colour == "" {
			t.Errorf("types[%d] (%s) has empty colour; colour must be stored, not a frontend constant", i, types[i].Code)
		}
		if types[i].Label == "" {
			t.Errorf("types[%d] (%s) has empty label", i, types[i].Code)
		}
	}
}

// Re-running the seed must not duplicate rows and must sync mutable fields.
func TestSeedRelationshipTypesIdempotent(t *testing.T) {
	db := newRelationshipTypeTestDB(t)
	defer db.Close()

	SeedRelationshipTypes(db)
	SeedRelationshipTypes(db)

	var n int
	db.Model(&RelationshipType{}).Count(&n)
	if n != 6 {
		t.Fatalf("after re-seed got %d rows, want 6 (seed must be idempotent)", n)
	}

	// A drifted colour is re-synced on the next seed.
	db.Model(&RelationshipType{}).Where("code = ?", "P").Update("colour", "#000000")
	SeedRelationshipTypes(db)
	var p RelationshipType
	db.Where("code = ?", "P").First(&p)
	if p.Colour == "#000000" {
		t.Errorf("colour not re-synced on re-seed: still %q", p.Colour)
	}
}
