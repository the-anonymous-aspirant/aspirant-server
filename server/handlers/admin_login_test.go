package handlers

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aspirant-online/server/data_models"
	"aspirant-online/server/internal/accesstest"
	"aspirant-online/server/middleware"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

// The Admin-tier gate (system_3 #8798, #9269): /login/admin issues Admin only
// behind a valid Cloudflare Access assertion or the break-glass token, and
// /login caps an Admin account at Member, whenever the gate is on.

const (
	gatePassword   = "s3cret!"
	gateBreakGlass = "a-long-break-glass-token-for-the-tests"
)

type gateFixture struct {
	db     *gorm.DB
	router *gin.Engine
	keys   *accesstest.Keys
}

func newGateFixture(t *testing.T) *gateFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	db.AutoMigrate(&data_models.Role{}, &data_models.User{}, &data_models.SiteSetting{})
	for _, seed := range []struct{ user, role string }{{"alice", "Admin"}, {"bob", "Member"}} {
		role := data_models.Role{RoleName: seed.role}
		if err := db.Where(data_models.Role{RoleName: seed.role}).FirstOrCreate(&role).Error; err != nil {
			t.Fatalf("seed role: %v", err)
		}
		user := data_models.User{Username: seed.user, Email: seed.user + "@example.com", RoleID: role.ID}
		if err := user.HashPassword(gatePassword); err != nil {
			t.Fatalf("hash: %v", err)
		}
		if err := db.Create(&user).Error; err != nil {
			t.Fatalf("seed user: %v", err)
		}
		// Verified directly, as login_test.go does: the gate under test is not
		// the email one.
		if err := db.Model(&user).Update("email_verified_at", time.Now()).Error; err != nil {
			t.Fatalf("verify fixture: %v", err)
		}
	}

	keys := accesstest.NewKeys(t, "kid-1")
	prev := middleware.AccessCertsURL
	middleware.AccessCertsURL = func(string) string { return keys.URL() }
	t.Cleanup(func() { middleware.AccessCertsURL = prev })

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("db", db); c.Next() })
	router.POST("/login", LoginHandler)
	router.POST("/login/admin", AdminLoginHandler)
	router.GET("/health", HealthCheckHandler)
	return &gateFixture{db: db, router: router, keys: keys}
}

func setGate(t *testing.T, team, aud, email, breakGlass string) {
	t.Helper()
	t.Setenv(middleware.AccessTeamDomainEnv, team)
	t.Setenv(middleware.AccessAUDEnv, aud)
	t.Setenv(middleware.AccessAdminEmailEnv, email)
	t.Setenv(middleware.BreakGlassTokenEnv, breakGlass)
}

func enforce(t *testing.T) {
	setGate(t, accesstest.Team, accesstest.AUD, accesstest.AdminEmail, gateBreakGlass)
}

// signIn posts credentials and returns the status and the role claim of the
// cookie it set ("" when none).
func (f *gateFixture) signIn(t *testing.T, path, user, password string, headers map[string]string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": user, "password": password})
	req, _ := http.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == "auth_token" && cookie.Value != "" {
			return w.Code, roleClaim(t, cookie.Value)
		}
	}
	return w.Code, ""
}

// roleClaim reads the token's role claim without verifying it; the signature
// is the auth middleware's concern and has its own tests.
func roleClaim(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", token)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("claims: %v", err)
	}
	role, _ := claims["role"].(string)
	return role
}

func (f *gateFixture) assertion(t *testing.T) map[string]string {
	return map[string]string{middleware.AccessAssertionHeader: f.keys.Mint(t, accesstest.Claims())}
}

func breakGlass() map[string]string {
	return map[string]string{middleware.BreakGlassHeader: gateBreakGlass}
}

func expect(t *testing.T, what string, gotCode int, gotRole string, wantCode int, wantRole string) {
	t.Helper()
	if gotCode != wantCode || gotRole != wantRole {
		t.Fatalf("%s: status %d role %q, want %d %q", what, gotCode, gotRole, wantCode, wantRole)
	}
}

func TestAdminGateOff(t *testing.T) {
	f := newGateFixture(t)
	setGate(t, "", "", "", "")

	code, role := f.signIn(t, "/login", "alice", gatePassword, nil)
	expect(t, "/login as admin", code, role, http.StatusOK, "Admin")
	code, role = f.signIn(t, "/login/admin", "alice", gatePassword, nil)
	expect(t, "/login/admin, password alone", code, role, http.StatusOK, "Admin")
	code, role = f.signIn(t, "/login/admin", "bob", gatePassword, nil)
	expect(t, "/login/admin as a member", code, role, http.StatusForbidden, "")
	code, role = f.signIn(t, "/login/admin", "alice", "wrong", nil)
	expect(t, "/login/admin, wrong password", code, role, http.StatusUnauthorized, "")
}

func TestAdminGateEnforced(t *testing.T) {
	f := newGateFixture(t)
	enforce(t)

	code, role := f.signIn(t, "/login", "alice", gatePassword, nil)
	expect(t, "/login as admin is capped", code, role, http.StatusOK, "Member")
	code, role = f.signIn(t, "/login", "bob", gatePassword, nil)
	expect(t, "/login as a member", code, role, http.StatusOK, "Member")

	code, role = f.signIn(t, "/login/admin", "alice", gatePassword, f.assertion(t))
	expect(t, "assertion + password", code, role, http.StatusOK, "Admin")
	code, role = f.signIn(t, "/login/admin", "alice", gatePassword, breakGlass())
	expect(t, "break-glass + password", code, role, http.StatusOK, "Admin")

	code, role = f.signIn(t, "/login/admin", "alice", gatePassword, nil)
	expect(t, "password alone", code, role, http.StatusForbidden, "")
	code, role = f.signIn(t, "/login/admin", "alice", gatePassword, map[string]string{middleware.BreakGlassHeader: "guess"})
	expect(t, "wrong break-glass token", code, role, http.StatusForbidden, "")
	code, role = f.signIn(t, "/login/admin", "alice", gatePassword, map[string]string{middleware.AccessAssertionHeader: "not.a.jwt"})
	expect(t, "garbage assertion", code, role, http.StatusForbidden, "")

	// The gate runs before the password: with no second factor a wrong
	// password gets the gate's 403, not the password's 401, so the admin
	// password cannot be guessed without the PIN.
	code, role = f.signIn(t, "/login/admin", "alice", "wrong", nil)
	expect(t, "wrong password, no second factor", code, role, http.StatusForbidden, "")
	// The second factor never replaces the password.
	code, role = f.signIn(t, "/login/admin", "alice", "wrong", f.assertion(t))
	expect(t, "assertion + wrong password", code, role, http.StatusUnauthorized, "")
	code, role = f.signIn(t, "/login/admin", "alice", "wrong", breakGlass())
	expect(t, "break-glass + wrong password", code, role, http.StatusUnauthorized, "")

	code, role = f.signIn(t, "/login/admin", "bob", gatePassword, f.assertion(t))
	expect(t, "a member through Access", code, role, http.StatusForbidden, "")
}

func TestAdminGateMisconfiguredFailsClosed(t *testing.T) {
	f := newGateFixture(t)
	setGate(t, "", accesstest.AUD, "", gateBreakGlass)

	code, role := f.signIn(t, "/login", "alice", gatePassword, nil)
	expect(t, "/login as admin is capped", code, role, http.StatusOK, "Member")
	code, role = f.signIn(t, "/login/admin", "alice", gatePassword, f.assertion(t))
	expect(t, "assertion is not enough", code, role, http.StatusForbidden, "")
	code, role = f.signIn(t, "/login/admin", "alice", gatePassword, breakGlass())
	expect(t, "break-glass still works", code, role, http.StatusOK, "Admin")
}

func TestAdminGateEnforcedWithoutBreakGlassToken(t *testing.T) {
	f := newGateFixture(t)
	setGate(t, accesstest.Team, accesstest.AUD, accesstest.AdminEmail, "")

	code, role := f.signIn(t, "/login/admin", "alice", gatePassword, map[string]string{middleware.BreakGlassHeader: ""})
	expect(t, "empty header with no token configured", code, role, http.StatusForbidden, "")
	code, role = f.signIn(t, "/login/admin", "alice", gatePassword, f.assertion(t))
	expect(t, "assertion + password", code, role, http.StatusOK, "Admin")
}

func TestHealthPublishesTheAdminGateMode(t *testing.T) {
	f := newGateFixture(t)
	for _, tc := range []struct {
		name string
		set  func()
		want string
	}{
		{"off", func() { setGate(t, "", "", "", "") }, "off"},
		{"enforced", func() { enforce(t) }, "enforced"},
		{"misconfigured", func() { setGate(t, accesstest.Team, "", "", "") }, "misconfigured"},
	} {
		tc.set()
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/health", nil)
		f.router.ServeHTTP(w, req)
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: health body: %v", tc.name, err)
		}
		if body["admin_second_factor"] != tc.want {
			t.Fatalf("%s: admin_second_factor = %v, want %q", tc.name, body["admin_second_factor"], tc.want)
		}
	}
}

func TestReconcileAdminGateModeRevokesAdminSessionsOnEveryChange(t *testing.T) {
	f := newGateFixture(t)
	epochs := func() (alice, bob uint) {
		var a, b data_models.User
		f.db.Where("username = ?", "alice").First(&a)
		f.db.Where("username = ?", "bob").First(&b)
		return a.SessionEpoch, b.SessionEpoch
	}
	step := func(name string, set func(), wantAlice uint) {
		t.Helper()
		set()
		if _, err := ReconcileAdminGateMode(f.db); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		alice, bob := epochs()
		if alice != wantAlice || bob != 0 {
			t.Fatalf("%s: epochs alice=%d bob=%d, want alice=%d bob=0", name, alice, bob, wantAlice)
		}
	}
	off := func() { setGate(t, "", "", "", "") }

	step("first start, off: nothing to revoke", off, 0)
	step("same mode again", off, 0)
	step("off -> enforced", func() { enforce(t) }, 1)
	step("enforced again", func() { enforce(t) }, 1)
	step("enforced -> off (a lost .env value)", off, 2)
	step("off -> misconfigured", func() { setGate(t, accesstest.Team, "", "", "") }, 3)

	var stored data_models.SiteSetting
	f.db.Where("key = ?", AdminGateModeSetting).First(&stored)
	if stored.Value != "misconfigured" {
		t.Fatalf("stored mode = %q, want misconfigured", stored.Value)
	}
}

func TestReconcileAdminGateModeFirstStartEnforcedRevokes(t *testing.T) {
	f := newGateFixture(t)
	enforce(t)
	if _, err := ReconcileAdminGateMode(f.db); err != nil {
		t.Fatal(err)
	}
	var alice data_models.User
	f.db.Where("username = ?", "alice").First(&alice)
	if alice.SessionEpoch != 1 {
		t.Fatalf("alice's epoch = %d, want 1: an Admin cookie from before the gate must not survive it", alice.SessionEpoch)
	}
	var stored data_models.SiteSetting
	if err := f.db.Where("key = ?", AdminGateModeSetting).First(&stored).Error; err != nil || stored.Value != "enforced" {
		t.Fatalf("stored mode = %q (%v), want enforced", stored.Value, err)
	}
}
