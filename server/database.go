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

func AutoMigrate(db *gorm.DB) {
	// Step 1: Migrate roles table first, rename legacy Family role, seed the
	// four access tiers, then remap every user off the legacy six-role
	// vocabulary onto the tiers (system_3 epic #5113 subtask A2). Idempotent:
	// after the first run no user references a legacy role and the legacy rows
	// are gone, so every statement below matches nothing on re-run.
	db.AutoMigrate(&data_models.Role{})
	db.Exec("UPDATE roles SET role_name = 'Trusted', role_description = 'User with trusted access' WHERE role_name = 'Family'")
	data_models.SeedRoles(db)

	// Remap user FKs to the tier roles (access-preserving, per the #5113-A1
	// inventory). Admin keeps its name; the joins below match only rows still
	// pointing at a legacy role. Done before the legacy rows are deleted.
	db.Exec(`UPDATE users SET role_id = (SELECT id FROM roles WHERE role_name = 'Member')
		FROM roles r WHERE users.role_id = r.id AND r.role_name = 'Trusted'`)
	db.Exec(`UPDATE users SET role_id = (SELECT id FROM roles WHERE role_name = 'Viewer')
		FROM roles r WHERE users.role_id = r.id AND r.role_name IN ('User', 'Guest', 'Gamer')`)
	db.Exec(`UPDATE users SET role_id = (SELECT id FROM roles WHERE role_name = 'Blocked')
		FROM roles r WHERE users.role_id = r.id AND r.role_name = 'Deleted'`)
	// NB: the legacy role rows are NOT deleted here — the Step 2 legacy path
	// (a DB that still carries the access_role string column) name-matches
	// against them to backfill role_id. They are deleted after Step 2, once
	// both the modern role_id remap above and the legacy backfill below have
	// landed every user on a tier role.

	// Step 2: Check if the legacy access_role column still exists on users
	var colCount int
	db.Raw(`SELECT COUNT(*) FROM information_schema.columns
		WHERE table_name = 'users' AND column_name = 'access_role'`).Row().Scan(&colCount)

	if colCount > 0 {
		// Legacy column exists — backfill role_id then drop it
		log.Println("Migrating users.access_role → users.role_id...")

		// Remap the legacy access_role strings straight onto the tier names so
		// the name-match backfill below lands on a tier role (the legacy role
		// rows are deleted after Step 2). Family is the pre-Trusted legacy name.
		db.Exec(`UPDATE users SET access_role = CASE
				WHEN access_role IN ('Family', 'Trusted') THEN 'Member'
				WHEN access_role IN ('User', 'Guest', 'Gamer') THEN 'Viewer'
				WHEN access_role = 'Deleted' THEN 'Blocked'
				ELSE access_role END`)

		// AutoMigrate User so GORM adds the new role_id column
		db.AutoMigrate(&data_models.User{})

		// Backfill role_id from the matching role name
		db.Exec(`UPDATE users SET role_id = roles.id
			FROM roles WHERE users.access_role = roles.role_name`)

		// Default any unmatched rows to the "Viewer" tier (least privilege).
		db.Exec(`UPDATE users SET role_id = (SELECT id FROM roles WHERE role_name = 'Viewer')
			WHERE role_id IS NULL OR role_id = 0`)

		// Drop the legacy column
		db.Exec("ALTER TABLE users DROP COLUMN access_role")
		log.Println("Migration complete: access_role column dropped")
	} else {
		// Column already gone — normal migrate
		db.AutoMigrate(&data_models.User{})
	}

	// Every user now points at a tier role (via the Step 1 role_id remap on a
	// modern DB, or the Step 2 access_role backfill on a legacy one). Delete the
	// legacy role rows. Idempotent: matches nothing once they are gone.
	db.Exec("DELETE FROM roles WHERE role_name IN ('Trusted', 'User', 'Guest', 'Gamer', 'Deleted')")

	// Step 2b: Temporal display-name table (security-finding #3094). The login
	// username must not double as a public display identity, so a separate
	// history-carrying table holds display names; the login users.username
	// column is left untouched. Backfill one open row per existing user so
	// names are visually unchanged on deploy (idempotent — re-running is safe).
	db.AutoMigrate(&data_models.UserDisplayName{})
	if err := data_models.BackfillDisplayNames(db); err != nil {
		log.Printf("Warning: display-name backfill failed: %v", err)
	}

	// Step 3: Migrate remaining tables
	db.AutoMigrate(&data_models.Message{})
	db.AutoMigrate(&data_models.LuddeFeedingTime{})
	db.AutoMigrate(&data_models.GameScore{})
	db.AutoMigrate(&data_models.EasterHuntGame{})
	db.AutoMigrate(&data_models.EasterHuntClick{})
	db.AutoMigrate(&data_models.EasterHuntScore{})
	db.AutoMigrate(&data_models.EasterHuntEgg{})
	db.AutoMigrate(&data_models.EasterHuntEggCell{})

	// Step 4: Goal Mapper tables
	db.AutoMigrate(&data_models.GoalTree{})
	db.AutoMigrate(&data_models.GoalNode{})
	db.AutoMigrate(&data_models.GoalEdge{})
	db.AutoMigrate(&data_models.GoalComment{})

	// Step 5: Pappas armhävningar challenge
	db.AutoMigrate(&data_models.PushupEntry{})
	db.AutoMigrate(&data_models.PushupMilestone{})
	data_models.SeedPushupMilestones(db)

	// Step 6: Per-user scratchpad (one text buffer per user)
	db.AutoMigrate(&data_models.Scratchpad{})

	// Step 6b: Single-use expiring user tokens backing email verification and
	// password recovery (epic #5113, subtask #5219). No seed — tokens are
	// minted at runtime only. One table serves both flows; the TTL and the
	// retire-previous-token behaviour that differ between them live in the
	// policy table in data_models/user_token.go, not in a second schema.
	db.AutoMigrate(&data_models.UserToken{})

	// Step 7: Constellations companion app — relationship-type vocabulary
	// (epic #4587, subtask #4594-A2). Colours live on the row, not in the
	// frontend; seed is idempotent.
	db.AutoMigrate(&data_models.RelationshipType{})
	data_models.SeedRelationshipTypes(db)

	// Step 8: Constellations companion app — per-user game identity default
	// (epic #4587, subtask #4595-A3). No seed; a profile is created on first set.
	db.AutoMigrate(&data_models.ConstellationProfile{})

	// Step 9: Constellations companion app — rooms & membership schema
	// (epic #4587, subtask #4593-A1). No seed; rooms are created at runtime.
	db.AutoMigrate(&data_models.Room{})
	db.AutoMigrate(&data_models.RoomMember{})

	// Step 10: Constellations companion app — relationship-graph edges
	// (epic #4587, subtask #4596-B1). No seed; edges are created at runtime.
	db.AutoMigrate(&data_models.Relationship{})

	// Step 11: Constellations companion app — server-authoritative dice rolls
	// (epic #4587, subtask #4597-B2). No seed; a roll is created per roll.
	db.AutoMigrate(&data_models.DiceRoll{})

	// Step 12: Constellations companion app — per-player relationship edit
	// history (epic #4587, subtask #4599-C1). No seed; an action is appended
	// per relationship edit for undo/redo.
	db.AutoMigrate(&data_models.RelationshipAction{})

	// Step 13: Constellations companion app — goal-card victory-condition deck
	// (epic #4807, subtask #4807-A1). 16 cards over the six connection types;
	// seed is idempotent.
	db.AutoMigrate(&data_models.GoalCard{})
	data_models.SeedGoalCards(db)

	// Step 14: Constellations companion app — per-player selected goal
	// (epic #4807, subtask #4807-A1). No seed; a goal is chosen at runtime and
	// is private to the selecting player.
	db.AutoMigrate(&data_models.PlayerGoal{})
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
	db.AutoMigrate(&data_models.RelationshipEvent{})

	// Clean up legacy table
	db.Exec("DROP TABLE IF EXISTS word_weaver_scores")
}
