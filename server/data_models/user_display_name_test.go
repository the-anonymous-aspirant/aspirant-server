package data_models

import (
	"testing"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

func newDisplayNameTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	db.AutoMigrate(&Role{}, &User{}, &UserDisplayName{})
	return db
}

func seedUser(t *testing.T, db *gorm.DB, username string) User {
	t.Helper()
	u := User{Username: username, Email: username + "@example.com"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("failed to create user %q: %v", username, err)
	}
	return u
}

func openRowCount(db *gorm.DB, userID uint) int {
	var n int
	db.Model(&UserDisplayName{}).Where("user_id = ? AND valid_to IS NULL", userID).Count(&n)
	return n
}

// The AfterCreate hook must open exactly one display-name row per new user,
// defaulted to the login username, so every creation path is covered without
// per-caller edits.
func TestAfterCreateOpensDisplayName(t *testing.T) {
	db := newDisplayNameTestDB(t)
	defer db.Close()

	u := seedUser(t, db, "alice")

	if got := CurrentDisplayName(db, u.ID); got != "alice" {
		t.Errorf("CurrentDisplayName = %q, want %q", got, "alice")
	}
	if n := openRowCount(db, u.ID); n != 1 {
		t.Errorf("open display-name rows = %d, want 1", n)
	}
}

// Renaming must close the old row (preserving history) and open a new one, so
// the current name switches while the prior name survives with a non-null
// valid_to. Exactly one row stays open.
func TestSetDisplayNameKeepsHistory(t *testing.T) {
	db := newDisplayNameTestDB(t)
	defer db.Close()

	u := seedUser(t, db, "bob") // open row: display_name = "bob"

	if err := SetDisplayName(db, u.ID, "Bobby"); err != nil {
		t.Fatalf("SetDisplayName: %v", err)
	}

	if got := CurrentDisplayName(db, u.ID); got != "Bobby" {
		t.Errorf("after rename, CurrentDisplayName = %q, want %q", got, "Bobby")
	}
	if n := openRowCount(db, u.ID); n != 1 {
		t.Errorf("open rows after rename = %d, want exactly 1", n)
	}

	// The old "bob" row must survive as a closed historical record.
	var closed int
	db.Model(&UserDisplayName{}).
		Where("user_id = ? AND display_name = ? AND valid_to IS NOT NULL", u.ID, "bob").
		Count(&closed)
	if closed != 1 {
		t.Errorf("closed historical rows for %q = %d, want 1", "bob", closed)
	}

	// Total rows: one closed + one open.
	var total int
	db.Model(&UserDisplayName{}).Where("user_id = ?", u.ID).Count(&total)
	if total != 2 {
		t.Errorf("total display-name rows = %d, want 2 (history preserved)", total)
	}
}

// Renaming to the identical current name is a no-op — no new row, no churn.
func TestSetDisplayNameNoOpOnSameName(t *testing.T) {
	db := newDisplayNameTestDB(t)
	defer db.Close()

	u := seedUser(t, db, "carol")
	if err := SetDisplayName(db, u.ID, "carol"); err != nil {
		t.Fatalf("SetDisplayName: %v", err)
	}

	var total int
	db.Model(&UserDisplayName{}).Where("user_id = ?", u.ID).Count(&total)
	if total != 1 {
		t.Errorf("total rows after no-op rename = %d, want 1", total)
	}
}

// The batch resolver must return each user's current display name and fall back
// to the login username for any user missing an open row (defensive path).
func TestCurrentDisplayNamesBatchAndFallback(t *testing.T) {
	db := newDisplayNameTestDB(t)
	defer db.Close()

	renamed := seedUser(t, db, "dave")
	if err := SetDisplayName(db, renamed.ID, "DaveTheGreat"); err != nil {
		t.Fatalf("SetDisplayName: %v", err)
	}

	legacy := seedUser(t, db, "erin")
	// Simulate a legacy user with no open display row (created before the table).
	db.Where("user_id = ?", legacy.ID).Delete(&UserDisplayName{})

	names := CurrentDisplayNames(db, []uint{renamed.ID, legacy.ID})
	if names[renamed.ID] != "DaveTheGreat" {
		t.Errorf("batch name for renamed user = %q, want %q", names[renamed.ID], "DaveTheGreat")
	}
	if names[legacy.ID] != "erin" {
		t.Errorf("batch fallback for legacy user = %q, want login username %q", names[legacy.ID], "erin")
	}
}

// BackfillDisplayNames must open a row for a legacy user lacking one, and be
// idempotent on re-run (mirrors AutoMigrate being safe to re-run).
func TestBackfillDisplayNamesIdempotent(t *testing.T) {
	db := newDisplayNameTestDB(t)
	defer db.Close()

	u := seedUser(t, db, "frank")
	db.Where("user_id = ?", u.ID).Delete(&UserDisplayName{}) // legacy: no open row

	if err := BackfillDisplayNames(db); err != nil {
		t.Fatalf("BackfillDisplayNames: %v", err)
	}
	if got := CurrentDisplayName(db, u.ID); got != "frank" {
		t.Errorf("after backfill, CurrentDisplayName = %q, want %q", got, "frank")
	}
	if n := openRowCount(db, u.ID); n != 1 {
		t.Errorf("open rows after backfill = %d, want 1", n)
	}

	// Second run must not duplicate.
	if err := BackfillDisplayNames(db); err != nil {
		t.Fatalf("BackfillDisplayNames (2nd run): %v", err)
	}
	var total int
	db.Model(&UserDisplayName{}).Where("user_id = ?", u.ID).Count(&total)
	if total != 1 {
		t.Errorf("rows after second backfill = %d, want 1 (idempotent)", total)
	}
}
