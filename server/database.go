// Package server provides the server-side functionality for the application,
// including database setup and management.
//
// This file contains functions to set up a connection to a PostgreSQL database
// using GORM and to automatically migrate database schemas.

// SetupDBConnection initializes a connection to the PostgreSQL database using
// credentials and connection details specified in a .env file. It returns
// a pointer to the gorm.DB instance and an error if any occurs during the
// connection setup.
//
// AutoMigrate performs automatic migration of the database schema for the
// specified data model, ensuring that the database schema is up-to-date with
// the application's data models.
package server

import (
	"aspirant-online/server/data_models"
	"fmt"
	"log"
	"os"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/postgres"
	"github.com/joho/godotenv"
)

var db *gorm.DB

func SetupDBConnection() (*gorm.DB, error) {
	// Try to load .env file, but don't fail if it doesn't exist (for CI/CD)
	err := godotenv.Load(".env")
	if err != nil {
		log.Println("Warning: .env file not found, using environment variables")
	}

	dbUser := os.Getenv("DB_USER")
	dbPassword := os.Getenv("DB_PASSWORD")
	dbName := os.Getenv("DB_NAME")
	dbHost := os.Getenv("DB_HOST")
	dbPort := os.Getenv("DB_PORT")
	if dbPort == "" {
		dbPort = "5432"
	}

	connectionString := fmt.Sprintf("host=%s port=%s user=%s dbname=%s password=%s sslmode=disable", dbHost, dbPort, dbUser, dbName, dbPassword)

	db, err = gorm.Open("postgres", connectionString)
	if err != nil {
		log.Printf("Error opening database: %v", err)
		return nil, err
	}

	err = db.DB().Ping()
	if err != nil {
		log.Printf("Error connecting to database: %v", err)
		return nil, err
	}

	return db, nil
}

// migrate runs GORM's AutoMigrate for the given models and surfaces its
// error. Call sites used to invoke db.AutoMigrate(...) directly and drop the
// *gorm.DB it returns, which silently dropped the .Error it carries — so a
// migration failure never stopped AutoMigrate, and a migrate-only boot
// (#10688) exited 0 on a stale schema.
func migrate(db *gorm.DB, models ...interface{}) error {
	return db.AutoMigrate(models...).Error
}

// exec runs a raw statement and surfaces its error, for the same reason
// migrate does.
func exec(db *gorm.DB, sql string, args ...interface{}) error {
	return db.Exec(sql, args...).Error
}

func AutoMigrate(db *gorm.DB) error {
	// Step 1: Migrate roles table first, rename legacy Family role, seed the
	// four access tiers, then remap every user off the legacy six-role
	// vocabulary onto the tiers (system_3 epic #5113 subtask A2). Idempotent:
	// after the first run no user references a legacy role and the legacy rows
	// are gone, so every statement below matches nothing on re-run.
	if err := migrate(db, &data_models.Role{}); err != nil {
		return fmt.Errorf("migrating roles: %w", err)
	}
	if err := exec(db, "UPDATE roles SET role_name = 'Trusted', role_description = 'User with trusted access' WHERE role_name = 'Family'"); err != nil {
		return fmt.Errorf("renaming legacy Family role: %w", err)
	}
	if err := data_models.SeedRoles(db); err != nil {
		return fmt.Errorf("seeding roles: %w", err)
	}

	// Step 2: Check if the legacy access_role column still exists on users.
	// This also decides whether the `users` table exists at all: on a brand
	// new database neither it nor the column exist, colCount reads 0, and the
	// `else` branch's MigrateEmailVerified call is what creates `users` for
	// the first time. The modern role_id remap just below depends on `users`
	// already existing, so this check — and the table-creating AutoMigrate it
	// guards — must run before that remap, not after it (#10688: the remap
	// used to run first and silently fail off a dropped .Error on a fresh
	// database, before this fix made AutoMigrate return that error).
	var colCount int
	if err := db.Raw(`SELECT COUNT(*) FROM information_schema.columns
		WHERE table_name = 'users' AND column_name = 'access_role'`).Row().Scan(&colCount); err != nil {
		return fmt.Errorf("checking for legacy access_role column: %w", err)
	}

	if colCount > 0 {
		// Legacy column exists — backfill role_id then drop it
		log.Println("Migrating users.access_role → users.role_id...")

		// Remap the legacy access_role strings straight onto the tier names so
		// the name-match backfill below lands on a tier role (the legacy role
		// rows are deleted after Step 2). Family is the pre-Trusted legacy name.
		if err := exec(db, `UPDATE users SET access_role = CASE
				WHEN access_role IN ('Family', 'Trusted') THEN 'Member'
				WHEN access_role IN ('User', 'Guest', 'Gamer') THEN 'Viewer'
				WHEN access_role = 'Deleted' THEN 'Blocked'
				ELSE access_role END`); err != nil {
			return fmt.Errorf("remapping legacy access_role strings: %w", err)
		}

		// AutoMigrate User so GORM adds the new role_id column. Routed through
		// MigrateEmailVerified so the one-time email-verification stamp is
		// applied here too, under its own column-existence guard.
		if err := data_models.MigrateEmailVerified(db); err != nil {
			log.Printf("Warning: user migrate failed: %v", err)
		}

		// Backfill role_id from the matching role name
		if err := exec(db, `UPDATE users SET role_id = roles.id
			FROM roles WHERE users.access_role = roles.role_name`); err != nil {
			return fmt.Errorf("backfilling role_id from access_role: %w", err)
		}

		// Default any unmatched rows to the "Viewer" tier (least privilege).
		if err := exec(db, `UPDATE users SET role_id = (SELECT id FROM roles WHERE role_name = 'Viewer')
			WHERE role_id IS NULL OR role_id = 0`); err != nil {
			return fmt.Errorf("defaulting unmatched users to Viewer: %w", err)
		}

		// Drop the legacy column
		if err := exec(db, "ALTER TABLE users DROP COLUMN access_role"); err != nil {
			return fmt.Errorf("dropping legacy access_role column: %w", err)
		}
		log.Println("Migration complete: access_role column dropped")
	} else {
		// Column already gone — normal migrate. On a brand new database this
		// is also where the `users` table is created for the first time.
		if err := data_models.MigrateEmailVerified(db); err != nil {
			log.Printf("Warning: user migrate failed: %v", err)
		}
	}

	// Remap user FKs to the tier roles (access-preserving, per the #5113-A1
	// inventory). Admin keeps its name; the joins below match only rows still
	// pointing at a legacy role. `users` is guaranteed to exist by now (via
	// one of the two MigrateEmailVerified calls above). Done before the
	// legacy rows are deleted.
	if err := exec(db, `UPDATE users SET role_id = (SELECT id FROM roles WHERE role_name = 'Member')
		FROM roles r WHERE users.role_id = r.id AND r.role_name = 'Trusted'`); err != nil {
		return fmt.Errorf("remapping Trusted users to Member: %w", err)
	}
	if err := exec(db, `UPDATE users SET role_id = (SELECT id FROM roles WHERE role_name = 'Viewer')
		FROM roles r WHERE users.role_id = r.id AND r.role_name IN ('User', 'Guest', 'Gamer')`); err != nil {
		return fmt.Errorf("remapping legacy users to Viewer: %w", err)
	}
	if err := exec(db, `UPDATE users SET role_id = (SELECT id FROM roles WHERE role_name = 'Blocked')
		FROM roles r WHERE users.role_id = r.id AND r.role_name = 'Deleted'`); err != nil {
		return fmt.Errorf("remapping Deleted users to Blocked: %w", err)
	}

	// Every user now points at a tier role (via the role_id remap just above on
	// a modern DB, or the access_role backfill above on a legacy one). Delete
	// the legacy role rows. Idempotent: matches nothing once they are gone.
	if err := exec(db, "DELETE FROM roles WHERE role_name IN ('Trusted', 'User', 'Guest', 'Gamer', 'Deleted')"); err != nil {
		return fmt.Errorf("deleting legacy role rows: %w", err)
	}

	// Step 2a: release a soft-deleted account's identifiers back into the
	// namespace. The constraint AutoMigrate cannot express: a PARTIAL unique
	// index on username and email, scoped to live rows because the model
	// soft-deletes. See EnsureUserIdentifierUniqueIndexes — it also drops the
	// unconditional users_username_key / users_email_key this replaces, which
	// the `unique` struct tag created on every database that predates it
	// (task #7009, deciding #6783).
	if err := data_models.EnsureUserIdentifierUniqueIndexes(db); err != nil {
		log.Printf("users identifier unique indexes: %v", err)
	}

	// The email-verification stamp for pre-existing accounts is applied inside
	// MigrateEmailVerified above, guarded on the column having been absent
	// before this boot. It is NOT an unconditional statement here: an
	// every-boot UPDATE cannot tell a pre-existing admin account from a pending
	// sign-up — both have email_verified_at NULL — so it would mark every
	// unverified sign-up verified at the next restart and disable the
	// verification gate on a timer (security finding #5226).

	// Step 2c: Session-revocation watermark (epic #5113, subtask #5224). A
	// nullable column with no backfill — NULL means "never revoked", which is
	// correct for every account that predates it, so unlike the
	// email-verification stamp this migration carries no lockout risk.
	// AutoMigrate on User (Step 2 above) adds the column; this comment records
	// where it comes from.

	// Step 2b: Temporal display-name table (security-finding #3094). The login
	// username must not double as a public display identity, so a separate
	// history-carrying table holds display names; the login users.username
	// column is left untouched. Backfill one open row per existing user so
	// names are visually unchanged on deploy (idempotent — re-running is safe).
	if err := migrate(db, &data_models.UserDisplayName{}); err != nil {
		return fmt.Errorf("migrating user_display_names: %w", err)
	}
	if err := data_models.BackfillDisplayNames(db); err != nil {
		log.Printf("Warning: display-name backfill failed: %v", err)
	}

	// Step 3: Migrate remaining tables
	if err := migrate(db,
		&data_models.Message{},
		&data_models.LuddeFeedingTime{},
		&data_models.GameScore{},
		&data_models.EasterHuntGame{},
		&data_models.EasterHuntClick{},
		&data_models.EasterHuntScore{},
		&data_models.EasterHuntEgg{},
		&data_models.EasterHuntEggCell{},
	); err != nil {
		return fmt.Errorf("migrating step 3 tables: %w", err)
	}

	// Step 4: Goal Mapper tables
	if err := migrate(db,
		&data_models.GoalTree{},
		&data_models.GoalNode{},
		&data_models.GoalEdge{},
		&data_models.GoalComment{},
	); err != nil {
		return fmt.Errorf("migrating goal mapper tables: %w", err)
	}

	// Step 5: Pappas armhävningar challenge
	if err := migrate(db, &data_models.PushupEntry{}, &data_models.PushupMilestone{}); err != nil {
		return fmt.Errorf("migrating pushup tables: %w", err)
	}
	data_models.SeedPushupMilestones(db)

	// Step 5a: Pappas sit-ups challenge (#7992)
	if err := migrate(db, &data_models.SitupEntry{}); err != nil {
		return fmt.Errorf("migrating situp_entries: %w", err)
	}

	// Step 5a2: Invoicing app (#8367) — issuer profile, clients, audit trail,
	// imported payments, invoices with their line items and payment links.
	// invoicing_* tables, nothing shared with the retired finance service.
	if err := migrate(db,
		&data_models.InvoicingProfile{},
		&data_models.InvoicingClient{},
		&data_models.InvoicingAuditEvent{},
		&data_models.InvoicingPayment{},
		&data_models.InvoicingExpense{},         // #10414-A: hand-entered work expenses
		&data_models.InvoicingExpenseCategory{}, // #10414-A/#10424: the owner's expense categories
		&data_models.InvoicingExpenseReceipt{},  // #10537-A1: a receipt attached to a work expense
		&data_models.InvoicingInvoice{},
		&data_models.InvoicingLineItem{},
		&data_models.InvoicingInvoicePayment{},
		&data_models.InvoicingPaymentTombstone{},  // #10009: deleted payments stay out of re-imports
		&data_models.InvoicingExpenseTombstone{},  // #10555-A2: expenses deleted for good leave a tombstone too
		&data_models.InvoicingBookSelection{},     // #10555-A3: what the Book counts, and her presets
		&data_models.InvoicingBookPreset{},
		&data_models.InvoicingInvoicePDF{},
		&data_models.InvoicingProfileLogo{}, // #9858: the issuer's logo
	); err != nil {
		return fmt.Errorf("migrating invoicing tables: %w", err)
	}
	// #10054: a book per user. The tags above create the per-owner unique
	// indexes; this drops the shared-book ones AutoMigrate leaves behind.
	if err := data_models.EnsureInvoicingOwnerIndexes(db); err != nil {
		log.Printf("invoicing owner indexes: %v", err)
	}
	if err := data_models.EnsureInvoicingOwnerLockTable(db); err != nil {
		log.Printf("invoicing owner lock table: %v", err)
	}

	// Step 5b: Bootstrap marker (#5264). One row, fixed primary key, recording
	// that this deployment has been set up — so public admin creation can never
	// reopen because the live user count happened to reach zero. No seed: an
	// existing deployment has users, which ClaimBootstrap counts with
	// Unscoped() and refuses on, so it stays closed without a marker row.
	if err := migrate(db, &data_models.BootstrapRecord{}); err != nil {
		return fmt.Errorf("migrating bootstrap_records: %w", err)
	}

	// Step 5c: Site-wide policy switches (#5289) — today just the sign-up
	// kill-switch. No seed: an absent row reads as the setting's default, so a
	// fresh database and a configured one behave the same until an admin
	// actually flips something.
	if err := migrate(db, &data_models.SiteSetting{}); err != nil {
		return fmt.Errorf("migrating site_settings: %w", err)
	}

	// Step 6: Per-user scratchpad (one text buffer per user)
	if err := migrate(db, &data_models.Scratchpad{}); err != nil {
		return fmt.Errorf("migrating scratchpads: %w", err)
	}

	// Step 6b: Single-use expiring user tokens backing email verification and
	// password recovery (epic #5113, subtask #5219). No seed — tokens are
	// minted at runtime only. One table serves both flows; the TTL and the
	// retire-previous-token behaviour that differ between them live in the
	// policy table in data_models/user_token.go, not in a second schema.
	if err := migrate(db, &data_models.UserToken{}); err != nil {
		return fmt.Errorf("migrating user_tokens: %w", err)
	}

	// Step 7: Constellations companion app — relationship-type vocabulary
	// (epic #4587, subtask #4594-A2). Colours live on the row, not in the
	// frontend; seed is idempotent.
	if err := migrate(db, &data_models.RelationshipType{}); err != nil {
		return fmt.Errorf("migrating relationship_types: %w", err)
	}
	data_models.SeedRelationshipTypes(db)

	// Step 8: Constellations companion app — per-user game identity default
	// (epic #4587, subtask #4595-A3). No seed; a profile is created on first set.
	if err := migrate(db, &data_models.ConstellationProfile{}); err != nil {
		return fmt.Errorf("migrating constellation_profiles: %w", err)
	}

	// Step 9: Constellations companion app — rooms & membership schema
	// (epic #4587, subtask #4593-A1). No seed; rooms are created at runtime.
	if err := migrate(db, &data_models.Room{}, &data_models.RoomMember{}); err != nil {
		return fmt.Errorf("migrating room tables: %w", err)
	}

	// Step 10: Constellations companion app — relationship-graph edges
	// (epic #4587, subtask #4596-B1). No seed; edges are created at runtime.
	if err := migrate(db, &data_models.Relationship{}); err != nil {
		return fmt.Errorf("migrating relationships: %w", err)
	}

	// Step 11: Constellations companion app — server-authoritative dice rolls
	// (epic #4587, subtask #4597-B2). No seed; a roll is created per roll.
	if err := migrate(db, &data_models.DiceRoll{}); err != nil {
		return fmt.Errorf("migrating dice_rolls: %w", err)
	}

	// Step 12: Constellations companion app — per-player relationship edit
	// history (epic #4587, subtask #4599-C1). No seed; an action is appended
	// per relationship edit for undo/redo.
	if err := migrate(db, &data_models.RelationshipAction{}); err != nil {
		return fmt.Errorf("migrating relationship_actions: %w", err)
	}

	// Step 13: Constellations companion app — goal-card victory-condition deck
	// (epic #4807, subtask #4807-A1). 16 cards over the six connection types;
	// seed is idempotent.
	if err := migrate(db, &data_models.GoalCard{}); err != nil {
		return fmt.Errorf("migrating goal_cards: %w", err)
	}
	data_models.SeedGoalCards(db)

	// Step 14: Constellations companion app — per-player selected goal
	// (epic #4807, subtask #4807-A1). No seed; a goal is chosen at runtime and
	// is private to the selecting player.
	if err := migrate(db, &data_models.PlayerGoal{}); err != nil {
		return fmt.Errorf("migrating player_goals: %w", err)
	}
	// The "one goal per player per room" constraint AutoMigrate cannot express:
	// a PARTIAL unique index, scoped to live rows because the model
	// soft-deletes. See EnsurePlayerGoalUniqueIndex — the struct tag that was
	// meant to carry this was GORM v2 syntax and silently produced a
	// NON-unique index plus two `syntax error at or near "unique"` lines on
	// every boot (task #5157).
	if err := data_models.EnsurePlayerGoalUniqueIndex(db); err != nil {
		log.Printf("player_goals unique index: %v", err)
	}

	// Step 15: Constellations companion app — general append-only
	// relationship-event log (epic #4807, subtask #4829-A3). Uncapped, ordered;
	// distinct from the #4599 per-player capped undo stack. No seed; an event is
	// appended per edge state change (set / clear / undo / redo).
	if err := migrate(db, &data_models.RelationshipEvent{}); err != nil {
		return fmt.Errorf("migrating relationship_events: %w", err)
	}

	// Step 16: Proxy-call latency log (system_3 #5953). One row per outbound
	// proxied upstream call (upstream keyed commander.valuation / commander.decide
	// / advisor / voice / browser / monitor / translator, the join vocabulary
	// agreed with #5931's ceiling registry), request_log-shaped so system_3
	// (#5931) can compute per-upstream p99 and its margin against the recorded
	// timeout ceiling.
	if err := migrate(db, &data_models.ProxyRequestLog{}); err != nil {
		return fmt.Errorf("migrating proxy_request_log: %w", err)
	}

	// Nightly per-service usage rollup (system_3 #10490-C / #10496-C1): one
	// row per served container or server-native feature table, written by
	// handlers.StartServiceUsageRollup.
	if err := migrate(db, &data_models.ServiceUsageRollup{}); err != nil {
		return fmt.Errorf("migrating service_usage_rollups: %w", err)
	}

	// Failed invoice-import uploads: a durable record plus a time-limited
	// copy of the file (system_3 #10536, security ruling c65222). Swept by
	// handlers.StartImportFailureSweep.
	if err := migrate(db, &data_models.InvoicingImportFailure{}); err != nil {
		return fmt.Errorf("migrating invoicing_import_failures: %w", err)
	}

	// Step 18: Constellations card editor, types and templates (system_3
	// #8634, subtask #8643). Versioned, append-only template specs with an
	// `extends` parent; the seed is insert-if-absent and never resets an
	// edited row (these are the operator's editable starting points).
	if err := migrate(db, &data_models.CardType{}, &data_models.CardTemplate{}, &data_models.CardTemplateVersion{}); err != nil {
		return fmt.Errorf("migrating cardgen template tables: %w", err)
	}
	data_models.SeedCardgenTemplates(db)
	// Step 17: Constellations card editor, asset store (system_3 #8634,
	// subtask #8642). Content-addressed rows (sha256) pointing at
	// LocalStorage keys cardgen/<sha256>; no seed. Later subtasks add the
	// template, card and export-profile tables under the same app.
	if err := migrate(db, &data_models.CardAsset{}); err != nil {
		return fmt.Errorf("migrating card_assets: %w", err)
	}
	// Step 19: Constellations card editor, export profiles (system_3 #8634,
	// subtask #8644): the print configuration (card size, bleed, DPI, crop
	// marks, sheet grid, backs). Three presets are seeded once; a deleted or
	// edited preset is left alone on re-run.
	if err := migrate(db, &data_models.CardExportProfile{}); err != nil {
		return fmt.Errorf("migrating card_export_profiles: %w", err)
	}
	data_models.SeedCardExportProfiles(db)

	// Step 20: Constellations card editor, cards and card versions (system_3
	// #8634, subtask #8646). The bootstrap creates the 16 relationship cards
	// from goal_cards and the 6 connection cards from relationship_types once,
	// keyed by slug, and never resets them; it reads those two tables only.
	if err := migrate(db, &data_models.Card{}, &data_models.CardVersion{}); err != nil {
		return fmt.Errorf("migrating cardgen card tables: %w", err)
	}
	data_models.BootstrapCardgenCards(db)

	// Clean up legacy table
	if err := exec(db, "DROP TABLE IF EXISTS word_weaver_scores"); err != nil {
		return fmt.Errorf("dropping legacy word_weaver_scores table: %w", err)
	}

	return nil
}
