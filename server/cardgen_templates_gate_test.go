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

// #8634 / #8643: the card editor is admin-only (operator decision 1). This
// walks every route the shipped router registers under /cardgen/, so a
// template or type route outside the Admin group reds here. A1 (#8642) ships
// the same walk under its own helper names; both stay, both must pass.

const (
	cardgenTplMemberID uint = 1
	cardgenTplViewerID uint = 2
	cardgenTplAdminID  uint = 3
)

func cardgenTemplatesGateRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	db.AutoMigrate(&data_models.User{}, &data_models.SiteSetting{}, &data_models.CardType{}, &data_models.CardTemplate{}, &data_models.CardTemplateVersion{})
	for id, name := range map[uint]string{cardgenTplMemberID: "a_member", cardgenTplViewerID: "a_viewer", cardgenTplAdminID: "an_admin"} {
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

func cardgenTemplateRoutes(r *gin.Engine) []gin.RouteInfo {
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

func cardgenTemplateCall(r *gin.Engine, method, path, token string) int {
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

func TestCardgenTemplateRoutes_OnlyAdminPassesTheGate(t *testing.T) {
	loadTestJWTSecret(t)
	r := cardgenTemplatesGateRouter(t)
	routes := cardgenTemplateRoutes(r)
	if len(routes) < 12 {
		t.Fatalf("found %d /cardgen/ routes, want at least the 12 of #8643 — the enumeration is not seeing the shipped table", len(routes))
	}
	member := mustToken(t, cardgenTplMemberID, "Member")
	viewer := mustToken(t, cardgenTplViewerID, "Viewer")
	admin := mustToken(t, cardgenTplAdminID, "Admin")
	for _, ri := range routes {
		ri := ri
		t.Run(ri.Method+" "+ri.Path, func(t *testing.T) {
			if code := cardgenTemplateCall(r, ri.Method, ri.Path, ""); code != http.StatusUnauthorized {
				t.Errorf("unauthenticated: got %d, want 401", code)
			}
			if code := cardgenTemplateCall(r, ri.Method, ri.Path, viewer); code != http.StatusForbidden {
				t.Errorf("Viewer: got %d, want 403", code)
			}
			if code := cardgenTemplateCall(r, ri.Method, ri.Path, member); code != http.StatusForbidden {
				t.Errorf("Member: got %d, want 403", code)
			}
			if code := cardgenTemplateCall(r, ri.Method, ri.Path, admin); code == http.StatusUnauthorized || code == http.StatusForbidden {
				t.Errorf("Admin: got %d, want through the gate", code)
			}
		})
	}
}
