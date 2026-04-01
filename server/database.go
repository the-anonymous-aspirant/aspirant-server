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
	// Step 1: Migrate roles table first, rename legacy Family role, seed defaults
	db.AutoMigrate(&data_models.Role{})
	db.Exec("UPDATE roles SET role_name = 'Trusted', role_description = 'User with trusted access' WHERE role_name = 'Family'")
	data_models.SeedRoles(db)

	// Step 2: Check if the legacy access_role column still exists on users
	var colCount int
	db.Raw(`SELECT COUNT(*) FROM information_schema.columns
		WHERE table_name = 'users' AND column_name = 'access_role'`).Row().Scan(&colCount)

	if colCount > 0 {
		// Legacy column exists — backfill role_id then drop it
		log.Println("Migrating users.access_role → users.role_id...")

		// Rename any remaining Family values
		db.Exec("UPDATE users SET access_role = 'Trusted' WHERE access_role = 'Family'")

		// AutoMigrate User so GORM adds the new role_id column
		db.AutoMigrate(&data_models.User{})

		// Backfill role_id from the matching role name
		db.Exec(`UPDATE users SET role_id = roles.id
			FROM roles WHERE users.access_role = roles.role_name`)

		// Default any unmatched rows to the "User" role
		db.Exec(`UPDATE users SET role_id = (SELECT id FROM roles WHERE role_name = 'User')
			WHERE role_id IS NULL OR role_id = 0`)

		// Drop the legacy column
		db.Exec("ALTER TABLE users DROP COLUMN access_role")
		log.Println("Migration complete: access_role column dropped")
	} else {
		// Column already gone — normal migrate
		db.AutoMigrate(&data_models.User{})
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

	// Clean up legacy table
	db.Exec("DROP TABLE IF EXISTS word_weaver_scores")
}
