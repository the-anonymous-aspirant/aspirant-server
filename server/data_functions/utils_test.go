package data_functions

import (
	"log"
	"os"
	"testing"

	"github.com/joho/godotenv"
)

func TestMain(m *testing.M) {
	// Load environment variables from .env file (optional — CI won't have one)
	err := godotenv.Load("../../.env")
	if err != nil {
		log.Println("Warning: .env file not found, using environment variables as-is")
	}

	// Run tests
	os.Exit(m.Run())
}

func TestGetGitCommit(t *testing.T) {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	commit := GetGitCommit()
	if commit == "" {
		t.Errorf("GetGitCommit() returned empty string")
	}
	// When built without -ldflags, returns "unknown"; when built with, returns a SHA
	t.Logf("GitCommit: %s", commit)
}
