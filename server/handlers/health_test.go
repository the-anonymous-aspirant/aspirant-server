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

	// Parse response body
	var body SuccessResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse response body: %v", err)
	}

	if body.Status != "success" {
		t.Errorf("expected status 'success', got '%s'", body.Status)
	}

	// Verify the data contains expected fields
	data, ok := body.Data.(map[string]interface{})
	if !ok {
		t.Fatal("expected data to be a map")
	}

	requiredFields := []string{"status", "commit", "uptime", "database", "memory", "go_version"}
	for _, field := range requiredFields {
		if _, exists := data[field]; !exists {
			t.Errorf("expected field '%s' in health response data", field)
		}
	}

	// Without a DB, the status should be "degraded"
	if data["status"] != "degraded" {
		t.Errorf("expected health status 'degraded' without DB, got '%s'", data["status"])
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

	// Verify the response is valid JSON
	var raw map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}

	// Verify top-level structure has status, data, message
	if _, exists := raw["status"]; !exists {
		t.Error("expected 'status' field in response")
	}
	if _, exists := raw["data"]; !exists {
		t.Error("expected 'data' field in response")
	}
}
