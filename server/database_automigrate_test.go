package server

import (
	"testing"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

// TestAutoMigrateSurfacesErrors locks down #10688's fix: AutoMigrate used to
// drop every db.AutoMigrate/db.Exec call's error, so a migrate-only boot
// (SERVER_MODE=migrate) exited 0 on a stale schema even when a migration
// step failed. AutoMigrate now returns the first error it hits — exercised
// here by closing the connection before the very first statement, so the
// migrate() helper's db.AutoMigrate(&data_models.Role{}) fails immediately.
func TestAutoMigrateSurfacesErrors(t *testing.T) {
	testDB, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	if err := testDB.Close(); err != nil {
		t.Fatalf("failed to close test db: %v", err)
	}

	if err := AutoMigrate(testDB); err == nil {
		t.Fatal("AutoMigrate on a closed connection: got nil error, want non-nil")
	}
}
