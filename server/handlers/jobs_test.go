package handlers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// Locks the jobs proxy contract: aspirant-server sees /jobs* (nginx
// strips /api/) and must restore /api before forwarding to the browser
// service, preserving method, body, and query string. The Trusted-role
// /trusted/jobs overview page (#1290) calls /api/jobs (list) and
// /api/jobs/{id}/hide (PATCH); both must round-trip through this proxy.
func TestJobsProxyRestoresAPIPrefixAndPreservesPayload(t *testing.T) {
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
			name:        "GET /jobs (default list)",
			method:      "GET",
			path:        "/jobs",
			query:       "",
			body:        "",
			contentType: "application/json",
		},
		{
			name:        "GET /jobs with filter+sort+paging",
			method:      "GET",
			path:        "/jobs",
			query:       "q=barista&sort=distance&page=2&per_page=25",
			body:        "",
			contentType: "application/json",
		},
		{
			name:        "PATCH /jobs/<uuid>/hide",
			method:      "PATCH",
			path:        "/jobs/8b1f4e3a-2c11-4d8d-9bb6-0a1234567890/hide",
			query:       "",
			body:        "",
			contentType: "application/json",
		},
		{
			name:        "PATCH /jobs/<uuid>/hide with body (forward-compat)",
			method:      "PATCH",
			path:        "/jobs/8b1f4e3a-2c11-4d8d-9bb6-0a1234567890/hide",
			query:       "",
			body:        `{"reason":"already applied"}`,
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
			r.Any("/jobs", JobsProxyHandler)
			r.Any("/jobs/*path", JobsProxyHandler)

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

// Locks the upstream-failure path for the jobs proxy: when browser:8000
// is unreachable, aspirant-server must surface a 502 with the canonical
// error envelope rather than a hung connection or 500.
func TestJobsProxyReturns502WhenUpstreamUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Reserved TEST-NET-1 IP that will never accept a connection.
	t.Setenv("BROWSER_URL", "http://192.0.2.1:1")

	r := gin.New()
	r.Any("/jobs", JobsProxyHandler)

	req, _ := http.NewRequest("GET", "/jobs", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", w.Code, w.Body.String())
	}
}

// Locks the upstream-status passthrough: a 404 from /api/jobs/{id}/hide
// (e.g. unknown UUID) must reach the client as a 404, not a 200 with an
// error body — the operator's UI needs the HTTP-level signal to decide
// whether to retry or surface the missing-row state.
func TestJobsProxyPassesThroughUpstreamErrorStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)

	fakeBrowser := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"job not found"}`))
	}))
	defer fakeBrowser.Close()
	t.Setenv("BROWSER_URL", fakeBrowser.URL)

	r := gin.New()
	r.Any("/jobs/*path", JobsProxyHandler)

	req, _ := http.NewRequest("PATCH", "/jobs/00000000-0000-0000-0000-000000000000/hide", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 passthrough, got %d: %s", w.Code, w.Body.String())
	}
}
