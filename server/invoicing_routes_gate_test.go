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

// #8367: the invoicing app holds a third party's personal and financial data,
// and the operator ruled it is for flowah plus Admin, nobody else (decision 2,
// c51421). These tests walk every route the shipped router registers under
// /invoicing/ — including routes later subtasks add — so a route that misses
// the group gate reds here rather than in production.

const (
	invoicingOwnerID uint = 1
	invoicingOtherID uint = 2
	invoicingAdminID uint = 3
)

// invoicingGateRouter is realRouter with the gate's subjects seeded: the gate
// compares the caller's username, so the token ids must resolve to users.
func invoicingGateRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	db.AutoMigrate(&data_models.User{}, &data_models.SiteSetting{},
		&data_models.InvoicingProfile{}, &data_models.InvoicingClient{}, &data_models.InvoicingAuditEvent{},
		&data_models.InvoicingPayment{}, &data_models.InvoicingInvoice{}, &data_models.InvoicingLineItem{},
		&data_models.InvoicingInvoicePayment{}, &data_models.InvoicingInvoicePDF{})
	for id, name := range map[uint]string{
		invoicingOwnerID: invoicesOwnerUsername,
		invoicingOtherID: "someone_else",
		invoicingAdminID: "an_admin",
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

// invoicingRoutes lists every registered /invoicing/ route, with path params
// filled so the request matches the route.
func invoicingRoutes(r *gin.Engine) []gin.RouteInfo {
	var out []gin.RouteInfo
	for _, ri := range r.Routes() {
		if !strings.HasPrefix(ri.Path, "/invoicing/") {
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

func invoicingCall(r *gin.Engine, method, path, token string) int {
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

func TestInvoicingRoutes_OnlyFlowahAndAdminPassTheGate(t *testing.T) {
	loadTestJWTSecret(t)
	r := invoicingGateRouter(t)

	routes := invoicingRoutes(r)
	// Positive control: an empty enumeration would pass every assertion below.
	if len(routes) < 22 {
		t.Fatalf("found %d /invoicing/ routes, want at least the 22 A1..E2 register — the enumeration is not seeing the shipped table", len(routes))
	}

	owner := mustToken(t, invoicingOwnerID, "Member")
	other := mustToken(t, invoicingOtherID, "Member")
	admin := mustToken(t, invoicingAdminID, "Admin")

	for _, ri := range routes {
		ri := ri
		t.Run(ri.Method+" "+ri.Path, func(t *testing.T) {
			if code := invoicingCall(r, ri.Method, ri.Path, ""); code != http.StatusUnauthorized {
				t.Errorf("unauthenticated: got %d, want 401", code)
			}
			if code := invoicingCall(r, ri.Method, ri.Path, other); code != http.StatusForbidden {
				t.Errorf("another Member: got %d, want 403 — this route is not behind the flowah gate", code)
			}
			if code := invoicingCall(r, ri.Method, ri.Path, owner); code == http.StatusUnauthorized || code == http.StatusForbidden {
				t.Errorf("owner flowah: got %d, want the gate to let the owner through", code)
			}
			if code := invoicingCall(r, ri.Method, ri.Path, admin); code == http.StatusUnauthorized || code == http.StatusForbidden {
				t.Errorf("Admin: got %d, want the gate to let Admin through", code)
			}
		})
	}
}

func TestInvoicingOwnerIsFlowah(t *testing.T) {
	// The operator named the account (decision 2, c51421); a rename here is a
	// change of who can read a third party's financial data.
	if invoicesOwnerUsername != "flowah" {
		t.Fatalf("invoicesOwnerUsername = %q, want %q", invoicesOwnerUsername, "flowah")
	}
}
