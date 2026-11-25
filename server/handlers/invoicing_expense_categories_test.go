package handlers

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// system_3 #11579 (design §3.161 Decisions 2 and 5; until it merges,
// #11566): a category ze added can be removed and restored, and a category ze
// types into an expense's field is saved with that expense, both or neither.
// Each test names the task's item it pins.

func newCategoryFixture(t *testing.T) (*gorm.DB, *gin.Engine) {
	t.Helper()
	db, r := newBookSelectionFixture(t) // expenses, categories, Book selection and its CSV
	addCategoryRoutes(r)
	return db, r
}

func addCategoryRoutes(r *gin.Engine) {
	r.PATCH("/invoicing/expenses/:id", PatchInvoicingExpenseHandler)
	r.POST("/invoicing/expense-categories/:id/remove", ExpenseCategoryActionHandler("remove"))
	r.POST("/invoicing/expense-categories/:id/restore", ExpenseCategoryActionHandler("restore"))
}

func categoryNamed(t *testing.T, r *gin.Engine, name string) data_models.InvoicingExpenseCategory {
	t.Helper()
	for _, c := range listExpenseCategories(t, r) {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no category %q", name)
	return data_models.InvoicingExpenseCategory{}
}

func addCategory(t *testing.T, r *gin.Engine, body string) data_models.InvoicingExpenseCategory {
	t.Helper()
	w := invoicingDo(r, "POST", "/invoicing/expense-categories", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("add category %s: %d %s", body, w.Code, w.Body.String())
	}
	var resp struct {
		Category data_models.InvoicingExpenseCategory `json:"category"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	return resp.Category
}

func categoryAction(r *gin.Engine, id uint, action string) (int, string) {
	w := invoicingDo(r, "POST", fmt.Sprintf("/invoicing/expense-categories/%d/%s", id, action), "")
	return w.Code, w.Body.String()
}

func catCreateExpense(t *testing.T, r *gin.Engine, body string) data_models.InvoicingExpense {
	t.Helper()
	code, e, resp := createExpense(t, r, body)
	if code != http.StatusCreated {
		t.Fatalf("create expense %s: %d %s", body, code, resp)
	}
	return e
}

func catPatchExpense(r *gin.Engine, id uint, body string) (int, string) {
	w := invoicingDo(r, "PATCH", fmt.Sprintf("/invoicing/expenses/%d", id), body)
	return w.Code, w.Body.String()
}

func catCount(t *testing.T, db *gorm.DB, model interface{}) int {
	t.Helper()
	var n int
	db.Model(model).Count(&n)
	return n
}

func bookCSV(t *testing.T, r *gin.Engine) [][]string {
	t.Helper()
	w := invoicingDo(r, "GET", "/invoicing/export/book-report.csv", "")
	if w.Code != http.StatusOK {
		t.Fatalf("book report: %d %s", w.Code, w.Body.String())
	}
	rd := csv.NewReader(strings.NewReader(strings.TrimPrefix(w.Body.String(), string(rune(0xFEFF)))))
	rd.Comma = ';' // the German spreadsheet separator, as writeCSV writes it
	rd.FieldsPerRecord = -1
	rows, err := rd.ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	return rows
}

// Item 1: the ten starters and Private are refused, each with its reason,
// and stay as they were.
func TestExpenseCategoryRemove_BuiltinsRefused(t *testing.T) {
	db, r := newCategoryFixture(t)
	for _, tc := range []struct{ name, want string }{
		{"travel", "The starter categories can't be removed."},
		{"other", "The starter categories can't be removed."},
		{"private", "Private can't be removed."},
	} {
		cat := categoryNamed(t, r, tc.name)
		if !cat.Builtin {
			t.Errorf("%s is not marked builtin: %+v", tc.name, cat)
		}
		code, body := categoryAction(r, cat.ID, "remove")
		if code != http.StatusConflict || !strings.Contains(body, "builtin_category") || !strings.Contains(body, tc.want) {
			t.Errorf("remove %s: %d %s", tc.name, code, body)
		}
		if again := categoryNamed(t, r, tc.name); again.Removed() {
			t.Errorf("%s was removed anyway", tc.name)
		}
	}
	if n := auditCount(t, db, "expense_category"); n != 0 {
		t.Errorf("refusals wrote %d audit events", n)
	}
}

// Items 2, 3, 5, 6, 7: removing keeps the category and every expense that has
// it; the Book, its selection and the CSV count it as before; it can be saved
// unchanged; restore brings back its name and counts_as; each change is one
// audit event.
func TestExpenseCategoryRemove_KeepsWhatIsRecorded(t *testing.T) {
	db, r := newCategoryFixture(t)
	snacks := addCategory(t, r, `{"name":"Studio snacks","counts_as":"materials and supplies"}`)
	unused := addCategory(t, r, `{"name":"Unused one"}`)
	e := catCreateExpense(t, r, `{"paid_on":"2026-07-14","amount_cents":1250,"category":"Studio snacks","note":"Coffee"}`)
	categoriesBefore := catCount(t, db, &data_models.InvoicingExpenseCategory{})
	csvBefore := bookCSV(t, r)
	if row := readBookReportRow(csvBefore, "Studio snacks"); row == nil || !strings.Contains(strings.Join(row, ","), "12,50") {
		t.Fatalf("positive control: the CSV counts the expense before the remove: %v", csvBefore)
	}

	for _, cat := range []data_models.InvoicingExpenseCategory{snacks, unused} {
		if code, body := categoryAction(r, cat.ID, "remove"); code != http.StatusOK || !strings.Contains(body, `"removed_at":"`) {
			t.Fatalf("remove %s: %d %s", cat.Name, code, body)
		}
	}
	// Item 2: both rows are kept, the used and the unused.
	if n := catCount(t, db, &data_models.InvoicingExpenseCategory{}); n != categoriesBefore {
		t.Errorf("categories %d -> %d after removes", categoriesBefore, n)
	}
	// Item 6: the client can tell zir removed ones from live and built-in.
	listed := categoryNamed(t, r, "Studio snacks")
	if !listed.Removed() || listed.Builtin || listed.CountsAs != "materials and supplies" {
		t.Errorf("listed after remove: %+v", listed)
	}
	if live := categoryNamed(t, r, "travel"); live.Removed() || !live.Builtin {
		t.Errorf("a starter after the remove: %+v", live)
	}

	// Item 3: the expense keeps it, the list and its filter still show it,
	// the CSV counts it exactly as before, and the Book selection still takes it.
	if got, _ := data_models.GetInvoicingExpense(db, expenseFixtureUser, e.ID); got == nil || got.Category != "Studio snacks" {
		t.Errorf("expense after remove: %+v", got)
	}
	if got := listExpenses(t, r, "?category="+strings.ReplaceAll("Studio snacks", " ", "%20")); len(got) != 1 {
		t.Errorf("list filter by a removed category: %+v", got)
	}
	if after := bookCSV(t, r); strings.Join(readBookReportRow(after, "Studio snacks"), ",") != strings.Join(readBookReportRow(csvBefore, "Studio snacks"), ",") ||
		strings.Join(readBookReportRow(after, "Ausgaben gesamt"), ",") != strings.Join(readBookReportRow(csvBefore, "Ausgaben gesamt"), ",") {
		t.Errorf("CSV changed after the remove:\nbefore %v\nafter  %v", csvBefore, after)
	}
	if code, _, _, body := bookCall(t, r, "PUT", "/invoicing/book/selection", `{"income":["paid_work"],"expense_all":false,"expense":["Studio snacks"]}`); code != http.StatusOK {
		t.Errorf("Book selection with a removed category: %d %s", code, body)
	}
	if code, _, _, body := bookCall(t, r, "POST", "/invoicing/book/presets", `{"name":"Snacks","income":["paid_work"],"expense_all":false,"expense":["Studio snacks"]}`); code != http.StatusCreated && code != http.StatusOK {
		t.Errorf("preset with a removed category: %d %s", code, body)
	}

	// Item 4's exception: saving it unchanged succeeds, with or without the
	// category in the body.
	if code, body := catPatchExpense(r, e.ID, `{"note":"Coffee beans","category":"Studio snacks"}`); code != http.StatusOK {
		t.Errorf("save with its own removed category: %d %s", code, body)
	}
	if code, body := catPatchExpense(r, e.ID, `{"note":"Coffee and milk"}`); code != http.StatusOK {
		t.Errorf("save without touching the category: %d %s", code, body)
	}

	// Item 5: restore, with the name and counts_as it had.
	if code, body := categoryAction(r, snacks.ID, "restore"); code != http.StatusOK {
		t.Fatalf("restore: %d %s", code, body)
	}
	back := categoryNamed(t, r, "Studio snacks")
	if back.Removed() || back.CountsAs != "materials and supplies" || back.ID != snacks.ID {
		t.Errorf("after restore: %+v", back)
	}
	// A repeat is a no-op with no second event.
	if code, _ := categoryAction(r, snacks.ID, "restore"); code != http.StatusOK {
		t.Errorf("restore again: %d", code)
	}
	// Item 7: one event per change: create, remove, restore.
	events, _ := data_models.ListInvoicingAuditEvents(db, expenseFixtureUser, "expense_category", snacks.ID)
	actions := []string{}
	for _, ev := range events {
		actions = append(actions, ev.Action)
	}
	if strings.Join(actions, ",") != "restore,remove,create_manual" {
		t.Errorf("audit actions, newest first: %v", actions)
	}
}

// Item 4: a removed category is refused on another expense, on every path
// that puts one on an expense: create, edit (the row's control and "Set
// category" are edits), and both import confirms.
func TestExpenseCategoryRemove_NotNewlyChoosable(t *testing.T) {
	db, r := newCategoryFixture(t)
	gone := addCategory(t, r, `{"name":"Old supplier"}`)
	other := catCreateExpense(t, r, `{"paid_on":"2026-07-14","amount_cents":900,"category":"travel"}`)
	if code, body := categoryAction(r, gone.ID, "remove"); code != http.StatusOK {
		t.Fatalf("remove: %d %s", code, body)
	}
	expensesBefore := catCount(t, db, &data_models.InvoicingExpense{})

	code, _, body := createExpense(t, r, `{"paid_on":"2026-07-14","amount_cents":500,"category":"Old supplier"}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "removed_category") || !strings.Contains(body, "restore it") {
		t.Errorf("create with a removed category: %d %s", code, body)
	}
	if code, body := catPatchExpense(r, other.ID, `{"category":"Old supplier"}`); code != http.StatusBadRequest || !strings.Contains(body, "removed_category") {
		t.Errorf("edit onto a removed category: %d %s", code, body)
	}
	if got, _ := data_models.GetInvoicingExpense(db, expenseFixtureUser, other.ID); got.Category != "travel" {
		t.Errorf("refused edit changed the category: %+v", got)
	}
	if n := catCount(t, db, &data_models.InvoicingExpense{}); n != expensesBefore {
		t.Errorf("refusals stored expenses: %d -> %d", expensesBefore, n)
	}
	// Positive control: once restored, the same calls pass.
	categoryAction(r, gone.ID, "restore")
	if code, body := catPatchExpense(r, other.ID, `{"category":"Old supplier"}`); code != http.StatusOK {
		t.Errorf("edit onto the restored category: %d %s", code, body)
	}
}

func TestExpenseCategoryRemove_ImportConfirmsRefuseIt(t *testing.T) {
	t.Run("N26", func(t *testing.T) {
		db := setupN26ExpensesDB(t)
		r := setupN26ExpensesRouter(db, 7)
		addCategoryRoutes(r)
		gone := addCategory(t, r, `{"name":"Old supplier"}`)
		categoryAction(r, gone.ID, "remove")
		row := n26ExpenseConfirmRow{TransactionID: "stmt-0123456789abcdef01234567", PaidOn: "2026-08-03", Currency: "EUR", AmountCents: 4567, PayeeName: "Shop", Category: "Old supplier"}
		w := confirmN26Expenses(t, r, []n26ExpenseConfirmRow{row})
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_row") || !strings.Contains(w.Body.String(), "has been removed") {
			t.Errorf("N26 confirm with a removed category: %d %s", w.Code, w.Body.String())
		}
		if n := catCount(t, db, &data_models.InvoicingExpense{}); n != 0 {
			t.Errorf("N26 refusal stored %d expenses", n)
		}
		row.Category = "travel"
		if w := confirmN26Expenses(t, r, []n26ExpenseConfirmRow{row}); w.Code != http.StatusOK {
			t.Errorf("positive control, N26 with a live category: %d %s", w.Code, w.Body.String())
		}
	})
	t.Run("PayPal", func(t *testing.T) {
		db, r := setupPayPalExpensesRouter(t, 7)
		addCategoryRoutes(r)
		gone := addCategory(t, r, `{"name":"Old supplier"}`)
		categoryAction(r, gone.ID, "remove")
		c := paypalExpenseCandidate{TransactionID: "8HH88888HH8888888", PaidOn: "2026-07-25", Currency: "EUR", AmountCents: 2500, PayeeName: "Some Shop"}
		w := confirmPayPalExpenses(r, []paypalExpenseCandidate{c}, "Old supplier")
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_row") || !strings.Contains(w.Body.String(), "has been removed") {
			t.Errorf("PayPal confirm with a removed category: %d %s", w.Code, w.Body.String())
		}
		if n := catCount(t, db, &data_models.InvoicingExpense{}); n != 0 {
			t.Errorf("PayPal refusal stored %d expenses", n)
		}
		if w := confirmPayPalExpenses(r, []paypalExpenseCandidate{c}, "travel"); w.Code != http.StatusOK {
			t.Errorf("positive control, PayPal with a live category: %d %s", w.Code, w.Body.String())
		}
	})
}

// Item 8: one owner's remove and restore never touch another's book.
func TestExpenseCategoryRemove_OwnerScoped(t *testing.T) {
	db := setupExpensesDB(t)
	db.AutoMigrate(&data_models.InvoicingBookSelection{}, &data_models.InvoicingBookPreset{})
	a, b := setupExpensesRouter(db, 7), setupExpensesRouter(db, 8)
	addCategoryRoutes(a)
	addCategoryRoutes(b)
	mine := addCategory(t, a, `{"name":"A's own"}`)
	for _, action := range []string{"remove", "restore"} {
		if code, body := categoryAction(b, mine.ID, action); code != http.StatusNotFound {
			t.Errorf("B's %s of A's category: %d %s", action, code, body)
		}
	}
	if got := categoryNamed(t, a, "A's own"); got.Removed() {
		t.Errorf("A's category was removed by B's refused request")
	}
	if code, _ := categoryAction(a, mine.ID, "remove"); code != http.StatusOK {
		t.Errorf("positive control, A removes zir own: %d", code)
	}
	// B's own same-named category is unaffected by A's remove.
	theirs := addCategory(t, b, `{"name":"A's own"}`)
	if got := categoryNamed(t, b, "A's own"); got.Removed() || got.ID != theirs.ID {
		t.Errorf("B's category after A's remove: %+v", got)
	}
}

// Items 9 to 12: a typed category is saved with its expense, both or neither.
func TestExpenseTypedCategory_SavedWithTheExpense(t *testing.T) {
	db, r := newCategoryFixture(t)
	categories := func() int { return catCount(t, db, &data_models.InvoicingExpenseCategory{}) }
	expenses := func() int { return catCount(t, db, &data_models.InvoicingExpense{}) }
	listExpenseCategories(t, r) // seed
	c0, e0 := categories(), expenses()

	// Item 12: a new name is made, counting as Other, with the expense.
	e := catCreateExpense(t, r, `{"paid_on":"2026-07-14","amount_cents":700,"category":"  Guitar strings ","create_category":true}`)
	got := categoryNamed(t, r, "Guitar strings")
	if e.Category != "Guitar strings" || got.CountsAs != "other" || got.Removed() || categories() != c0+1 || expenses() != e0+1 {
		t.Fatalf("typed new category: expense %+v category %+v (%d/%d)", e, got, categories(), expenses())
	}
	if events, _ := data_models.ListInvoicingAuditEvents(db, expenseFixtureUser, "expense_category", got.ID); len(events) != 1 || events[0].Action != "create_manual" {
		t.Errorf("typed category audit: %+v", events)
	}

	// Item 11: a live name in another case is used, not made again.
	e2 := catCreateExpense(t, r, `{"paid_on":"2026-07-14","amount_cents":800,"category":"GUITAR STRINGS","create_category":true}`)
	if e2.Category != "Guitar strings" || categories() != c0+1 {
		t.Errorf("live name, other case: expense %+v, categories %d", e2, categories())
	}
	e3 := catCreateExpense(t, r, `{"paid_on":"2026-07-14","amount_cents":850,"category":"Travel","create_category":true}`)
	if e3.Category != "travel" || categories() != c0+1 {
		t.Errorf("a starter typed in another case: expense %+v, categories %d", e3, categories())
	}

	// Item 10: a removed name in another case is restored, with its counts_as.
	old := addCategory(t, r, `{"name":"Old supplier","counts_as":"advice"}`)
	categoryAction(r, old.ID, "remove")
	c1 := categories()
	if code, body := catPatchExpense(r, e.ID, `{"category":"old SUPPLIER","create_category":true}`); code != http.StatusOK || !strings.Contains(body, `"category":"Old supplier"`) {
		t.Errorf("typed removed name: %d %s", code, body)
	}
	if back := categoryNamed(t, r, "Old supplier"); back.Removed() || back.CountsAs != "advice" || back.ID != old.ID || categories() != c1 {
		t.Errorf("restored by typing: %+v, categories %d -> %d", back, c1, categories())
	}

	// Item 9, a refused name: nothing saved, and the error names the category.
	c2, e2n := categories(), expenses()
	for _, typed := range []string{"", "   ", strings.Repeat("x", 101), "two\nlines"} {
		body, _ := json.Marshal(map[string]any{"paid_on": "2026-07-14", "amount_cents": 300, "category": typed, "create_category": true})
		w := invoicingDo(r, "POST", "/invoicing/expenses/manual", string(body))
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_category") || !strings.Contains(w.Body.String(), "category ") {
			t.Errorf("typed %q: %d %s", typed, w.Code, w.Body.String())
		}
	}
	if categories() != c2 || expenses() != e2n {
		t.Errorf("refused names saved something: categories %d -> %d, expenses %d -> %d", c2, categories(), e2n, expenses())
	}

	// Item 9, a refused expense: no category is left behind, on create and on edit.
	if code, _, body := createExpense(t, r, `{"paid_on":"2026-07-14","amount_cents":0,"category":"Brand new","create_category":true}`); code != http.StatusBadRequest {
		t.Errorf("create with a bad amount: %d %s", code, body)
	}
	if code, _, body := createExpense(t, r, `{"paid_on":"2026-07-14","amount_cents":300,"category":"Brand new","create_category":true,"client_id":999}`); code != http.StatusBadRequest || !strings.Contains(body, "client_id") {
		t.Errorf("create with an unknown client, refused after the category is made: %d %s", code, body)
	}
	if code, body := catPatchExpense(r, e.ID, `{"category":"Brand new too","create_category":true,"amount_cents":-1}`); code != http.StatusBadRequest {
		t.Errorf("edit with a bad amount: %d %s", code, body)
	}
	for _, name := range []string{"Brand new", "Brand new too"} {
		for _, c := range listExpenseCategories(t, r) {
			if strings.EqualFold(c.Name, name) {
				t.Errorf("refused expense left category %q behind", name)
			}
		}
	}
	if categories() != c2 || expenses() != e2n {
		t.Errorf("refused expenses saved something: categories %d -> %d, expenses %d -> %d", c2, categories(), e2n, expenses())
	}
}

// The flag is explicit: without it a typed name is refused as before, so a
// typo never makes a category.
func TestExpenseTypedCategory_OnlyWithTheFlag(t *testing.T) {
	db, r := newCategoryFixture(t)
	listExpenseCategories(t, r)
	before := catCount(t, db, &data_models.InvoicingExpenseCategory{})
	code, _, body := createExpense(t, r, `{"paid_on":"2026-07-14","amount_cents":300,"category":"Brand new"}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "is not one of your expense categories") {
		t.Errorf("a new name without the flag: %d %s", code, body)
	}
	for _, b := range []string{
		`{"paid_on":"2026-07-14","amount_cents":300,"create_category":true}`,
		`{"paid_on":"2026-07-14","amount_cents":300,"category":"X","create_category":"yes"}`,
	} {
		if w := invoicingDo(r, "POST", "/invoicing/expenses/manual", b); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "create_category") {
			t.Errorf("bad flag %s: %d %s", b, w.Code, w.Body.String())
		}
	}
	if n := catCount(t, db, &data_models.InvoicingExpenseCategory{}); n != before {
		t.Errorf("categories %d -> %d", before, n)
	}
}

// POST /expense-categories with a removed name restores it, as it was,
// instead of answering duplicate; a live duplicate is still a conflict.
func TestCreateExpenseCategory_RemovedNameRestores(t *testing.T) {
	db, r := newCategoryFixture(t)
	old := addCategory(t, r, `{"name":"Old supplier","counts_as":"advice"}`)
	categoryAction(r, old.ID, "remove")
	before := catCount(t, db, &data_models.InvoicingExpenseCategory{})
	w := invoicingDo(r, "POST", "/invoicing/expense-categories", `{"name":"OLD SUPPLIER","counts_as":"travel"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"restored":true`) {
		t.Fatalf("re-add a removed name: %d %s", w.Code, w.Body.String())
	}
	back := categoryNamed(t, r, "Old supplier")
	if back.Removed() || back.CountsAs != "advice" || back.ID != old.ID || catCount(t, db, &data_models.InvoicingExpenseCategory{}) != before {
		t.Errorf("after re-add: %+v", back)
	}
	if w := invoicingDo(r, "POST", "/invoicing/expense-categories", `{"name":"Old supplier"}`); w.Code != http.StatusConflict {
		t.Errorf("live duplicate: %d %s", w.Code, w.Body.String())
	}
}
