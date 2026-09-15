package handlers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// Locks the proxy contract: when the operator's browser POSTs
// /api/commander/valuation-statement/generate?format=pdf, the upstream
// commander call must carry ?format=pdf — without it commander defaults to
// docx and the operator receives a docx download instead of a PDF.
//
// Regression motivated by the live-test report that #908 missed because it
// hit commander direct (always with ?format=pdf), bypassing the proxy.
func TestCommanderProxyPreservesQueryString(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name        string
		method      string
		handler     gin.HandlerFunc
		path        string
		query       string
		body        string
		contentType string
	}{
		{
			name:        "POST /valuation-statement/generate?format=pdf",
			method:      "POST",
			handler:     PostCommanderValuationGenerateHandler,
			path:        "/commander/valuation-statement/generate",
			query:       "format=pdf",
			body:        `{"objekt":"LGH 1001"}`,
			contentType: "application/json",
		},
		{
			name:        "POST /valuation-statement/extract (no query)",
			method:      "POST",
			handler:     PostCommanderValuationExtractHandler,
			path:        "/commander/valuation-statement/extract",
			query:       "",
			body:        "multipart-body-stub",
			contentType: "multipart/form-data; boundary=xyz",
		},
		{
			name:        "POST /valuation-statement/decide (no query)",
			method:      "POST",
			handler:     PostCommanderValuationDecideHandler,
			path:        "/commander/valuation-statement/decide",
			query:       "",
			body:        "multipart-body-stub",
			contentType: "multipart/form-data; boundary=xyz",
		},
		{
			name:        "GET /valuation-statement/operator-defaults (no query)",
			method:      "GET",
			handler:     GetCommanderValuationOperatorDefaultsHandler,
			path:        "/commander/valuation-statement/operator-defaults",
			query:       "",
			body:        "",
			contentType: "application/json",
		},
		{
			name:        "PUT /valuation-statement/operator-defaults (no query)",
			method:      "PUT",
			handler:     PutCommanderValuationOperatorDefaultsHandler,
			path:        "/commander/valuation-statement/operator-defaults",
			query:       "",
			body:        `{"ort":"Nynäshamn"}`,
			contentType: "application/json",
		},
		{
			name:        "POST /valuation-statement/processed (no query)",
			method:      "POST",
			handler:     PostCommanderValuationProcessedHandler,
			path:        "/commander/valuation-statement/processed",
			query:       "",
			body:        `{"name":"sample","input_files":[]}`,
			contentType: "application/json",
		},
		{
			name:        "GET /valuation-statement/processed?limit=10&offset=5",
			method:      "GET",
			handler:     ListCommanderValuationProcessedHandler,
			path:        "/commander/valuation-statement/processed",
			query:       "limit=10&offset=5",
			body:        "",
			contentType: "application/json",
		},
		{
			name:        "GET /valuation-statement/processed/export.csv",
			method:      "GET",
			handler:     ExportCommanderValuationProcessedCsvHandler,
			path:        "/commander/valuation-statement/processed/export.csv",
			query:       "",
			body:        "",
			contentType: "application/json",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var receivedURL string
			var receivedMethod string
			var receivedBody string

			fakeCommander := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				receivedURL = r.URL.String()
				receivedMethod = r.Method
				b, _ := io.ReadAll(r.Body)
				receivedBody = string(b)
				w.Header().Set("Content-Type", "application/pdf")
				w.Header().Set("Content-Disposition", `attachment; filename="vardeutlatande.pdf"`)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("%PDF-1.4\n"))
			}))
			defer fakeCommander.Close()
			t.Setenv("COMMANDER_URL", fakeCommander.URL)

			r := gin.New()
			r.Handle(tc.method, tc.path, tc.handler)

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
				t.Errorf("commander received method %q, want %q", receivedMethod, tc.method)
			}
			// Commander must see the path *without* the /commander/ prefix
			// the server strips, plus the original query string verbatim.
			wantPath := strings.TrimPrefix(tc.path, "/commander")
			if tc.query != "" {
				wantPath += "?" + tc.query
			}
			if receivedURL != wantPath {
				t.Errorf("commander received URL %q, want %q", receivedURL, wantPath)
			}
			if receivedBody != tc.body {
				t.Errorf("commander received body %q, want %q", receivedBody, tc.body)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/pdf" {
				t.Errorf("client sees Content-Type %q, want application/pdf", ct)
			}
			if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, ".pdf") {
				t.Errorf("client sees Content-Disposition %q, want filename ending in .pdf", cd)
			}
		})
	}
}

// Locks the timeout-vs-refusal discrimination (#5919): when the commander call
// times out — the OCR-era failure jenny hit — the proxy must answer 504 with a
// message that names slowness, not a generic 502 "Commander service unavailable"
// that reads as "the server refused your file". A genuine connection failure
// (nothing listening) stays a 502. This is what lets the client's uploadError
// path distinguish "took too long" from "the server refused" off the status.
func TestCommanderProxyDistinguishesTimeoutFromRefusal(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("upstream timeout -> 504 with slowness message", func(t *testing.T) {
		// A commander that never answers within the client deadline.
		slowCommander := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(200 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
		}))
		defer slowCommander.Close()
		t.Setenv("COMMANDER_URL", slowCommander.URL)

		// Shrink the client deadline for the test, restore the production value.
		saved := commanderClient
		commanderClient = &http.Client{Timeout: 40 * time.Millisecond}
		defer func() { commanderClient = saved }()

		r := gin.New()
		r.POST("/commander/valuation-statement/extract", PostCommanderValuationExtractHandler)
		req, _ := http.NewRequest("POST", "/commander/valuation-statement/extract",
			strings.NewReader("multipart-body-stub"))
		req.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusGatewayTimeout {
			t.Fatalf("expected 504 on upstream timeout, got %d: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "lång tid") {
			t.Errorf("504 body %q should name slowness (lång tid)", w.Body.String())
		}
	})

	t.Run("connection refused -> 502 unavailable", func(t *testing.T) {
		// Start then immediately close a server so its port is free: connecting
		// there is a deterministic, immediate refusal — a net.Error whose
		// Timeout() is false — not a slow/filtered hang.
		deadCommander := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		deadURL := deadCommander.URL
		deadCommander.Close()
		t.Setenv("COMMANDER_URL", deadURL)

		r := gin.New()
		r.POST("/commander/valuation-statement/extract", PostCommanderValuationExtractHandler)
		req, _ := http.NewRequest("POST", "/commander/valuation-statement/extract",
			strings.NewReader("multipart-body-stub"))
		req.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusBadGateway {
			t.Fatalf("expected 502 on connection failure, got %d: %s", w.Code, w.Body.String())
		}
	})
}

// Locks the identity-propagation contract (security-finding #3096): the proxy
// must send commander an X-Aspirant-User-Id header carrying the server-verified
// user_id (set by AuthMiddleware), and must NOT let a client forge it — an
// inbound X-Aspirant-User-Id on the caller's request is overwritten by the
// authed value, never forwarded. commander scopes per-owner off this header.
func TestCommanderProxyPropagatesCallerIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("sets header from authed user_id", func(t *testing.T) {
		var receivedUID string
		fakeCommander := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			receivedUID = r.Header.Get("X-Aspirant-User-Id")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}))
		defer fakeCommander.Close()
		t.Setenv("COMMANDER_URL", fakeCommander.URL)

		r := gin.New()
		// Stand in for AuthMiddleware: seed the authed identity in context.
		r.Use(func(c *gin.Context) { c.Set("user_id", uint(42)) })
		r.GET("/commander/valuation-statement/processed", ListCommanderValuationProcessedHandler)

		req, _ := http.NewRequest("GET", "/commander/valuation-statement/processed", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		if receivedUID != "42" {
			t.Errorf("commander received X-Aspirant-User-Id %q, want %q", receivedUID, "42")
		}
	})

	t.Run("client cannot forge the header", func(t *testing.T) {
		var receivedUID string
		fakeCommander := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			receivedUID = r.Header.Get("X-Aspirant-User-Id")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}))
		defer fakeCommander.Close()
		t.Setenv("COMMANDER_URL", fakeCommander.URL)

		r := gin.New()
		r.Use(func(c *gin.Context) { c.Set("user_id", uint(42)) })
		r.GET("/commander/valuation-statement/processed", ListCommanderValuationProcessedHandler)

		req, _ := http.NewRequest("GET", "/commander/valuation-statement/processed", nil)
		req.Header.Set("X-Aspirant-User-Id", "999") // attacker attempts to impersonate user 999
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if receivedUID != "42" {
			t.Errorf("commander received X-Aspirant-User-Id %q, want %q (forged inbound value must not survive)", receivedUID, "42")
		}
	})
}

// Locks the /:id routes for the processed-valuations store: when the client
// hits /api/commander/valuation-statement/processed/<uuid>, the upstream
// commander call must carry the same id in the path. Regression guard for
// the edit-in-place + delete + detail flows (system_3 #1154).
func TestCommanderProcessedIDRoutesPreservePath(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const sampleID = "8b1f4e3a-2c11-4d8d-9bb6-0a1234567890"

	cases := []struct {
		name    string
		method  string
		handler gin.HandlerFunc
		body    string
	}{
		{
			name:    "GET /valuation-statement/processed/:id",
			method:  "GET",
			handler: GetCommanderValuationProcessedHandler,
			body:    "",
		},
		{
			name:    "PATCH /valuation-statement/processed/:id",
			method:  "PATCH",
			handler: UpdateCommanderValuationProcessedHandler,
			body:    `{"name":"renamed"}`,
		},
		{
			name:    "DELETE /valuation-statement/processed/:id",
			method:  "DELETE",
			handler: DeleteCommanderValuationProcessedHandler,
			body:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var receivedURL string
			var receivedMethod string

			fakeCommander := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				receivedURL = r.URL.String()
				receivedMethod = r.Method
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer fakeCommander.Close()
			t.Setenv("COMMANDER_URL", fakeCommander.URL)

			r := gin.New()
			r.Handle(tc.method, "/commander/valuation-statement/processed/:id", tc.handler)

			target := "/commander/valuation-statement/processed/" + sampleID
			req, _ := http.NewRequest(tc.method, target, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
			}
			if receivedMethod != tc.method {
				t.Errorf("commander received method %q, want %q", receivedMethod, tc.method)
			}
			wantPath := "/valuation-statement/processed/" + sampleID
			if receivedURL != wantPath {
				t.Errorf("commander received URL %q, want %q", receivedURL, wantPath)
			}
		})
	}
}
