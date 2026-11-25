package handlers

import (
	"bytes"
	"fmt"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// An id of 2^31 or more names no invoicing row: the keys are 32-bit integer
// columns, and Postgres refuses to compare such a value at all, so the request
// answered 500 (system_3 #12268, the #11498 review's C3-14). It now answers
// what an unknown id answers in the same place: 404 in a path, an empty list
// in a query, the existing refusal in a body.
//
// The tests run on sqlite, which compares any of these ids without complaint.
// postgresIntegerRange makes the test database refuse, as Postgres does, a
// query that carries an integer outside the 32-bit range, so a route that
// still sends the id to the database answers 500 here too.

const (
	unknownInRangeID = "2147483647" // the largest id the column holds; no row has it
	idTwoPow31       = "2147483648"
	idTwoPow63       = "9223372036854775808"
)

// idRangeFixture is the invoice fixture with every invoicing route an id
// reaches, a draft, an expense and the expense tables.
type idRangeFixture struct {
	*invoiceFixture
	draftID   uint
	expenseID uint
}

func newIDRangeFixture(t *testing.T) *idRangeFixture {
	t.Helper()
	f := newInvoiceFixture(t)
	postgresIntegerRange(f.db)
	f.db.AutoMigrate(&data_models.InvoicingExpense{}, &data_models.InvoicingExpenseCategory{},
		&data_models.InvoicingExpenseReceipt{}, &data_models.InvoicingExpenseTombstone{},
		&data_models.InvoicingBookSelection{}, &data_models.InvoicingBookPreset{},
		&data_models.InvoicingImportFailure{})
	r := f.r
	r.POST("/invoicing/expenses/manual", CreateManualExpenseHandler)
	r.PATCH("/invoicing/expenses/:id", PatchInvoicingExpenseHandler)
	registerExpenseRowActions(r)
	r.PUT("/invoicing/expenses/:id/receipt", PutInvoicingExpenseReceiptHandler)
	r.GET("/invoicing/expenses/:id/receipt", GetInvoicingExpenseReceiptHandler)
	r.DELETE("/invoicing/expenses/:id/receipt", DeleteInvoicingExpenseReceiptHandler)
	r.POST("/invoicing/expense-categories/:id/remove", ExpenseCategoryActionHandler("remove"))
	r.POST("/invoicing/expense-categories/:id/restore", ExpenseCategoryActionHandler("restore"))
	addBookSelectionRoutes(r)
	r.POST("/invoicing/payers", AddPayersHandler)
	setupMergeRouter(f)
	r.GET("/admin/invoicing/import-failures/:id/download", DownloadImportFailureHandler)

	x := &idRangeFixture{invoiceFixture: f, draftID: f.draft(t, 0)}
	// The second payment waits for a client, so a payer can be added to it.
	if err := f.db.Model(&data_models.InvoicingPayment{}).Where("id = ?", f.payments[1].ID).
		UpdateColumn("client_id", gorm.Expr("NULL")).Error; err != nil {
		t.Fatalf("unlink payment: %v", err)
	}
	w := invoicingDo(r, "POST", "/invoicing/expenses/manual", `{"paid_on":"2026-07-01","amount_cents":1500}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("seed expense: %d %s", w.Code, w.Body.String())
	}
	var e data_models.InvoicingExpense
	if err := f.db.Order("id desc").First(&e).Error; err != nil {
		t.Fatalf("read expense: %v", err)
	}
	x.expenseID = e.ID
	return x
}

// postgresIntegerRange fails every query, update, insert and delete on db
// whose values hold an integer outside the 32-bit range, with the error
// Postgres gives for an integer column. The fixtures' amounts and ids are all
// far inside it.
func postgresIntegerRange(db *gorm.DB) {
	check := func(scope *gorm.Scope) {
		for _, v := range scope.SQLVars {
			rv := reflect.Indirect(reflect.ValueOf(v))
			out := false
			switch rv.Kind() {
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
				out = rv.Uint() > math.MaxInt32
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				out = rv.Int() > math.MaxInt32 || rv.Int() < math.MinInt32
			}
			if out {
				scope.Err(fmt.Errorf("pq: value \"%v\" is out of range for type integer", rv.Interface()))
				return
			}
		}
	}
	cb := db.Callback()
	cb.Query().After("gorm:query").Register("test:pg_integer_range", check)
	cb.RowQuery().After("gorm:row_query").Register("test:pg_integer_range", check)
	cb.Update().After("gorm:update").Register("test:pg_integer_range", check)
	cb.Create().After("gorm:create").Register("test:pg_integer_range", check)
	cb.Delete().After("gorm:delete").Register("test:pg_integer_range", check)
}

// receiptUpload sends a one-pixel PNG as the receipt.
func receiptUpload(r *gin.Engine, path string, png []byte) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("receipt", "r.png")
	fw.Write(png)
	mw.Close()
	req := httptest.NewRequest("PUT", path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

type idRangeCase struct {
	name  string
	where string // path, query or body
	do    func(x *idRangeFixture, id string) *httptest.ResponseRecorder
}

func idRangeCases(t *testing.T) []idRangeCase {
	png := testReceiptPNG(t)
	call := func(method, path, body string) func(x *idRangeFixture, id string) *httptest.ResponseRecorder {
		return func(x *idRangeFixture, id string) *httptest.ResponseRecorder {
			p := strings.ReplaceAll(path, "{id}", id)
			p = strings.ReplaceAll(p, "{draft}", fmt.Sprint(x.draftID))
			p = strings.ReplaceAll(p, "{payment}", fmt.Sprint(x.payments[1].ID))
			p = strings.ReplaceAll(p, "{expense}", fmt.Sprint(x.expenseID))
			b := strings.ReplaceAll(body, "{id}", id)
			b = strings.ReplaceAll(b, "{payment}", fmt.Sprint(x.payments[1].ID))
			b = strings.ReplaceAll(b, "{client}", fmt.Sprint(x.clientID))
			return invoicingDo(x.r, method, p, b)
		}
	}
	return []idRangeCase{
		// In a path.
		{"PUT /clients/:id", "path", call("PUT", "/invoicing/clients/{id}", `{"name":"Atelier Probe","address":"Probeweg 1\n10115 Berlin"}`)},
		{"PATCH /payments/:id", "path", call("PATCH", "/invoicing/payments/{id}", `{"note":"n"}`)},
		{"POST /payments/:id/remove", "path", call("POST", "/invoicing/payments/{id}/remove", "")},
		{"POST /payments/:id/restore", "path", call("POST", "/invoicing/payments/{id}/restore", "")},
		{"POST /payments/:id/purge", "path", call("POST", "/invoicing/payments/{id}/purge", "")},
		{"PATCH /expenses/:id", "path", call("PATCH", "/invoicing/expenses/{id}", `{"note":"n"}`)},
		{"POST /expenses/:id/remove", "path", call("POST", "/invoicing/expenses/{id}/remove", "")},
		{"POST /expenses/:id/purge", "path", call("POST", "/invoicing/expenses/{id}/purge", "")},
		{"PUT /expenses/:id/receipt", "path", func(x *idRangeFixture, id string) *httptest.ResponseRecorder {
			return receiptUpload(x.r, "/invoicing/expenses/"+id+"/receipt", png)
		}},
		{"GET /expenses/:id/receipt", "path", call("GET", "/invoicing/expenses/{id}/receipt", "")},
		{"DELETE /expenses/:id/receipt", "path", call("DELETE", "/invoicing/expenses/{id}/receipt", "")},
		{"POST /expense-categories/:id/remove", "path", call("POST", "/invoicing/expense-categories/{id}/remove", "")},
		{"POST /expense-categories/:id/restore", "path", call("POST", "/invoicing/expense-categories/{id}/restore", "")},
		{"PUT /book/presets/:id", "path", call("PUT", "/invoicing/book/presets/{id}", `{"name":"Probe","income":["paid_work"],"expense_all":false,"expense":["travel"]}`)},
		{"DELETE /book/presets/:id", "path", call("DELETE", "/invoicing/book/presets/{id}", "")},
		{"GET /invoices/:id", "path", call("GET", "/invoicing/invoices/{id}", "")},
		{"PUT /invoices/:id", "path", call("PUT", "/invoicing/invoices/{id}", `{"client_id":{client}}`)},
		{"DELETE /invoices/:id", "path", call("DELETE", "/invoicing/invoices/{id}", "")},
		{"POST /invoices/:id/issue", "path", call("POST", "/invoicing/invoices/{id}/issue", "")},
		{"POST /invoices/:id/cancel", "path", call("POST", "/invoicing/invoices/{id}/cancel", `{"reason":"wrong amount"}`)},
		{"GET /invoices/:id/pdf", "path", call("GET", "/invoicing/invoices/{id}/pdf", "")},
		{"GET /admin/invoicing/import-failures/:id/download", "path", call("GET", "/admin/invoicing/import-failures/{id}/download", "")},
		// In a query.
		{"GET /audit-events?entity_id=", "query", call("GET", "/invoicing/audit-events?entity_id={id}", "")},
		{"GET /invoices?client_id=", "query", call("GET", "/invoicing/invoices?client_id={id}", "")},
		{"GET /export/invoices-merged.pdf?ids=", "query", call("GET", "/invoicing/export/invoices-merged.pdf?ids={id}", "")},
		// In a body.
		{"POST /payments/remove ids", "body", call("POST", "/invoicing/payments/remove", `{"ids":[{id}]}`)},
		{"POST /expenses/remove ids", "body", call("POST", "/invoicing/expenses/remove", `{"ids":[{id}]}`)},
		{"POST /invoices payment_ids", "body", call("POST", "/invoicing/invoices", `{"payment_ids":[{id}]}`)},
		{"POST /invoices/generate payment_ids", "body", call("POST", "/invoicing/invoices/generate", `{"payment_ids":[{id}]}`)},
		{"PUT /invoices/:draft client_id", "body", call("PUT", "/invoicing/invoices/{draft}", `{"client_id":{id}}`)},
		{"PATCH /payments/:payment client_id", "body", call("PATCH", "/invoicing/payments/{payment}", `{"client_id":{id}}`)},
		{"POST /payments/manual client_id", "body", call("POST", "/invoicing/payments/manual", `{"paid_on":"2026-08-01","gross_cents":5000,"method":"cash","reference":"R-1","client_id":{id}}`)},
		{"POST /expenses/manual client_id", "body", call("POST", "/invoicing/expenses/manual", `{"paid_on":"2026-07-01","amount_cents":1500,"client_id":{id}}`)},
		{"PUT /book/selection preset", "body", call("PUT", "/invoicing/book/selection", `{"preset":"{id}","income":["paid_work"],"expense_all":true,"expense":[]}`)},
		{"POST /payers client_id", "body", call("POST", "/invoicing/payers", `{"payers":[{"payment_ids":[{payment}],"client_id":{id}}]}`)},
		{"POST /payers payment_ids", "body", call("POST", "/invoicing/payers", `{"payers":[{"payment_ids":[{id}],"client_id":{client}}]}`)},
	}
}

// answer is a response with the id it was asked about taken out, so an
// answer that names the id compares equal across ids.
func answer(w *httptest.ResponseRecorder, id string) string {
	return fmt.Sprintf("%d %s", w.Code, strings.ReplaceAll(w.Body.String(), id, "<id>"))
}

func TestInvoicingIDOutOfRange_AnswersAsAnUnknownID(t *testing.T) {
	for _, tc := range idRangeCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			// A fresh book for every id, so an answer cannot depend on an
			// earlier request's write.
			unknown := tc.do(newIDRangeFixture(t), unknownInRangeID)
			if unknown.Code >= 500 {
				t.Fatalf("an unknown id answers %s", answer(unknown, unknownInRangeID))
			}
			want := answer(unknown, unknownInRangeID)
			for _, id := range []string{idTwoPow31, idTwoPow63} {
				w := tc.do(newIDRangeFixture(t), id)
				if got := answer(w, id); got != want {
					t.Errorf("%s id %s:\n got  %s\n want %s (what an unknown id answers)", tc.where, id, got, want)
				}
			}
			switch tc.where {
			case "path":
				if unknown.Code != http.StatusNotFound {
					t.Errorf("an unknown id in a path answers %d, want 404", unknown.Code)
				}
			case "query":
				if strings.Contains(tc.name, "merged") {
					break // the merge refuses an unknown id as invoice_not_found
				}
				if unknown.Code != http.StatusOK {
					t.Errorf("an unknown id in a query answers %d, want 200 with an empty list", unknown.Code)
				}
			}
		})
	}
}

func TestInvoicingIDInRange_TheColumnsBounds(t *testing.T) {
	for id, want := range map[uint]bool{0: false, 1: true, 2147483647: true, 2147483648: false, 1 << 63: false} {
		if got := data_models.InvoicingIDInRange(id); got != want {
			t.Errorf("InvoicingIDInRange(%d) = %v, want %v", id, got, want)
		}
	}
}

func TestMergedPDF_ALookupFailureCarriesNoDriverText(t *testing.T) {
	f := newInvoiceFixture(t)
	id := f.draft(t, 0)
	if code, _, body := f.issue(id); code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, body)
	}
	// The table is gone: the lookup fails with the driver's own words.
	f.db.DropTable(&data_models.InvoicingInvoice{})
	_, g := invoicesForMerge(f.db, invoiceFixtureUser, []uint{id})
	if g == nil || g.Status != http.StatusInternalServerError || g.Code != "lookup_failed" {
		t.Fatalf("want a 500 lookup_failed, got %+v", g)
	}
	if strings.Contains(g.Message, "no such table") || strings.Contains(g.Message, "invoicing_invoices") {
		t.Errorf("the answer carries the driver's text: %s", g.Message)
	}
}
