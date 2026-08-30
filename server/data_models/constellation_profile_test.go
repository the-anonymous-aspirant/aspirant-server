package data_models

import (
	"testing"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

func newConstellationProfileTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	db.AutoMigrate(&ConstellationProfile{})
	return db
}

// First Set creates a row; a second Set updates the same row in place — never a
// second row — and Get returns the latest name.
func TestSetConstellationUsernameCreatesThenUpdates(t *testing.T) {
	db := newConstellationProfileTestDB(t)
	defer db.Close()

	if _, err := SetConstellationUsername(db, 7, "supernova"); err != nil {
		t.Fatalf("first Set: %v", err)
	}
	if _, err := SetConstellationUsername(db, 7, "SUPERNOVA"); err != nil {
		t.Fatalf("second Set: %v", err)
	}

	var n int
	db.Model(&ConstellationProfile{}).Where("user_id = ?", 7).Count(&n)
	if n != 1 {
		t.Fatalf("got %d rows for user 7, want 1 (Set must update in place)", n)
	}

	p, ok := GetConstellationProfile(db, 7)
	if !ok {
		t.Fatal("GetConstellationProfile: profile missing after Set")
	}
	if p.GameUsername != "SUPERNOVA" {
		t.Errorf("GameUsername = %q, want %q (latest Set wins)", p.GameUsername, "SUPERNOVA")
	}
}

// Get for a user with no profile is a normal state: ok=false, no error, no panic.
func TestGetConstellationProfileEmptyWhenUnset(t *testing.T) {
	db := newConstellationProfileTestDB(t)
	defer db.Close()

	if _, ok := GetConstellationProfile(db, 99); ok {
		t.Error("GetConstellationProfile reported a profile for a user that never set one")
	}
}

// Distinct users get distinct rows; one Set each leaves exactly two rows.
func TestConstellationProfileOneRowPerUser(t *testing.T) {
	db := newConstellationProfileTestDB(t)
	defer db.Close()

	SetConstellationUsername(db, 1, "jim")
	SetConstellationUsername(db, 2, "sally")
	SetConstellationUsername(db, 1, "jim-again")

	var total int
	db.Model(&ConstellationProfile{}).Count(&total)
	if total != 2 {
		t.Fatalf("got %d total rows, want 2 (one per user)", total)
	}
	if p, _ := GetConstellationProfile(db, 1); p.GameUsername != "jim-again" {
		t.Errorf("user 1 GameUsername = %q, want %q", p.GameUsername, "jim-again")
	}
	if p, _ := GetConstellationProfile(db, 2); p.GameUsername != "sally" {
		t.Errorf("user 2 GameUsername = %q, want %q", p.GameUsername, "sally")
	}
}
