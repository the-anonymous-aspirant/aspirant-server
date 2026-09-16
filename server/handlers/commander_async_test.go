package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// The async extraction proxy (system_3 #5977/#5978): the server must forward the
// submit and the poll to commander, propagate the authed caller identity, and
// return commander's status/body verbatim (including its 404).
func TestExtractAsyncProxiesSubmit(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotPath, gotUID string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUID = r.Header.Get("X-Aspirant-User-Id")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_id":"abc-123"}`))
	}))
	defer fake.Close()
	t.Setenv("COMMANDER_URL", fake.URL)

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("user_id", uint(7)) })
	r.POST("/commander/valuation-statement/extract-async", PostCommanderValuationExtractAsyncHandler)

	req, _ := http.NewRequest("POST", "/commander/valuation-statement/extract-async", strings.NewReader("payload"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/valuation-statement/extract-async" {
		t.Errorf("commander path = %q, want /valuation-statement/extract-async", gotPath)
	}
	if gotUID != "7" {
		t.Errorf("X-Aspirant-User-Id = %q, want 7", gotUID)
	}
	if !strings.Contains(w.Body.String(), "job_id") {
		t.Errorf("body = %q, want it to carry job_id verbatim", w.Body.String())
	}
}

func TestGetExtractionJobProxiesById(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("forwards the id and returns the upstream result", func(t *testing.T) {
		var gotPath string
		fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"done","result":{"documents":[]},"error":null}`))
		}))
		defer fake.Close()
		t.Setenv("COMMANDER_URL", fake.URL)

		r := gin.New()
		r.GET("/commander/valuation-statement/jobs/:id", GetCommanderValuationJobHandler)
		req, _ := http.NewRequest("GET", "/commander/valuation-statement/jobs/job-xyz", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		if gotPath != "/valuation-statement/jobs/job-xyz" {
			t.Errorf("commander path = %q, want /valuation-statement/jobs/job-xyz", gotPath)
		}
		if !strings.Contains(w.Body.String(), `"status":"done"`) {
			t.Errorf("body = %q, want the upstream status verbatim", w.Body.String())
		}
	})

	t.Run("forwards a 404 for an unknown job", func(t *testing.T) {
		fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"Extraction job not found."}`))
		}))
		defer fake.Close()
		t.Setenv("COMMANDER_URL", fake.URL)

		r := gin.New()
		r.GET("/commander/valuation-statement/jobs/:id", GetCommanderValuationJobHandler)
		req, _ := http.NewRequest("GET", "/commander/valuation-statement/jobs/nope", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected commander's 404 forwarded, got %d", w.Code)
		}
	})
}
