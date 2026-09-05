package handlers

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"aspirant-online/server/data_models"
	"aspirant-online/server/middleware"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

// Locks the nginx auth_request contract: /internal/verify-admin returns
// 200 to admin callers, 401 to unauthenticated callers, and 403 to
// authenticated non-admins. Nginx maps 200 → allow and both 401/403 →
// error_page → /login redirect, so any code path that reaches an
// upstream 5xx or bare status here would silently let the operator into
// the matrix without an Admin role.
func TestVerifyAdmin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// middleware.jwtSecret is captured at package init, so GenerateToken and
	// AuthMiddleware agree on whatever the process-wide value is — the test
	// does not need to override JWT_SECRET.

	adminToken, err := middleware.GenerateToken(1, "Admin")
	if err != nil {
		t.Fatalf("failed to mint admin token: %v", err)
	}
	// Legacy role name — a JWT minted before the #5113-A2 tier migration; it
	// must still resolve (Trusted → member tier) and be rejected below admin.
	trustedToken, err := middleware.GenerateToken(2, "Trusted")
	if err != nil {
		t.Fatalf("failed to mint trusted token: %v", err)
	}
	// New tier vocabulary — Member is below admin.
	memberToken, err := middleware.GenerateToken(3, "Member")
	if err != nil {
		t.Fatalf("failed to mint member token: %v", err)
	}

	router := gin.New()
	// AuthMiddleware checks the caller's session-revocation watermark since
	// #5224 and fails closed without a db, so the fixture supplies one holding
	// the three token subjects, none of whom has revoked.
	router.Use(func(c *gin.Context) { c.Set("db", internalTestDB(t)); c.Next() })
	adminGroup := router.Group("/")
	adminGroup.Use(middleware.AuthMiddleware())
	adminGroup.Use(RequireTier(TierAdmin))
	adminGroup.GET("/internal/verify-admin", VerifyAdminHandler)

	cases := []struct {
		name       string
		header     string
		cookie     string
		wantStatus int
	}{
		{"admin bearer token → 200", "Bearer " + adminToken, "", http.StatusOK},
		{"admin auth_token cookie → 200", "", adminToken, http.StatusOK},
		{"no credentials → 401", "", "", http.StatusUnauthorized},
		{"gibberish token → 401", "Bearer not-a-jwt", "", http.StatusUnauthorized},
		{"legacy trusted (member tier) → 403", "Bearer " + trustedToken, "", http.StatusForbidden},
		{"member tier → 403", "Bearer " + memberToken, "", http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, "/internal/verify-admin", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: "auth_token", Value: tc.cookie})
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", w.Code, tc.wantStatus, w.Body.String())
			}
		})
	}
}

// internalTestDB holds the three token subjects (ids 1, 2, 3), none of whom
// has revoked their sessions.
func internalTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	db.AutoMigrate(&data_models.User{})
	for id := uint(1); id <= 3; id++ {
		u := data_models.User{Username: fmt.Sprintf("u%d", id), Email: fmt.Sprintf("u%d@example.com", id)}
		u.ID = id
		if err := db.Create(&u).Error; err != nil {
			t.Fatalf("seeding user %d: %v", id, err)
		}
	}
	t.Cleanup(func() { db.Close() })
	return db
}
