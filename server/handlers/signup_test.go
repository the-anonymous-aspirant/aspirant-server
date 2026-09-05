package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aspirant-online/server/data_models"
	"aspirant-online/server/email"
	"aspirant-online/server/middleware"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

// --- harness ----------------------------------------------------------------

// captureSender records what would have been mailed. It satisfies
// email.Sender, so the handlers exercise the same seam they use in production.
type captureSender struct {
	sent []capturedMail
	err  error
}

type capturedMail struct {
	to      string
	subject string
	body    string
}

func (s *captureSender) Send(to, subject, body string) error {
	s.sent = append(s.sent, capturedMail{to: to, subject: subject, body: body})
	return s.err
}

func (s *captureSender) to(addr string) []capturedMail {
	var out []capturedMail
	for _, m := range s.sent {
		if m.to == addr {
			out = append(out, m)
		}
	}
	return out
}

var _ email.Sender = (*captureSender)(nil)

type signupHarness struct {
	router *gin.Engine
	db     *gorm.DB
	mail   *captureSender
}

func newSignupHarness(t *testing.T) *signupHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)

	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	db.AutoMigrate(
		&data_models.Role{}, &data_models.User{},
		&data_models.UserDisplayName{}, &data_models.UserToken{},
		&data_models.BootstrapRecord{},
		// SignupHandler reads the #5289 kill-switch before it touches the users
		// table, and a missing table is a read ERROR, not an absent row — so
		// without this every case in this file 500s. That is the flag failing
		// closed on purpose (a database blip must not reopen a closed site),
		// which makes the harness's job to carry the schema the server has.
		&data_models.SiteSetting{},
	)
	// One connection: sqlite's in-memory driver does not do concurrent
	// writers, and BootstrapUserHandler now opens a transaction. Without this
	// the concurrency test deadlocks every racer instead of serialising them,
	// which would read as "the guard rejected everyone" rather than "exactly
	// one won".
	db.DB().SetMaxOpenConns(1)
	if err := data_models.SeedRoles(db); err != nil {
		t.Fatalf("SeedRoles: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	sender := &captureSender{}
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("db", db); c.Next() })
	r.Use(func(c *gin.Context) { c.Set("mailer", email.Sender(sender)); c.Next() })
	r.POST("/signup", SignupHandler)
	r.POST("/verify-email", VerifyEmailHandler)
	r.POST("/login", LoginHandler)

	return &signupHarness{router: r, db: db, mail: sender}
}

func (h *signupHarness) post(t *testing.T, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w
}

func (h *signupHarness) put(t *testing.T, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w
}

func (h *signupHarness) signup(t *testing.T, username, address, password string) *httptest.ResponseRecorder {
	t.Helper()
	return h.post(t, "/signup", gin.H{"username": username, "email": address, "password": password})
}

// tokenFromMail pulls the token out of the most recent message to an address.
// It reads the mail the way a person would, so a broken link in the body fails
// the test rather than passing on a token taken from the database.
func (h *signupHarness) tokenFromMail(t *testing.T, addr string) string {
	t.Helper()
	msgs := h.mail.to(addr)
	if len(msgs) == 0 {
		t.Fatalf("no mail was sent to %s", addr)
	}
	body := msgs[len(msgs)-1].body
	_, after, ok := strings.Cut(body, "?token=")
	if !ok {
		t.Fatalf("no token link in the mail to %s:\n%s", addr, body)
	}
	token, _, _ := strings.Cut(after, "\n")
	return strings.TrimSpace(token)
}

func (h *signupHarness) user(t *testing.T, username string) *data_models.User {
	t.Helper()
	var u data_models.User
	if err := h.db.Preload("Role").Where("username = ?", username).First(&u).Error; err != nil {
		t.Fatalf("user %q not found: %v", username, err)
	}
	return &u
}

func (h *signupHarness) userCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.db.Model(&data_models.User{}).Count(&n).Error; err != nil {
		t.Fatalf("Count: %v", err)
	}
	return n
}

const goodPassword = "correct-horse-battery"

// --- sign-up ----------------------------------------------------------------

func TestSignupCreatesUnverifiedViewer(t *testing.T) {
	h := newSignupHarness(t)

	w := h.signup(t, "newcomer", "newcomer@example.com", goodPassword)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	u := h.user(t, "newcomer")
	if u.Role.RoleName != "Viewer" {
		t.Errorf("role = %q, want Viewer", u.Role.RoleName)
	}
	if u.IsEmailVerified() {
		t.Error("a new sign-up is already verified; it must not be")
	}
	if u.Password == goodPassword {
		t.Error("the password was stored in the clear")
	}
	if err := u.CheckPassword(goodPassword); err != nil {
		t.Errorf("the stored hash does not match the chosen password: %v", err)
	}

	// The display-name row is what public surfaces render; without it a new
	// account shows as blank (security-finding #3094).
	if got := data_models.CurrentDisplayName(h.db, u.ID); got != "newcomer" {
		t.Errorf("display name = %q, want newcomer", got)
	}

	msgs := h.mail.to("newcomer@example.com")
	if len(msgs) != 1 {
		t.Fatalf("sent %d messages, want 1", len(msgs))
	}
	if !strings.Contains(msgs[0].body, "/verify-email?token=") {
		t.Errorf("verification mail has no link:\n%s", msgs[0].body)
	}
}

// Privilege escalation at the front door. BootstrapUserHandler reads a role
// from the body, which is right there and would be catastrophic here.
func TestSignupIgnoresACallerSuppliedRole(t *testing.T) {
	h := newSignupHarness(t)

	w := h.post(t, "/signup", gin.H{
		"username": "sneaky", "email": "sneaky@example.com",
		"password": goodPassword, "access_role": "Admin", "role": "Admin", "RoleID": 1,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := h.user(t, "sneaky").Role.RoleName; got != "Viewer" {
		t.Fatalf("role = %q, want Viewer — a caller chose their own tier", got)
	}
}

// --- the existence oracle ---------------------------------------------------

// The load-bearing property: a taken username or address must be
// indistinguishable from a fresh sign-up. This codebase has leaked account
// existence before (CWE-204, finding #1380), which is why LoginHandler carries
// a fixed dummy bcrypt hash.
func TestSignupDoesNotRevealThatAnAccountExists(t *testing.T) {
	h := newSignupHarness(t)

	if w := h.signup(t, "taken", "taken@example.com", goodPassword); w.Code != http.StatusOK {
		t.Fatalf("seed signup failed: %d", w.Code)
	}
	fresh := h.signup(t, "brandnew", "brandnew@example.com", goodPassword)
	before := h.userCount(t)

	cases := []struct {
		name              string
		username, address string
	}{
		{"username taken", "taken", "different@example.com"},
		{"email taken", "differentname", "taken@example.com"},
		{"both taken", "taken", "taken@example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := h.signup(t, tc.username, tc.address, goodPassword)

			if w.Code != fresh.Code {
				t.Errorf("status = %d, want %d (identical to a fresh sign-up)", w.Code, fresh.Code)
			}
			if w.Body.String() != fresh.Body.String() {
				t.Errorf("body differs from a fresh sign-up:\n got: %s\nwant: %s", w.Body.String(), fresh.Body.String())
			}
			if ct, want := w.Header().Get("Content-Type"), fresh.Header().Get("Content-Type"); ct != want {
				t.Errorf("Content-Type = %q, want %q", ct, want)
			}
			if n := h.userCount(t); n != before {
				t.Errorf("user count moved from %d to %d — an account was created", before, n)
			}
		})
	}
}

// The notice goes to the address on file, never to the address in the request.
// In the username-collision case the requester supplied a different address,
// and mailing them would confirm the username is taken — the exact leak the
// identical response is there to prevent.
func TestSignupNotifiesTheOwnerNotTheRequester(t *testing.T) {
	h := newSignupHarness(t)

	if w := h.signup(t, "owner", "owner@example.com", goodPassword); w.Code != http.StatusOK {
		t.Fatalf("seed signup failed: %d", w.Code)
	}
	h.mail.sent = nil

	if w := h.signup(t, "owner", "attacker@example.com", goodPassword); w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	if got := len(h.mail.to("attacker@example.com")); got != 0 {
		t.Errorf("sent %d messages to the requester; want 0", got)
	}
	notices := h.mail.to("owner@example.com")
	if len(notices) != 1 {
		t.Fatalf("sent %d messages to the account owner, want 1", len(notices))
	}
	if strings.Contains(notices[0].body, "/verify-email?token=") {
		t.Error("the duplicate-signup notice carries a verification link — it must not create or confirm anything")
	}
}

// A send failure must not become an oracle either: "we couldn't mail that
// address" and "we did mail that address" answer the same question.
func TestSignupAnswersTheSameWhenSendingFails(t *testing.T) {
	h := newSignupHarness(t)
	good := h.signup(t, "first", "first@example.com", goodPassword)

	h.mail.err = errTestSendFailed
	w := h.signup(t, "second", "second@example.com", goodPassword)

	if w.Code != good.Code || w.Body.String() != good.Body.String() {
		t.Errorf("a send failure changed the response:\n got %d %s\nwant %d %s",
			w.Code, w.Body.String(), good.Code, good.Body.String())
	}
}

var errTestSendFailed = &testError{"relay unavailable"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

// --- input validation -------------------------------------------------------

// These DO answer distinctly: they describe the request, not the database.
func TestSignupValidatesInput(t *testing.T) {
	cases := []struct {
		name                        string
		username, address, password string
	}{
		{"blank username", "   ", "a@example.com", goodPassword},
		{"missing username", "", "a@example.com", goodPassword},
		{"unparseable email", "user", "not an address", goodPassword},
		{"missing email", "user", "", goodPassword},
		{"short password", "user", "a@example.com", "short"},
		{"missing password", "user", "a@example.com", ""},
		// bcrypt ignores everything past 72 bytes. Accepting a longer password
		// would authenticate a user by its first 72 bytes while they believed
		// otherwise — and would keep authenticating after they changed the tail.
		{"password past bcrypt's 72-byte limit", "user", "a@example.com", strings.Repeat("x", 73)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newSignupHarness(t)
			w := h.signup(t, tc.username, tc.address, tc.password)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body: %s", w.Code, w.Body.String())
			}
			if n := h.userCount(t); n != 0 {
				t.Errorf("%d users created from an invalid request", n)
			}
		})
	}
}

// Exactly 72 bytes is the boundary bcrypt accepts, so it must be allowed.
func TestSignupAcceptsExactly72BytePassword(t *testing.T) {
	h := newSignupHarness(t)
	pw := strings.Repeat("x", 72)

	if w := h.signup(t, "longpw", "longpw@example.com", pw); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if err := h.user(t, "longpw").CheckPassword(pw); err != nil {
		t.Errorf("72-byte password does not verify: %v", err)
	}
}

// --- verification -----------------------------------------------------------

func TestVerifyEmailConfirmsTheAccount(t *testing.T) {
	h := newSignupHarness(t)
	if w := h.signup(t, "newcomer", "newcomer@example.com", goodPassword); w.Code != http.StatusOK {
		t.Fatalf("signup: %d", w.Code)
	}
	token := h.tokenFromMail(t, "newcomer@example.com")

	if w := h.post(t, "/verify-email", gin.H{"token": token}); w.Code != http.StatusOK {
		t.Fatalf("verify status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if !h.user(t, "newcomer").IsEmailVerified() {
		t.Fatal("the account is still unverified after following the link")
	}
}

func TestVerifyEmailRejectsBadTokensIdentically(t *testing.T) {
	h := newSignupHarness(t)
	if w := h.signup(t, "newcomer", "newcomer@example.com", goodPassword); w.Code != http.StatusOK {
		t.Fatalf("signup: %d", w.Code)
	}
	token := h.tokenFromMail(t, "newcomer@example.com")

	first := h.post(t, "/verify-email", gin.H{"token": token})
	if first.Code != http.StatusOK {
		t.Fatalf("first verify: %d", first.Code)
	}

	replay := h.post(t, "/verify-email", gin.H{"token": token})
	unknown := h.post(t, "/verify-email", gin.H{"token": "not-a-real-token"})

	if replay.Code != http.StatusBadRequest {
		t.Errorf("replay status = %d, want 400", replay.Code)
	}
	if replay.Body.String() != unknown.Body.String() {
		t.Errorf("a replayed token is distinguishable from an unknown one:\n replay: %s\nunknown: %s",
			replay.Body.String(), unknown.Body.String())
	}
}

// A verification token must not be spendable as a password reset, and vice
// versa — otherwise the weaker flow mints credentials for the stronger one.
func TestVerifyEmailRejectsAPasswordResetToken(t *testing.T) {
	h := newSignupHarness(t)
	if w := h.signup(t, "newcomer", "newcomer@example.com", goodPassword); w.Code != http.StatusOK {
		t.Fatalf("signup: %d", w.Code)
	}
	u := h.user(t, "newcomer")

	reset, err := data_models.IssueUserToken(h.db, u.ID, data_models.PurposePasswordReset)
	if err != nil {
		t.Fatalf("IssueUserToken: %v", err)
	}

	if w := h.post(t, "/verify-email", gin.H{"token": reset}); w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — a reset token verified an address", w.Code)
	}
	if h.user(t, "newcomer").IsEmailVerified() {
		t.Fatal("a password-reset token confirmed the address")
	}
}

// --- the login gate ---------------------------------------------------------

func TestUnverifiedAccountCannotLogIn(t *testing.T) {
	h := newSignupHarness(t)
	loadSignupTestJWTSecret(t)

	if w := h.signup(t, "newcomer", "newcomer@example.com", goodPassword); w.Code != http.StatusOK {
		t.Fatalf("signup: %d", w.Code)
	}

	unverified := h.post(t, "/login", gin.H{"username": "newcomer", "password": goodPassword})
	if unverified.Code != http.StatusUnauthorized {
		t.Fatalf("unverified login status = %d, want 401", unverified.Code)
	}

	// Indistinguishable from a wrong password: otherwise "this account exists
	// and you have its password" becomes an observable outcome.
	wrongPassword := h.post(t, "/login", gin.H{"username": "newcomer", "password": "definitely-wrong"})
	if unverified.Code != wrongPassword.Code || unverified.Body.String() != wrongPassword.Body.String() {
		t.Errorf("an unverified login is distinguishable from a wrong password:\nunverified: %d %s\n     wrong: %d %s",
			unverified.Code, unverified.Body.String(), wrongPassword.Code, wrongPassword.Body.String())
	}
	if len(unverified.Result().Cookies()) != 0 {
		t.Error("a cookie was issued to an unverified account")
	}
}

func TestVerifiedAccountCanLogIn(t *testing.T) {
	h := newSignupHarness(t)
	loadSignupTestJWTSecret(t)

	if w := h.signup(t, "newcomer", "newcomer@example.com", goodPassword); w.Code != http.StatusOK {
		t.Fatalf("signup: %d", w.Code)
	}
	token := h.tokenFromMail(t, "newcomer@example.com")
	if w := h.post(t, "/verify-email", gin.H{"token": token}); w.Code != http.StatusOK {
		t.Fatalf("verify: %d", w.Code)
	}

	w := h.post(t, "/login", gin.H{"username": "newcomer", "password": goodPassword})
	if w.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var found bool
	for _, ck := range w.Result().Cookies() {
		if ck.Name == "auth_token" && ck.Value != "" {
			found = true
		}
	}
	if !found {
		t.Error("no auth_token cookie was issued to a verified account")
	}
}

// The migration's risk, and the test that would have caught locking out the
// operator. It calls the same MigrateEmailVerified that AutoMigrate calls — a
// test re-typing the SQL would pass while the shipped statement was wrong.
func TestMigrateEmailVerifiedLetsPreExistingAccountsLogIn(t *testing.T) {
	h := newSignupHarness(t)
	loadSignupTestJWTSecret(t)

	// An account as it exists before this feature: created by an admin, never
	// mailed a verification link, email_verified_at NULL.
	role, err := data_models.GetRoleByName(h.db, "Admin")
	if err != nil {
		t.Fatalf("GetRoleByName: %v", err)
	}
	legacy := data_models.User{Username: "operator", Email: "op@example.com", RoleID: role.ID}
	if err := legacy.HashPassword(goodPassword); err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := legacy.CreateUser(h.db); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if w := h.post(t, "/login", gin.H{"username": "operator", "password": goodPassword}); w.Code != http.StatusUnauthorized {
		t.Fatalf("pre-migration login status = %d, want 401 (the lockout this migration exists to prevent)", w.Code)
	}

	// The harness already created the column, so remove it first to reproduce
	// the state a real deploy migrates FROM: the column absent, every account
	// predating it.
	dropColumn(t, h.db, "users", "email_verified_at")
	if h.db.Dialect().HasColumn("users", "email_verified_at") {
		t.Fatal("the column survived the rebuild; this test would not exercise the migration path")
	}

	if err := data_models.MigrateEmailVerified(h.db); err != nil {
		t.Fatalf("MigrateEmailVerified: %v", err)
	}

	if w := h.post(t, "/login", gin.H{"username": "operator", "password": goodPassword}); w.Code != http.StatusOK {
		t.Fatalf("post-migration login status = %d, want 200 — the migration locks out existing users", w.Code)
	}
}

// The defect the security review of PR #102 caught (finding #5226, severity
// high), and the regression test for it.
//
// An unconditional every-boot backfill cannot tell a pre-existing admin
// account from a pending sign-up — both have email_verified_at NULL — so it
// stamps the sign-up verified at the next restart. The bot filter and the
// proof of address ownership are bypassed on a timer, with nothing in the logs
// to say so. The account may have been created at an address the person
// signing up does not own.
//
// The earlier idempotency test did not catch this because it only asserted
// that an already-VERIFIED row survived a second run. The property that
// matters is the opposite one: a NULL that should stay NULL is left alone.
func TestSecondMigrationDoesNotVerifyAPendingSignup(t *testing.T) {
	h := newSignupHarness(t)
	loadSignupTestJWTSecret(t)

	// A genuine, unverified sign-up. The link was never followed.
	if w := h.signup(t, "pending", "pending@example.com", goodPassword); w.Code != http.StatusOK {
		t.Fatalf("signup: %d %s", w.Code, w.Body.String())
	}
	if h.user(t, "pending").IsEmailVerified() {
		t.Fatal("a fresh sign-up is already verified")
	}

	// Every subsequent boot runs the migration again.
	for i := 0; i < 3; i++ {
		if err := data_models.MigrateEmailVerified(h.db); err != nil {
			t.Fatalf("MigrateEmailVerified (boot %d): %v", i+2, err)
		}
	}

	if h.user(t, "pending").IsEmailVerified() {
		t.Fatal("a restart marked an unverified sign-up verified — the verification gate is disabled on a timer")
	}
	if w := h.post(t, "/login", gin.H{"username": "pending", "password": goodPassword}); w.Code != http.StatusUnauthorized {
		t.Fatalf("login status = %d, want 401 — an unverified account can log in after a restart", w.Code)
	}

	// And the link still works afterwards: the guard must not have broken the
	// ordinary path.
	token := h.tokenFromMail(t, "pending@example.com")
	if w := h.post(t, "/verify-email", gin.H{"token": token}); w.Code != http.StatusOK {
		t.Fatalf("verify after restarts: %d %s", w.Code, w.Body.String())
	}
	if !h.user(t, "pending").IsEmailVerified() {
		t.Fatal("the account did not verify after the link was followed")
	}
}

// A real verification timestamp must survive a migration run unchanged.
func TestMigrationDoesNotOverwriteARealVerification(t *testing.T) {
	h := newSignupHarness(t)

	if w := h.signup(t, "verified", "verified@example.com", goodPassword); w.Code != http.StatusOK {
		t.Fatalf("signup: %d", w.Code)
	}
	token := h.tokenFromMail(t, "verified@example.com")
	if w := h.post(t, "/verify-email", gin.H{"token": token}); w.Code != http.StatusOK {
		t.Fatalf("verify: %d", w.Code)
	}

	before := h.user(t, "verified").EmailVerifiedAt
	if err := data_models.MigrateEmailVerified(h.db); err != nil {
		t.Fatalf("MigrateEmailVerified: %v", err)
	}
	after := h.user(t, "verified").EmailVerifiedAt

	if before == nil || after == nil || !before.Equal(*after) {
		t.Errorf("the migration changed a real verification timestamp: %v -> %v", before, after)
	}
}

func loadSignupTestJWTSecret(t *testing.T) {
	t.Helper()
	t.Setenv("JWT_SECRET", "test-jwt-secret-must-be-at-least-32-bytes-long!!")
	if err := middleware.LoadJWTSecret(); err != nil {
		t.Fatalf("LoadJWTSecret: %v", err)
	}
}

// dropColumn rebuilds a table without one column.
//
// The bundled SQLite predates ALTER TABLE ... DROP COLUMN, so this uses the
// copy-and-rename dance. The rebuilt table is declared with each surviving
// column's TYPE, not created by CREATE TABLE ... AS SELECT: that shortcut
// produces columns with no type affinity, so a datetime comes back as a string
// and gorm fails to scan it — a broken fixture that looks exactly like a
// broken migration. Column names and types come from the live schema, so
// adding a field to User later does not silently drop it here and leave a test
// asserting against a schema nobody ships.
func dropColumn(t *testing.T, db *gorm.DB, table, column string) {
	t.Helper()

	rows, err := db.Raw("PRAGMA table_info(" + table + ")").Rows()
	if err != nil {
		t.Fatalf("reading %s schema: %v", table, err)
	}
	var (
		decls []string
		names []string
	)
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             interface{}
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			t.Fatalf("scanning %s schema: %v", table, err)
		}
		if name == column {
			continue
		}
		decl := name + " " + ctype
		if pk == 1 {
			decl += " PRIMARY KEY"
		}
		decls = append(decls, decl)
		names = append(names, name)
	}
	rows.Close()
	if len(names) == 0 {
		t.Fatalf("no columns found on %s", table)
	}

	cols := strings.Join(names, ", ")
	for _, stmt := range []string{
		"CREATE TABLE " + table + "_rebuild (" + strings.Join(decls, ", ") + ")",
		"INSERT INTO " + table + "_rebuild (" + cols + ") SELECT " + cols + " FROM " + table,
		"DROP TABLE " + table,
		"ALTER TABLE " + table + "_rebuild RENAME TO " + table,
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("rebuilding %s (%s): %v", table, stmt, err)
		}
	}
}
