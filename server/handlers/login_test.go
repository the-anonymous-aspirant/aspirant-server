package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

// Locks the auth-cookie contract: on successful login the server must set an
// HttpOnly Secure SameSite=Strict auth_token cookie so nginx auth_request
// (used by /browser-flows and any future full-page-nav route) receives
// credentials on browser navigations that don't carry an Authorization header.
// Without the cookie the auth gate 401s and nginx redirects to /login.
func TestLoginHandlerSetsAuthCookie(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	defer db.Close()
	db.AutoMigrate(&data_models.Role{}, &data_models.User{})

	role := data_models.Role{RoleName: "Admin", RoleDescription: "Administrator"}
	if err := db.Create(&role).Error; err != nil {
		t.Fatalf("failed to seed role: %v", err)
	}

	user := data_models.User{
		Username: "alice",
		Email:    "alice@example.com",
		RoleID:   role.ID,
	}
	if err := user.HashPassword("s3cret!"); err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("failed to seed user: %v", err)
	}
	// LoginHandler refuses an unverified account (#5113-C1), and a row created
	// directly like this one has email_verified_at NULL. Stamp it: this test is
	// about the auth cookie, not the gate, and the gate has its own coverage in
	// signup_test.go. Set directly rather than by calling the migration —
	// migration code is not a test fixture, and reaching for it here is what
	// made the every-boot backfill look reasonable (finding #5226).
	verifiedAt := time.Now()
	if err := db.Model(&user).Update("email_verified_at", verifiedAt).Error; err != nil {
		t.Fatalf("failed to mark the fixture verified: %v", err)
	}

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("db", db)
		c.Next()
	})
	router.POST("/login", LoginHandler)

	body := `{"username":"alice","password":"s3cret!"}`
	req, _ := http.NewRequest(http.MethodPost, "/login", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", w.Code, w.Body.String())
	}

	setCookie := w.Header().Get("Set-Cookie")
	if setCookie == "" {
		t.Fatalf("no Set-Cookie header on successful login; got headers: %v", w.Header())
	}
	if !strings.HasPrefix(setCookie, "auth_token=") {
		t.Errorf("Set-Cookie does not set auth_token: %q", setCookie)
	}
	if !strings.Contains(setCookie, "HttpOnly") {
		t.Errorf("auth_token cookie missing HttpOnly flag: %q", setCookie)
	}
	if !strings.Contains(setCookie, "Secure") {
		t.Errorf("auth_token cookie missing Secure flag: %q", setCookie)
	}
	if !strings.Contains(setCookie, "SameSite=Strict") {
		t.Errorf("auth_token cookie missing SameSite=Strict: %q", setCookie)
	}
	if !strings.Contains(setCookie, "Path=/") {
		t.Errorf("auth_token cookie missing Path=/: %q", setCookie)
	}
	if !strings.Contains(setCookie, "Max-Age=86400") {
		t.Errorf("auth_token cookie missing Max-Age=86400: %q", setCookie)
	}

	// The token must be delivered ONLY on the HttpOnly cookie, never echoed in
	// the JS-readable JSON body (security-finding #3095): a body copy is a dead
	// credential path the cookie-only client never reads, and echoing it would
	// hand any XSS running at login time a 24h token.
	respCookie := extractCookieValue(setCookie, "auth_token")
	if respCookie == "" {
		t.Fatalf("failed to extract auth_token cookie value from %q", setCookie)
	}
	if strings.Contains(w.Body.String(), respCookie) {
		t.Errorf("login response body leaked the token %q; it must live only in the HttpOnly cookie: %s", respCookie, w.Body.String())
	}
}

// Locks the failure path: no cookie on bad credentials, so an unauthenticated
// caller cannot accidentally acquire an auth_token by hitting /login with any
// input.
func TestLoginHandlerNoCookieOnBadPassword(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	defer db.Close()
	db.AutoMigrate(&data_models.Role{}, &data_models.User{})

	role := data_models.Role{RoleName: "Admin"}
	db.Create(&role)
	user := data_models.User{Username: "bob", Email: "bob@example.com", RoleID: role.ID}
	if err := user.HashPassword("correct-horse"); err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}
	db.Create(&user)

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("db", db)
		c.Next()
	})
	router.POST("/login", LoginHandler)

	body := `{"username":"bob","password":"wrong"}`
	req, _ := http.NewRequest(http.MethodPost, "/login", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body = %s", w.Code, w.Body.String())
	}
	if setCookie := w.Header().Get("Set-Cookie"); setCookie != "" {
		t.Errorf("unexpected Set-Cookie on failed login: %q", setCookie)
	}
}

func extractCookieValue(setCookie, name string) string {
	for _, part := range strings.Split(setCookie, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, name+"=") {
			return strings.TrimPrefix(part, name+"=")
		}
	}
	return ""
}
