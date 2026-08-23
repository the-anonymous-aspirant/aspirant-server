package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

// #4196: the Jobs Member app is single-user (owned by vinoly). Its proxy routes
// are gated with ValidateUserOrAdmin("vinoly") — the vinoly account and any
// Admin may reach it; every other authenticated user is refused. These tests
// exercise the middleware directly (the real handler proxies to aspirant-browser
// and is out of a unit test's reach), asserting the access decision only.

func setupJobsGateDB(t *testing.T) *gorm.DB {
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	db.AutoMigrate(&data_models.User{})
	return db
}

// jobsGateRouter builds a router that injects db + (optionally) user_id/role the
// way AuthMiddleware would, then applies the gate ahead of a trivial 200 handler.
func jobsGateRouter(db *gorm.DB, userID uint, role string) *gin.Engine {
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
	r.GET("/jobs", ValidateUserOrAdmin("vinoly"), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r
}

func TestValidateUserOrAdmin_JobsGate(t *testing.T) {
	db := setupJobsGateDB(t)

	vinoly := data_models.User{Username: "vinoly", Email: "vinoly@example.test"}
	if err := db.Create(&vinoly).Error; err != nil {
		t.Fatalf("create vinoly: %v", err)
	}
	jenny := data_models.User{Username: "jenny", Email: "jenny@example.test"}
	if err := db.Create(&jenny).Error; err != nil {
		t.Fatalf("create jenny: %v", err)
	}
	admin := data_models.User{Username: "the_admin", Email: "admin@example.test"}
	if err := db.Create(&admin).Error; err != nil {
		t.Fatalf("create admin: %v", err)
	}

	cases := []struct {
		name   string
		userID uint
		role   string
		want   int
	}{
		{"owner vinoly passes", vinoly.ID, "Trusted", http.StatusOK},
		{"non-owner jenny is refused", jenny.ID, "Trusted", http.StatusForbidden},
		{"admin bypasses ownership", admin.ID, "Admin", http.StatusOK},
		{"missing user_id is refused", 0, "Trusted", http.StatusForbidden},
		{"unknown user_id is refused", 9999, "Trusted", http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := jobsGateRouter(db, tc.userID, tc.role)
			w := httptest.NewRecorder()
			req, _ := http.NewRequest("GET", "/jobs", nil)
			r.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Errorf("%s: got status %d, want %d (body: %s)", tc.name, w.Code, tc.want, w.Body.String())
			}
		})
	}
}
