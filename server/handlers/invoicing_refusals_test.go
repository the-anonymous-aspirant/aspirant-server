package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// system_3 #12300 ruling 1: each refusal of the payment and expense dialogs
// answers its own code, the field it is about, and the number the client's
// sentence prints. The client holds one sentence per code; these tests are
// the server's half of that contract.

type refusalEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Field   string `json:"field"`
		Limit   int64  `json:"limit"`
	} `json:"error"`
}

// assertRefusal checks a 400 with exactly this code, field and limit, and a
// message that still names the field it is about (the logs read it).
func assertRefusal(t *testing.T, name string, w *httptest.ResponseRecorder, code, field string, limit int64) {
	t.Helper()
	var e refusalEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || w.Code != http.StatusBadRequest {
		t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		return
	}
	if e.Error.Code != code || e.Error.Field != field || e.Error.Limit != limit {
		t.Errorf("%s: got code %q field %q limit %d, want %q %q %d (%s)", name, e.Error.Code, e.Error.Field, e.Error.Limit, code, field, limit, w.Body.String())
	}
	if field != "" && !strings.HasPrefix(e.Error.Message, field+" ") {
		t.Errorf("%s: message %q does not start with its field", name, e.Error.Message)
	}
}

type refusalCase struct {
	name, body, code, field string
	limit                   int64
}

// The causes that used to share one code and one sentence: a length and a
// hidden character, an amount of zero and one too large, a name of only
// punctuation, the new client's email.
func TestManualPayment_EachCauseHasItsOwnCode(t *testing.T) {
	f := newInvoiceFixture(t)
	ok := `"paid_on":"2026-07-14","gross_cents":1000,"method":"bank_transfer"`
	cases := []refusalCase{
		{"a note too long", `{` + ok + `,"note":"` + strings.Repeat("n", maxManualNoteRunes+1) + `"}`, codeTooLong, "note", maxManualNoteRunes},
		{"a tab in the note", `{` + ok + `,"note":"Wand\tbild"}`, codeHiddenCharacters, "note", 0},
		{"a payer too long", `{` + ok + `,"sender_name":"` + strings.Repeat("p", maxManualSenderRunes+1) + `"}`, codeTooLong, "sender_name", maxManualSenderRunes},
		{"a payer on two lines", `{` + ok + `,"sender_name":"Galerie\nNord"}`, codeTooLong, "sender_name", maxManualSenderRunes},
		{"a tab in the reference", `{` + ok + `,"reference":"RE\t71"}`, codeHiddenCharacters, "reference", 0},
		{"an amount too large", fmt.Sprintf(`{"paid_on":"2026-07-14","gross_cents":%d,"method":"cash"}`, maxUnitPriceCents+1), codeAmountTooLarge, "gross_cents", maxUnitPriceCents},
		{"an amount that is not whole cents", `{"paid_on":"2026-07-14","gross_cents":10.5,"method":"cash"}`, codeInvalidAmount, "gross_cents", 0},
		{"a date that is not a string", `{"paid_on":20260714,"gross_cents":1000,"method":"cash"}`, codeNotAString, "paid_on", 0},
		{"a new client of only punctuation", `{` + ok + `,"new_client_name":"..."}`, codeNoLetterOrDigit, "new_client_name", 0},
		{"a new client and a client", fmt.Sprintf(`{`+ok+`,"new_client_name":"Atelier Ost","client_id":%d}`, f.clientID), codeClientConflict, "new_client_name", 0},
		{"a new client's wrong email", `{` + ok + `,"new_client_name":"Atelier Ost","new_client_email":"ost@"}`, codeInvalidEmail, "new_client_email", 0},
		{"a new client's address too long", `{` + ok + `,"new_client_name":"Atelier Ost","new_client_address":"` + strings.Repeat("a", maxNewClientAddressRunes+1) + `"}`, codeTooLong, "new_client_address", maxNewClientAddressRunes},
		{"an address without a new client", `{` + ok + `,"new_client_address":"Weg 1"}`, codeNeedsNewClient, "new_client_address", 0},
	}
	for _, tc := range cases {
		assertRefusal(t, tc.name, invoicingDo(f.r, "POST", "/invoicing/payments/manual", tc.body), tc.code, tc.field, tc.limit)
	}
}

func TestManualExpense_EachCauseHasItsOwnCode(t *testing.T) {
	_, r := newExpenseFixture(t)
	ok := `"paid_on":"2026-07-14","amount_cents":1000`
	cases := []refusalCase{
		{"a future date", `{"paid_on":"2026-10-22","amount_cents":1000}`, codeDateInFuture, "paid_on", 0},
		{"a payee too long", `{` + ok + `,"payee":"` + strings.Repeat("p", maxManualSenderRunes+1) + `"}`, codeTooLong, "payee", maxManualSenderRunes},
		{"a zero-width space in the payee", `{` + ok + `,"payee":"Musik​haus"}`, codeHiddenCharacters, "payee", 0},
		{"a note too long", `{` + ok + `,"note":"` + strings.Repeat("n", maxManualNoteRunes+1) + `"}`, codeTooLong, "note", maxManualNoteRunes},
		{"an amount too large", fmt.Sprintf(`{"paid_on":"2026-07-14","amount_cents":%d}`, maxUnitPriceCents+1), codeAmountTooLarge, "amount_cents", maxUnitPriceCents},
		{"a typed category too long", `{` + ok + `,"category":"` + strings.Repeat("c", maxExpenseCategoryNameRunes+1) + `","create_category":true}`, codeTooLong, "category", maxExpenseCategoryNameRunes},
		{"a typed category on two lines", `{` + ok + `,"category":"Studio\nmiete","create_category":true}`, codeTooLong, "category", maxExpenseCategoryNameRunes},
	}
	for _, tc := range cases {
		assertRefusal(t, tc.name, invoicingDo(r, "POST", "/invoicing/expenses/manual", tc.body), tc.code, tc.field, tc.limit)
	}
}

// An edit answers as the entry does: one rule, one code, wherever it is met.
func TestEdit_RefusalsAnswerTheEntrysCodes(t *testing.T) {
	f := newEditFixture(t)
	_, p, _ := createManual(t, f, `{"paid_on":"2026-07-14","gross_cents":1000,"method":"cash"}`)
	_, e, _ := createExpense(t, f.r, `{"paid_on":"2026-07-01","amount_cents":1500}`)
	pay := fmt.Sprintf("/invoicing/payments/%d", p.ID)
	exp := fmt.Sprintf("/invoicing/expenses/%d", e.ID)
	cases := []struct {
		refusalCase
		path string
	}{
		{refusalCase{"payment: before 2000", `{"paid_on":"1999-12-31"}`, codeDateTooEarly, "paid_on", 2000}, pay},
		{refusalCase{"payment: a future date", `{"paid_on":"2026-10-22"}`, codeDateInFuture, "paid_on", 0}, pay},
		{refusalCase{"payment: zero", `{"gross_cents":0}`, codeInvalidAmount, "gross_cents", 0}, pay},
		{refusalCase{"payment: a reference too long", `{"reference":"` + strings.Repeat("x", maxManualReferenceRunes+1) + `"}`, codeTooLong, "reference", maxManualReferenceRunes}, pay},
		{refusalCase{"payment: a tab in the note", `{"note":"a\tb"}`, codeHiddenCharacters, "note", 0}, pay},
		{refusalCase{"payment: an unknown method", `{"method":"crypto"}`, codeInvalidChoice, "method", 0}, pay},
		{refusalCase{"payment: an unknown client", `{"client_id":999}`, codeNoSuchClient, "client_id", 0}, pay},
		{refusalCase{"payment: a new client's wrong email", `{"new_client_name":"Atelier Ost","new_client_email":"ost@"}`, codeInvalidEmail, "new_client_email", 0}, pay},
		{refusalCase{"expense: a payee on two lines", `{"payee":"a\nb"}`, codeTooLong, "payee", maxManualSenderRunes}, exp},
		{refusalCase{"expense: an amount too large", fmt.Sprintf(`{"amount_cents":%d}`, maxUnitPriceCents+1), codeAmountTooLarge, "amount_cents", maxUnitPriceCents}, exp},
		{refusalCase{"expense: an unknown category", `{"category":"crypto"}`, codeInvalidCategory, "category", 0}, exp},
		{refusalCase{"expense: a typed category too long", `{"category":"` + strings.Repeat("c", maxExpenseCategoryNameRunes+1) + `","create_category":true}`, codeTooLong, "category", maxExpenseCategoryNameRunes}, exp},
	}
	for _, tc := range cases {
		assertRefusal(t, tc.name, invoicingDo(f.r, "PATCH", tc.path, tc.body), tc.code, tc.field, tc.limit)
	}
}
