package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// system_3 #10555-A3 (design §3.143 Decision 8): what the Book counts is a
// stored selection of categories, saved as presets; "Work only" is built in;
// Private is a built-in expense category that never counts.

func newBookSelectionFixture(t *testing.T) (*gorm.DB, *gin.Engine) {
	t.Helper()
	db, r := newExpenseFixture(t)
	db.AutoMigrate(&data_models.InvoicingBookSelection{}, &data_models.InvoicingBookPreset{},
		&data_models.InvoicingPayment{}, &data_models.InvoicingPaymentTombstone{})
	addBookSelectionRoutes(r)
	r.GET("/invoicing/export/book-report.csv", ExportBookReportHandler)
	return db, r
}

func addBookSelectionRoutes(r *gin.Engine) {
	r.GET("/invoicing/book/selection", GetInvoicingBookSelectionHandler)
	r.PUT("/invoicing/book/selection", PutInvoicingBookSelectionHandler)
	r.POST("/invoicing/book/presets", CreateInvoicingBookPresetHandler)
	r.PUT("/invoicing/book/presets/:id", UpdateInvoicingBookPresetHandler)
	r.DELETE("/invoicing/book/presets/:id", DeleteInvoicingBookPresetHandler)
}

type bookResp struct {
	Selection struct {
		Income     []string `json:"income"`
		ExpenseAll bool     `json:"expense_all"`
		Expense    []string `json:"expense"`
		Preset     string   `json:"preset"`
		Changed    bool     `json:"changed"`
	} `json:"selection"`
	Presets []struct {
		ID         string   `json:"id"`
		Name       string   `json:"name"`
		Builtin    bool     `json:"builtin"`
		Income     []string `json:"income"`
		ExpenseAll bool     `json:"expense_all"`
		Expense    []string `json:"expense"`
	} `json:"presets"`
	Preset struct {
		ID string `json:"id"`
	} `json:"preset"`
}

func bookCall(t *testing.T, r *gin.Engine, method, path, body string) (int, bookResp, errEnvelope, string) {
	t.Helper()
	w := invoicingDo(r, method, path, body)
	var resp bookResp
	json.Unmarshal(w.Body.Bytes(), &resp)
	var e errEnvelope
	json.Unmarshal(w.Body.Bytes(), &e)
	return w.Code, resp, e, w.Body.String()
}

func auditCount(t *testing.T, db *gorm.DB, entity string) int {
	t.Helper()
	var n int
	db.Model(&data_models.InvoicingAuditEvent{}).Where("entity_type = ?", entity).Count(&n)
	return n
}

func TestBookSelection_DefaultIsWorkOnly(t *testing.T) {
	_, r := newBookSelectionFixture(t)
	code, got, _, body := bookCall(t, r, "GET", "/invoicing/book/selection", "")
	if code != http.StatusOK {
		t.Fatalf("get: %d %s", code, body)
	}
	s := got.Selection
	if strings.Join(s.Income, ",") != "paid_work" || !s.ExpenseAll || len(s.Expense) != 0 || s.Preset != "work_only" || s.Changed {
		t.Errorf("default selection: %+v", s)
	}
	if len(got.Presets) != 1 || got.Presets[0].ID != "work_only" || got.Presets[0].Name != "Work only" || !got.Presets[0].Builtin {
		t.Errorf("presets: %+v", got.Presets)
	}
}

func TestBookSelection_SetAndReadBack(t *testing.T) {
	db, r := newBookSelectionFixture(t)
	before := auditCount(t, db, "book_selection")
	code, got, _, body := bookCall(t, r, "PUT", "/invoicing/book/selection",
		`{"income":["paid_work","gift"],"expense_all":false,"expense":["travel","advice"],"preset":"work_only"}`)
	if code != http.StatusOK {
		t.Fatalf("put: %d %s", code, body)
	}
	if strings.Join(got.Selection.Income, ",") != "gift,paid_work" || got.Selection.ExpenseAll || strings.Join(got.Selection.Expense, ",") != "advice,travel" {
		t.Errorf("stored selection: %+v", got.Selection)
	}
	if got.Selection.Preset != "work_only" || !got.Selection.Changed {
		t.Errorf("it came from Work only and differs from it: %+v", got.Selection)
	}
	if _, again, _, _ := bookCall(t, r, "GET", "/invoicing/book/selection", ""); strings.Join(again.Selection.Expense, ",") != "advice,travel" {
		t.Errorf("read back: %+v", again.Selection)
	}
	if after := auditCount(t, db, "book_selection"); after != before+1 {
		t.Errorf("one audit row per write: %d -> %d", before, after)
	}
	// Setting it back to exactly Work only reads as unchanged.
	bookCall(t, r, "PUT", "/invoicing/book/selection", `{"income":["paid_work"],"expense_all":true,"expense":[],"preset":"work_only"}`)
	if _, back, _, _ := bookCall(t, r, "GET", "/invoicing/book/selection", ""); back.Selection.Changed {
		t.Errorf("Work only's own ticks are not a change: %+v", back.Selection)
	}
}

func TestBookSelection_RefusalsWriteNothing(t *testing.T) {
	db, r := newBookSelectionFixture(t)
	before := auditCount(t, db, "book_selection")
	for _, tc := range []struct{ body, code string }{
		{`{"income":["salary"],"expense_all":true,"expense":[]}`, "invalid_field"},
		{`{"income":["gift","gift"],"expense_all":true,"expense":[]}`, "invalid_field"},
		{`{"income":[],"expense_all":false,"expense":["not mine"]}`, "invalid_field"},
		{`{"income":[],"expense_all":false,"expense":["private"]}`, "private_never_counts"},
		{`{"income":[],"expense_all":false,"expense":["Private"]}`, "private_never_counts"},
		{`{"income":[],"expense_all":true,"expense":["travel"]}`, "invalid_field"},
		// expense_all is Work only's rule alone (design_agent c66120).
		{`{"income":["paid_work","gift"],"expense_all":true,"expense":[]}`, "invalid_field"},
		{`{"income":[],"expense_all":true}`, "invalid_field"},
		{`{"income":["paid_work"],"expense_all":true,"expense":[],"preset":"99"}`, "not_found"},
		{`{"income":[],"expense_all":true,"expense":[],"colour":"red"}`, "unknown_field"},
	} {
		code, _, e, body := bookCall(t, r, "PUT", "/invoicing/book/selection", tc.body)
		if code < 400 || code >= 500 || e.Error.Code != tc.code {
			t.Errorf("%s: %d %s", tc.body, code, body)
		}
	}
	if after := auditCount(t, db, "book_selection"); after != before {
		t.Errorf("refusals wrote %d audit rows", after-before)
	}
	if s, _ := data_models.GetInvoicingBookSelection(db, expenseFixtureUser); s != nil {
		t.Errorf("a refusal stored a selection: %+v", s)
	}
}

func TestBookPresets_CRUDAndTheBuiltin(t *testing.T) {
	db, r := newBookSelectionFixture(t)
	code, got, _, body := bookCall(t, r, "POST", "/invoicing/book/presets",
		`{"name":"Jobcenter","income":["paid_work","gift"],"expense_all":false,"expense":["travel"]}`)
	if code != http.StatusCreated || got.Preset.ID == "" {
		t.Fatalf("create: %d %s", code, body)
	}
	id := got.Preset.ID
	if len(got.Presets) != 2 || got.Presets[0].ID != "work_only" || got.Presets[1].Name != "Jobcenter" {
		t.Errorf("presets after create: %+v", got.Presets)
	}
	for _, tc := range []struct{ body, code string }{
		{`{"name":"jobcenter","income":[],"expense_all":false,"expense":[]}`, "duplicate_preset"},
		{`{"name":"Work Only","income":[],"expense_all":false,"expense":[]}`, "duplicate_preset"},
		{`{"name":"Private too","income":[],"expense_all":false,"expense":["private"]}`, "private_never_counts"},
		{`{"name":"","income":[],"expense_all":false,"expense":[]}`, "invalid_field"},
		{`{"name":"Everything","income":["paid_work"],"expense_all":true,"expense":[]}`, "invalid_field"},
	} {
		if code, _, e, body := bookCall(t, r, "POST", "/invoicing/book/presets", tc.body); code < 400 || e.Error.Code != tc.code {
			t.Errorf("create %s: %d %s", tc.body, code, body)
		}
	}
	// Select from it, then update it: the selection reads as changed.
	bookCall(t, r, "PUT", "/invoicing/book/selection", fmt.Sprintf(`{"income":["paid_work","gift"],"expense_all":false,"expense":["travel"],"preset":%q}`, id))
	if code, _, _, body := bookCall(t, r, "PUT", "/invoicing/book/presets/"+id, `{"name":"Jobcenter 2026","income":["paid_work"],"expense_all":false,"expense":["travel","advice"]}`); code != http.StatusOK {
		t.Fatalf("update: %d %s", code, body)
	}
	_, now, _, _ := bookCall(t, r, "GET", "/invoicing/book/selection", "")
	if now.Selection.Preset != id || !now.Selection.Changed || now.Presets[1].Name != "Jobcenter 2026" {
		t.Errorf("after updating the active preset: %+v %+v", now.Selection, now.Presets)
	}
	// The built-in can be neither updated nor deleted.
	for _, m := range []string{"PUT", "DELETE"} {
		if code, _, e, body := bookCall(t, r, m, "/invoicing/book/presets/work_only", `{"income":[],"expense_all":false,"expense":[]}`); code != http.StatusConflict || e.Error.Code != "builtin_preset" {
			t.Errorf("%s work_only: %d %s", m, code, body)
		}
	}
	// Another owner sees none of it.
	other := setupExpensesRouter(db, expenseFixtureUser+1)
	addBookSelectionRoutes(other)
	if code, _, _, body := bookCall(t, other, "DELETE", "/invoicing/book/presets/"+id, ""); code != http.StatusNotFound {
		t.Errorf("another owner deleted it: %d %s", code, body)
	}
	if _, theirs, _, _ := bookCall(t, other, "GET", "/invoicing/book/selection", ""); len(theirs.Presets) != 1 {
		t.Errorf("another owner sees her presets: %+v", theirs.Presets)
	}
	// Delete: the selection keeps its ticks and names no preset.
	if code, _, _, body := bookCall(t, r, "DELETE", "/invoicing/book/presets/"+id, ""); code != http.StatusOK {
		t.Fatalf("delete: %d %s", code, body)
	}
	_, after, _, _ := bookCall(t, r, "GET", "/invoicing/book/selection", "")
	if after.Selection.Preset != "" || strings.Join(after.Selection.Expense, ",") != "travel" || len(after.Presets) != 1 {
		t.Errorf("after deleting the active preset: %+v %+v", after.Selection, after.Presets)
	}
	if n := auditCount(t, db, "book_preset"); n != 3 {
		t.Errorf("preset audit rows (create, update, delete): %d", n)
	}
}

func TestPrivateCategory_BuiltInLastAndNeverCounted(t *testing.T) {
	db, r := newBookSelectionFixture(t)
	// A book seeded before Private existed gains it on first touch, last.
	for _, name := range data_models.DefaultExpenseCategories {
		db.Create(&data_models.InvoicingExpenseCategory{OwnerUserID: expenseFixtureUser, Name: name, CountsAs: name})
	}
	db.Create(&data_models.InvoicingExpenseCategory{OwnerUserID: expenseFixtureUser, Name: "Studio snacks", CountsAs: "other"})
	got := listExpenseCategories(t, r)
	if len(got) != len(data_models.DefaultExpenseCategories)+2 || got[len(got)-1].Name != data_models.ExpenseCategoryPrivate {
		t.Fatalf("categories: %+v", got)
	}
	if again := listExpenseCategories(t, r); len(again) != len(got) {
		t.Errorf("Private was added twice: %d", len(again))
	}
	// It cannot be made again, under any case, nor counted as.
	for _, body := range []string{`{"name":"Private"}`, `{"name":"PRIVATE"}`} {
		if w := invoicingDo(r, "POST", "/invoicing/expense-categories", body); w.Code != http.StatusConflict || errOf(t, w.Body.Bytes()).Error.Code != "reserved_category" {
			t.Errorf("create %s: %d %s", body, w.Code, w.Body.String())
		}
	}
	if w := invoicingDo(r, "POST", "/invoicing/expense-categories", `{"name":"Hidden","counts_as":"private"}`); w.Code != http.StatusBadRequest {
		t.Errorf("counts_as private: %d %s", w.Code, w.Body.String())
	}
	// A Private expense can be recorded, and the Book report leaves it out.
	if code, _, body := createExpense(t, r, `{"paid_on":"2026-07-14","amount_cents":777777,"category":"private"}`); code != http.StatusCreated {
		t.Fatalf("private expense: %d %s", code, body)
	}
	if code, _, body := createExpense(t, r, `{"paid_on":"2026-07-14","amount_cents":1234,"category":"travel"}`); code != http.StatusCreated {
		t.Fatalf("travel expense: %d %s", code, body)
	}
	w := invoicingDo(r, "GET", "/invoicing/export/book-report.csv", "")
	report := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(report, "12,34") && !strings.Contains(report, "12.34") {
		t.Fatalf("control: the counted travel expense must be in the report: %d %s", w.Code, report)
	}
	if strings.Contains(report, "7777") || strings.Contains(strings.ToLower(report), "private") {
		t.Errorf("a Private expense reached the report:\n%s", report)
	}
}
