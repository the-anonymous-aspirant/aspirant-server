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

// system_3 #11494. No response of this API is meant for a shared cache, and a
// response with no Cache-Control leaves that to whatever sits in front of the
// server. These tests walk the shipped route table, so a route added later is
// covered without anyone remembering to add it here.

// keepsOutOfSharedCaches is true for the two values this server uses:
// "no-store" (the default) and "private, ..." (a handler's own choice).
func keepsOutOfSharedCaches(cacheControl string) bool {
	for _, part := range strings.Split(cacheControl, ",") {
		switch strings.TrimSpace(part) {
		case "no-store", "private":
			return true
		}
	}
	return false
}

// cacheRouter is the production route table over a database that holds every
// invoicing table, so the exports answer 200 for a signed-in owner.
func cacheRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	t.Setenv("COMMANDER_URL", "http://127.0.0.1:1")
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	db.AutoMigrate(&data_models.User{}, &data_models.SiteSetting{},
		&data_models.InvoicingProfile{}, &data_models.InvoicingClient{}, &data_models.InvoicingAuditEvent{},
		&data_models.InvoicingPayment{}, &data_models.InvoicingInvoice{}, &data_models.InvoicingLineItem{},
		&data_models.InvoicingInvoicePayment{}, &data_models.InvoicingInvoicePDF{}, &data_models.InvoicingProfileLogo{},
		&data_models.InvoicingExpense{}, &data_models.InvoicingExpenseCategory{}, &data_models.InvoicingExpenseReceipt{},
		&data_models.InvoicingExpenseTombstone{}, &data_models.InvoicingPaymentTombstone{},
		&data_models.InvoicingBookSelection{}, &data_models.InvoicingBookPreset{}, &data_models.InvoicingImportFailure{})
	u := data_models.User{Username: invoicesOwnerUsername, Email: invoicesOwnerUsername + "@example.test"}
	u.ID = invoicingOwnerID
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("seeding the owner: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("db", db); c.Next() })
	RegisterRoutes(r, db)
	return r
}

func cacheCall(r *gin.Engine, method, path, token string) *httptest.ResponseRecorder {
	body := strings.NewReader("")
	if method != http.MethodGet && method != http.MethodDelete {
		body = strings.NewReader("{}")
	}
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// Every route the server registers, asked without a session: whatever it
// answers (a public page, a 401, a 404) must not be storable by a shared cache.
func TestEveryRoute_KeepsItsResponseOutOfSharedCaches(t *testing.T) {
	loadTestJWTSecret(t)
	r := cacheRouter(t)

	routes := r.Routes()
	// Positive control: an empty enumeration would pass every assertion below.
	if len(routes) < 150 {
		t.Fatalf("found %d routes, want the shipped table (over 150): the enumeration is not seeing it", len(routes))
	}
	for _, ri := range routes {
		segs := strings.Split(ri.Path, "/")
		for i, s := range segs {
			if strings.HasPrefix(s, ":") || strings.HasPrefix(s, "*") {
				segs[i] = "1"
			}
		}
		path := strings.Join(segs, "/")
		w := cacheCall(r, ri.Method, path, "")
		if got := w.Header().Get("Cache-Control"); !keepsOutOfSharedCaches(got) {
			t.Errorf("%s %s (status %d): Cache-Control is %q, want no-store or private", ri.Method, ri.Path, w.Code, got)
		}
	}

	// A path no route matches is answered by the router itself.
	w := cacheCall(r, http.MethodGet, "/no-such-route.csv", "")
	if got := w.Header().Get("Cache-Control"); w.Code != http.StatusNotFound || got != "no-store" {
		t.Errorf("unknown path: status %d, Cache-Control %q; want 404 and no-store", w.Code, got)
	}
}

// The file downloads, asked by the signed-in owner: the file itself, on its
// 200, says no-store.
func TestSignedInDownloads_SayNoStore(t *testing.T) {
	loadTestJWTSecret(t)
	r := cacheRouter(t)
	owner := mustToken(t, invoicingOwnerID, "Member")

	// The CSV exports of an empty book are still files: each must be the 200
	// with its attachment, or this test is not pinning the success path.
	for _, path := range []string{
		"/invoicing/export/book-report.csv",
		"/invoicing/export/invoices.csv",
		"/invoicing/export/clients.csv",
	} {
		w := cacheCall(r, http.MethodGet, path, owner)
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") {
			t.Errorf("%s: status %d, Content-Disposition %q; want the 200 file", path, w.Code, w.Header().Get("Content-Disposition"))
		}
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control is %q, want no-store", path, got)
		}
	}

	// The other routes that answer with a file. Their status depends on data this test does not seed (an empty book has
	// no PDFs to zip, the user has no files on disk), so only the header is
	// asserted: it is set before the handler runs.
	for _, path := range []string{
		"/invoicing/export/invoices.zip",
		"/invoicing/export/invoices-merged.pdf?ids=1",
		"/invoicing/invoices/1/pdf",
		"/files/download/report.pdf",
		"/files/shared/download/report.pdf",
		"/commander/valuation-statement/processed/export.csv",
	} {
		w := cacheCall(r, http.MethodGet, path, owner)
		if w.Code == http.StatusUnauthorized {
			t.Errorf("%s: 401 for the signed-in owner: the request did not pass sign-in, so this is not the signed-in path", path)
		}
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s (status %d): Cache-Control is %q, want no-store", path, w.Code, got)
		}
	}
}
