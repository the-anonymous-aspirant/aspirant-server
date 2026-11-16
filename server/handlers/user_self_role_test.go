package handlers

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
)

// No one lowers their own access (system_3 #11176, security c68795 on #11004).
// UpdateUserHandler refuses a role change on the caller's own account to
// Blocked or to any tier below the account's stored one, before anything is
// saved; everything else behaves as before.

const selfLowerRefusal = "You cannot block your own account or lower its access. Another admin has to do it."

// newSelfRoleHarness routes PUT /caller/:caller/users/:id to UpdateUserHandler
// with `user_id` and `role` set on the context the way AuthMiddleware sets
// them, so the handler sees a real caller identity.
func newSelfRoleHarness(t *testing.T) *signupHarness {
	t.Helper()
	h := newRecoveryHarness(t)
	h.router.PUT("/caller/:caller/users/:id", func(c *gin.Context) {
		id, err := strconv.ParseUint(c.Param("caller"), 10, 64)
		if err != nil {
			t.Fatalf("caller param: %v", err)
		}
		c.Set("user_id", uint(id))
		c.Set("role", "Admin")
		UpdateUserHandler(c)
	})
	return h
}

func setRole(t *testing.T, h *signupHarness, user *data_models.User, name string) {
	t.Helper()
	role, err := data_models.GetRoleByName(h.db, name)
	if err != nil {
		t.Fatalf("role %s: %v", name, err)
	}
	if err := h.db.Model(user).Update("role_id", role.ID).Error; err != nil {
		t.Fatalf("set role: %v", err)
	}
}

func roleOf(t *testing.T, h *signupHarness, username string) string {
	t.Helper()
	var u data_models.User
	if err := h.db.Preload("Role").Where("username = ?", username).First(&u).Error; err != nil {
		t.Fatalf("reload %s: %v", username, err)
	}
	return u.Role.RoleName
}

func (h *signupHarness) putAs(t *testing.T, caller, target *data_models.User, body gin.H) (int, string) {
	t.Helper()
	w := h.put(t, "/caller/"+itoaU(caller.ID)+"/users/"+itoaU(target.ID), body)
	return w.Code, w.Body.String()
}

func TestAdminCannotBlockTheirOwnAccount(t *testing.T) {
	h := newSelfRoleHarness(t)
	admin := seedVerifiedUser(t, h, "root", "root@example.com", goodPassword)
	setRole(t, h, admin, "Admin")

	code, body := h.putAs(t, admin, admin, gin.H{
		"username": "root", "email": "root@example.com", "access_role": "Blocked",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d %s, want 400", code, body)
	}
	if !strings.Contains(body, selfLowerRefusal) {
		t.Errorf("body %s does not carry the refusal sentence", body)
	}
	if got := roleOf(t, h, "root"); got != "Admin" {
		t.Errorf("role = %s after a refused self-block, want Admin unchanged", got)
	}
	if got := h.user(t, "root").SessionEpoch; got != 0 {
		t.Errorf("session epoch = %d after a refused self-block, want 0 (no session revoked)", got)
	}
}

func TestAdminCannotLowerTheirOwnTier(t *testing.T) {
	h := newSelfRoleHarness(t)
	admin := seedVerifiedUser(t, h, "root", "root@example.com", goodPassword)
	setRole(t, h, admin, "Admin")

	for _, lower := range []string{"Member", "Viewer"} {
		code, body := h.putAs(t, admin, admin, gin.H{
			"username": "root", "email": "root@example.com", "access_role": lower,
		})
		if code != http.StatusBadRequest {
			t.Errorf("Admin → %s on your own account: status = %d %s, want 400", lower, code, body)
		}
	}
	if got := roleOf(t, h, "root"); got != "Admin" {
		t.Errorf("role = %s, want Admin unchanged", got)
	}
}

func TestOwnAccountAtTheSameTierStillSavesOtherFields(t *testing.T) {
	h := newSelfRoleHarness(t)
	admin := seedVerifiedUser(t, h, "root", "root@example.com", goodPassword)
	setRole(t, h, admin, "Admin")

	code, body := h.putAs(t, admin, admin, gin.H{
		"username": "root", "email": "root@example.com", "comment": "on call this week", "access_role": "Admin",
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d %s, want 200", code, body)
	}
	if got := h.user(t, "root").Comment; got != "on call this week" {
		t.Errorf("comment = %q, want the edit saved", got)
	}
	if got := roleOf(t, h, "root"); got != "Admin" {
		t.Errorf("role = %s, want Admin", got)
	}
}

func TestAdminCanStillBlockAnotherAccount(t *testing.T) {
	h := newSelfRoleHarness(t)
	admin := seedVerifiedUser(t, h, "root", "root@example.com", goodPassword)
	setRole(t, h, admin, "Admin")
	other := seedVerifiedUser(t, h, "bob", "bob@example.com", goodPassword)

	code, body := h.putAs(t, admin, other, gin.H{
		"username": "bob", "email": "bob@example.com", "access_role": "Blocked",
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d %s, want 200", code, body)
	}
	if got := roleOf(t, h, "bob"); got != "Blocked" {
		t.Errorf("bob's role = %s, want Blocked", got)
	}
	if got := h.user(t, "bob").SessionEpoch; got == 0 {
		t.Error("blocking another account did not revoke its sessions")
	}
}
