package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
)

// #4203: the Pappas pushup-challenge log is gated to robert + Admin (the
// operator ruled it robert's own private log, so a route gate — not a per-user
// schema rework). This asserts the gate for the robert owner specifically;
// ValidateUserOrAdmin's fuller behavior is covered in jobs_gate_test.go.
func TestValidateUserOrAdmin_PushupsRobertGate(t *testing.T) {
	db := setupJobsGateDB(t)

	robert := data_models.User{Username: "robert", Email: "robert@example.test"}
	if err := db.Create(&robert).Error; err != nil {
		t.Fatalf("create robert: %v", err)
	}
	other := data_models.User{Username: "someone_else", Email: "else@example.test"}
	if err := db.Create(&other).Error; err != nil {
		t.Fatalf("create other: %v", err)
	}

	call := func(userID uint, role string) int {
		gin.SetMode(gin.TestMode)
		r := gin.New()
		r.Use(func(c *gin.Context) {
			c.Set("db", db)
			if userID != 0 {
				c.Set("user_id", userID)
			}
			if role != "" {
				c.Set("role", role)
			}
			c.Next()
		})
		r.GET("/pushups/entries", ValidateUserOrAdmin("robert"), func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"ok": true})
		})
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/pushups/entries", nil)
		r.ServeHTTP(w, req)
		return w.Code
	}

	if code := call(robert.ID, "Trusted"); code != http.StatusOK {
		t.Errorf("owner robert: got %d, want 200", code)
	}
	if code := call(other.ID, "Trusted"); code != http.StatusForbidden {
		t.Errorf("non-owner Trusted: got %d, want 403", code)
	}
	if code := call(other.ID, "Admin"); code != http.StatusOK {
		t.Errorf("admin bypass: got %d, want 200", code)
	}
}
