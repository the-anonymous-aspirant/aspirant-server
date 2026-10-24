package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

// #8634 / #8644: every /cardgen/ route must sit in the Admin group. Same walk
// as #8642 and #8643 under its own helper names, so each branch is gated on
// its own and the walks coexist after merge.

func cardgenExportGateRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	db.AutoMigrate(&data_models.User{}, &data_models.SiteSetting{}, &data_models.CardExportProfile{})
	for id, name := range map[uint]string{1: "a_member", 2: "a_viewer", 3: "an_admin"} {
		u := data_models.User{Username: name, Email: name + "@example.test"}
		u.ID = id
		if err := db.Create(&u).Error; err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}
	t.Cleanup(func() { db.Close() })
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("db", db); c.Next() })
	RegisterRoutes(r, db)
	return r
}

func TestCardgenExportRoutes_OnlyAdminPassesTheGate(t *testing.T) {
	loadTestJWTSecret(t)
	r := cardgenExportGateRouter(t)
	var routes []gin.RouteInfo
	for _, ri := range r.Routes() {
		if strings.HasPrefix(ri.Path, "/cardgen/") {
			ri.Path = strings.ReplaceAll(ri.Path, ":id", "1")
			routes = append(routes, ri)
		}
	}
	if len(routes) < 5 {
		t.Fatalf("found %d /cardgen/ routes, want at least the 5 of #8644", len(routes))
	}
	member := mustToken(t, 1, "Member")
	viewer := mustToken(t, 2, "Viewer")
	admin := mustToken(t, 3, "Admin")
	call := func(method, path, token string) int {
		body := strings.NewReader("{}")
		if method == http.MethodGet || method == http.MethodDelete {
			body = strings.NewReader("")
		}
		req := httptest.NewRequest(method, path, body)
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	for _, ri := range routes {
		ri := ri
		t.Run(ri.Method+" "+ri.Path, func(t *testing.T) {
			if code := call(ri.Method, ri.Path, ""); code != http.StatusUnauthorized {
				t.Errorf("unauthenticated: got %d, want 401", code)
			}
			if code := call(ri.Method, ri.Path, viewer); code != http.StatusForbidden {
				t.Errorf("Viewer: got %d, want 403", code)
			}
			if code := call(ri.Method, ri.Path, member); code != http.StatusForbidden {
				t.Errorf("Member: got %d, want 403", code)
			}
			if code := call(ri.Method, ri.Path, admin); code == http.StatusUnauthorized || code == http.StatusForbidden {
				t.Errorf("Admin: got %d, want through the gate", code)
			}
		})
	}
}
