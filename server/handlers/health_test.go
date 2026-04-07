package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
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
	requiredFields := []string{"status", "service", "version", "checks"}
	for _, field := range requiredFields {
		if _, exists := body[field]; !exists {
			t.Errorf("expected field '%s' in health response", field)
		}
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
