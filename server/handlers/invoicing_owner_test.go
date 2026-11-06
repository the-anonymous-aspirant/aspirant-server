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

// system_3 #10054-A2: payments, tombstones and imports belong to one book.
// The same PayPal file imports in full into a second book (communicator
// c62288, point 2), another book's payment cannot be edited or deleted, and a
// deletion in one book does not hide the row from another's import.
func TestInvoicingOwner_PaymentsAreSeparate(t *testing.T) {
	db := setupPaymentsDB(t)
	a, b := setupPaymentsRouter(db, 5), setupPaymentsRouter(db, 6)

	sa := decodeSummary(t, uploadCSV(t, a, paypalFixtureEN, ""))
	sb := decodeSummary(t, uploadCSV(t, b, paypalFixtureEN, ""))
	if sa.Imported == 0 || sb.Imported != sa.Imported || sb.Duplicates != 0 {
		t.Fatalf("second book's import: %+v, first: %+v; want the same rows imported, no duplicates", sb, sa)
	}
	if again := decodeSummary(t, uploadCSV(t, a, paypalFixtureEN, "")); again.Imported != 0 || again.Duplicates != sa.Imported {
		t.Errorf("re-import into the same book: %+v, want all %d as duplicates", again, sa.Imported)
	}

	list := func(r *gin.Engine) []data_models.InvoicingPayment {
		var out struct {
			Payments []data_models.InvoicingPayment `json:"payments"`
		}
		w := invoicingDo(r, "GET", "/invoicing/payments", "")
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("list: %v %s", err, w.Body.String())
		}
		return out.Payments
	}
	la, lb := list(a), list(b)
	if len(la) == 0 || len(la) != len(lb) {
		t.Fatalf("lists: %d and %d rows, want the same non-zero count", len(la), len(lb))
	}
	ids := map[uint]bool{}
	for _, p := range la {
		ids[p.ID] = true
	}
	for _, p := range lb {
		if ids[p.ID] {
			t.Errorf("payment %d is in both books", p.ID)
		}
	}

	victim := la[0].ID
	if w := invoicingDo(b, "PATCH", fmt.Sprintf("/invoicing/payments/%d", victim), `{"category":"gift"}`); w.Code != http.StatusNotFound {
		t.Errorf("another book edits payment %d: %d, want 404", victim, w.Code)
	}
	if w := invoicingDo(b, "DELETE", fmt.Sprintf("/invoicing/payments/%d", victim), ""); w.Code != http.StatusNotFound {
		t.Errorf("another book deletes payment %d: %d, want 404", victim, w.Code)
	}
	if p, _ := data_models.GetInvoicingPayment(db, 5, victim); p == nil || p.Category != la[0].Category {
		t.Errorf("payment %d changed or vanished: %+v", victim, p)
	}

	// A delete tombstones the row in its own book only.
	if w := invoicingDo(a, "DELETE", fmt.Sprintf("/invoicing/payments/%d", victim), ""); w.Code != http.StatusOK && w.Code != http.StatusNoContent {
		t.Fatalf("own delete: %d %s", w.Code, w.Body.String())
	}
	if seen, _ := data_models.InvoicingPaymentSeen(db, 5, la[0].Source, la[0].TransactionID); !seen {
		t.Errorf("positive control: the deleting book does not see its tombstone")
	}
	if seen, _ := data_models.InvoicingPaymentSeen(db, 7, la[0].Source, la[0].TransactionID); seen {
		t.Errorf("a third book sees the first book's tombstone")
	}
	c := setupPaymentsRouter(db, 7)
	if sc := decodeSummary(t, uploadCSV(t, c, paypalFixtureEN, "")); sc.Imported != sa.Imported {
		t.Errorf("a third book's import after the delete: %+v, want all %d rows", sc, sa.Imported)
	}
}

// system_3 #10054-A3: invoices, their numbers, PDFs, exports, the export check
// and the audit trail belong to one book. Each book numbers from <prefix>-001,
// and another book's invoice reads as 404 everywhere.
func TestInvoicingOwner_InvoicesAreSeparate(t *testing.T) {
	f := newInvoiceFixture(t) // user 9's book: profile, "Galerie Nord", payments
	withExports := func(r *gin.Engine) *gin.Engine {
		r.GET("/invoicing/export/invoices.zip", ExportInvoicesZipHandler)
		r.GET("/invoicing/export/invoices-merged.pdf", ExportInvoicesMergedPDFHandler)
		return r
	}
	withExports(f.r)
	id9 := f.draft(t, 0)
	if code, d, body := f.issue(id9); code != http.StatusOK || d.Invoice.InvoiceNumber == nil || *d.Invoice.InvoiceNumber != "2026-001" {
		t.Fatalf("user 9 issue: %d %s", code, body)
	}

	// User 10 sets up a book of their own in the same database.
	const other uint = 10
	r10 := withExports(setupInvoiceRouter(f.db, other))
	if w := invoicingDo(r10, "PUT", "/invoicing/profile", validProfileBody); w.Code != http.StatusOK {
		t.Fatalf("user 10 profile: %d %s", w.Code, w.Body.String())
	}
	w := invoicingDo(r10, "POST", "/invoicing/clients", `{"name":"Zehn Kunde","address":"Zehnweg 10\n10115 Berlin"}`)
	var cl struct {
		Client data_models.InvoicingClient `json:"client"`
	}
	json.Unmarshal(w.Body.Bytes(), &cl)
	cid := cl.Client.ID
	pay := data_models.InvoicingPayment{OwnerUserID: other, Source: "paypal", TransactionID: "TX10", Kind: "incoming",
		PaidOn: f.payments[0].PaidOn, Currency: "EUR", GrossCents: 10000, Note: "Work 10", Category: "paid_work", ClientID: &cid}
	if err := f.db.Create(&pay).Error; err != nil {
		t.Fatalf("seed user 10 payment: %v", err)
	}
	w = invoicingDo(r10, "POST", "/invoicing/invoices", fmt.Sprintf(`{"payment_ids":[%d]}`, pay.ID))
	if w.Code != http.StatusCreated {
		t.Fatalf("user 10 draft: %d %s", w.Code, w.Body.String())
	}
	id10 := decodeDetail(t, w.Body.Bytes()).Invoice.ID
	w = invoicingDo(r10, "POST", fmt.Sprintf("/invoicing/invoices/%d/issue", id10), "")
	if d := decodeDetail(t, w.Body.Bytes()); w.Code != http.StatusOK || d.Invoice.InvoiceNumber == nil || *d.Invoice.InvoiceNumber != "2026-001" {
		t.Fatalf("user 10's first invoice: %d %s, want number 2026-001 in its own book", w.Code, w.Body.String())
	}

	// List, detail, PDF, cancel.
	if w := invoicingDo(r10, "GET", "/invoicing/invoices", ""); !strings.Contains(w.Body.String(), "Zehn Kunde") || strings.Contains(w.Body.String(), "Galerie Nord") {
		t.Errorf("user 10's list: %s", w.Body.String())
	}
	for _, req := range []struct{ method, path, body string }{
		{"GET", fmt.Sprintf("/invoicing/invoices/%d", id9), ""},
		{"GET", fmt.Sprintf("/invoicing/invoices/%d/pdf", id9), ""},
		{"POST", fmt.Sprintf("/invoicing/invoices/%d/cancel", id9), `{"reason":"Falsche Adresse"}`},
		{"DELETE", fmt.Sprintf("/invoicing/invoices/%d", id9), ""},
	} {
		if w := invoicingDo(r10, req.method, req.path, req.body); w.Code != http.StatusNotFound {
			t.Errorf("user 10 %s %s: %d, want 404", req.method, req.path, w.Code)
		}
	}
	if inv, _ := data_models.GetInvoicingInvoice(f.db, invoiceFixtureUser, id9); inv == nil || inv.Status != "issued" {
		t.Errorf("user 9's invoice changed: %+v", inv)
	}

	// Exports and the export check.
	if body := invoicingDo(r10, "GET", "/invoicing/export/invoices.csv", "").Body.String(); strings.Contains(body, "Galerie Nord") || !strings.Contains(body, "Zehn Kunde") {
		t.Errorf("user 10's invoice CSV holds the wrong book: %q", body)
	}
	if w := invoicingDo(r10, "GET", "/invoicing/export/invoices.zip", ""); w.Code != http.StatusOK || len(unzipXLSX(t, w.Body.Bytes())) != 1 {
		t.Errorf("user 10's ZIP: %d, want exactly its one PDF", w.Code)
	}
	if w := invoicingDo(r10, "GET", fmt.Sprintf("/invoicing/export/invoices-merged.pdf?ids=%d", id9), ""); w.Code == http.StatusOK {
		t.Errorf("user 10 merged user 9's invoice into a PDF")
	}
	if w := invoicingDo(r10, "GET", "/invoicing/validation", ""); w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"code"`) {
		t.Errorf("user 10's export check: %d %s, want clean", w.Code, w.Body.String())
	}

	// The audit trail.
	if w := invoicingDo(r10, "GET", fmt.Sprintf("/invoicing/audit-events?entity_type=invoice&entity_id=%d", id9), ""); strings.Contains(w.Body.String(), `"action"`) {
		t.Errorf("user 10 reads user 9's invoice history: %s", w.Body.String())
	}
	if w := invoicingDo(f.r, "GET", fmt.Sprintf("/invoicing/audit-events?entity_type=invoice&entity_id=%d", id9), ""); !strings.Contains(w.Body.String(), `"issue"`) {
		t.Errorf("positive control: user 9 lacks its own invoice history: %s", w.Body.String())
	}

	// A draft in user 10's book pointing at user 9's client id: the list shows
	// no name for it and ?q= does not match it (security c62302).
	foreign := f.clientID
	if err := f.db.Create(&data_models.InvoicingInvoice{OwnerUserID: other, Status: "draft", ClientID: &foreign, Currency: "EUR", VatRateBP: 1900}).Error; err != nil {
		t.Fatalf("seed foreign-client draft: %v", err)
	}
	if body := invoicingDo(r10, "GET", "/invoicing/invoices?q=galerie", "").Body.String(); strings.Contains(body, `"id"`) {
		t.Errorf("?q= matched another book's client name: %s", body)
	}
	if body := invoicingDo(r10, "GET", "/invoicing/invoices", "").Body.String(); strings.Contains(body, "Galerie Nord") {
		t.Errorf("the list shows another book's client name: %s", body)
	}
}
