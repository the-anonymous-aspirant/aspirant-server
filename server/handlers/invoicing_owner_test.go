package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
)

// system_3 #10054-A1: each signed-in user keeps their own invoicing book. The
// operator (an Admin) and the owner see only what they entered themselves
// (operator c62279: "a separate book per user").

// Two users each save a profile; neither sees the other's.
func TestInvoicingOwner_ProfilesAreSeparate(t *testing.T) {
	db := setupInvoicingDB(t)
	admin, owner := setupInvoicingRouter(db, 7), setupInvoicingRouter(db, 8)

	if w := invoicingDo(admin, "PUT", "/invoicing/profile", validProfileBody); w.Code != http.StatusOK {
		t.Fatalf("admin put: %d %s", w.Code, w.Body.String())
	}
	w := invoicingDo(owner, "GET", "/invoicing/profile", "")
	var got struct {
		Saved   bool                         `json:"saved"`
		Profile data_models.InvoicingProfile `json:"profile"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v %s", err, w.Body.String())
	}
	if got.Saved || got.Profile.FullName != "" {
		t.Fatalf("the owner sees the admin's profile: saved=%v name=%q", got.Saved, got.Profile.FullName)
	}

	ownerBody := strings.Replace(validProfileBody, "Flower Example", "Vinoly Example", 1)
	if w := invoicingDo(owner, "PUT", "/invoicing/profile", ownerBody); w.Code != http.StatusOK {
		t.Fatalf("owner put: %d %s", w.Code, w.Body.String())
	}
	for user, want := range map[uint]string{7: "Flower Example", 8: "Vinoly Example"} {
		p, err := data_models.GetInvoicingProfile(db, user)
		if err != nil || p == nil || p.FullName != want {
			t.Errorf("user %d's profile = %+v, %v; want %q", user, p, err, want)
		}
	}
	var rows int
	db.Model(&data_models.InvoicingProfile{}).Count(&rows)
	if rows != 2 {
		t.Errorf("profile rows = %d, want one per user (2)", rows)
	}
}

// A client belongs to the book it was created in. Another user cannot list,
// edit or export it: to them it does not exist (404, not 403).
func TestInvoicingOwner_ClientsAreSeparate(t *testing.T) {
	db := setupInvoicingDB(t)
	admin, owner := setupInvoicingRouter(db, 7), setupInvoicingRouter(db, 8)

	w := invoicingDo(admin, "POST", "/invoicing/clients", `{"name":"Admin Testkunde"}`)
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Client data_models.InvoicingClient `json:"client"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.Client.ID == 0 {
		t.Fatalf("decode: %v %s", err, w.Body.String())
	}

	if w := invoicingDo(owner, "GET", "/invoicing/clients", ""); w.Code != http.StatusOK || strings.Contains(w.Body.String(), "Admin Testkunde") {
		t.Errorf("the owner's list holds the admin's client: %d %s", w.Code, w.Body.String())
	}
	if w := invoicingDo(admin, "GET", "/invoicing/clients", ""); !strings.Contains(w.Body.String(), "Admin Testkunde") {
		t.Errorf("positive control: the admin's own list lacks the client: %s", w.Body.String())
	}
	w = invoicingDo(owner, "PUT", fmt.Sprintf("/invoicing/clients/%d", created.Client.ID), `{"name":"Renamed"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("owner edits the admin's client: %d, want 404", w.Code)
	}
	if cl, _ := data_models.GetInvoicingClient(db, 7, created.Client.ID); cl == nil || cl.Name != "Admin Testkunde" {
		t.Errorf("the admin's client changed: %+v", cl)
	}

	exportAs := func(user uint) string {
		r := setupInvoicingRouter(db, user)
		r.GET("/invoicing/export/clients.csv", ExportClientsCSVHandler)
		return invoicingDo(r, "GET", "/invoicing/export/clients.csv", "").Body.String()
	}
	if strings.Contains(exportAs(8), "Admin Testkunde") {
		t.Errorf("the owner's client CSV holds the admin's client")
	}
	if !strings.Contains(exportAs(7), "Admin Testkunde") {
		t.Errorf("positive control: the admin's client CSV lacks the client")
	}
}

// Each user has their own logo row.
func TestInvoicingOwner_LogosAreSeparate(t *testing.T) {
	db := setupInvoicingDB(t)
	db.AutoMigrate(&data_models.InvoicingProfileLogo{})
	for _, user := range []uint{7, 8} {
		l := data_models.InvoicingProfileLogo{Content: []byte{byte(user)}, SHA256: fmt.Sprint("sha-", user), Width: 1, Height: 1}
		if err := data_models.SaveInvoicingProfileLogo(db, &l, user); err != nil {
			t.Fatalf("save %d: %v", user, err)
		}
	}
	// A second save replaces the user's own row, never another's.
	again := data_models.InvoicingProfileLogo{Content: []byte{70}, SHA256: "sha-7b", Width: 1, Height: 1}
	if err := data_models.SaveInvoicingProfileLogo(db, &again, 7); err != nil {
		t.Fatalf("resave: %v", err)
	}
	for user, want := range map[uint]string{7: "sha-7b", 8: "sha-8"} {
		if l, err := data_models.GetInvoicingProfileLogo(db, user); err != nil || l == nil || l.SHA256 != want {
			t.Errorf("user %d's logo = %+v, %v; want %s", user, l, err, want)
		}
	}
	if gone, err := data_models.DeleteInvoicingProfileLogo(db, 8); err != nil || !gone {
		t.Fatalf("delete 8: %v %v", gone, err)
	}
	if l, _ := data_models.GetInvoicingProfileLogo(db, 7); l == nil {
		t.Errorf("deleting user 8's logo removed user 7's")
	}
}

// With no signed-in user the group refuses the request: rows from before the
// owner column have owner 0, and a user-0 read would reach them.
func TestInvoicingOwner_NoUserIsRefused(t *testing.T) {
	db := setupInvoicingDB(t)
	legacy := data_models.InvoicingClient{Name: "Unowned Altkunde"}
	if err := db.Create(&legacy).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, user := range []uint{0, 7} {
		gin.SetMode(gin.TestMode)
		r := gin.New()
		r.Use(func(c *gin.Context) {
			c.Set("db", db)
			if user > 0 {
				c.Set("user_id", user)
			}
			c.Next()
		})
		g := r.Group("/invoicing", RequireInvoicingUser())
		g.GET("/clients", ListInvoicingClientsHandler)
		w := invoicingDo(r, "GET", "/invoicing/clients", "")
		switch {
		case user == 0 && w.Code != http.StatusUnauthorized:
			t.Errorf("no user: %d, want 401 (%s)", w.Code, w.Body.String())
		case user != 0 && w.Code != http.StatusOK:
			t.Errorf("user %d: %d, want 200", user, w.Code)
		case strings.Contains(w.Body.String(), "Unowned Altkunde"):
			t.Errorf("user %d sees an unowned client", user)
		}
	}
}

// The shared-book unique indexes are dropped, the per-owner ones hold, and a
// second run changes nothing.
func TestInvoicingOwner_IndexSwap(t *testing.T) {
	db := setupPaymentsDB(t)
	db.AutoMigrate(&data_models.InvoicingInvoice{})
	// What a database from before #10054 carries alongside the new tags.
	for _, ddl := range []string{
		"CREATE UNIQUE INDEX idx_invoicing_payments_source_txn ON invoicing_payments(source, transaction_id)",
		"CREATE UNIQUE INDEX idx_invoicing_payment_tombstone ON invoicing_payment_tombstones(source, transaction_id)",
		"CREATE UNIQUE INDEX idx_invoicing_invoices_number ON invoicing_invoices(invoice_number)",
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatalf("seed index: %v", err)
		}
	}
	for run := 1; run <= 2; run++ {
		if err := data_models.EnsureInvoicingOwnerIndexes(db); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
	}
	for _, name := range []string{"idx_invoicing_payments_source_txn", "idx_invoicing_payment_tombstone", "idx_invoicing_invoices_number"} {
		if db.Dialect().HasIndex("invoicing_payments", name) || db.Dialect().HasIndex("invoicing_payment_tombstones", name) || db.Dialect().HasIndex("invoicing_invoices", name) {
			t.Errorf("%s still exists", name)
		}
	}

	pay := func(owner uint) error {
		p := data_models.InvoicingPayment{OwnerUserID: owner, Source: "paypal", TransactionID: "TX1", Kind: "incoming", Currency: "EUR", GrossCents: 100}
		return db.Create(&p).Error
	}
	if err := pay(7); err != nil {
		t.Fatalf("first import: %v", err)
	}
	if err := pay(8); err != nil {
		t.Errorf("the same transaction in another book is refused: %v", err)
	}
	if err := pay(7); err == nil {
		t.Errorf("the same transaction twice in one book is accepted")
	}
	number := "2026-1"
	inv := func(owner uint) error {
		return db.Create(&data_models.InvoicingInvoice{OwnerUserID: owner, Status: "issued", InvoiceNumber: &number}).Error
	}
	if err := inv(7); err != nil {
		t.Fatalf("first invoice: %v", err)
	}
	if err := inv(8); err != nil {
		t.Errorf("2026-1 in a second book is refused: %v", err)
	}
	if err := inv(7); err == nil {
		t.Errorf("2026-1 twice in one book is accepted")
	}
}

// Security review of PR 172, F1: the book lock holds for a user who has not
// saved a profile, which after the per-user change is every user at first.
// The locked write paths take the lock row, not a profile row.
func TestInvoicingOwner_BookLockHoldsWithoutAProfile(t *testing.T) {
	db := setupPaymentsDB(t)
	lockRows := func(owner uint) int {
		var n int
		db.Raw("SELECT count(*) FROM invoicing_owner_locks WHERE owner_user_id = ?", owner).Row().Scan(&n)
		return n
	}

	tx := db.Begin()
	hasProfile, err := lockIssuing(tx, 8)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	if hasProfile {
		t.Errorf("lockIssuing reports a profile for a user who saved none")
	}
	if err := tx.Commit().Error; err != nil {
		t.Fatalf("commit: %v", err)
	}
	if n := lockRows(8); n != 1 {
		t.Fatalf("lock rows for user 8 = %d, want 1", n)
	}
	// A second lock reuses the row.
	tx = db.Begin()
	if _, err := lockIssuing(tx, 8); err != nil {
		t.Fatalf("relock: %v", err)
	}
	tx.Commit()
	if n := lockRows(8); n != 1 {
		t.Errorf("lock rows for user 8 after a relock = %d, want 1", n)
	}

	// A write path that discards lockIssuing's bool still locks: the PayPal
	// import, by a user with no profile and no lock row yet.
	if n := lockRows(9); n != 0 {
		t.Fatalf("precondition: user 9 already has a lock row")
	}
	r := setupPaymentsRouter(db, 9)
	if w := uploadCSV(t, r, paypalFixtureEN, ""); w.Code != http.StatusOK {
		t.Fatalf("import: %d %s", w.Code, w.Body.String())
	}
	if n := lockRows(9); n != 1 {
		t.Errorf("the import ran without taking user 9's book lock (lock rows = %d)", n)
	}
	if p, _ := data_models.GetInvoicingProfile(db, 9); p != nil {
		t.Errorf("the lock created a profile row; GET would read it as saved")
	}
}

// Security review of PR 172, F2: an import attaches a client only from the
// importer's own book, matched on the sender's PayPal email.
func TestInvoicingOwner_ImportMatchesOnlyTheImportersClients(t *testing.T) {
	clientOf := func(t *testing.T, clientBook uint) *uint {
		db := setupPaymentsDB(t)
		if err := db.Create(&data_models.InvoicingClient{OwnerUserID: clientBook, Name: "Galerie Nord", PayPalEmail: "pay@galerie.test"}).Error; err != nil {
			t.Fatalf("seed client: %v", err)
		}
		if w := uploadCSV(t, setupPaymentsRouter(db, 8), paypalFixtureEN, ""); w.Code != http.StatusOK {
			t.Fatalf("import: %d %s", w.Code, w.Body.String())
		}
		var p data_models.InvoicingPayment
		if err := db.Where("transaction_id = ?", "1AA11111AA1111111").First(&p).Error; err != nil {
			t.Fatalf("imported payment: %v", err)
		}
		return p.ClientID
	}
	if cid := clientOf(t, 7); cid != nil {
		t.Errorf("user 8's import attached client %d from user 7's book", *cid)
	}
	if cid := clientOf(t, 8); cid == nil {
		t.Errorf("positive control: user 8's import did not attach user 8's own client")
	}
}
