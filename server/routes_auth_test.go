package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aspirant-online/server/data_models"
	"aspirant-online/server/middleware"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

// loadTestJWTSecret installs a strong signing key so GenerateToken /
// AuthMiddleware agree on a secret. The server package has no TestMain, so
// each auth test that mints tokens calls this first.
func loadTestJWTSecret(t *testing.T) {
	t.Helper()
	t.Setenv("JWT_SECRET", "test-jwt-secret-must-be-at-least-32-bytes-long!!")
	if err := middleware.LoadJWTSecret(); err != nil {
		t.Fatalf("LoadJWTSecret: %v", err)
	}
}

// realRouter builds the production router (the same RegisterRoutes the server
// boots) so the role-group membership of each route is exercised as shipped,
// not re-declared in the test.
//
// It used to pass a nil db, on the stated premise that "AuthMiddleware reads
// the role from the JWT claim ... neither touches the db". That premise ended
// with #5224: AuthMiddleware now reads the caller's session-revocation
// watermark on every request and fails closed without a db, so this wires one
// the way main.go does. The commander proxy handlers still make an outbound
// HTTP call and touch nothing.
func realRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	// Point the commander proxy at an address that refuses fast, so an
	// Admin request that clears the role gate reaches the proxy and returns
	// 502 immediately instead of blocking on the 30s client timeout.
	t.Setenv("COMMANDER_URL", "http://127.0.0.1:1")

	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	db.AutoMigrate(&data_models.User{})
	// The token subjects these tests mint, none of whom has revoked.
	for id := uint(1); id <= 3; id++ {
		u := data_models.User{Username: fmt.Sprintf("u%d", id), Email: fmt.Sprintf("u%d@example.com", id)}
		u.ID = id
		if err := db.Create(&u).Error; err != nil {
			t.Fatalf("seeding user %d: %v", id, err)
		}
	}
	t.Cleanup(func() { db.Close() })

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("db", db); c.Next() })
	RegisterRoutes(r, db)
	return r
}

func mustToken(t *testing.T, uid uint, role string) string {
	t.Helper()
	tok, err := middleware.GenerateToken(uid, role)
	if err != nil {
		t.Fatalf("GenerateToken(%s): %v", role, err)
	}
	return tok
}

func putOperatorDefaults(t *testing.T, r *gin.Engine, token string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut,
		"/commander/valuation-statement/operator-defaults",
		strings.NewReader(`{"ort":"Nynäshamn"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

// TestOperatorDefaultsIsAdminOnly locks the #3182 fix: the shared valuation
// operator-defaults config (appraiser identity + default likviditet) is
// writable only by Admin, not by any Trusted user. Follow-up to the #3096
// broken-access-control remediation; OWASP A01:2021.
func TestOperatorDefaultsIsAdminOnly(t *testing.T) {
	loadTestJWTSecret(t)
	r := realRouter(t)

	t.Run("Trusted is forbidden", func(t *testing.T) {
		code := putOperatorDefaults(t, r, mustToken(t, 1, "Trusted"))
		if code != http.StatusForbidden {
			t.Fatalf("Trusted PUT operator-defaults = %d, want 403 — the route must be Admin-only", code)
		}
	})

	t.Run("Admin clears the role gate", func(t *testing.T) {
		// Admin passes ValidateRole and reaches the commander proxy, which
		// fails to connect (127.0.0.1:1) and returns 502. The assertion is
		// role-scoped: anything but 403/401 proves the Admin role is allowed
		// through; 502 is the expected reach-through with no commander up.
		code := putOperatorDefaults(t, r, mustToken(t, 2, "Admin"))
		if code == http.StatusForbidden || code == http.StatusUnauthorized {
			t.Fatalf("Admin PUT operator-defaults = %d, want the request to clear the role gate (not 401/403)", code)
		}
	})

	t.Run("Trusted token is otherwise valid on a sibling trusted route", func(t *testing.T) {
		// Guards against a false-green where the 403 above is a bad token
		// rather than role scoping: the same Trusted token must NOT be
		// forbidden on the sibling /generate route, which stays Trusted.
		req := httptest.NewRequest(http.MethodPost,
			"/commander/valuation-statement/generate",
			strings.NewReader(`{"objekt":"LGH 1001"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+mustToken(t, 1, "Trusted"))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code == http.StatusForbidden {
			t.Fatalf("Trusted POST generate = 403, want the Trusted token accepted on a sibling trusted route")
		}
	})
}
