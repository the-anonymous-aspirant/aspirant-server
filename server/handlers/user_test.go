package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// --- callerIsAdmin / callerOwnsUserID helper tests ---

func TestCallerIsAdmin_TrueWhenRoleAdmin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("role", "Admin")
	if !callerIsAdmin(c) {
		t.Fatalf("expected true for Admin role")
	}
}

func TestCallerIsAdmin_FalseForOtherRoles(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, role := range []string{"Trusted", "User", "", "admin"} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Set("role", role)
		if callerIsAdmin(c) {
			t.Fatalf("expected false for role %q", role)
		}
	}
}

func TestCallerIsAdmin_FalseWhenRoleAbsent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if callerIsAdmin(c) {
		t.Fatalf("expected false when role absent")
	}
}

func TestCallerOwnsUserID_TrueForSelfLookup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("user_id", uint(42))
	if !callerOwnsUserID(c, "42") {
		t.Fatalf("expected true for matching id")
	}
}

func TestCallerOwnsUserID_FalseForOtherID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("user_id", uint(42))
	if callerOwnsUserID(c, "43") {
		t.Fatalf("expected false for mismatched id")
	}
}

// --- GetUserHandler auth-gate short-circuit ---

// TestGetUserHandler_NonAdminCrossIDForbidden verifies the auth gate
// added by #1380 blocks a non-Admin caller from harvesting arbitrary
// users' emails. The DB call is never reached — the guard trips
// before it, so this test needs no DB fixture.
func TestGetUserHandler_NonAdminCrossIDForbidden(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		// Simulate a Trusted-role JWT looking up somebody else's id.
		c.Set("role", "Trusted")
		c.Set("user_id", uint(1))
		c.Next()
	})
	r.GET("/data_models/users/:id", GetUserHandler)

	req := httptest.NewRequest(http.MethodGet, "/data_models/users/2", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d — body=%s", w.Code, w.Body.String())
	}
	var body ErrorResponse
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Error.Code != "forbidden" {
		t.Fatalf("expected error.code=forbidden in response body, got %q", w.Body.String())
	}
}

// TestGetUserHandler_MissingRoleForbidden covers the pathological
// case where AuthMiddleware runs but doesn't set "role" (shouldn't
// happen but the guard defaults to forbid).
func TestGetUserHandler_MissingRoleForbidden(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/data_models/users/:id", GetUserHandler)

	req := httptest.NewRequest(http.MethodGet, "/data_models/users/1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403 when auth context missing, got %d", w.Code)
	}
}
