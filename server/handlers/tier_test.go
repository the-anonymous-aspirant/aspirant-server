package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"aspirant-online/server/middleware"

	"github.com/gin-gonic/gin"
)

// Locks the #5113-A2 access-tier mapping. tierOf must accept BOTH the new tier
// names and the legacy six-role names (so a JWT minted before the migration,
// 24h expiry, keeps resolving), and rank them public<viewer<member<admin (with
// public represented by the absence of a role, i.e. TierBlocked for a role that
// grants nothing). This is the runtime half of the access-preservation property
// the migration relies on: if the mapping is right, every legacy role reaches
// exactly the tier the #5113-A1 inventory assigned it.
func TestTierOf(t *testing.T) {
	cases := map[string]AccessTier{
		// new tier vocabulary
		"Admin":   TierAdmin,
		"Member":  TierMember,
		"Viewer":  TierViewer,
		"Blocked": TierBlocked,
		// legacy six-role names (transition JWTs)
		"Trusted": TierMember,
		"User":    TierViewer,
		"Guest":   TierViewer,
		"Gamer":   TierViewer,
		"Deleted": TierBlocked,
		// anything unknown/empty is least-privilege
		"":            TierBlocked,
		"nonsense":    TierBlocked,
		"admin":       TierBlocked, // case-sensitive: only "Admin" is admin
	}
	for role, want := range cases {
		if got := tierOf(role); got != want {
			t.Errorf("tierOf(%q) = %d, want %d", role, got, want)
		}
	}
}

// Locks the RequireTier floor behaviour end-to-end: for each tier gate, a token
// at-or-above the floor gets 200 and below it gets 403, and an unauthenticated
// caller gets 401 (from AuthMiddleware). This is the enforcement matrix the
// three route groups (viewer/member/admin) rely on.
func TestRequireTierFloors(t *testing.T) {
	gin.SetMode(gin.TestMode)

	token := func(role string) string {
		tok, err := middleware.GenerateToken(1, role)
		if err != nil {
			t.Fatalf("mint %s token: %v", role, err)
		}
		return tok
	}

	// floor → which roles must pass (200); every other authenticated role 403s.
	floors := []struct {
		name  string
		floor AccessTier
		pass  map[string]bool
	}{
		{"viewer", TierViewer, map[string]bool{"Viewer": true, "Member": true, "Admin": true, "User": true, "Guest": true, "Gamer": true, "Trusted": true}},
		{"member", TierMember, map[string]bool{"Member": true, "Admin": true, "Trusted": true}},
		{"admin", TierAdmin, map[string]bool{"Admin": true}},
	}
	roles := []string{"Viewer", "Member", "Admin", "Blocked", "User", "Guest", "Gamer", "Trusted", "Deleted"}

	for _, f := range floors {
		router := gin.New()
		g := router.Group("/")
		g.Use(middleware.AuthMiddleware())
		g.Use(RequireTier(f.floor))
		g.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

		for _, role := range roles {
			want := http.StatusForbidden
			if f.pass[role] {
				want = http.StatusOK
			}
			req, _ := http.NewRequest(http.MethodGet, "/x", nil)
			req.Header.Set("Authorization", "Bearer "+token(role))
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != want {
				t.Errorf("floor=%s role=%s: status %d, want %d", f.name, role, w.Code, want)
			}
		}

		// Unauthenticated → 401, never 403 (nginx maps both to /login, but the
		// distinction is the contract AuthMiddleware owns).
		req, _ := http.NewRequest(http.MethodGet, "/x", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("floor=%s no-creds: status %d, want 401", f.name, w.Code)
		}
	}
}
