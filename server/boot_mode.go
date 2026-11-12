package server

import (
	"fmt"
	"os"
)

// SERVER_MODE=migrate selects a boot that runs AutoMigrate and exits instead
// of serving (#10688, following #9324's decision to run the migration role's
// AutoMigrate as a one-shot compose step ahead of the runtime-role server).
const serverModeMigrate = "migrate"

// SKIP_AUTOMIGRATE=1 lets the serve-mode boot skip AutoMigrate, for the
// cutover where a separate migrate-only step already ran it as the migration
// role and the runtime role lacks the DDL privileges to run it again. Any
// other value, including unset, keeps today's default: AutoMigrate runs at
// every serve-mode boot.
const skipAutoMigrateEnv = "SKIP_AUTOMIGRATE"

// IsMigrateOnlyMode reports whether this boot should run AutoMigrate and
// exit rather than serve traffic.
func IsMigrateOnlyMode() bool {
	return os.Getenv("SERVER_MODE") == serverModeMigrate
}

// ShouldSkipAutoMigrate reports whether a serve-mode boot should skip
// AutoMigrate.
func ShouldSkipAutoMigrate() bool {
	return os.Getenv(skipAutoMigrateEnv) == "1"
}

// RunMigrateOnly connects to the database, runs AutoMigrate, and closes the
// connection. It returns a non-nil error on any connection or migration
// failure, so the migrate-only boot can exit non-zero instead of leaving the
// schema stale while reporting success.
func RunMigrateOnly() error {
	db, err := SetupDBConnection()
	if err != nil {
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer db.Close()

	if err := AutoMigrate(db); err != nil {
		return fmt.Errorf("running migrations: %w", err)
	}
	return nil
}
