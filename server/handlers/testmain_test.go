package handlers

import (
	"log"
	"os"
	"testing"

	"aspirant-online/server/middleware"
)

// TestMain seeds a strong JWT_SECRET for the handlers test binary so
// middleware.GenerateToken (used by login-flow and internal tests) has
// a working key. Without this, LoadJWTSecret refuses to install a
// placeholder / empty secret — the intended production behaviour
// (system_3 #1374) that would otherwise break the test suite.
func TestMain(m *testing.M) {
	os.Setenv("JWT_SECRET", "test-jwt-secret-must-be-at-least-32-bytes-long!!")
	if err := middleware.LoadJWTSecret(); err != nil {
		log.Fatalf("handlers test setup: %v", err)
	}
	os.Exit(m.Run())
}
