package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// Locks the session-termination contract (security-finding #2589).
//
// auth_token is HttpOnly, so the SPA cannot clear it — a document.cookie
// write against an HttpOnly cookie is a silent no-op. This route is the
// only thing that ends a session, and it only works if the expiring
// Set-Cookie repeats the attributes LoginHandler set: a Set-Cookie whose
// path/domain/Secure/SameSite differ does not identify the same cookie,
// so the browser keeps the original and logout silently fails again one
// layer down. Each assertion below is one of those attributes.
func TestLogoutExpiresAuthCookie(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.POST("/logout", LogoutHandler)

	req, _ := http.NewRequest(http.MethodPost, "/logout", nil)
	// A live session cookie, as a real logout would carry.
	req.AddCookie(&http.Cookie{Name: "auth_token", Value: "a-live-session-token"})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", w.Code, w.Body.String())
	}

	var authCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "auth_token" {
			authCookie = c
		}
	}
	if authCookie == nil {
		t.Fatal("no auth_token Set-Cookie on the logout response: the session cookie is never cleared")
	}

	// Expiry. Go's cookie parser surfaces Max-Age=0 as MaxAge<0 and a past
	// Expires as a non-zero time; accept either encoding, reject a cookie
	// that would outlive the response.
	if authCookie.MaxAge >= 0 && authCookie.Expires.IsZero() {
		t.Errorf("auth_token is not expired: MaxAge = %d, Expires = %v", authCookie.MaxAge, authCookie.Expires)
	}
	if authCookie.Value != "" {
		t.Errorf("auth_token value = %q, want empty", authCookie.Value)
	}

	// Attribute parity with LoginHandler's c.SetCookie(..., "/", "", true, true).
	if authCookie.Path != "/" {
		t.Errorf("Path = %q, want %q — a path mismatch clears a different cookie", authCookie.Path, "/")
	}
	if !authCookie.Secure {
		t.Error("Secure not set: the expiry would not match the Secure cookie login issued")
	}
	if !authCookie.HttpOnly {
		t.Error("HttpOnly not set: the expiry would not match the HttpOnly cookie login issued")
	}
	if authCookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite = %v, want Strict", authCookie.SameSite)
	}
}

// Logout must work without a valid session. Requiring auth here would
// strand exactly the sessions most in need of clearing (expired or
// malformed token), and it must stay idempotent so a double-click or a
// client retry is not an error.
func TestLogoutWithoutValidSession(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.POST("/logout", LogoutHandler)

	cases := []struct {
		name   string
		cookie string
	}{
		{"no cookie at all", ""},
		{"malformed token", "not-a-jwt"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, "/logout", nil)
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: "auth_token", Value: tc.cookie})
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body = %s", w.Code, w.Body.String())
			}
		})
	}
}
