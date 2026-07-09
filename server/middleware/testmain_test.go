package middleware

import (
	"log"
	"os"
	"testing"
)

// TestMain seeds a strong JWT_SECRET for the middleware test binary so
// GenerateToken / AuthMiddleware / ParseTokenIfPresent have a working
// key. Individual tests may still exercise LoadJWTSecret with different
// values via os.Setenv followed by a re-load — they must restore the
// good value before returning so downstream tests keep working.
func TestMain(m *testing.M) {
	os.Setenv("JWT_SECRET", "test-jwt-secret-must-be-at-least-32-bytes-long!!")
	if err := LoadJWTSecret(); err != nil {
		log.Fatalf("middleware test setup: %v", err)
	}
	os.Exit(m.Run())
}
