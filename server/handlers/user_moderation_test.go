package handlers

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"aspirant-online/server/middleware"

	"github.com/gin-gonic/gin"
)

// Admin moderation that actually takes effect (system_3 #5290).
//
// The two halves of the moderation surface the operator asked for: blocking an
// account has to end its live sessions, and the roster has to show which
// self-service accounts never confirmed an address.

// TestTierChangeRevokesLiveSessions is the defect this subtask was filed on.
//
// AuthMiddleware reads `role` from the JWT claim and never re-reads the row, and
// a token is good for 24 hours. So before this, an admin moving an abusive
// account to Blocked changed a database column and nothing else: the account
// kept every capability its live token carried until that token expired. The
// assertion runs the token through the real middleware rather than reading the
// epoch column, because the column moving is not the property that matters.
func TestTierChangeRevokesLiveSessions(t *testing.T) {
	h := newRevocationHarness(t)
	loadSignupTestJWTSecret(t)
	user := seedVerifiedUser(t, h, "abusive", "abusive@example.com", goodPassword)

	live, err := middleware.GenerateToken(user.ID, "Viewer")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if code := h.gatedWith(t, live); code != http.StatusOK {
		t.Fatalf("the session was not live to begin with: %d", code)
	}

	// The moderation action, exactly as the admin page issues it: the whole
	// user, with the tier changed. No password.
	if w := h.put(t, "/data_models/users/"+itoaU(user.ID), gin.H{
		"username": "abusive", "email": "abusive@example.com", "access_role": "Blocked",
	}); w.Code != http.StatusOK {
		t.Fatalf("blocking the account: %d %s", w.Code, w.Body.String())
	}

	if code := h.gatedWith(t, live); code != http.StatusUnauthorized {
		t.Fatalf("the blocked account's live session survived: %d — the block is advisory until the token expires", code)
	}
	if got := h.user(t, "abusive").Role.RoleName; got != "Blocked" {
		t.Fatalf("role = %q after the block, want Blocked", got)
	}
}

// The same mechanism read from the other side: a promotion also revokes.
//
// It is one rule, not two — the tier a token carries is stale the moment the
// row changes, and a token asserting less than the account now has is as wrong
// as one asserting more. Making the promotion path an exception would mean the
// rule was about punishment rather than about staleness.
func TestPromotionAlsoRevokes(t *testing.T) {
	h := newRevocationHarness(t)
	loadSignupTestJWTSecret(t)
	user := seedVerifiedUser(t, h, "promoted", "promoted@example.com", goodPassword)

	live, err := middleware.GenerateToken(user.ID, "Viewer")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if code := h.gatedWith(t, live); code != http.StatusOK {
		t.Fatalf("the session was not live to begin with: %d", code)
	}

	if w := h.put(t, "/data_models/users/"+itoaU(user.ID), gin.H{
		"username": "promoted", "email": "promoted@example.com", "access_role": "Member",
	}); w.Code != http.StatusOK {
		t.Fatalf("promoting the account: %d %s", w.Code, w.Body.String())
	}

	if code := h.gatedWith(t, live); code != http.StatusUnauthorized {
		t.Fatalf("a stale-tier token survived a promotion: %d", code)
	}
	// And the account gets a current token by logging in again.
	if w := h.login(t, "promoted", goodPassword); w.Code != http.StatusOK {
		t.Fatalf("login after the tier change: %d %s", w.Code, w.Body.String())
	}
}

// The guard is on the value CHANGING, not on the field being present.
//
// The admin form PUTs the whole user on every save, so a rule keyed on
// presence would sign an account out every time an admin corrected a typo in
// its comment. This asserts the no-op case for the tier specifically; the
// sibling assertion for a comment edit is
// TestAdminEditWithoutPasswordChangeKeepsSessions.
func TestResendingTheSameTierDoesNotRevoke(t *testing.T) {
	h := newRevocationHarness(t)
	loadSignupTestJWTSecret(t)
	user := seedVerifiedUser(t, h, "steady", "steady@example.com", goodPassword)

	live, err := middleware.GenerateToken(user.ID, "Viewer")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	if w := h.put(t, "/data_models/users/"+itoaU(user.ID), gin.H{
		"username": "steady", "email": "steady@example.com",
		"access_role": "Viewer", "comment": "a corrected note",
	}); w.Code != http.StatusOK {
		t.Fatalf("admin update: %d %s", w.Code, w.Body.String())
	}

	if code := h.gatedWith(t, live); code != http.StatusOK {
		t.Fatalf("re-sending the account's existing tier signed it out: %d", code)
	}
	if got := h.user(t, "steady").SessionEpoch; got != 0 {
		t.Errorf("SessionEpoch = %d after a no-op tier write, want 0", got)
	}
}

// TestAdminRosterShowsVerificationState: the column the operator moderates on.
//
// A self-service account that never followed its link is what a bot sign-up
// looks like from the admin page. Before this the roster had no way to show it.
func TestAdminRosterShowsVerificationState(t *testing.T) {
	h := newRevocationHarness(t)

	if w := h.signup(t, "pending", "pending@example.com", goodPassword); w.Code != http.StatusOK {
		t.Fatalf("signup: %d", w.Code)
	}
	pending := h.user(t, "pending")
	if pending.IsEmailVerified() {
		t.Fatal("the account under test should be unverified")
	}

	raw, err := json.Marshal(pending.ToResponse())
	if err != nil {
		t.Fatalf("marshalling the admin DTO: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decoding the admin DTO: %v", err)
	}
	got, present := fields["email_verified_at"]
	if !present {
		t.Fatal("the admin DTO carries no email_verified_at; the roster cannot tell a bot sign-up from a real user")
	}
	if string(got) != "null" {
		t.Fatalf("email_verified_at = %s for an unverified account, want null", got)
	}

	// And a confirmed address reports when, not merely that.
	verified := seedVerifiedUser(t, h, "real", "real@example.com", goodPassword)
	raw, err = json.Marshal(verified.ToResponse())
	if err != nil {
		t.Fatalf("marshalling the verified DTO: %v", err)
	}
	var dto struct {
		EmailVerifiedAt *time.Time `json:"email_verified_at"`
	}
	if err := json.Unmarshal(raw, &dto); err != nil {
		t.Fatalf("decoding the verified DTO: %v", err)
	}
	if dto.EmailVerifiedAt == nil {
		t.Fatal("a verified account reported a null verification time")
	}
}

// The non-admin DTO must not gain the field.
//
// PublicUserResponse exists to keep account facts off the path a Viewer can
// reach (#1380/#3093), and whether a stranger has confirmed their address is
// such a fact. Asserted as ABSENT rather than null, because a null field is
// still a field: it would tell a caller the account exists and is unverified.
func TestPublicUserDTOOmitsVerificationState(t *testing.T) {
	h := newRevocationHarness(t)
	user := seedVerifiedUser(t, h, "real", "real@example.com", goodPassword)

	raw, err := json.Marshal(user.ToPublicResponse())
	if err != nil {
		t.Fatalf("marshalling the public DTO: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decoding the public DTO: %v", err)
	}
	if _, present := fields["email_verified_at"]; present {
		t.Fatalf("the non-admin DTO leaked email_verified_at: %s", raw)
	}
	// Positive control: the same marshal-and-inspect construction does find a
	// field that IS on this DTO, so the absence above is the DTO's shape and
	// not a broken assertion.
	if _, present := fields["username"]; !present {
		t.Fatalf("the public DTO check found no username either; the assertion is not reading the DTO: %s", raw)
	}
}
