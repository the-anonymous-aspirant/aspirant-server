package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

// Tests for the sign-up kill-switch (system_3 #5289).
//
// The harness extends newSignupHarness rather than standing up its own, so the
// switch is exercised against the same router and the same handlers as the
// sign-up flow itself. A kill-switch tested against a stub of the endpoint it
// closes would be testing the stub.

type killSwitchHarness struct {
	*signupHarness
}

func newKillSwitchHarness(t *testing.T) *killSwitchHarness {
	t.Helper()
	h := newSignupHarness(t)

	h.router.GET("/signup/status", GetSignupStatusHandler)
	// The admin identity is set inline rather than by AuthMiddleware: this
	// file's subject is the switch. That the write route is registered inside
	// the admin group — and so unreachable by a Member or Viewer — is the route
	// table's property and is asserted in routes_auth_test.go.
	h.router.PUT("/settings/signup", func(c *gin.Context) {
		c.Set("user_id", uint(1))
		c.Set("role", "Admin")
		c.Next()
	}, PutSignupSettingHandler)

	return &killSwitchHarness{signupHarness: h}
}

func (h *killSwitchHarness) status(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/signup/status", nil))
	return w
}

// setSwitch flips the switch through the endpoint an admin would use, so the
// tests below never write the row directly and never diverge from what the
// admin page actually does.
func (h *killSwitchHarness) setSwitch(t *testing.T, enabled bool) {
	t.Helper()
	w := h.put(t, "/settings/signup", gin.H{"enabled": enabled})
	if w.Code != http.StatusOK {
		t.Fatalf("PUT /settings/signup enabled=%v = %d, want 200: %s", enabled, w.Code, w.Body.String())
	}
}

// TestSignupOpenByDefault pins the deploy-safety property: this change ships
// with no row in site_settings, and a site that had open sign-up before the
// deploy still has it after.
func TestSignupOpenByDefault(t *testing.T) {
	h := newKillSwitchHarness(t)

	var rows int
	if err := h.db.Model(&data_models.SiteSetting{}).Count(&rows).Error; err != nil {
		t.Fatalf("counting settings: %v", err)
	}
	if rows != 0 {
		t.Fatalf("expected no seeded settings row, found %d", rows)
	}

	w := h.status(t)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /signup/status = %d, want 200", w.Code)
	}
	var status struct {
		SignupEnabled bool `json:"signup_enabled"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatalf("decoding status: %v", err)
	}
	if !status.SignupEnabled {
		t.Fatal("sign-up should read as open with no setting row")
	}

	if w := h.signup(t, "opener", "opener@example.com", goodPassword); w.Code != http.StatusOK {
		t.Fatalf("POST /signup = %d, want 200 with the switch untouched", w.Code)
	}
	if got := h.userCount(t); got != 1 {
		t.Fatalf("expected the account to be created, user count = %d", got)
	}
}

// TestSignupClosedRefusesAndCreatesNothing is the switch doing its job: no
// account, no mail.
func TestSignupClosedRefusesAndCreatesNothing(t *testing.T) {
	h := newKillSwitchHarness(t)
	h.setSwitch(t, false)

	w := h.signup(t, "bot", "bot@example.com", goodPassword)
	if w.Code != http.StatusForbidden {
		t.Fatalf("POST /signup with the switch off = %d, want 403: %s", w.Code, w.Body.String())
	}
	if got := h.userCount(t); got != 0 {
		t.Fatalf("expected no account to be created, user count = %d", got)
	}
	if len(h.mail.sent) != 0 {
		t.Fatalf("expected no mail, sent %d", len(h.mail.sent))
	}

	if !bytes.Contains(h.status(t).Body.Bytes(), []byte(`"signup_enabled":false`)) {
		t.Fatalf("GET /signup/status did not report the closed switch: %s", h.status(t).Body.String())
	}
}

// TestClosedSignupIsNotAnExistenceOracle is the reason the check sits before
// the users-table read.
//
// Every branch of SignupHandler below the switch depends on what is in the
// users table. If the refusal were emitted after any of them, a closed site
// would answer differently for a taken username than for a free one — and the
// switch would have opened the hole the whole file exists to close (CWE-204,
// the shape of security-finding #1380 on the login path).
func TestClosedSignupIsNotAnExistenceOracle(t *testing.T) {
	h := newKillSwitchHarness(t)

	if w := h.signup(t, "taken", "taken@example.com", goodPassword); w.Code != http.StatusOK {
		t.Fatalf("seeding the taken account: %d %s", w.Code, w.Body.String())
	}
	h.setSwitch(t, false)

	taken := h.signup(t, "taken", "taken@example.com", goodPassword)
	free := h.signup(t, "nottaken", "nottaken@example.com", goodPassword)

	if taken.Code != free.Code {
		t.Fatalf("status differs by account existence: taken=%d free=%d", taken.Code, free.Code)
	}
	if taken.Body.String() != free.Body.String() {
		t.Fatalf("body differs by account existence:\n taken: %s\n free:  %s", taken.Body.String(), free.Body.String())
	}
	// And the holder of the existing account is not notified either: the
	// request never reached the code that would send that notice, which is the
	// same oracle by a slower route.
	if len(h.mail.to("taken@example.com")) != 1 {
		t.Fatalf("a closed site mailed the existing account holder; want only the original verification mail, got %d",
			len(h.mail.to("taken@example.com")))
	}
}

// TestSignupSettingSurvivesProcessRestart pins the flag as a row rather than a
// package variable. A restart is simulated the only way it can be in-process:
// drop every handle to the state and read it back through a fresh one.
func TestSignupSettingSurvivesProcessRestart(t *testing.T) {
	dsn := "file:killswitch_restart?mode=memory&cache=shared"

	first, err := gorm.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	defer first.Close()
	first.AutoMigrate(&data_models.SiteSetting{})
	if err := data_models.SetSignupEnabled(first, false); err != nil {
		t.Fatalf("closing sign-up: %v", err)
	}

	// A second connection to the same store stands in for the restarted
	// process: no shared Go state, only the row.
	second, err := gorm.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("reopening db: %v", err)
	}
	defer second.Close()

	enabled, err := data_models.SignupEnabled(second)
	if err != nil {
		t.Fatalf("reading the setting back: %v", err)
	}
	if enabled {
		t.Fatal("the switch reopened across a restart")
	}
}

// TestSignupSettingReopens covers the other direction — an operator who closed
// sign-up during an incident has to be able to open it again.
func TestSignupSettingReopens(t *testing.T) {
	h := newKillSwitchHarness(t)

	h.setSwitch(t, false)
	if w := h.signup(t, "early", "early@example.com", goodPassword); w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 while closed, got %d", w.Code)
	}
	h.setSwitch(t, true)
	if w := h.signup(t, "early", "early@example.com", goodPassword); w.Code != http.StatusOK {
		t.Fatalf("expected the account to be creatable after reopening, got %d: %s", w.Code, w.Body.String())
	}
	if got := h.userCount(t); got != 1 {
		t.Fatalf("user count = %d, want 1", got)
	}
}

// TestSignupSettingRejectsAnAmbiguousBody: a body with no 'enabled' field must
// not be read as "close it". The field is a *bool for exactly that reason —
// Go's zero value for the missing key would otherwise be the destructive one.
func TestSignupSettingRejectsAnAmbiguousBody(t *testing.T) {
	h := newKillSwitchHarness(t)

	for _, body := range []any{
		gin.H{"signup_enabled": false}, // right idea, wrong key
		gin.H{},                        // empty object
	} {
		w := h.put(t, "/settings/signup", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("PUT %v = %d, want 400", body, w.Code)
		}
	}

	enabled, err := data_models.SignupEnabled(h.db)
	if err != nil {
		t.Fatalf("reading the setting: %v", err)
	}
	if !enabled {
		t.Fatal("a rejected write changed the setting")
	}
}

// TestKillSwitchDoesNotCloseVerification pins the scope boundary.
//
// Someone who signed up while the door was open must still be able to finish.
// The switch closes the door to NEW accounts; closing verification too would
// strand the people already through it, which is not what "stop the bots
// signing up" asks for.
func TestKillSwitchDoesNotCloseVerification(t *testing.T) {
	h := newKillSwitchHarness(t)

	if w := h.signup(t, "inflight", "inflight@example.com", goodPassword); w.Code != http.StatusOK {
		t.Fatalf("seeding the in-flight account: %d", w.Code)
	}
	token := h.tokenFromMail(t, "inflight@example.com")

	h.setSwitch(t, false)

	w := h.post(t, "/verify-email", gin.H{"token": token})
	if w.Code != http.StatusOK {
		t.Fatalf("POST /verify-email while sign-up is closed = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !h.user(t, "inflight").IsEmailVerified() {
		t.Fatal("the in-flight account was not verified")
	}
}
