package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// #10414-A: work expenses entered by hand, mirroring the manual-payment
// path (invoicing_manual_payments_test.go), minus everything about invoices
// (an expense is never invoiced).

func setupExpensesDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := setupInvoicingDB(t)
	// InvoicingExpenseReceipt is always migrated alongside InvoicingExpense
	// in production (database.go); DeleteExpenseHandler reads it to cascade
	// a receipt's delete with its expense (system_3 #10602).
	db.AutoMigrate(&data_models.InvoicingExpense{}, &data_models.InvoicingExpenseCategory{}, &data_models.InvoicingExpenseReceipt{})
	return db
}

func setupExpensesRouter(db *gorm.DB, userID uint) *gin.Engine {
	r := setupInvoicingRouter(db, userID)
	r.GET("/invoicing/expenses", ListInvoicingExpensesHandler)
	r.POST("/invoicing/expenses/manual", CreateManualExpenseHandler)
	r.DELETE("/invoicing/expenses/:id", DeleteExpenseHandler)
	r.GET("/invoicing/expense-categories", ListInvoicingExpenseCategoriesHandler)
	r.POST("/invoicing/expense-categories", CreateExpenseCategoryHandler)
	return r
}

func listExpenseCategories(t *testing.T, r *gin.Engine) []data_models.InvoicingExpenseCategory {
	t.Helper()
	w := invoicingDo(r, "GET", "/invoicing/expense-categories", "")
	var resp struct {
		Categories []data_models.InvoicingExpenseCategory `json:"categories"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	return resp.Categories
}

func createExpense(t *testing.T, r *gin.Engine, body string) (int, data_models.InvoicingExpense, string) {
	t.Helper()
	w := invoicingDo(r, "POST", "/invoicing/expenses/manual", body)
	var resp struct {
		Expense data_models.InvoicingExpense `json:"expense"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	return w.Code, resp.Expense, w.Body.String()
}

func listExpenses(t *testing.T, r *gin.Engine, query string) []data_models.InvoicingExpense {
	t.Helper()
	w := invoicingDo(r, "GET", "/invoicing/expenses"+query, "")
	var resp struct {
		Expenses []data_models.InvoicingExpense `json:"expenses"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	return resp.Expenses
}

const expenseFixtureUser uint = 11

func newExpenseFixture(t *testing.T) (*gorm.DB, *gin.Engine) {
	t.Helper()
	db := setupExpensesDB(t)
	r := setupExpensesRouter(db, expenseFixtureUser)
	withInvoicingClock(t, time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	return db, r
}

func TestManualExpense_CreateListAndDelete(t *testing.T) {
	db, r := newExpenseFixture(t)
	w := invoicingDo(r, "POST", "/invoicing/clients", `{"name":"Supplies Co"}`)
	var cl struct {
		Client data_models.InvoicingClient `json:"client"`
	}
	json.Unmarshal(w.Body.Bytes(), &cl)

	code, e, body := createExpense(t, r, fmt.Sprintf(`{"paid_on":"2026-07-14","amount_cents":4200,"category":"office and postage","reference":"RE-9","note":"Printer paper","client_id":%d}`, cl.Client.ID))
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	if e.Entry != "manual" || e.Source != "manual" || !strings.HasPrefix(e.TransactionID, "M-") ||
		e.Category != "office and postage" || e.Currency != "EUR" || e.Reference != "RE-9" || e.ClientID == nil {
		t.Fatalf("stored expense: %+v", e)
	}
	events, _ := data_models.ListInvoicingAuditEvents(db, expenseFixtureUser, "expense", e.ID)
	if len(events) != 1 || events[0].Action != "create_manual" || !strings.Contains(events[0].Details, "RE-9") {
		t.Errorf("audit: %+v", events)
	}

	got := listExpenses(t, r, "")
	if len(got) != 1 || got[0].ID != e.ID {
		t.Fatalf("list: %+v", got)
	}
	if got := listExpenses(t, r, "?category=travel"); len(got) != 0 {
		t.Errorf("category filter: %+v", got)
	}

	if w := invoicingDo(r, "DELETE", fmt.Sprintf("/invoicing/expenses/%d", e.ID), ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if got, _ := data_models.GetInvoicingExpense(db, expenseFixtureUser, e.ID); got != nil {
		t.Errorf("expense still stored after delete")
	}
	if w := invoicingDo(r, "DELETE", fmt.Sprintf("/invoicing/expenses/%d", e.ID), ""); w.Code != http.StatusNotFound {
		t.Errorf("delete again: %d", w.Code)
	}
	deleteEvents, _ := data_models.ListInvoicingAuditEvents(db, expenseFixtureUser, "expense", e.ID)
	if len(deleteEvents) != 2 || deleteEvents[0].Action != "delete" {
		t.Errorf("delete audit: %+v", deleteEvents)
	}
}

func TestManualExpense_DefaultCategoryIsOther(t *testing.T) {
	_, r := newExpenseFixture(t)
	code, e, body := createExpense(t, r, `{"paid_on":"2026-07-14","amount_cents":1000}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	if e.Category != "other" {
		t.Errorf("default category = %q, want other", e.Category)
	}
}

func TestManualExpense_Refusals(t *testing.T) {
	_, r := newExpenseFixture(t)
	ok := `"paid_on":"2026-07-14","amount_cents":1000`
	cases := []struct {
		name, body, code string
	}{
		{"future date", `{"paid_on":"2026-10-22","amount_cents":1000}`, "invalid_field"},
		{"not a date", `{"paid_on":"14.07.2026","amount_cents":1000}`, "invalid_field"},
		{"zero amount", `{"paid_on":"2026-07-14","amount_cents":0}`, "invalid_field"},
		{"negative amount", `{"paid_on":"2026-07-14","amount_cents":-5}`, "invalid_field"},
		{"missing amount", `{"paid_on":"2026-07-14"}`, "invalid_field"},
		{"unknown category", `{` + ok + `,"category":"crypto"}`, "invalid_field"},
		{"lower-case currency", `{` + ok + `,"currency":"eur"}`, "invalid_field"},
		{"reference with a newline", `{` + ok + `,"reference":"a\nb"}`, "invalid_field"},
		{"reference too long", `{` + ok + `,"reference":"` + strings.Repeat("x", 101) + `"}`, "invalid_field"},
		{"unknown client", `{` + ok + `,"client_id":999}`, "invalid_field"},
		{"transaction id is server-owned", `{` + ok + `,"transaction_id":"X"}`, "unknown_field"},
		{"entry is server-owned", `{` + ok + `,"entry":"import"}`, "unknown_field"},
		{"right-to-left override in the reference", `{` + ok + `,"reference":"RE‮71"}`, "invalid_field"},
		{"zero-width space in the reference", `{` + ok + `,"reference":"RE​71"}`, "invalid_field"},
		{"before 2000", `{"paid_on":"1999-12-31","amount_cents":1000}`, "invalid_field"},
	}
	for _, tc := range cases {
		w := invoicingDo(r, "POST", "/invoicing/expenses/manual", tc.body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.code) {
			t.Errorf("%s: %d %s", tc.name, w.Code, w.Body.String())
		}
	}
	got := listExpenses(t, r, "")
	if len(got) != 0 {
		t.Errorf("refusals stored %d expenses", len(got))
	}
}

// Each owner keeps their own expenses (system_3 #10054's per-owner book,
// extended to expenses).
func TestManualExpense_OwnerIsolation(t *testing.T) {
	db := setupExpensesDB(t)
	a, b := setupExpensesRouter(db, 7), setupExpensesRouter(db, 8)
	withInvoicingClock(t, time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))

	codeA, expA, bodyA := createExpense(t, a, `{"paid_on":"2026-07-14","amount_cents":1000,"note":"A's expense"}`)
	if codeA != http.StatusCreated {
		t.Fatalf("create A: %d %s", codeA, bodyA)
	}

	if got := listExpenses(t, b, ""); len(got) != 0 {
		t.Errorf("owner B sees owner A's expenses: %+v", got)
	}
	if got := listExpenses(t, a, ""); len(got) != 1 {
		t.Errorf("positive control: owner A's own list is missing its expense: %+v", got)
	}
	if w := invoicingDo(b, "DELETE", fmt.Sprintf("/invoicing/expenses/%d", expA.ID), ""); w.Code != http.StatusNotFound {
		t.Errorf("owner B deleted owner A's expense: %d", w.Code)
	}
	if got, _ := data_models.GetInvoicingExpense(db, 7, expA.ID); got == nil {
		t.Errorf("owner A's expense was deleted by B's refused request")
	}

	// Security review of PR 175 F1: the category-validity check itself must
	// be owner-scoped, not just the expense rows. Owner A adds her own
	// category; owner B must not be able to file an expense under it.
	if w := invoicingDo(a, "POST", "/invoicing/expense-categories", `{"name":"A's own category"}`); w.Code != http.StatusCreated {
		t.Fatalf("create category for A: %d %s", w.Code, w.Body.String())
	}
	code, _, body := createExpense(t, b, `{"paid_on":"2026-07-14","amount_cents":1000,"category":"A's own category"}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "invalid_field") {
		t.Errorf("owner B used owner A's category: %d %s", code, body)
	}
}

// #10414-A/#10424: a fresh book is seeded with the operator-confirmed ten
// categories (c63803 on #9656) the first time it's touched, then the
// built-in Private, last (#10555-A3, §3.143 Decision 8).
func TestExpenseCategories_SeededOnFirstUse(t *testing.T) {
	_, r := newExpenseFixture(t)
	got := listExpenseCategories(t, r)
	if len(got) != len(data_models.DefaultExpenseCategories)+1 || got[len(got)-1].Name != data_models.ExpenseCategoryPrivate {
		t.Fatalf("seeded categories = %d, want the %d starters then Private: %+v", len(got), len(data_models.DefaultExpenseCategories), got)
	}
	for i, want := range data_models.DefaultExpenseCategories {
		if got[i].Name != want || got[i].CountsAs != want {
			t.Errorf("category %d = %+v, want name/counts_as %q", i, got[i], want)
		}
	}
	// Listing again does not reseed (no duplicates).
	if got2 := listExpenseCategories(t, r); len(got2) != len(data_models.DefaultExpenseCategories)+1 {
		t.Errorf("second list reseeded: %d categories", len(got2))
	}
}

// The operator's own ask (#10424, c63803): she can add a category on top of
// the starting list, and it is then usable on an expense. Its counts_as
// defaults to "other" (design_agent, c63823) when not given.
func TestExpenseCategories_AddAndUse(t *testing.T) {
	db, r := newExpenseFixture(t)
	w := invoicingDo(r, "POST", "/invoicing/expense-categories", `{"name":"Studio snacks"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create category: %d %s", w.Code, w.Body.String())
	}
	got := listExpenseCategories(t, r)
	// Her own category comes after the starters; Private stays last.
	last := got[len(got)-2]
	if len(got) != len(data_models.DefaultExpenseCategories)+2 || last.Name != "Studio snacks" || last.CountsAs != "other" || got[len(got)-1].Name != data_models.ExpenseCategoryPrivate {
		t.Fatalf("categories after add: %+v", got)
	}
	// Security review of PR 175 F4: a category add is audited like an
	// expense create/delete.
	events, _ := data_models.ListInvoicingAuditEvents(db, expenseFixtureUser, "expense_category", last.ID)
	if len(events) != 1 || events[0].Action != "create_manual" || !strings.Contains(events[0].Details, "Studio snacks") {
		t.Errorf("category audit: %+v", events)
	}
	code, e, body := createExpense(t, r, `{"paid_on":"2026-07-14","amount_cents":500,"category":"Studio snacks"}`)
	if code != http.StatusCreated || e.Category != "Studio snacks" {
		t.Fatalf("create with a user-added category: %d %s", code, body)
	}
}

// An explicit counts_as is honoured; an invalid one (not a starter type) is
// refused (design_agent, c63823: "every expense category resolves to one
// non-null starter type").
func TestExpenseCategories_CountsAs(t *testing.T) {
	_, r := newExpenseFixture(t)
	w := invoicingDo(r, "POST", "/invoicing/expense-categories", `{"name":"Guitar strings","counts_as":"materials and supplies"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create with counts_as: %d %s", w.Code, w.Body.String())
	}
	got := listExpenseCategories(t, r)
	// Hers is the last before Private, which is always last (#10555-A3).
	if got[len(got)-2].Name != "Guitar strings" || got[len(got)-2].CountsAs != "materials and supplies" {
		t.Errorf("counts_as not honoured: %+v", got[len(got)-2])
	}
	if w := invoicingDo(r, "POST", "/invoicing/expense-categories", `{"name":"Mystery item","counts_as":"not a starter type"}`); w.Code != http.StatusBadRequest {
		t.Errorf("invalid counts_as: %d %s", w.Code, w.Body.String())
	}
}

func TestExpenseCategories_DuplicateRefused(t *testing.T) {
	_, r := newExpenseFixture(t)
	if w := invoicingDo(r, "POST", "/invoicing/expense-categories", `{"name":"Travel"}`); w.Code != http.StatusConflict {
		t.Errorf("duplicate of a seeded category (case-insensitive, 'travel' vs 'Travel'): %d %s", w.Code, w.Body.String())
	}
	if w := invoicingDo(r, "POST", "/invoicing/expense-categories", `{"name":""}`); w.Code != http.StatusBadRequest {
		t.Errorf("empty name: %d", w.Code)
	}
	if w := invoicingDo(r, "POST", "/invoicing/expense-categories", `{"name":"`+strings.Repeat("x", 101)+`"}`); w.Code != http.StatusBadRequest {
		t.Errorf("too long: %d", w.Code)
	}
	got := listExpenseCategories(t, r)
	if len(got) != len(data_models.DefaultExpenseCategories)+1 { // the starters and Private
		t.Errorf("refusals added a category: %+v", got)
	}
}

func TestExpenseCategories_OwnerIsolation(t *testing.T) {
	db := setupExpensesDB(t)
	a, b := setupExpensesRouter(db, 7), setupExpensesRouter(db, 8)
	withInvoicingClock(t, time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))

	if w := invoicingDo(a, "POST", "/invoicing/expense-categories", `{"name":"A's own category"}`); w.Code != http.StatusCreated {
		t.Fatalf("create for A: %d %s", w.Code, w.Body.String())
	}
	gotB := listExpenseCategories(t, b)
	for _, cat := range gotB {
		if cat.Name == "A's own category" {
			t.Errorf("owner B sees owner A's category: %+v", gotB)
		}
	}
	if len(gotB) != len(data_models.DefaultExpenseCategories)+1 { // the starters and Private
		t.Errorf("owner B's own seeded list is wrong: %+v", gotB)
	}
}
