package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aspirant-online/server/data_models"
	"aspirant-online/server/middleware"

	"github.com/gin-gonic/gin"
)

// The recovery tests reuse signupHarness: these flows share a database, a mail
// seam and a login endpoint, and a second near-identical harness would be the
// thing most likely to drift away from what the handlers actually see.
func newRecoveryHarness(t *testing.T) *signupHarness {
	t.Helper()
	h := newSignupHarness(t)
	h.router.POST("/password/forgot", ForgotPasswordHandler)
	h.router.POST("/password/reset", ResetPasswordHandler)
	return h
}

// seedVerifiedUser creates an account that has completed sign-up.
func seedVerifiedUser(t *testing.T, h *signupHarness, username, address, password string) *data_models.User {
	t.Helper()
	if w := h.signup(t, username, address, password); w.Code != http.StatusOK {
		t.Fatalf("signup: %d %s", w.Code, w.Body.String())
	}
	token := h.tokenFromMail(t, address)
	if w := h.post(t, "/verify-email", map[string]string{"token": token}); w.Code != http.StatusOK {
		t.Fatalf("verify: %d %s", w.Code, w.Body.String())
	}
	h.mail.sent = nil
	return h.user(t, username)
}

func (h *signupHarness) forgot(t *testing.T, address string) *httptest.ResponseRecorder {
	t.Helper()
	return h.post(t, "/password/forgot", gin.H{"email": address})
}

func (h *signupHarness) reset(t *testing.T, token, password string) *httptest.ResponseRecorder {
	t.Helper()
	return h.post(t, "/password/reset", gin.H{"token": token, "password": password})
}

func (h *signupHarness) login(t *testing.T, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	return h.post(t, "/login", gin.H{"username": username, "password": password})
}

const newPassword = "a-completely-different-one"

// --- the round trip ---------------------------------------------------------

func TestForgotThenReset(t *testing.T) {
	h := newRecoveryHarness(t)
	loadSignupTestJWTSecret(t)
	seedVerifiedUser(t, h, "alice", "alice@example.com", goodPassword)

	if w := h.forgot(t, "alice@example.com"); w.Code != http.StatusOK {
		t.Fatalf("forgot: %d %s", w.Code, w.Body.String())
	}
	token := h.tokenFromMail(t, "alice@example.com")

	if w := h.reset(t, token, newPassword); w.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", w.Code, w.Body.String())
	}

	if w := h.login(t, "alice", newPassword); w.Code != http.StatusOK {
		t.Errorf("the new password does not authenticate: %d %s", w.Code, w.Body.String())
	}
	if w := h.login(t, "alice", goodPassword); w.Code != http.StatusUnauthorized {
		t.Errorf("the old password still authenticates: %d", w.Code)
	}
}

// --- the address oracle -----------------------------------------------------

// /password/forgot takes an address and nothing else. A distinguishable answer
// would make it a bulk address-membership oracle for any list fed to it.
func TestForgotDoesNotRevealWhetherTheAddressIsKnown(t *testing.T) {
	h := newRecoveryHarness(t)
	seedVerifiedUser(t, h, "alice", "alice@example.com", goodPassword)

	known := h.forgot(t, "alice@example.com")
	unknown := h.forgot(t, "nobody@example.com")

	if known.Code != unknown.Code {
		t.Errorf("status differs: known %d, unknown %d", known.Code, unknown.Code)
	}
	if known.Body.String() != unknown.Body.String() {
		t.Errorf("body differs:\n  known: %s\nunknown: %s", known.Body.String(), unknown.Body.String())
	}
	if ct, want := unknown.Header().Get("Content-Type"), known.Header().Get("Content-Type"); ct != want {
		t.Errorf("Content-Type differs: %q vs %q", ct, want)
	}

	// And the side effects must match the story: mail and a token for the
	// account we know, neither for the address we do not.
	if got := len(h.mail.to("alice@example.com")); got != 1 {
		t.Errorf("sent %d messages to the known address, want 1", got)
	}
	if got := len(h.mail.to("nobody@example.com")); got != 0 {
		t.Errorf("sent %d messages to an unknown address, want 0", got)
	}
	var tokens int
	h.db.Model(&data_models.UserToken{}).
		Where("purpose = ?", data_models.PurposePasswordReset).Count(&tokens)
	if tokens != 1 {
		t.Errorf("%d reset tokens exist, want 1 — an unknown address minted one", tokens)
	}
}

func TestForgotAnswersTheSameWhenSendingFails(t *testing.T) {
	h := newRecoveryHarness(t)
	seedVerifiedUser(t, h, "alice", "alice@example.com", goodPassword)

	good := h.forgot(t, "alice@example.com")
	h.mail.err = errTestSendFailed
	failed := h.forgot(t, "alice@example.com")

	if good.Code != failed.Code || good.Body.String() != failed.Body.String() {
		t.Errorf("a send failure changed the response:\n  ok: %d %s\nfail: %d %s",
			good.Code, good.Body.String(), failed.Code, failed.Body.String())
	}
}

func TestForgotRejectsAMalformedAddress(t *testing.T) {
	h := newRecoveryHarness(t)

	// A shape check describes the request, not the database, so this one is
	// allowed to answer distinctly.
	if w := h.forgot(t, "not an address"); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// --- token handling ---------------------------------------------------------

func TestResetTokenIsSingleUse(t *testing.T) {
	h := newRecoveryHarness(t)
	loadSignupTestJWTSecret(t)
	seedVerifiedUser(t, h, "alice", "alice@example.com", goodPassword)

	if w := h.forgot(t, "alice@example.com"); w.Code != http.StatusOK {
		t.Fatalf("forgot: %d", w.Code)
	}
	token := h.tokenFromMail(t, "alice@example.com")

	if w := h.reset(t, token, newPassword); w.Code != http.StatusOK {
		t.Fatalf("first reset: %d %s", w.Code, w.Body.String())
	}

	replay := h.reset(t, token, "yet-another-password-x")
	unknown := h.reset(t, "not-a-real-token", "yet-another-password-x")
	if replay.Code != http.StatusBadRequest {
		t.Errorf("replay status = %d, want 400", replay.Code)
	}
	if replay.Body.String() != unknown.Body.String() {
		t.Errorf("a replayed token is distinguishable from an unknown one:\n replay: %s\nunknown: %s",
			replay.Body.String(), unknown.Body.String())
	}
	// The replay must not have changed the password either.
	if w := h.login(t, "alice", newPassword); w.Code != http.StatusOK {
		t.Errorf("the replay changed the password: %d", w.Code)
	}
}

// Asking again retires the earlier link. This is the property that makes
// recovery safe to use when you suspect someone else has your account.
func TestRequestingASecondResetRetiresTheFirstLink(t *testing.T) {
	h := newRecoveryHarness(t)
	seedVerifiedUser(t, h, "alice", "alice@example.com", goodPassword)

	if w := h.forgot(t, "alice@example.com"); w.Code != http.StatusOK {
		t.Fatalf("first forgot: %d", w.Code)
	}
	first := h.tokenFromMail(t, "alice@example.com")

	if w := h.forgot(t, "alice@example.com"); w.Code != http.StatusOK {
		t.Fatalf("second forgot: %d", w.Code)
	}
	second := h.tokenFromMail(t, "alice@example.com")

	if first == second {
		t.Fatal("the second request reissued the same token")
	}
	if w := h.reset(t, first, newPassword); w.Code != http.StatusBadRequest {
		t.Errorf("the superseded link still works: %d", w.Code)
	}
	if w := h.reset(t, second, newPassword); w.Code != http.StatusOK {
		t.Errorf("the newest link was rejected: %d %s", w.Code, w.Body.String())
	}
}

// A verification token must not be spendable as a reset. Otherwise sign-up —
// the weaker flow — mints credentials for the stronger one.
func TestResetRejectsAVerificationToken(t *testing.T) {
	h := newRecoveryHarness(t)
	loadSignupTestJWTSecret(t)

	if w := h.signup(t, "bob", "bob@example.com", goodPassword); w.Code != http.StatusOK {
		t.Fatalf("signup: %d", w.Code)
	}
	verifyToken := h.tokenFromMail(t, "bob@example.com")

	if w := h.reset(t, verifyToken, newPassword); w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — a verification token reset a password", w.Code)
	}
	// And it must not have been burned by the attempt.
	if w := h.post(t, "/verify-email", map[string]string{"token": verifyToken}); w.Code != http.StatusOK {
		t.Errorf("the failed reset consumed the verification token: %d", w.Code)
	}
}

// --- password validation ----------------------------------------------------

// Validation runs before the token is consumed: rejecting a typo'd password
// after burning the link would send the user back to their inbox.
func TestResetValidatesThePasswordBeforeSpendingTheToken(t *testing.T) {
	cases := []struct {
		name     string
		password string
	}{
		{"too short", "short"},
		{"past bcrypt's 72-byte limit", strings.Repeat("x", 73)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newRecoveryHarness(t)
			loadSignupTestJWTSecret(t)
			seedVerifiedUser(t, h, "alice", "alice@example.com", goodPassword)

			if w := h.forgot(t, "alice@example.com"); w.Code != http.StatusOK {
				t.Fatalf("forgot: %d", w.Code)
			}
			token := h.tokenFromMail(t, "alice@example.com")

			if w := h.reset(t, token, tc.password); w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", w.Code)
			}
			// The link must still work.
			if w := h.reset(t, token, newPassword); w.Code != http.StatusOK {
				t.Fatalf("the token was spent by the rejected attempt: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

// --- interactions with the other flows --------------------------------------

// Recovery proves control of the mailbox just as verification does, which is
// exactly why folding them together is tempting and wrong: it would make
// recovery a way around the sign-up gate.
func TestResetDoesNotVerifyAnUnverifiedAccount(t *testing.T) {
	h := newRecoveryHarness(t)
	loadSignupTestJWTSecret(t)

	if w := h.signup(t, "bob", "bob@example.com", goodPassword); w.Code != http.StatusOK {
		t.Fatalf("signup: %d", w.Code)
	}
	h.mail.sent = nil

	if w := h.forgot(t, "bob@example.com"); w.Code != http.StatusOK {
		t.Fatalf("forgot: %d", w.Code)
	}
	token := h.tokenFromMail(t, "bob@example.com")
	if w := h.reset(t, token, newPassword); w.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", w.Code, w.Body.String())
	}

	if h.user(t, "bob").IsEmailVerified() {
		t.Fatal("a password reset confirmed an unverified address — recovery is a verification bypass")
	}
	if w := h.login(t, "bob", newPassword); w.Code != http.StatusUnauthorized {
		t.Errorf("an unverified account can log in after a reset: %d", w.Code)
	}
}

// Someone who has just recovered their account is exactly the person whose
// failed attempts filled the rate-limit bucket. Leaving them locked out at
// that moment is when they give up.
//
// This mounts the real LoginRateLimit middleware and asserts the user-visible
// outcome — that the recovered login is not throttled — rather than reading
// internal bucket state through an accessor that exists only for the test.
func TestResetClearsTheLoginRateLimitBucket(t *testing.T) {
	h := newRecoveryHarness(t)
	loadSignupTestJWTSecret(t)
	middleware.ResetLoginRateLimiter()
	t.Cleanup(middleware.ResetLoginRateLimiter)

	seedVerifiedUser(t, h, "alice", "alice@example.com", goodPassword)

	// A second router whose /login carries the throttle the production router
	// puts in front of it.
	gated := gin.New()
	gated.Use(func(c *gin.Context) { c.Set("db", h.db); c.Next() })
	gated.POST("/login", middleware.LoginRateLimit(), LoginHandler)
	gatedLogin := func(username, password string) int {
		raw, err := json.Marshal(gin.H{"username": username, "password": password})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		gated.ServeHTTP(w, req)
		return w.Code
	}

	// Fill the per-username bucket, and confirm the throttle actually engaged —
	// otherwise the assertion after the reset would pass vacuously.
	throttled := false
	for i := 0; i < middleware.PerUsernameLimit+2; i++ {
		if gatedLogin("alice", "wrong-password-attempt") == http.StatusTooManyRequests {
			throttled = true
		}
	}
	if !throttled {
		t.Fatal("the login throttle never engaged; the rest of this test would prove nothing")
	}
	if code := gatedLogin("alice", goodPassword); code != http.StatusTooManyRequests {
		t.Fatalf("a correct password is not throttled (%d); the lockout this test is about does not exist", code)
	}

	if w := h.forgot(t, "alice@example.com"); w.Code != http.StatusOK {
		t.Fatalf("forgot: %d", w.Code)
	}
	token := h.tokenFromMail(t, "alice@example.com")
	if w := h.reset(t, token, newPassword); w.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", w.Code, w.Body.String())
	}

	if code := gatedLogin("alice", newPassword); code != http.StatusOK {
		t.Errorf("login after recovery = %d, want 200 — the reset did not release the lockout", code)
	}
}
