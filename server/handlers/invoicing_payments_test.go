package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// #8367-B1: PayPal import (dedupe, summary, client match) and payment
// categorisation, driven through the handlers on a sqlite db.

func setupPaymentsDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := setupInvoicingDB(t)
	db.AutoMigrate(&data_models.InvoicingPayment{})
	return db
}

func setupPaymentsRouter(db *gorm.DB, userID uint) *gin.Engine {
	r := setupInvoicingRouter(db, userID)
	r.POST("/invoicing/payments/import", ImportPayPalPaymentsHandler)
	r.GET("/invoicing/payments", ListInvoicingPaymentsHandler)
	r.PATCH("/invoicing/payments/:id", PatchInvoicingPaymentHandler)
	return r
}

func uploadCSV(t *testing.T, r *gin.Engine, content string, dateOrder string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "activity.csv")
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	fw.Write([]byte(content))
	if dateOrder != "" {
		mw.WriteField("date_order", dateOrder)
	}
	mw.Close()
	req, _ := http.NewRequest("POST", "/invoicing/payments/import", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decodeSummary(t *testing.T, w *httptest.ResponseRecorder) invoicingImportSummary {
	t.Helper()
	var resp struct {
		Summary invoicingImportSummary `json:"summary"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode summary: %v (%s)", err, w.Body.String())
	}
	return resp.Summary
}

func listPayments(t *testing.T, r *gin.Engine, query string) []data_models.InvoicingPayment {
	t.Helper()
	w := invoicingDo(r, "GET", "/invoicing/payments"+query, "")
	if w.Code != http.StatusOK {
		t.Fatalf("list payments: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Payments []data_models.InvoicingPayment `json:"payments"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	return resp.Payments
}

func TestImportPayPal_SummaryAndReuploadIsANoOp(t *testing.T) {
	db := setupPaymentsDB(t)
	r := setupPaymentsRouter(db, 5)

	w := uploadCSV(t, r, paypalFixtureEN, "")
	if w.Code != http.StatusOK {
		t.Fatalf("first import: %d %s", w.Code, w.Body.String())
	}
	s := decodeSummary(t, w)
	if s.RowsRead != 9 || s.Imported != 3 || s.Withdrawals != 1 || s.Duplicates != 0 || s.Skipped != 6 {
		t.Fatalf("first summary: %+v", s)
	}
	if s.RowsRead != s.Imported+s.Duplicates+s.Skipped {
		t.Errorf("summary does not add up: %+v", s)
	}

	// The German export of the same activity carries the same transaction
	// ids, so it is entirely duplicates.
	s2 := decodeSummary(t, uploadCSV(t, r, paypalFixtureDE, ""))
	if s2.Imported != 0 || s2.Duplicates != 3 {
		t.Fatalf("re-upload summary: %+v", s2)
	}
	var rows int
	db.Model(&data_models.InvoicingPayment{}).Count(&rows)
	if rows != 3 {
		t.Fatalf("payment rows after re-upload: %d, want 3", rows)
	}
	for _, p := range listPayments(t, r, "") {
		if p.Category != data_models.PaymentCategoryUncategorised {
			t.Errorf("payment %s starts as %q, want uncategorised", p.TransactionID, p.Category)
		}
	}
	events, _ := data_models.ListInvoicingAuditEvents(db, "import", 0)
	if len(events) != 2 || events[0].UserID != 5 || events[0].Action != "import" {
		t.Errorf("import audit: %+v", events)
	}
}

func TestImportPayPal_DuplicateIDWithinOneFileCountsOnce(t *testing.T) {
	r := setupPaymentsRouter(setupPaymentsDB(t), 5)
	csv := "Date,Name,Type,Status,Currency,Gross,Transaction ID\n" +
		"14.07.2026,A,Payment,Completed,EUR,10.00,T1\n14.07.2026,A,Payment,Completed,EUR,10.00,T1\n"
	s := decodeSummary(t, uploadCSV(t, r, csv, ""))
	if s.Imported != 1 || s.Duplicates != 1 {
		t.Fatalf("summary: %+v", s)
	}
}

func TestImportPayPal_MatchesClientByPayPalEmail(t *testing.T) {
	db := setupPaymentsDB(t)
	r := setupPaymentsRouter(db, 5)
	w := invoicingDo(r, "POST", "/invoicing/clients", `{"name":"Galerie Nord","address":"Nordweg 2","paypal_email":"PAY@galerie.test"}`)
	var created struct {
		Client data_models.InvoicingClient `json:"client"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)

	uploadCSV(t, r, paypalFixtureEN, "")
	for _, p := range listPayments(t, r, "?kind=incoming") {
		switch p.SenderEmail {
		case "pay@galerie.test":
			if p.ClientID == nil || *p.ClientID != created.Client.ID {
				t.Errorf("galerie payment not linked to client %d: %v", created.Client.ID, p.ClientID)
			}
		default:
			if p.ClientID != nil {
				t.Errorf("payment from %s linked to client %d", p.SenderEmail, *p.ClientID)
			}
		}
	}
}

func TestImportPayPal_RefusalsWriteNothing(t *testing.T) {
	db := setupPaymentsDB(t)
	r := setupPaymentsRouter(db, 5)

	ambiguous := "Date,Name,Type,Status,Currency,Gross,Transaction ID\n07/08/2026,A,Payment,Completed,EUR,10.00,T1\n"
	w := uploadCSV(t, r, ambiguous, "")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "date_order_ambiguous") {
		t.Errorf("ambiguous dates: %d %s", w.Code, w.Body.String())
	}
	if w := uploadCSV(t, r, "hello,world\n1,2\n", ""); w.Code != http.StatusBadRequest {
		t.Errorf("non-PayPal CSV: %d", w.Code)
	}
	if w := invoicingDo(r, "POST", "/invoicing/payments/import", `{}`); w.Code != http.StatusBadRequest {
		t.Errorf("no file: %d", w.Code)
	}
	big := strings.Repeat("x", maxPayPalImportBytes+1)
	if w := uploadCSV(t, r, big, ""); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize file: %d", w.Code)
	}
	var rows int
	db.Model(&data_models.InvoicingPayment{}).Count(&rows)
	if rows != 0 || countAuditEvents(t, db) != 0 {
		t.Errorf("refused imports wrote %d payments and %d audit rows", rows, countAuditEvents(t, db))
	}

	// The same ambiguous file imports once the order is given.
	if s := decodeSummary(t, uploadCSV(t, r, ambiguous, "dmy")); s.Imported != 1 {
		t.Errorf("explicit date_order import: %+v", s)
	}
}

func TestPatchPayment_CategoryClientAndDeposit(t *testing.T) {
	db := setupPaymentsDB(t)
	r := setupPaymentsRouter(db, 5)
	uploadCSV(t, r, paypalFixtureEN, "")
	incoming := listPayments(t, r, "?kind=incoming")
	withdrawals := listPayments(t, r, "?kind=withdrawal")
	if len(incoming) != 2 || len(withdrawals) != 1 {
		t.Fatalf("fixture import: %d incoming, %d withdrawals", len(incoming), len(withdrawals))
	}
	w := invoicingDo(r, "POST", "/invoicing/clients", `{"name":"Anna","address":"Weg 1"}`)
	var created struct {
		Client data_models.InvoicingClient `json:"client"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)

	p := incoming[0]
	path := fmt.Sprintf("/invoicing/payments/%d", p.ID)
	body := fmt.Sprintf(`{"category":"paid_work","client_id":%d,"bank_deposit_id":%d}`, created.Client.ID, withdrawals[0].ID)
	if w := invoicingDo(r, "PATCH", path, body); w.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	got := listPayments(t, r, "?category=paid_work")
	if len(got) != 1 || got[0].ID != p.ID || got[0].ClientID == nil || *got[0].ClientID != created.Client.ID ||
		got[0].BankDepositID == nil || *got[0].BankDepositID != withdrawals[0].ID {
		t.Fatalf("after patch: %+v", got)
	}

	// Leaving a field out keeps it; null clears it.
	if w := invoicingDo(r, "PATCH", path, `{"client_id":null}`); w.Code != http.StatusOK {
		t.Fatalf("clear client: %d %s", w.Code, w.Body.String())
	}
	after, _ := data_models.GetInvoicingPayment(db, p.ID)
	if after.ClientID != nil || after.Category != "paid_work" || after.BankDepositID == nil {
		t.Errorf("partial patch: %+v", after)
	}

	events, _ := data_models.ListInvoicingAuditEvents(db, "payment", p.ID)
	if len(events) != 2 || events[0].Action != "categorise" {
		t.Errorf("payment audit: %+v", events)
	}
}

func TestPatchPayment_RefusalsWriteNothing(t *testing.T) {
	db := setupPaymentsDB(t)
	r := setupPaymentsRouter(db, 5)
	uploadCSV(t, r, paypalFixtureEN, "")
	incoming := listPayments(t, r, "?kind=incoming")
	withdrawal := listPayments(t, r, "?kind=withdrawal")[0]
	before := countAuditEvents(t, db)

	in := fmt.Sprintf("/invoicing/payments/%d", incoming[0].ID)
	out := fmt.Sprintf("/invoicing/payments/%d", withdrawal.ID)
	cases := []struct {
		name, path, body string
		want             int
	}{
		{"unknown category", in, `{"category":"bogus"}`, http.StatusBadRequest},
		{"category on a withdrawal", out, `{"category":"paid_work"}`, http.StatusBadRequest},
		{"unknown client", in, `{"client_id":999}`, http.StatusBadRequest},
		{"zero client id", in, `{"client_id":0}`, http.StatusBadRequest},
		{"deposit is an incoming row", in, fmt.Sprintf(`{"bank_deposit_id":%d}`, incoming[1].ID), http.StatusBadRequest},
		{"withdrawal under a deposit", out, fmt.Sprintf(`{"bank_deposit_id":%d}`, withdrawal.ID), http.StatusBadRequest},
		{"unknown field", in, `{"amount":1}`, http.StatusBadRequest},
		{"empty body", in, `{}`, http.StatusBadRequest},
		{"unknown payment", "/invoicing/payments/999", `{"category":"gift"}`, http.StatusNotFound},
		{"bad id", "/invoicing/payments/x", `{"category":"gift"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		if w := invoicingDo(r, "PATCH", tc.path, tc.body); w.Code != tc.want {
			t.Errorf("%s: got %d, want %d (%s)", tc.name, w.Code, tc.want, w.Body.String())
		}
	}
	if after := countAuditEvents(t, db); after != before {
		t.Errorf("refused patches wrote %d audit rows", after-before)
	}
	for _, p := range listPayments(t, r, "") {
		if p.Category != data_models.PaymentCategoryUncategorised || p.BankDepositID != nil {
			t.Errorf("refused patch changed payment %d: %+v", p.ID, p)
		}
	}
	if w := invoicingDo(r, "GET", "/invoicing/payments?category=bogus", ""); w.Code != http.StatusBadRequest {
		t.Errorf("list with unknown category: %d", w.Code)
	}
}
