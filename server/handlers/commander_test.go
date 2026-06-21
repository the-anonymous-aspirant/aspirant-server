package handlers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
			name:        "PUT /valuation-statement/operator-defaults (no query)",
			method:      "PUT",
			handler:     PutCommanderValuationOperatorDefaultsHandler,
			path:        "/commander/valuation-statement/operator-defaults",
			query:       "",
			body:        `{"ort":"Nynäshamn"}`,
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
