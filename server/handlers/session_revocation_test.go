package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"aspirant-online/server/data_models"
	"aspirant-online/server/middleware"

	"github.com/gin-gonic/gin"
)

// Session revocation at the handler level (system_3 #5224).
//
// The middleware tests prove a revoked token is refused. These prove the two
// handlers that change a credential actually revoke — which is the half that
// makes the mechanism reach a user.

func newRevocationHarness(t *testing.T) *signupHarness {
	t.Helper()
	h := newRecoveryHarness(t)
	h.router.PUT("/data_models/users/:id", UpdateUserHandler)
	// A route behind AuthMiddleware, so a token can be tried for real rather
	// than by inspecting the column.
	gated := h.router.Group("/")
	gated.Use(middleware.AuthMiddleware())
	gated.GET("/gated", func(c *gin.Context) { c.Status(http.StatusOK) })
	return h
}

// gatedWith runs one request through AuthMiddleware carrying the given token.
func (h *signupHarness) gatedWith(t *testing.T, token string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/gated", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w.Code
}

// The case the whole task exists for: a reset performed BECAUSE someone else
// has the account must evict them. Before this the attacker's session outlived
// the recovery by up to 24 hours — the entire window recovery exists to close.
func TestPasswordResetRevokesLiveSessions(t *testing.T) {
	h := newRevocationHarness(t)
	loadSignupTestJWTSecret(t)
	user := seedVerifiedUser(t, h, "alice", "alice@example.com", goodPassword)

	// The session someone else is holding.
	stolen, err := middleware.GenerateToken(user.ID, "Viewer")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if code := h.gatedWith(t, stolen); code != http.StatusOK {
		t.Fatalf("the session was not live to begin with: %d", code)
	}

	// No delay of any kind between minting and revoking: an epoch has no tick
	// to fall inside, which is the whole reason it replaced a timestamp
	// comparison (system_3 #5275).

	if w := h.forgot(t, "alice@example.com"); w.Code != http.StatusOK {
		t.Fatalf("forgot: %d", w.Code)
	}
	token := h.tokenFromMail(t, "alice@example.com")
	if w := h.reset(t, token, newPassword); w.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", w.Code, w.Body.String())
	}

	if code := h.gatedWith(t, stolen); code != http.StatusUnauthorized {
		t.Fatalf("the stolen session survived the reset: %d — recovery does not evict", code)
	}

	// And the owner can still get back in.
	if w := h.login(t, "alice", newPassword); w.Code != http.StatusOK {
		t.Fatalf("owner login after reset: %d %s", w.Code, w.Body.String())
	}
}

// An admin changing someone's password is the other reason a credential changes
// hands: the account is compromised, or the person lost access. Both want the
// old sessions gone.
func TestAdminPasswordChangeRevokesLiveSessions(t *testing.T) {
	h := newRevocationHarness(t)
	loadSignupTestJWTSecret(t)
	user := seedVerifiedUser(t, h, "alice", "alice@example.com", goodPassword)

	live, err := middleware.GenerateToken(user.ID, "Viewer")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if code := h.gatedWith(t, live); code != http.StatusOK {
		t.Fatalf("session not live to begin with: %d", code)
	}

	// No delay of any kind between minting and revoking: an epoch has no tick
	// to fall inside, which is the whole reason it replaced a timestamp
	// comparison (system_3 #5275).

	if w := h.put(t, "/data_models/users/"+itoaU(user.ID), gin.H{
		"username": "alice", "email": "alice@example.com",
		"password": "an-admin-chosen-one", "access_role": "Viewer",
	}); w.Code != http.StatusOK {
		t.Fatalf("admin update: %d %s", w.Code, w.Body.String())
	}

	if code := h.gatedWith(t, live); code != http.StatusUnauthorized {
		t.Fatalf("the session survived an admin password change: %d", code)
	}
}

// ...but an edit that does NOT change the password must leave sessions alone.
// Signing someone out for a corrected comment would be its own small bug.
func TestAdminEditWithoutPasswordChangeKeepsSessions(t *testing.T) {
	h := newRevocationHarness(t)
	loadSignupTestJWTSecret(t)
	user := seedVerifiedUser(t, h, "alice", "alice@example.com", goodPassword)

	live, err := middleware.GenerateToken(user.ID, "Viewer")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	if w := h.put(t, "/data_models/users/"+itoaU(user.ID), gin.H{
		"username": "alice", "email": "alice@example.com",
		"comment": "a corrected note", "access_role": "Viewer",
	}); w.Code != http.StatusOK {
		t.Fatalf("admin update: %d %s", w.Code, w.Body.String())
	}

	if code := h.gatedWith(t, live); code != http.StatusOK {
		t.Fatalf("an edit that changed no password signed the user out: %d", code)
	}
	if h.user(t, "alice").SessionEpoch != 0 {
		t.Error("a non-password edit bumped the session epoch")
	}
}

// Signing up does not revoke anything — there is nothing to revoke, and a
// watermark set at creation would be a needless trap for the first login.
func TestSignupLeavesTheWatermarkUnset(t *testing.T) {
	h := newRevocationHarness(t)
	loadSignupTestJWTSecret(t)

	if w := h.signup(t, "newcomer", "newcomer@example.com", goodPassword); w.Code != http.StatusOK {
		t.Fatalf("signup: %d", w.Code)
	}
	if got := h.user(t, "newcomer").SessionEpoch; got != 0 {
		t.Errorf("SessionEpoch = %d on a fresh account, want 0", got)
	}
}

// RevokeSessions increments rather than setting a flag, so a second revocation
// is distinguishable from the first.
func TestRevokeSessionsIncrementsTheEpoch(t *testing.T) {
	h := newRevocationHarness(t)
	user := seedVerifiedUser(t, h, "alice", "alice@example.com", goodPassword)

	if got := h.user(t, "alice").SessionEpoch; got != 0 {
		t.Fatalf("starting epoch = %d, want 0", got)
	}
	for want := uint(1); want <= 3; want++ {
		if err := data_models.RevokeSessions(h.db, user.ID); err != nil {
			t.Fatalf("RevokeSessions: %v", err)
		}
		if got := h.user(t, "alice").SessionEpoch; got != want {
			t.Fatalf("epoch = %d after %d revocations, want %d", got, want, want)
		}
	}
}

func itoaU(u uint) string {
	if u == 0 {
		return "0"
	}
	var b []byte
	for u > 0 {
		b = append([]byte{byte('0' + u%10)}, b...)
		u /= 10
	}
	return string(b)
}
