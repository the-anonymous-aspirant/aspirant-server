package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
)

// #4195: the Ludde meal-tracker feeding log is gated to jenny + Admin (the
// operator ruled it a single-user app — "Only Jenny needs this one" — so a
// route gate, not a per-user schema rework). This asserts cross-user isolation
// for the jenny owner specifically: a non-owner Trusted user is refused, the
// owner passes, and Admin bypasses. ValidateUserOrAdmin's fuller behavior is
// covered in jobs_gate_test.go.
func TestValidateUserOrAdmin_LuddeFeedingJennyGate(t *testing.T) {
	db := setupJobsGateDB(t)

	jenny := data_models.User{Username: "jenny", Email: "jenny@example.test"}
	if err := db.Create(&jenny).Error; err != nil {
		t.Fatalf("create jenny: %v", err)
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
		r.GET("/data_models/ludde_feeding_times", ValidateUserOrAdmin("jenny"), func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"ok": true})
		})
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/data_models/ludde_feeding_times", nil)
		r.ServeHTTP(w, req)
		return w.Code
	}

	if code := call(jenny.ID, "Trusted"); code != http.StatusOK {
		t.Errorf("owner jenny: got %d, want 200", code)
	}
	if code := call(other.ID, "Trusted"); code != http.StatusForbidden {
		t.Errorf("non-owner Trusted: got %d, want 403", code)
	}
	if code := call(other.ID, "Admin"); code != http.StatusOK {
		t.Errorf("admin bypass: got %d, want 200", code)
	}
}
