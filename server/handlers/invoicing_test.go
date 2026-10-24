package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

// #8367-A1: the invoicing profile, clients and audit trail. The owner+Admin
// gate is asserted on the shipped router in server/invoicing_routes_gate_test.go;
// these tests drive the handlers directly.

func setupInvoicingDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	db.AutoMigrate(&data_models.InvoicingProfile{}, &data_models.InvoicingClient{}, &data_models.InvoicingAuditEvent{},
		&data_models.InvoicingInvoice{}, &data_models.InvoicingLineItem{}, &data_models.InvoicingInvoicePayment{},
		&data_models.InvoicingInvoicePDF{})
	t.Cleanup(func() { db.Close() })
	return db
}

func setupInvoicingRouter(db *gorm.DB, userID uint) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("db", db)
		if userID > 0 {
			c.Set("user_id", userID)
		}
		c.Next()
	})
	r.GET("/invoicing/profile", GetInvoicingProfileHandler)
	r.PUT("/invoicing/profile", PutInvoicingProfileHandler)
	r.GET("/invoicing/clients", ListInvoicingClientsHandler)
	r.POST("/invoicing/clients", CreateInvoicingClientHandler)
	r.PUT("/invoicing/clients/:id", UpdateInvoicingClientHandler)
	r.GET("/invoicing/audit-events", ListInvoicingAuditEventsHandler)
	return r
}

func invoicingDo(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(method, path, bytes.NewBufferString(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	r.ServeHTTP(w, req)
	return w
}

func countAuditEvents(t *testing.T, db *gorm.DB) int {
	t.Helper()
	var n int
	if err := db.Model(&data_models.InvoicingAuditEvent{}).Count(&n).Error; err != nil {
		t.Fatalf("count audit events: %v", err)
	}
	return n
}

const validProfileBody = `{"full_name":"Flower Example","address":"Beispielstr. 1\n10117 Berlin","steuernummer":"12/345/67890","email":"flower@example.test","is_kleinunternehmer":true,"invoice_number_prefix":"2026","next_invoice_number":1}`

func TestInvoicingProfile_GetBeforeSaveReturnsDefaults(t *testing.T) {
	r := setupInvoicingRouter(setupInvoicingDB(t), 42)
	w := invoicingDo(r, "GET", "/invoicing/profile", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET: got %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Profile data_models.InvoicingProfile `json:"profile"`
		Saved   bool                         `json:"saved"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Saved {
		t.Errorf("saved: got true on an empty db")
	}
	if resp.Profile.NextInvoiceNumber != 1 || !resp.Profile.IsKleinunternehmer || resp.Profile.InvoiceNumberPrefix == "" {
		t.Errorf("defaults: got %+v", resp.Profile)
	}
}

func TestInvoicingProfile_PutThenGetRoundTripsAndAudits(t *testing.T) {
	db := setupInvoicingDB(t)
	r := setupInvoicingRouter(db, 42)

	if w := invoicingDo(r, "PUT", "/invoicing/profile", validProfileBody); w.Code != http.StatusOK {
		t.Fatalf("PUT: got %d: %s", w.Code, w.Body.String())
	}
	w := invoicingDo(r, "GET", "/invoicing/profile", "")
	var resp struct {
		Profile data_models.InvoicingProfile `json:"profile"`
		Saved   bool                         `json:"saved"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Saved || resp.Profile.FullName != "Flower Example" || resp.Profile.Steuernummer != "12/345/67890" ||
		resp.Profile.Address != "Beispielstr. 1\n10117 Berlin" || resp.Profile.UpdatedBy != 42 {
		t.Errorf("round trip: got %+v saved=%v", resp.Profile, resp.Saved)
	}

	// A second save is an update, not a second profile row.
	if w := invoicingDo(r, "PUT", "/invoicing/profile", validProfileBody); w.Code != http.StatusOK {
		t.Fatalf("second PUT: got %d", w.Code)
	}
	var rows int
	db.Model(&data_models.InvoicingProfile{}).Count(&rows)
	if rows != 1 {
		t.Errorf("profile rows: got %d, want 1", rows)
	}

	events, _ := data_models.ListInvoicingAuditEvents(db, "profile", 0)
	if len(events) != 2 {
		t.Fatalf("audit events: got %d, want 2", len(events))
	}
	// Newest first: the update, then the create.
	if events[0].Action != "update" || events[1].Action != "create" || events[0].UserID != 42 {
		t.Errorf("audit actions: got %q/%q user %d", events[0].Action, events[1].Action, events[0].UserID)
	}
}

func TestInvoicingProfile_EmptySteuernummerSaves(t *testing.T) {
	// Issuing is what an empty Steuernummer blocks (spec §4.1), not editing.
	r := setupInvoicingRouter(setupInvoicingDB(t), 42)
	body := `{"full_name":"F","address":"A","steuernummer":"","email":"","is_kleinunternehmer":true,"invoice_number_prefix":"2026","next_invoice_number":1}`
	if w := invoicingDo(r, "PUT", "/invoicing/profile", body); w.Code != http.StatusOK {
		t.Fatalf("PUT: got %d: %s", w.Code, w.Body.String())
	}
}

func TestInvoicingProfile_InvalidBodiesAreRefusedAndWriteNothing(t *testing.T) {
	cases := map[string]string{
		"missing name":     `{"full_name":"","address":"A","is_kleinunternehmer":true,"invoice_number_prefix":"2026","next_invoice_number":1}`,
		"missing address":  `{"full_name":"F","address":"  ","is_kleinunternehmer":true,"invoice_number_prefix":"2026","next_invoice_number":1}`,
		"bad email":        `{"full_name":"F","address":"A","email":"not-an-email","is_kleinunternehmer":true,"invoice_number_prefix":"2026","next_invoice_number":1}`,
		"bad prefix":       `{"full_name":"F","address":"A","is_kleinunternehmer":true,"invoice_number_prefix":"20/26","next_invoice_number":1}`,
		"empty prefix":     `{"full_name":"F","address":"A","is_kleinunternehmer":true,"invoice_number_prefix":"","next_invoice_number":1}`,
		"zero next number": `{"full_name":"F","address":"A","is_kleinunternehmer":true,"invoice_number_prefix":"2026","next_invoice_number":0}`,
		"missing vat flag": `{"full_name":"F","address":"A","invoice_number_prefix":"2026","next_invoice_number":1}`,
		"missing next":     `{"full_name":"F","address":"A","is_kleinunternehmer":true,"invoice_number_prefix":"2026"}`,
		"not json":         `full_name=F`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			db := setupInvoicingDB(t)
			r := setupInvoicingRouter(db, 42)
			if w := invoicingDo(r, "PUT", "/invoicing/profile", body); w.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400: %s", w.Code, w.Body.String())
			}
			var rows int
			db.Model(&data_models.InvoicingProfile{}).Count(&rows)
			if rows != 0 || countAuditEvents(t, db) != 0 {
				t.Errorf("refused PUT wrote %d profile rows and %d audit rows", rows, countAuditEvents(t, db))
			}
		})
	}
}

func TestInvoicingClients_CreateListUpdateRoundTripsAndAudits(t *testing.T) {
	db := setupInvoicingDB(t)
	r := setupInvoicingRouter(db, 7)

	w := invoicingDo(r, "POST", "/invoicing/clients", `{"name":"Galerie Nord","address":"Nordweg 2\n13353 Berlin","email":"info@galerie.test","paypal_email":" Pay@Galerie.TEST "}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST: got %d: %s", w.Code, w.Body.String())
	}
	var created struct {
		Client data_models.InvoicingClient `json:"client"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)
	if created.Client.ID == 0 || created.Client.PayPalEmail != "pay@galerie.test" {
		t.Fatalf("created: %+v (paypal email should be trimmed and lower-cased for matching)", created.Client)
	}

	w = invoicingDo(r, "PUT", fmt.Sprintf("/invoicing/clients/%d", created.Client.ID), `{"name":"Galerie Nord GmbH","address":"Nordweg 2\n13353 Berlin"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT: got %d: %s", w.Code, w.Body.String())
	}

	w = invoicingDo(r, "GET", "/invoicing/clients", "")
	var list struct {
		Clients []data_models.InvoicingClient `json:"clients"`
	}
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Clients) != 1 || list.Clients[0].Name != "Galerie Nord GmbH" || list.Clients[0].Email != "" {
		t.Fatalf("list after update: %+v", list.Clients)
	}

	events, _ := data_models.ListInvoicingAuditEvents(db, "client", created.Client.ID)
	if len(events) != 2 || events[0].Action != "update" || events[1].Action != "create" || events[0].UserID != 7 {
		t.Fatalf("client audit: %+v", events)
	}
	if events[0].Details == "" {
		t.Errorf("update audit row carries no before/after details")
	}
}

func TestInvoicingClients_InvalidBodiesAreRefusedAndWriteNothing(t *testing.T) {
	cases := map[string]string{
		"missing name":     `{"name":" ","address":"A"}`,
		"missing address":  `{"name":"N","address":""}`,
		"bad email":        `{"name":"N","address":"A","email":"x@"}`,
		"bad paypal email": `{"name":"N","address":"A","paypal_email":"nope"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			db := setupInvoicingDB(t)
			r := setupInvoicingRouter(db, 7)
			if w := invoicingDo(r, "POST", "/invoicing/clients", body); w.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400: %s", w.Code, w.Body.String())
			}
			var rows int
			db.Model(&data_models.InvoicingClient{}).Count(&rows)
			if rows != 0 || countAuditEvents(t, db) != 0 {
				t.Errorf("refused POST wrote %d client rows and %d audit rows", rows, countAuditEvents(t, db))
			}
		})
	}
}

func TestInvoicingClients_UpdateUnknownOrBadIDIsRefused(t *testing.T) {
	db := setupInvoicingDB(t)
	r := setupInvoicingRouter(db, 7)
	if w := invoicingDo(r, "PUT", "/invoicing/clients/999", `{"name":"N","address":"A"}`); w.Code != http.StatusNotFound {
		t.Errorf("unknown id: got %d, want 404", w.Code)
	}
	if w := invoicingDo(r, "PUT", "/invoicing/clients/abc", `{"name":"N","address":"A"}`); w.Code != http.StatusBadRequest {
		t.Errorf("non-numeric id: got %d, want 400", w.Code)
	}
	if n := countAuditEvents(t, db); n != 0 {
		t.Errorf("refused updates wrote %d audit rows", n)
	}
}

func TestInvoicingAuditEvents_FilterNarrowsToOneEntity(t *testing.T) {
	db := setupInvoicingDB(t)
	r := setupInvoicingRouter(db, 7)
	invoicingDo(r, "PUT", "/invoicing/profile", validProfileBody)
	invoicingDo(r, "POST", "/invoicing/clients", `{"name":"A","address":"x"}`)
	invoicingDo(r, "POST", "/invoicing/clients", `{"name":"B","address":"y"}`)

	decode := func(w *httptest.ResponseRecorder) []data_models.InvoicingAuditEvent {
		var resp struct {
			Events []data_models.InvoicingAuditEvent `json:"events"`
		}
		json.Unmarshal(w.Body.Bytes(), &resp)
		return resp.Events
	}
	if all := decode(invoicingDo(r, "GET", "/invoicing/audit-events", "")); len(all) != 3 {
		t.Fatalf("unfiltered: got %d events, want 3", len(all))
	}
	clients := decode(invoicingDo(r, "GET", "/invoicing/audit-events?entity_type=client", ""))
	if len(clients) != 2 {
		t.Fatalf("entity_type=client: got %d, want 2", len(clients))
	}
	one := decode(invoicingDo(r, "GET", fmt.Sprintf("/invoicing/audit-events?entity_type=client&entity_id=%d", clients[1].EntityID), ""))
	if len(one) != 1 || one[0].EntityID != clients[1].EntityID {
		t.Fatalf("single entity: got %+v", one)
	}
	if w := invoicingDo(r, "GET", "/invoicing/audit-events?entity_id=x", ""); w.Code != http.StatusBadRequest {
		t.Errorf("bad entity_id: got %d, want 400", w.Code)
	}
}
