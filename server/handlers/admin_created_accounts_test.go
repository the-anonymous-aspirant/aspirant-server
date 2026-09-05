package handlers

import (
	"net/http"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
)

// Regression tests for the lockout the #5220 dogfood walk found on merged main
// (system_3 #5232).
//
// The login gate added with self-service sign-up refuses any account with no
// verification timestamp, and neither admin creation path set one. A fresh
// install's first admin was created and then could never log in — 200 from
// /bootstrap/admin, 401 on every login afterwards, with no second chance,
// because the one-time migration that stamps pre-existing accounts had already
// run at boot.
//
// Why nothing here caught it before: every existing test creates users through
// a path that was already under consideration — the sign-up handler, or a
// direct row insert the test then stamps itself. Nothing exercised "an admin
// makes an account, that person logs in", which is how every account on this
// system was made before self-service existed.

func newAdminCreationHarness(t *testing.T) *signupHarness {
	t.Helper()
	h := newSignupHarness(t)
	h.router.POST("/bootstrap/admin", BootstrapUserHandler)
	h.router.POST("/data_models/users", CreateUserHandler)
	return h
}

// A fresh install: bootstrap the first admin, then log in as them.
func TestBootstrapAdminCanLogIn(t *testing.T) {
	h := newAdminCreationHarness(t)
	loadSignupTestJWTSecret(t)

	if w := h.post(t, "/bootstrap/admin", gin.H{
		"username": "admin", "email": "admin@example.com", "password": "admin-password-1",
	}); w.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	w := h.post(t, "/login", gin.H{"username": "admin", "password": "admin-password-1"})
	if w.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200 — a fresh install's first admin is locked out; body: %s",
			w.Code, w.Body.String())
	}
	if !h.user(t, "admin").IsEmailVerified() {
		t.Error("the bootstrap admin was created unverified")
	}
}

// An admin creating an account for someone else.
func TestAdminCreatedUserCanLogIn(t *testing.T) {
	h := newAdminCreationHarness(t)
	loadSignupTestJWTSecret(t)

	if w := h.post(t, "/data_models/users", gin.H{
		"username": "colleague", "email": "colleague@example.com",
		"password": "their-password-here", "access_role": "Member",
	}); w.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	w := h.post(t, "/login", gin.H{"username": "colleague", "password": "their-password-here"})
	if w.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200 — an admin-created account cannot log in; body: %s",
			w.Code, w.Body.String())
	}
	u := h.user(t, "colleague")
	if !u.IsEmailVerified() {
		t.Error("the admin-created account was created unverified")
	}
	if u.Role.RoleName != "Member" {
		t.Errorf("role = %q, want Member — the admin's choice of tier was not honoured", u.Role.RoleName)
	}
}

// The exemption must not leak onto the public path: a person signing themselves
// up still has to prove they can receive mail at the address they typed.
func TestSelfServiceSignupIsStillUnverified(t *testing.T) {
	h := newAdminCreationHarness(t)
	loadSignupTestJWTSecret(t)

	if w := h.signup(t, "newcomer", "newcomer@example.com", goodPassword); w.Code != http.StatusOK {
		t.Fatalf("signup: %d", w.Code)
	}

	if h.user(t, "newcomer").IsEmailVerified() {
		t.Fatal("a self-service sign-up was created already verified — the admin exemption leaked onto the public path")
	}
	if w := h.post(t, "/login", gin.H{"username": "newcomer", "password": goodPassword}); w.Code != http.StatusUnauthorized {
		t.Fatalf("login status = %d, want 401 — an unverified sign-up can log in", w.Code)
	}
}

// The stamp is a real timestamp, not a zero value that happens to be non-nil.
func TestMarkEmailVerifiedNowStampsAUsableTime(t *testing.T) {
	var u data_models.User
	if u.IsEmailVerified() {
		t.Fatal("a zero User reports itself verified")
	}
	u.MarkEmailVerifiedNow()
	if !u.IsEmailVerified() {
		t.Fatal("MarkEmailVerifiedNow left the account unverified")
	}
	if u.EmailVerifiedAt.IsZero() {
		t.Error("the stamped time is the zero time")
	}
}
