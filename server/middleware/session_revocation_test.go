package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aspirant-online/server/data_models"

	"github.com/golang-jwt/jwt/v5"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

// Session revocation (system_3 #5224).
//
// A JWT here is stateless with a 24h expiry and there is no denylist, so before
// this a password change left every session already issued alive for up to a
// day — including the session of whoever the password was being changed to lock
// out. Revocation compares each token's `iat` against a per-user watermark.

func revokeTestDB(t *testing.T, ids ...uint) *gorm.DB {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	db.AutoMigrate(&data_models.User{})
	for _, id := range ids {
		u := data_models.User{Username: "u", Email: "u@example.com"}
		u.ID = id
		u.Username = "u" + itoa(id)
		u.Email = "u" + itoa(id) + "@example.com"
		if err := db.Create(&u).Error; err != nil {
			t.Fatalf("seeding user %d: %v", id, err)
		}
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func itoa(u uint) string {
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

// gatedGET runs one request through AuthMiddleware and returns the status.
func gatedGET(t *testing.T, db *gorm.DB, token string) int {
	t.Helper()
	r := authRouterWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/gated", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

// --- the property -----------------------------------------------------------

// The whole point: a token minted before a revocation stops working, and one
// minted after does not.
func TestRevokedTokenIsRefusedAndAFreshOneIsNot(t *testing.T) {
	db := revokeTestDB(t, 7)

	before, err := GenerateToken(7, "Member")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if code := gatedGET(t, db, before); code != http.StatusOK {
		t.Fatalf("pre-revocation status = %d, want 200", code)
	}

	// No sleep, deliberately: mint, revoke and re-mint all inside one second.
	// That is the case second-resolution `iat` could not resolve — and the two
	// shipped attempts each got one side of it wrong — so running same-second
	// is exactly the assertion worth making now (system_3 #5275).

	// The real function, not a hand-written UPDATE.
	if err := data_models.RevokeSessions(db, 7); err != nil {
		t.Fatalf("RevokeSessions: %v", err)
	}
	if code := gatedGET(t, db, before); code != http.StatusUnauthorized {
		t.Fatalf("post-revocation status = %d, want 401 — the old session survived", code)
	}

	// Minted at the user's CURRENT epoch, which is what LoginHandler does after
	// reading the row. Using the epoch-blind GenerateToken here would mint at 0
	// and be refused — correctly, since that is the fail-safe default.
	epoch, err := data_models.SessionEpochFor(db, 7)
	if err != nil {
		t.Fatalf("SessionEpochFor: %v", err)
	}
	after, err := GenerateTokenAtEpoch(7, "Member", epoch)
	if err != nil {
		t.Fatalf("GenerateTokenAtEpoch: %v", err)
	}
	if code := gatedGET(t, db, after); code != http.StatusOK {
		t.Fatalf("post-revocation NEW token status = %d, want 200 — recovery left the account unusable", code)
	}
}

// Revocation is per user. Resetting one person's password must not sign
// everybody else out.
func TestRevocationDoesNotTouchOtherUsers(t *testing.T) {
	db := revokeTestDB(t, 7, 8)

	otherToken, err := GenerateToken(8, "Member")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if err := data_models.RevokeSessions(db, 7); err != nil {
		t.Fatalf("RevokeSessions: %v", err)
	}

	if code := gatedGET(t, db, otherToken); code != http.StatusOK {
		t.Fatalf("another user's session status = %d, want 200 — one reset signed out everyone", code)
	}
}

// Epoch 0 is never-revoked, which is every account that predates the column and
// also what a token with no epoch claim reads as — so the migration needs no
// backfill and nobody is signed out by the deploy.
func TestEpochZeroAcceptsEverything(t *testing.T) {
	db := revokeTestDB(t, 7)

	token, err := GenerateToken(7, "Member")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	var user data_models.User
	if err := db.Where("id = ?", 7).First(&user).Error; err != nil {
		t.Fatalf("reading user: %v", err)
	}
	if user.SessionEpoch != 0 {
		t.Fatalf("a freshly created user is already at epoch %d; the deploy would sign everyone out", user.SessionEpoch)
	}
	if code := gatedGET(t, db, token); code != http.StatusOK {
		t.Fatalf("status = %d, want 200 on a never-revoked account", code)
	}
}

// Ordering in both directions, with no delay of any kind between the calls.
//
// This is the case three timestamp-based attempts each got half-wrong
// (system_3 #5275): comparing two independently-taken clock readings cannot
// order events that fall inside the same tick, at any resolution. An epoch has
// no tick — the database orders the increment against the read — so both of
// these hold however fast the calls follow one another.
func TestSessionMintedJustAfterRevocationIsAccepted(t *testing.T) {
	db := revokeTestDB(t, 7)

	if err := data_models.RevokeSessions(db, 7); err != nil {
		t.Fatalf("RevokeSessions: %v", err)
	}
	epoch, err := data_models.SessionEpochFor(db, 7)
	if err != nil {
		t.Fatalf("SessionEpochFor: %v", err)
	}
	// The owner signing back in a moment later, as LoginHandler does.
	token, err := GenerateTokenAtEpoch(7, "Member", epoch)
	if err != nil {
		t.Fatalf("GenerateTokenAtEpoch: %v", err)
	}
	if code := gatedGET(t, db, token); code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the owner's fresh session was refused", code)
	}
}

func TestSessionMintedJustBeforeRevocationIsRefused(t *testing.T) {
	db := revokeTestDB(t, 7)

	// The session someone else is already holding, minted at the user's
	// current epoch.
	epoch, err := data_models.SessionEpochFor(db, 7)
	if err != nil {
		t.Fatalf("SessionEpochFor: %v", err)
	}
	token, err := GenerateTokenAtEpoch(7, "Member", epoch)
	if err != nil {
		t.Fatalf("GenerateTokenAtEpoch: %v", err)
	}
	if err := data_models.RevokeSessions(db, 7); err != nil {
		t.Fatalf("RevokeSessions: %v", err)
	}
	if code := gatedGET(t, db, token); code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — a session from before the reset survived it", code)
	}
}

// Revoking twice keeps working: the epoch is a counter, not a flag.
func TestSecondRevocationInvalidatesTheSessionIssuedAfterTheFirst(t *testing.T) {
	db := revokeTestDB(t, 7)

	if err := data_models.RevokeSessions(db, 7); err != nil {
		t.Fatalf("first RevokeSessions: %v", err)
	}
	epoch, _ := data_models.SessionEpochFor(db, 7)
	token, err := GenerateTokenAtEpoch(7, "Member", epoch)
	if err != nil {
		t.Fatalf("GenerateTokenAtEpoch: %v", err)
	}
	if code := gatedGET(t, db, token); code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the first revocation", code)
	}

	if err := data_models.RevokeSessions(db, 7); err != nil {
		t.Fatalf("second RevokeSessions: %v", err)
	}
	if code := gatedGET(t, db, token); code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — the second revocation did nothing", code)
	}
}

// A token minted before the epoch claim shipped, still inside its 24h life. It
// reads as epoch 0, which matches a never-revoked account — so existing
// sessions survive the deploy — and stops matching the moment anything revokes.
func TestLegacyTokenWithoutEpochClaim(t *testing.T) {
	db := revokeTestDB(t, 7)

	now := time.Now()
	legacyClaims := func() jwt.MapClaims {
		return jwt.MapClaims{
			"user_id": 7,
			"role":    "Member",
			"iat":     now.Unix(),
			"exp":     now.Add(24 * time.Hour).Unix(),
			// no epoch
		}
	}

	legacy := mintTokenWithSecret(t, []byte(testGoodSecret), legacyClaims())
	if code := gatedGET(t, db, legacy); code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the deploy signed out an existing session", code)
	}

	if err := data_models.RevokeSessions(db, 7); err != nil {
		t.Fatalf("RevokeSessions: %v", err)
	}
	if code := gatedGET(t, db, legacy); code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — a legacy token survived a revocation", code)
	}
}

// --- fail closed ------------------------------------------------------------

// A revocation a misconfiguration switches off is not a revocation. No db in
// context is a wiring error and the answer is 401, not "skip the check".
//
// This is the opposite call from the mail seam (#5235), and deliberately so:
// mail is peripheral, so failing closed there took the whole site down for
// nothing. Authentication is the thing itself, and the failure here is loud and
// immediate rather than silent.
func TestMissingDatabaseFailsClosed(t *testing.T) {
	token, err := GenerateToken(7, "Member")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if code := gatedGET(t, nil, token); code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when the revocation check cannot run", code)
	}
}

// A token for a user who no longer exists must not be honoured on the strength
// of a failed lookup.
func TestTokenForMissingUserIsRefused(t *testing.T) {
	db := revokeTestDB(t) // no users at all

	token, err := GenerateToken(7, "Member")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if code := gatedGET(t, db, token); code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a token whose user is gone", code)
	}
}

// --- the quiet door ---------------------------------------------------------

// ParseTokenIfPresent is the second parse path, used by /fetch-object to let an
// admin bypass the published-ETag whitelist (#1381). If only AuthMiddleware
// checked the watermark, a revoked admin token would still unlock that — the
// same shape as the #5232 lockout, where a path nobody was thinking about
// behaved differently from the one under review.
func TestParseTokenIfPresentHonoursRevocation(t *testing.T) {
	db := revokeTestDB(t, 7)

	token, err := GenerateToken(7, "Admin")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	ctx := func() *gin.Context {
		gin.SetMode(gin.TestMode)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		c.Request.Header.Set("Authorization", "Bearer "+token)
		c.Set("db", db)
		return c
	}

	if _, _, ok := ParseTokenIfPresent(ctx()); !ok {
		t.Fatal("a live token was rejected before any revocation")
	}

	if err := db.Model(&data_models.User{}).Where("id = ?", 7).
		Update("session_epoch", gorm.Expr("session_epoch + 1")).Error; err != nil {
		t.Fatalf("revoking: %v", err)
	}

	if _, _, ok := ParseTokenIfPresent(ctx()); ok {
		t.Fatal("a revoked token still granted capability through ParseTokenIfPresent")
	}
}

// Logging out has to work when the session has ALREADY been revoked — that is
// precisely when someone most wants their cookie cleared, and refusing to
// identify them would strand the room lock (#4778) held by a session nobody can
// use. ParseTokenIgnoringRevocation is the deliberately awkward name for that.
func TestParseTokenIgnoringRevocationStillIdentifiesARevokedSession(t *testing.T) {
	db := revokeTestDB(t, 7)

	token, err := GenerateToken(7, "Member")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if err := data_models.RevokeSessions(db, 7); err != nil {
		t.Fatalf("RevokeSessions: %v", err)
	}

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Request.Header.Set("Authorization", "Bearer "+token)
	c.Set("db", db)

	uid, _, ok := ParseTokenIgnoringRevocation(c)
	if !ok {
		t.Fatal("logout cannot identify a revoked session; the room lock would be stranded")
	}
	if uid != 7 {
		t.Errorf("uid = %d, want 7", uid)
	}
}

// It also must not need a db at all — logout is a public route and the cookie
// clear has to work regardless.
func TestParseTokenIgnoringRevocationNeedsNoDatabase(t *testing.T) {
	token, err := GenerateToken(7, "Member")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Request.Header.Set("Authorization", "Bearer "+token)

	if _, _, ok := ParseTokenIgnoringRevocation(c); !ok {
		t.Fatal("logout's parse path requires a db; it must not")
	}
}
