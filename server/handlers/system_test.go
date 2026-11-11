package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
)

// --- GetServiceUsageHandler (system_3 #10491-B) -----------------------------

func TestGetServiceUsageHandlerNoRollupYet(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newRollupTestDB(t)

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("db", db); c.Next() })
	r.GET("/u", GetServiceUsageHandler)

	req := httptest.NewRequest("GET", "/u", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"services":[]`) {
		t.Errorf("body = %s, want empty services array", body)
	}
}

func TestGetServiceUsageHandlerReturnsOnlyLatestRun(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newRollupTestDB(t)

	older := time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)
	latest := time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC)
	count := int64(42)

	rows := []data_models.ServiceUsageRollup{
		{ServiceName: "advisor", Kind: "container", AsOf: older, RequestCount: ptrInt64(1)},
		{ServiceName: "advisor", Kind: "container", AsOf: latest, RequestCount: &count},
		{ServiceName: "browser", Kind: "container", AsOf: latest, RequestCount: ptrInt64(7)},
	}
	for _, row := range rows {
		if err := db.Create(&row).Error; err != nil {
			t.Fatalf("seed row: %v", err)
		}
	}

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("db", db); c.Next() })
	r.GET("/u", GetServiceUsageHandler)

	req := httptest.NewRequest("GET", "/u", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Both services at the latest AsOf should appear; the older advisor row
	// (request_count 1) must not win over the latest one (42).
	if !strings.Contains(body, `"service_name":"advisor"`) || !strings.Contains(body, `"request_count":42`) {
		t.Errorf("body = %s, want latest advisor row (request_count 42)", body)
	}
	if !strings.Contains(body, `"service_name":"browser"`) {
		t.Errorf("body = %s, want browser row", body)
	}
	if strings.Contains(body, `"request_count":1`) {
		t.Errorf("body = %s, the older run's row leaked in", body)
	}
}

// --- container start/stop proxy (system_3 #10491-B / #10514) ---------------

func TestPostContainerStartHandlerForwardsNameAndToken(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotPath, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotPath = req.URL.Path
		gotAuth = req.Header.Get("Authorization")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"name":"aspirant-online-advisor-1","action":"start","accepted":true}`))
	}))
	defer upstream.Close()
	t.Setenv("MONITOR_URL", upstream.URL)
	t.Setenv("MONITOR_CONTROL_TOKEN", "test-control-token")

	r := gin.New()
	r.POST("/c/:name/start", PostContainerStartHandler)

	req := httptest.NewRequest("POST", "/c/aspirant-online-advisor-1/start", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", w.Code, w.Body.String())
	}
	if gotPath != "/containers/aspirant-online-advisor-1/start" {
		t.Errorf("upstream path = %q, want /containers/aspirant-online-advisor-1/start", gotPath)
	}
	if gotAuth != "Bearer test-control-token" {
		t.Errorf("Authorization = %q, want Bearer test-control-token", gotAuth)
	}
}

func TestPostContainerStopHandlerForwardsNameAndToken(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotPath, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotPath = req.URL.Path
		gotAuth = req.Header.Get("Authorization")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"name":"aspirant-online-browser-1","action":"stop","accepted":true}`))
	}))
	defer upstream.Close()
	t.Setenv("MONITOR_URL", upstream.URL)
	t.Setenv("MONITOR_CONTROL_TOKEN", "test-control-token")

	r := gin.New()
	r.POST("/c/:name/stop", PostContainerStopHandler)

	req := httptest.NewRequest("POST", "/c/aspirant-online-browser-1/stop", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", w.Code, w.Body.String())
	}
	if gotPath != "/containers/aspirant-online-browser-1/stop" {
		t.Errorf("upstream path = %q, want /containers/aspirant-online-browser-1/stop", gotPath)
	}
	if gotAuth != "Bearer test-control-token" {
		t.Errorf("Authorization = %q, want Bearer test-control-token", gotAuth)
	}
}

func TestPostContainerStartHandlerNoTokenConfiguredSendsNoAuthHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotAuth string
	var sawHeader bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotAuth, sawHeader = req.Header.Get("Authorization"), req.Header.Get("Authorization") != ""
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"control_disabled"}}`))
	}))
	defer upstream.Close()
	t.Setenv("MONITOR_URL", upstream.URL)
	t.Setenv("MONITOR_CONTROL_TOKEN", "")

	r := gin.New()
	r.POST("/c/:name/start", PostContainerStartHandler)

	req := httptest.NewRequest("POST", "/c/aspirant-online-advisor-1/start", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if sawHeader {
		t.Errorf("Authorization header = %q, want none when MONITOR_CONTROL_TOKEN is unset", gotAuth)
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want the upstream's 503 passed through", w.Code)
	}
}

func TestPostContainerStartHandlerPathEscapesName(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotEscapedPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// req.URL.Path is the server's already-decoded form (back to a literal
		// space); the escaped form actually sent on the wire is RequestURI /
		// EscapedPath, which is what proves PostContainerStartHandler escaped
		// the name rather than passing it through unescaped.
		gotEscapedPath = req.URL.EscapedPath()
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"not_controllable"}}`))
	}))
	defer upstream.Close()
	t.Setenv("MONITOR_URL", upstream.URL)

	r := gin.New()
	r.POST("/c/:name/start", PostContainerStartHandler)

	// gin's :name param cannot itself carry a literal "/", but a name with a
	// character url.PathEscape must encode (a space) proves the escaping path
	// is exercised rather than a no-op passthrough.
	req := httptest.NewRequest("POST", "/c/weird%20name/start", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if gotEscapedPath != "/containers/weird%20name/start" {
		t.Errorf("upstream escaped path = %q, want /containers/weird%%20name/start", gotEscapedPath)
	}
}
