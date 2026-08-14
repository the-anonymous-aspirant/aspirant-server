package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

func TestHealthCheckHandler_Returns200(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()

	// Register the health handler — no DB in context to simulate degraded mode
	r.GET("/health", HealthCheckHandler)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/health", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	// Parse response body into convention schema
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse response body: %v", err)
	}

	// Verify required top-level fields
	requiredFields := []string{"status", "service", "checks"}
	for _, field := range requiredFields {
		if _, exists := body[field]; !exists {
			t.Errorf("expected field '%s' in health response", field)
		}
	}

	// The git commit must NOT be exposed on this unauthenticated
	// surface (CWE-200, system_3 #3865).
	if _, exists := body["version"]; exists {
		t.Error("health response must not expose 'version' (git commit) to unauthenticated callers")
	}

	// Without a DB, the status should be "degraded"
	if body["status"] != "degraded" {
		t.Errorf("expected status 'degraded' without DB, got '%s'", body["status"])
	}

	if body["service"] != "server" {
		t.Errorf("expected service 'server', got '%s'", body["service"])
	}

	// Verify checks contains database
	checks, ok := body["checks"].(map[string]interface{})
	if !ok {
		t.Fatal("expected checks to be a map")
	}
	if _, exists := checks["database"]; !exists {
		t.Error("expected 'database' in checks")
	}
}

func TestHealthCheckHandler_DBErrorNotReflected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()

	// A closed sqlite handle makes Ping fail with a real driver error
	// ("sql: database is closed") — the redaction contract (CWE-209,
	// system_3 #3865) is that no part of it reaches the response body.
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	db.Close()

	r.Use(func(c *gin.Context) {
		c.Set("db", db)
		c.Next()
	})
	r.GET("/health", HealthCheckHandler)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/health", nil)
	r.ServeHTTP(w, req)

	if strings.Contains(w.Body.String(), "closed") {
		t.Errorf("raw driver error leaked into health response: %s", w.Body.String())
	}

	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse response body: %v", err)
	}
	checks, ok := body["checks"].(map[string]interface{})
	if !ok {
		t.Fatal("expected checks to be a map")
	}
	if checks["database"] != "error" {
		t.Errorf("expected checks.database 'error', got %v", checks["database"])
	}
	if body["status"] != "degraded" {
		t.Errorf("expected status 'degraded' on DB ping failure, got '%s'", body["status"])
	}
}

func TestHealthCheckHandler_ResponseStructure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/health", HealthCheckHandler)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/health", nil)
	r.ServeHTTP(w, req)

	// Verify Content-Type header
	contentType := w.Header().Get("Content-Type")
	if contentType != "application/json; charset=utf-8" {
		t.Errorf("expected Content-Type 'application/json; charset=utf-8', got '%s'", contentType)
	}

	// Verify the response is valid JSON with convention schema
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}

	// Should NOT have the old SuccessResponse envelope fields
	if _, exists := body["data"]; exists {
		t.Error("response should not have 'data' wrapper (old envelope format)")
	}
	if _, exists := body["message"]; exists {
		t.Error("response should not have 'message' field (old envelope format)")
	}
}
