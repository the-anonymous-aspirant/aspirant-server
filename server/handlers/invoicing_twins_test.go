package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"aspirant-online/server/data_models"
)

// system_3 #12295: a hand entry that looks like a row already in the book is
// asked about once, with 409 possible_duplicate naming that row, and saved
// when the request says confirm_duplicate: true. It is never refused.

// duplicateOf reads a possible_duplicate answer: the other row's id, or 0
// when the answer is not that question.
func duplicateOf(t *testing.T, code int, body string) uint {
	t.Helper()
	var resp struct {
		Error       ErrorDetail `json:"error"`
		DuplicateOf uint        `json:"duplicate_of"`
	}
	if code != http.StatusConflict {
		return 0
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil || resp.Error.Code != "possible_duplicate" || resp.DuplicateOf == 0 {
		t.Fatalf("a 409 that is not the question: %v %s", err, body)
	}
	return resp.DuplicateOf
}

func countManual(t *testing.T, f *invoiceFixture) int {
	t.Helper()
	var n int
	f.db.Model(&data_models.InvoicingPayment{}).Where("owner_user_id = ? AND source = ?", invoiceFixtureUser, "manual").Count(&n)
	return n
}

func TestManualPayment_LookAlikeIsAskedOnce(t *testing.T) {
	f := newInvoiceFixture(t)
	const lena = `{"paid_on":"2026-08-03","gross_cents":7000,"method":"cash","sender_name":"Lena Maier","note":"Skizze"%s}`
	code, first, body := createManual(t, f, fmt.Sprintf(lena, ""))
	if code != http.StatusCreated {
		t.Fatalf("first entry: %d %s", code, body)
	}

	// The same again: the question, naming the first, and nothing saved.
	code, _, body = createManual(t, f, fmt.Sprintf(lena, ""))
	if id := duplicateOf(t, code, body); id != first.ID {
		t.Fatalf("second entry: %d %s, want possible_duplicate of %d", code, body, first.ID)
	}
	var resp struct {
		Payment data_models.InvoicingPayment `json:"payment"`
	}
	json.Unmarshal([]byte(body), &resp)
	if resp.Payment.ID != first.ID || resp.Payment.SenderName != "Lena Maier" {
		t.Errorf("the answer's payment = %+v, want the first entry", resp.Payment)
	}
	if n := countManual(t, f); n != 1 {
		t.Fatalf("hand entries after the question = %d, want 1", n)
	}
	// Decision 4's "the same": case, accents and a full stop do not matter.
	code, _, body = createManual(t, f, `{"paid_on":"2026-08-03","gross_cents":7000,"method":"bank_transfer","sender_name":"léna maier."}`)
	if id := duplicateOf(t, code, body); id != first.ID {
		t.Errorf("a differently written Lena Maier: %d %s, want the question", code, body)
	}
	// The second press saves it, and a false flag asks as before.
	code, _, body = createManual(t, f, fmt.Sprintf(lena, `,"confirm_duplicate":false`))
	if duplicateOf(t, code, body) != first.ID {
		t.Errorf("confirm_duplicate false: %d %s, want the question", code, body)
	}
	code, second, body := createManual(t, f, fmt.Sprintf(lena, `,"confirm_duplicate":true`))
	if code != http.StatusCreated || second.ID == first.ID {
		t.Fatalf("confirmed: %d %s, want a second payment", code, body)
	}
	if n := countManual(t, f); n != 2 {
		t.Errorf("hand entries after confirming = %d, want 2", n)
	}

	// Another day, amount or payer is no look-alike.
	for _, b := range []string{
		`{"paid_on":"2026-08-04","gross_cents":7000,"method":"cash","sender_name":"Lena Maier"}`,
		`{"paid_on":"2026-08-03","gross_cents":7001,"method":"cash","sender_name":"Lena Maier"}`,
		`{"paid_on":"2026-08-03","gross_cents":7000,"method":"cash","sender_name":"Lena Meier"}`,
		`{"paid_on":"2026-08-03","gross_cents":7000,"method":"cash"}`,
	} {
		if code, _, body := createManual(t, f, b); code != http.StatusCreated {
			t.Errorf("%s: %d %s, want saved", b, code, body)
		}
	}

	if code, _, body := createManual(t, f, fmt.Sprintf(lena, `,"confirm_duplicate":"yes"`)); code != http.StatusBadRequest {
		t.Errorf("confirm_duplicate as a string: %d %s, want 400", code, body)
	}
}

// Review point 3: any payment not removed is a twin, an imported one too, and
// a payer held as a client is the same as one typed as a name.
func TestManualPayment_LookAlikeOfAnImportAndAClient(t *testing.T) {
	f := newInvoiceFixture(t)
	imported := f.payments[0] // PayPal, 2026-07-10, 100.00 EUR, client Galerie Nord, no sender name
	code, _, body := createManual(t, f, `{"paid_on":"2026-07-10","gross_cents":10000,"method":"paypal","sender_name":"Galerie Nord"}`)
	if id := duplicateOf(t, code, body); id != imported.ID {
		t.Errorf("a typed name against an imported client's payment: %d %s, want the question naming %d", code, body, imported.ID)
	}
	code, _, body = createManual(t, f, fmt.Sprintf(`{"paid_on":"2026-07-10","gross_cents":10000,"method":"cash","client_id":%d}`, f.clientID))
	if id := duplicateOf(t, code, body); id != imported.ID {
		t.Errorf("the same client: %d %s, want the question", code, body)
	}
	// The client is the payer here, so a payment without a name is no twin
	// of it.
	if code, _, body := createManual(t, f, `{"paid_on":"2026-07-10","gross_cents":10000,"method":"cash"}`); code != http.StatusCreated {
		t.Errorf("no payer against a client's payment: %d %s, want saved", code, body)
	}
}

// A removed payment counts nowhere, so it is no twin.
func TestManualPayment_RemovedTwinIsNotAsked(t *testing.T) {
	f := newInvoiceFixture(t)
	const b = `{"paid_on":"2026-08-06","gross_cents":3000,"method":"cash"}`
	code, first, body := createManual(t, f, b)
	if code != http.StatusCreated {
		t.Fatalf("first: %d %s", code, body)
	}
	// An empty payer matches an empty payer.
	if code, _, body := createManual(t, f, b); duplicateOf(t, code, body) != first.ID {
		t.Fatalf("an empty payer twice: %d %s, want the question", code, body)
	}
	removedAt := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)
	f.db.Model(&data_models.InvoicingPayment{}).Where("id = ?", first.ID).Update("removed_at", removedAt)
	if code, _, body := createManual(t, f, b); code != http.StatusCreated {
		t.Errorf("with the twin removed: %d %s, want saved", code, body)
	}
}

// Another user's book is never searched.
func TestManualPayment_LookAlikeOnlyInOwnBook(t *testing.T) {
	f := newInvoiceFixture(t)
	other := data_models.InvoicingPayment{OwnerUserID: invoiceFixtureUser + 1, Source: "manual", TransactionID: "M-OTHER", Kind: "incoming",
		PaidOn: time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC), Currency: "EUR", GrossCents: 7000, NetCents: 7000, SenderName: "Lena Maier", Category: "paid_work", Entry: "manual", Method: "cash"}
	if err := f.db.Create(&other).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	if code, _, body := createManual(t, f, `{"paid_on":"2026-08-03","gross_cents":7000,"method":"cash","sender_name":"Lena Maier"}`); code != http.StatusCreated {
		t.Errorf("another book's twin: %d %s, want saved", code, body)
	}
}

func TestManualExpense_LookAlikeIsAskedOnce(t *testing.T) {
	db, r := newExpenseFixture(t)
	const boesner = `{"paid_on":"2026-10-03","amount_cents":1299,"payee":"Boesner"%s}`
	code, first, body := createExpense(t, r, fmt.Sprintf(boesner, ""))
	if code != http.StatusCreated {
		t.Fatalf("first: %d %s", code, body)
	}
	code, _, body = createExpense(t, r, fmt.Sprintf(boesner, ""))
	if id := duplicateOf(t, code, body); id != first.ID {
		t.Fatalf("second: %d %s, want possible_duplicate of %d", code, body, first.ID)
	}
	var resp struct {
		Expense data_models.InvoicingExpense `json:"expense"`
	}
	json.Unmarshal([]byte(body), &resp)
	if resp.Expense.ID != first.ID {
		t.Errorf("the answer's expense = %+v, want the first", resp.Expense)
	}
	if code, _, body := createExpense(t, r, `{"paid_on":"2026-10-03","amount_cents":1299,"payee":"BOESNER"}`); duplicateOf(t, code, body) != first.ID {
		t.Errorf("BOESNER: %d %s, want the question", code, body)
	}
	if code, _, body := createExpense(t, r, fmt.Sprintf(boesner, `,"confirm_duplicate":true`)); code != http.StatusCreated {
		t.Errorf("confirmed: %d %s, want saved", code, body)
	}
	for _, b := range []string{
		`{"paid_on":"2026-10-04","amount_cents":1299,"payee":"Boesner"}`,
		`{"paid_on":"2026-10-03","amount_cents":1300,"payee":"Boesner"}`,
		`{"paid_on":"2026-10-03","amount_cents":1299,"payee":"Gerstaecker"}`,
		`{"paid_on":"2026-10-03","amount_cents":1299}`,
	} {
		if code, _, body := createExpense(t, r, b); code != http.StatusCreated {
			t.Errorf("%s: %d %s, want saved", b, code, body)
		}
	}
	// The empty payee saved just above is now a twin of an empty payee, until
	// it is removed.
	code, _, body = createExpense(t, r, `{"paid_on":"2026-10-03","amount_cents":1299}`)
	empty := duplicateOf(t, code, body)
	if empty == 0 {
		t.Fatalf("an empty payee twice: %d %s, want the question", code, body)
	}
	db.Model(&data_models.InvoicingExpense{}).Where("id = ?", empty).Update("removed_at", time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC))
	if code, _, body := createExpense(t, r, `{"paid_on":"2026-10-03","amount_cents":1299}`); code != http.StatusCreated {
		t.Errorf("with the twin removed: %d %s, want saved", code, body)
	}

	// An import keeps its payee in Reference.
	imp := data_models.InvoicingExpense{OwnerUserID: expenseFixtureUser, Source: "paypal", TransactionID: "EXP-IMP1",
		PaidOn: time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC), Currency: "EUR", AmountCents: 4500, Category: "other", Entry: "import", Reference: "Gerstaecker"}
	if err := db.Create(&imp).Error; err != nil {
		t.Fatalf("seed import: %v", err)
	}
	if code, _, body := createExpense(t, r, `{"paid_on":"2026-10-09","amount_cents":4500,"payee":"Gerstaecker"}`); duplicateOf(t, code, body) != imp.ID {
		t.Errorf("against an imported expense: %d %s, want the question naming %d", code, body, imp.ID)
	}
	if code, _, body := createExpense(t, r, fmt.Sprintf(boesner, `,"confirm_duplicate":1`)); code != http.StatusBadRequest {
		t.Errorf("confirm_duplicate as a number: %d %s, want 400", code, body)
	}
}
