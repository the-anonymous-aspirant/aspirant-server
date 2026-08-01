package server

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

func TestCorsAllowedOrigins_DefaultWhenUnset(t *testing.T) {
	t.Setenv(corsAllowedOriginsEnv, "")
	got := corsAllowedOrigins()
	want := []string{defaultCORSOrigin}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("default origins = %v, want %v", got, want)
	}
}

func TestCorsAllowedOrigins_EnvOverrideParsesAndTrims(t *testing.T) {
	t.Setenv(corsAllowedOriginsEnv, " https://the-aspirant.com , http://localhost:5173 ")
	got := corsAllowedOrigins()
	want := []string{"https://the-aspirant.com", "http://localhost:5173"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("env origins = %v, want %v", got, want)
	}
}

func TestCorsAllowedOrigins_AllBlankFallsBackToDefault(t *testing.T) {
	t.Setenv(corsAllowedOriginsEnv, " , ,  ")
	got := corsAllowedOrigins()
	want := []string{defaultCORSOrigin}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("all-blank env origins = %v, want %v", got, want)
	}
}

// newCorsEngine builds a minimal engine using the real CORS middleware plus a
// single GET handler, so tests exercise the same config the server registers.
func newCorsEngine(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(cors.New(corsConfig()))
	r.GET("/health", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	return r
}

func TestCors_AllowedOriginIsEchoedWithVary(t *testing.T) {
	t.Setenv(corsAllowedOriginsEnv, "")
	r := newCorsEngine(t)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Origin", defaultCORSOrigin)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != defaultCORSOrigin {
		t.Fatalf("Access-Control-Allow-Origin = %q, want %q", got, defaultCORSOrigin)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got == "*" {
		t.Fatalf("Access-Control-Allow-Origin must never be a wildcard")
	}
	if vary := w.Header().Get("Vary"); vary == "" {
		t.Fatalf("expected Vary: Origin, got empty Vary header")
	}
	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("Access-Control-Allow-Credentials must stay unset, got %q", got)
	}
}

func TestCors_DisallowedOriginIsRejected(t *testing.T) {
	t.Setenv(corsAllowedOriginsEnv, "")
	r := newCorsEngine(t)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got == "*" || got == "https://evil.example.com" {
		t.Fatalf("Access-Control-Allow-Origin leaked to disallowed origin: %q", got)
	}
	if w.Code == http.StatusOK {
		t.Fatalf("disallowed cross-origin request should not return 200, got %d", w.Code)
	}
}

func TestCors_NoOriginHeaderPassesThrough(t *testing.T) {
	t.Setenv(corsAllowedOriginsEnv, "")
	r := newCorsEngine(t)

	// No Origin header: server-to-server and same-origin requests must be
	// unaffected by the CORS policy.
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("no-Origin request status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("no-Origin request should not carry ACAO, got %q", got)
	}
}
