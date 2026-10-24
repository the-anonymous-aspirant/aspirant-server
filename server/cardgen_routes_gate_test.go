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

// #8634 / #8642: the card editor is an admin-only app (operator decision 1).
// This walks every route the shipped router registers under /cardgen/ —
// including the ones later subtasks add — so a route that lands outside the
// Admin group reds here rather than in production.

const (
	cardgenMemberID uint = 1
	cardgenViewerID uint = 2
	cardgenAdminID  uint = 3
)

func cardgenGateRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	db.AutoMigrate(&data_models.User{}, &data_models.SiteSetting{}, &data_models.CardAsset{})
	for id, name := range map[uint]string{
		cardgenMemberID: "a_member",
		cardgenViewerID: "a_viewer",
		cardgenAdminID:  "an_admin",
	} {
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

// cardgenRoutes lists every registered /cardgen/ route, with path params
// filled so the request matches the route.
func cardgenRoutes(r *gin.Engine) []gin.RouteInfo {
	var out []gin.RouteInfo
	for _, ri := range r.Routes() {
		if !strings.HasPrefix(ri.Path, "/cardgen/") {
			continue
		}
		segs := strings.Split(ri.Path, "/")
		for i, s := range segs {
			if strings.HasPrefix(s, ":") || strings.HasPrefix(s, "*") {
				segs[i] = "1"
			}
		}
		ri.Path = strings.Join(segs, "/")
		out = append(out, ri)
	}
	return out
}

func cardgenCall(r *gin.Engine, method, path, token string) int {
	var body *strings.Reader
	if method == http.MethodGet || method == http.MethodDelete {
		body = strings.NewReader("")
	} else {
		body = strings.NewReader("{}")
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

func TestCardgenRoutes_OnlyAdminPassesTheGate(t *testing.T) {
	loadTestJWTSecret(t)
	r := cardgenGateRouter(t)

	routes := cardgenRoutes(r)
	// Positive control: an empty enumeration would pass every assertion below.
	if len(routes) < 5 {
		t.Fatalf("found %d /cardgen/ routes, want at least the 5 asset routes of #8642 — the enumeration is not seeing the shipped table", len(routes))
	}

	member := mustToken(t, cardgenMemberID, "Member")
	viewer := mustToken(t, cardgenViewerID, "Viewer")
	admin := mustToken(t, cardgenAdminID, "Admin")

	for _, ri := range routes {
		ri := ri
		t.Run(ri.Method+" "+ri.Path, func(t *testing.T) {
			if code := cardgenCall(r, ri.Method, ri.Path, ""); code != http.StatusUnauthorized {
				t.Errorf("unauthenticated: got %d, want 401", code)
			}
			if code := cardgenCall(r, ri.Method, ri.Path, viewer); code != http.StatusForbidden {
				t.Errorf("Viewer: got %d, want 403 — this route is not behind the Admin gate", code)
			}
			if code := cardgenCall(r, ri.Method, ri.Path, member); code != http.StatusForbidden {
				t.Errorf("Member: got %d, want 403 — this route is not behind the Admin gate", code)
			}
			if code := cardgenCall(r, ri.Method, ri.Path, admin); code == http.StatusUnauthorized || code == http.StatusForbidden {
				t.Errorf("Admin: got %d, want the gate to let Admin through", code)
			}
		})
	}
}
