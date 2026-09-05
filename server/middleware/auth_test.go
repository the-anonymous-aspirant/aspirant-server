package middleware

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

const testGoodSecret = "test-jwt-secret-must-be-at-least-32-bytes-long!!"

// withSecret runs fn with JWT_SECRET set to `val`, then restores the
// package-level jwtSecret from testGoodSecret so downstream tests keep
// working. Returns whatever LoadJWTSecret returned inside the swap.
func withSecret(t *testing.T, val string, fn func(loadErr error)) {
	t.Helper()
	os.Setenv("JWT_SECRET", val)
	loadErr := LoadJWTSecret()
	fn(loadErr)
	os.Setenv("JWT_SECRET", testGoodSecret)
	if err := LoadJWTSecret(); err != nil {
		t.Fatalf("failed to restore good JWT_SECRET: %v", err)
	}
}

func TestLoadJWTSecret_EmptyFails(t *testing.T) {
	withSecret(t, "", func(err error) {
		if err == nil {
			t.Fatal("expected LoadJWTSecret to reject empty JWT_SECRET")
		}
		if !strings.Contains(err.Error(), "JWT_SECRET must be set") {
			t.Errorf("expected 'must be set' error, got: %v", err)
		}
	})
}

func TestLoadJWTSecret_PlaceholderChangeMeFails(t *testing.T) {
	withSecret(t, "change-me", func(err error) {
		if err == nil {
			t.Fatal("expected LoadJWTSecret to reject 'change-me' placeholder")
		}
		if !strings.Contains(err.Error(), "placeholder") {
			t.Errorf("expected 'placeholder' error, got: %v", err)
		}
	})
}

func TestLoadJWTSecret_PlaceholderAspirantSecretFails(t *testing.T) {
	withSecret(t, "aspirant_secret", func(err error) {
		if err == nil {
			t.Fatal("expected LoadJWTSecret to reject 'aspirant_secret' placeholder")
		}
	})
}

func TestLoadJWTSecret_PlaceholderChangeMeSuffixFails(t *testing.T) {
	withSecret(t, "aspirant_secret_CHANGE_ME", func(err error) {
		if err == nil {
			t.Fatal("expected LoadJWTSecret to reject 'aspirant_secret_CHANGE_ME' placeholder")
		}
	})
}

func TestLoadJWTSecret_ShortSecretFails(t *testing.T) {
	withSecret(t, "too-short", func(err error) {
		if err == nil {
			t.Fatal("expected LoadJWTSecret to reject a short secret")
		}
		if !strings.Contains(err.Error(), "too short") {
			t.Errorf("expected 'too short' error, got: %v", err)
		}
	})
}

func TestLoadJWTSecret_StrongSecretAccepted(t *testing.T) {
	withSecret(t, "another-strong-secret-of-more-than-thirty-two-bytes", func(err error) {
		if err != nil {
			t.Fatalf("expected LoadJWTSecret to accept a strong secret, got: %v", err)
		}
	})
}

// authRouter returns a minimal gin engine that runs AuthMiddleware and
// returns 200 when it passes.
// authTestDB gives the router a database holding one user (id 1) who has never
// revoked their sessions. AuthMiddleware needs it since #5224: it reads the
// user's revocation watermark on every request and fails closed without one,
// so a fixture with no db would 401 everything and prove nothing about tokens.
func authTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	db.AutoMigrate(&data_models.User{})
	if err := db.Create(&data_models.User{Username: "u1", Email: "u1@example.com"}).Error; err != nil {
		t.Fatalf("seeding user: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func authRouter() *gin.Engine {
	return authRouterWithDB(nil)
}

// authRouterWithDB wires db into the request context the way main.go does. A
// nil db is left unset, which is what the fail-closed tests want.
func authRouterWithDB(db *gorm.DB) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	if db != nil {
		r.Use(func(c *gin.Context) { c.Set("db", db); c.Next() })
	}
	r.GET("/gated", AuthMiddleware(), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})
	return r
}

func TestAuthMiddleware_ValidHS256Accepted(t *testing.T) {
	token, err := GenerateToken(1, "Admin")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	r := authRouterWithDB(authTestDB(t))
	req := httptest.NewRequest(http.MethodGet, "/gated", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 on valid HS256 token, got %d — body=%s", w.Code, w.Body.String())
	}
}

// mintTokenWithSecret signs a token with an arbitrary secret so tests
// can prove that a token forged with the old / wrong secret is rejected.
func mintTokenWithSecret(t *testing.T, secret []byte, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := token.SignedString(secret)
	if err != nil {
		t.Fatalf("signing test token: %v", err)
	}
	return s
}

func TestAuthMiddleware_ForgedWithOldKeyRejected(t *testing.T) {
	// The finding: an attacker crafts an admin JWT signed with the
	// hardcoded "aspirant_secret". Post-fix the parser rejects it because
	// the process is running with a different key.
	forged := mintTokenWithSecret(t, []byte("aspirant_secret"), jwt.MapClaims{
		"user_id": 1,
		"role":    "Admin",
		"iat":     time.Now().Unix(),
		"exp":     time.Now().Add(time.Hour).Unix(),
	})
	r := authRouter()
	req := httptest.NewRequest(http.MethodGet, "/gated", nil)
	req.Header.Set("Authorization", "Bearer "+forged)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 on forged-key token, got %d — body=%s", w.Code, w.Body.String())
	}
}

// craftAlgNoneToken assembles a JWT with `alg:none` and an empty
// signature so we can prove parseAndValidate rejects it. jwt/v5 will not
// mint an unsigned token via the library helper, so we assemble it by
// hand.
func craftAlgNoneToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	if err != nil {
		t.Fatalf("header: %v", err)
	}
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("body: %v", err)
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString(header) + "." + enc.EncodeToString(body) + "."
}

func TestAuthMiddleware_AlgNoneRejected(t *testing.T) {
	forged := craftAlgNoneToken(t, map[string]any{
		"user_id": 1,
		"role":    "Admin",
		"exp":     time.Now().Add(time.Hour).Unix(),
	})
	r := authRouter()
	req := httptest.NewRequest(http.MethodGet, "/gated", nil)
	req.Header.Set("Authorization", "Bearer "+forged)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 on alg:none token, got %d — body=%s", w.Code, w.Body.String())
	}
}

func TestAuthMiddleware_ExpiredRejected(t *testing.T) {
	// Sign with the good secret but exp in the past.
	expired := mintTokenWithSecret(t, []byte(testGoodSecret), jwt.MapClaims{
		"user_id": 1,
		"role":    "Admin",
		"iat":     time.Now().Add(-2 * time.Hour).Unix(),
		"exp":     time.Now().Add(-time.Hour).Unix(),
	})
	r := authRouter()
	req := httptest.NewRequest(http.MethodGet, "/gated", nil)
	req.Header.Set("Authorization", "Bearer "+expired)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 on expired token, got %d — body=%s", w.Code, w.Body.String())
	}
}

func TestAuthMiddleware_IatInFutureRejected(t *testing.T) {
	// jwt/v5 with WithIssuedAt() enforces that iat is not in the future.
	future := mintTokenWithSecret(t, []byte(testGoodSecret), jwt.MapClaims{
		"user_id": 1,
		"role":    "Admin",
		"iat":     time.Now().Add(2 * time.Hour).Unix(),
		"exp":     time.Now().Add(3 * time.Hour).Unix(),
	})
	r := authRouter()
	req := httptest.NewRequest(http.MethodGet, "/gated", nil)
	req.Header.Set("Authorization", "Bearer "+future)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 on iat-in-future token, got %d — body=%s", w.Code, w.Body.String())
	}
}

func TestParseTokenIfPresent_ValidReturnsClaims(t *testing.T) {
	token, err := GenerateToken(42, "Trusted")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Request.Header.Set("Authorization", "Bearer "+token)
	// ParseTokenIfPresent checks session revocation since #5224, so the user
	// has to exist. Seeded with id 42 to match the token.
	db := authTestDB(t)
	if err := db.Create(&data_models.User{Model: gorm.Model{ID: 42}, Username: "u42", Email: "u42@example.com"}).Error; err != nil {
		t.Fatalf("seeding user 42: %v", err)
	}
	c.Set("db", db)

	uid, role, ok := ParseTokenIfPresent(c)
	if !ok {
		t.Fatal("expected ok=true on valid token")
	}
	if uid != 42 {
		t.Errorf("want uid=42, got %d", uid)
	}
	if role != "Trusted" {
		t.Errorf("want role=Trusted, got %s", role)
	}
}

func TestParseTokenIfPresent_ForgedReturnsFalse(t *testing.T) {
	forged := mintTokenWithSecret(t, []byte("aspirant_secret"), jwt.MapClaims{
		"user_id": 1,
		"role":    "Admin",
		"iat":     time.Now().Unix(),
		"exp":     time.Now().Add(time.Hour).Unix(),
	})
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Request.Header.Set("Authorization", "Bearer "+forged)

	if _, _, ok := ParseTokenIfPresent(c); ok {
		t.Fatal("expected ok=false on forged token")
	}
}
