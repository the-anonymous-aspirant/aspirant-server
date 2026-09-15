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
	// SiteSetting joins User here for #5289: the sign-up kill-switch reads its
	// row on the public status route, and a missing table would make that a
	// 500 rather than the 200 the gate test is asserting.
	db.AutoMigrate(&data_models.User{}, &data_models.SiteSetting{})
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

// TestOperatorDefaultsIsMemberWritable locks the #5924 modelling fix: appraiser
// identity is a user's OWN record, so the write is Member-tier (no Admin gate),
// while an unauthenticated caller is still refused. This supersedes the #3182
// Admin-only lock — that gate protected a single shared global record; identity
// is now per-user and scoped by the commander on the forge-proof caller header,
// so there is no shared record to corrupt and no Admin gate to justify. The
// no-cross-user-write property is enforced (and tested) in the commander.
func TestOperatorDefaultsIsMemberWritable(t *testing.T) {
	loadTestJWTSecret(t)
	r := realRouter(t)

	t.Run("Member clears the tier gate", func(t *testing.T) {
		// A Member reaches the commander proxy, which fails to connect
		// (127.0.0.1:1) and returns 502. Anything but 401/403 proves the Member
		// tier is allowed through — the point of #5924.
		code := putOperatorDefaults(t, r, mustToken(t, 1, "Member"))
		if code == http.StatusForbidden || code == http.StatusUnauthorized {
			t.Fatalf("Member PUT operator-defaults = %d, want the request to clear the tier gate (not 401/403) — identity is the user's own to write", code)
		}
	})

	t.Run("unauthenticated is refused", func(t *testing.T) {
		// No token → the auth middleware rejects before any tier check. The
		// write must never be anonymous.
		req := httptest.NewRequest(http.MethodPut,
			"/commander/valuation-statement/operator-defaults",
			strings.NewReader(`{"maklare_namn":"x"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated PUT operator-defaults = %d, want 401", w.Code)
		}
	})
}

// putSignupSetting issues the admin kill-switch write with the given token
// (empty for unauthenticated).
func putSignupSetting(t *testing.T, r *gin.Engine, token string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/settings/signup", strings.NewReader(`{"enabled":false}`))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

// TestSignupKillSwitchRouteGates locks the two halves of #5289 to the tiers
// they belong to: anyone may READ whether sign-up is open, only an Admin may
// change it.
//
// It runs against the real router so the group membership is the shipped one.
func TestSignupKillSwitchRouteGates(t *testing.T) {
	loadTestJWTSecret(t)
	r := realRouter(t)

	t.Run("the status read is public", func(t *testing.T) {
		// The sign-up page's visitor has no account, so a gate here would make
		// the switch unreadable by its only consumer.
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/signup/status", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("unauthenticated GET /signup/status = %d, want 200: %s", w.Code, w.Body.String())
		}
	})

	t.Run("the write refuses an unauthenticated caller", func(t *testing.T) {
		if code := putSignupSetting(t, r, ""); code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated PUT /settings/signup = %d, want 401", code)
		}
	})

	t.Run("the write refuses a Member", func(t *testing.T) {
		if code := putSignupSetting(t, r, mustToken(t, 1, "Member")); code != http.StatusForbidden {
			t.Fatalf("Member PUT /settings/signup = %d, want 403 — the route must be Admin-only", code)
		}
	})

	t.Run("the Member token is otherwise valid", func(t *testing.T) {
		// Positive control: without it, a 403 above could be a bad token rather
		// than role scoping, and the gate assertion would pass for the wrong
		// reason.
		req := httptest.NewRequest(http.MethodGet, "/files/list", nil)
		req.Header.Set("Authorization", "Bearer "+mustToken(t, 1, "Member"))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
			t.Fatalf("Member GET /files/list = %d, want the Member token accepted on a sibling member route", w.Code)
		}
	})

	t.Run("Admin clears the gate", func(t *testing.T) {
		if code := putSignupSetting(t, r, mustToken(t, 2, "Admin")); code == http.StatusForbidden || code == http.StatusUnauthorized {
			t.Fatalf("Admin PUT /settings/signup = %d, want the request to clear the role gate", code)
		}
	})
}
