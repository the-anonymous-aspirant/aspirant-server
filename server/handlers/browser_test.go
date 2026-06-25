package handlers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// Locks the proxy contract: aspirant-server sees /browser-flows*
// (nginx strips the /api/ prefix) and must restore /api before forwarding
// to the browser service, preserving method, body, and query string. The
// HTML routes that share the /browser-flows prefix are served directly by
// nginx → browser:8000 and never reach this handler.
func TestBrowserProxyRestoresAPIPrefixAndPreservesPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name        string
		method      string
		path        string
		query       string
		body        string
		contentType string
	}{
		{
			name:        "GET /browser-flows (list)",
			method:      "GET",
			path:        "/browser-flows",
			query:       "",
			body:        "",
			contentType: "application/json",
		},
		{
			name:        "POST /browser-flows (create)",
			method:      "POST",
			path:        "/browser-flows",
			query:       "",
			body:        `{"name":"google-search"}`,
			contentType: "application/json",
		},
		{
			name:        "GET /browser-flows/<uuid>/runs?limit=5",
			method:      "GET",
			path:        "/browser-flows/8b1f4e3a-2c11-4d8d-9bb6-0a1234567890/runs",
			query:       "limit=5",
			body:        "",
			contentType: "application/json",
		},
		{
			name:        "POST /browser-flows/<uuid>/runs (trigger)",
			method:      "POST",
			path:        "/browser-flows/8b1f4e3a-2c11-4d8d-9bb6-0a1234567890/runs",
			query:       "",
			body:        `{"inputs":{}}`,
			contentType: "application/json",
		},
		{
			name:        "POST /browser-flows/generate",
			method:      "POST",
			path:        "/browser-flows/generate",
			query:       "",
			body:        `{"description":"open hacker news"}`,
			contentType: "application/json",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var receivedURL string
			var receivedMethod string
			var receivedBody string

			fakeBrowser := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				receivedURL = r.URL.String()
				receivedMethod = r.Method
				b, _ := io.ReadAll(r.Body)
				receivedBody = string(b)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer fakeBrowser.Close()
			t.Setenv("BROWSER_URL", fakeBrowser.URL)

			r := gin.New()
			r.Any("/browser-flows", BrowserProxyHandler)
			r.Any("/browser-flows/*path", BrowserProxyHandler)

			target := tc.path
			if tc.query != "" {
				target += "?" + tc.query
			}
			req, _ := http.NewRequest(tc.method, target, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.contentType)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
			}
			if receivedMethod != tc.method {
				t.Errorf("browser received method %q, want %q", receivedMethod, tc.method)
			}
			wantPath := "/api" + tc.path
			if tc.query != "" {
				wantPath += "?" + tc.query
			}
			if receivedURL != wantPath {
				t.Errorf("browser received URL %q, want %q", receivedURL, wantPath)
			}
			if receivedBody != tc.body {
				t.Errorf("browser received body %q, want %q", receivedBody, tc.body)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("client sees Content-Type %q, want application/json", ct)
			}
		})
	}
}

// Locks the upstream-failure path: when browser:8000 is unreachable,
// aspirant-server must surface a 502 with the canonical error envelope.
func TestBrowserProxyReturns502WhenUpstreamUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Reserved TEST-NET-1 IP that will never accept a connection.
	t.Setenv("BROWSER_URL", "http://192.0.2.1:1")

	r := gin.New()
	r.Any("/browser-flows", BrowserProxyHandler)

	req, _ := http.NewRequest("GET", "/browser-flows", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", w.Code, w.Body.String())
	}
}
