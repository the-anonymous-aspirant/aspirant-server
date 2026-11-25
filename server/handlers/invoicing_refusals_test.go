package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
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
		Lines   int64  `json:"lines"`
		Item    int    `json:"item"`
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

// The issuer profile and the client book: the causes that shared one
// sentence ("full_name and address are required", "email and paypal_email
// must be valid addresses") now say which field, and the codes that were
// bad_request say why (#12300).
func TestProfileAndClient_EachCauseHasItsOwnCode(t *testing.T) {
	f := newInvoiceFixture(t)
	profile := func(fields string) string {
		base := map[string]string{"full_name": `"Mara Esempio"`, "address": `"Weg 1"`, "is_kleinunternehmer": "true", "invoice_number_prefix": `"2026"`, "next_invoice_number": "1"}
		var parts []string
		for k, v := range base {
			if !strings.Contains(fields, `"`+k+`"`) {
				parts = append(parts, `"`+k+`":`+v)
			}
		}
		if fields != "" {
			parts = append(parts, fields)
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	cases := []struct {
		refusalCase
		method, path string
	}{
		{refusalCase{"profile: no name", profile(`"full_name":""`), codeRequired, "full_name", 0}, "PUT", "/invoicing/profile"},
		{refusalCase{"profile: no address", profile(`"address":"  "`), codeRequired, "address", 0}, "PUT", "/invoicing/profile"},
		{refusalCase{"profile: a wrong email", profile(`"email":"mara@"`), codeInvalidEmail, "email", 0}, "PUT", "/invoicing/profile"},
		{refusalCase{"profile: a prefix with a slash", profile(`"invoice_number_prefix":"RE/26"`), codeInvalidPrefix, "invoice_number_prefix", maxInvoicePrefixRunes}, "PUT", "/invoicing/profile"},
		{refusalCase{"profile: a prefix too long", profile(`"invoice_number_prefix":"` + strings.Repeat("R", maxInvoicePrefixRunes+1) + `"`), codeInvalidPrefix, "invoice_number_prefix", maxInvoicePrefixRunes}, "PUT", "/invoicing/profile"},
		{refusalCase{"profile: no Kleinunternehmer answer", `{"full_name":"Mara Esempio","address":"Weg 1","invoice_number_prefix":"2026","next_invoice_number":1}`, codeRequired, "is_kleinunternehmer", 0}, "PUT", "/invoicing/profile"},
		{refusalCase{"profile: no next number", `{"full_name":"Mara Esempio","address":"Weg 1","is_kleinunternehmer":true,"invoice_number_prefix":"2026"}`, codeRequired, "next_invoice_number", 0}, "PUT", "/invoicing/profile"},
		{refusalCase{"profile: next number 0", profile(`"next_invoice_number":0`), codeNumberTooSmall, "next_invoice_number", 1}, "PUT", "/invoicing/profile"},
		{refusalCase{"profile: a name too long", profile(`"full_name":"` + strings.Repeat("m", maxProfileLineRunes+1) + `"`), codeFullNameTooLong, "full_name", maxProfileLineRunes}, "PUT", "/invoicing/profile"},
		{refusalCase{"profile: a wrong phone", profile(`"phone":"call me"`), "invalid_phone", "phone", 0}, "PUT", "/invoicing/profile"},
		{refusalCase{"client: a wrong email", `{"name":"Atelier Ost","email":"ost@"}`, codeInvalidEmail, "email", 0}, "POST", "/invoicing/clients"},
		{refusalCase{"client: a wrong PayPal email", `{"name":"Atelier Ost","paypal_email":"ost@"}`, codeInvalidEmail, "paypal_email", 0}, "POST", "/invoicing/clients"},
		{refusalCase{"client: notes too long", `{"name":"Atelier Ost","notes":"` + strings.Repeat("n", maxClientNotesRunes+1) + `"}`, codeTooLong, "notes", maxClientNotesRunes}, "POST", "/invoicing/clients"},
		{refusalCase{"client: no name", `{"name":"  "}`, codeRequired, "name", 0}, "POST", "/invoicing/clients"},
		{refusalCase{"client: a wrong IBAN", `{"name":"Atelier Ost","iban":"DE00 1234"}`, "invalid_iban", "iban", 0}, "POST", "/invoicing/clients"},
	}
	for _, tc := range cases {
		assertRefusal(t, tc.name, invoicingDo(f.r, tc.method, tc.path, tc.body), tc.code, tc.field, tc.limit)
	}
	// An address of too many lines names both of its numbers.
	w := invoicingDo(f.r, "PUT", "/invoicing/profile", profile(`"address":"`+strings.Repeat("Zeile\\n", maxProfileAddressLines)+`Ende"`))
	var e refusalEnvelope
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if e.Error.Code != codeAddressTooLong || e.Error.Field != "address" || e.Error.Limit != maxProfileLineRunes || e.Error.Lines != maxProfileAddressLines {
		t.Errorf("profile address of %d lines: %+v", maxProfileAddressLines+1, e.Error)
	}
}

// A draft: each line's refusal is about that line's own input, with its
// 1-based position, so the editor prints it there (c77188 on #12300).
func TestDraft_EachCauseHasItsOwnCode(t *testing.T) {
	f := newInvoiceFixture(t)
	path := fmt.Sprintf("/invoicing/invoices/%d", f.draft(t, 0))
	line := func(q, p int64) string {
		return fmt.Sprintf(`{"description":"Wandbild","quantity_milli":%d,"unit_price_cents":%d}`, q, p)
	}
	ok := line(1000, 1999)
	for _, tc := range []struct {
		name, body, code, field string
		limit                   int64
		item                    int
	}{
		{"a negative quantity on line 2", `{"line_items":[` + ok + `,` + line(-1000, 1999) + `]}`, codeBelowZero, "quantity_milli", 0, 2},
		{"a negative price on line 1", `{"line_items":[` + line(1000, -1) + `]}`, codeBelowZero, "unit_price_cents", 0, 1},
		{"a quantity over 10,000 on line 3", `{"line_items":[` + ok + `,` + ok + `,` + line(maxQuantityMilli+1, 1999) + `]}`, codeQuantityTooLarge, "quantity_milli", 10000, 3},
		{"a price too large on line 2", `{"line_items":[` + ok + `,` + line(1000, maxUnitPriceCents+1) + `]}`, codeAmountTooLarge, "unit_price_cents", maxUnitPriceCents, 2},
		{"a quantity as text", `{"line_items":[{"description":"x","quantity_milli":"zwei","unit_price_cents":1}]}`, codeInvalidLineItems, "line_items", 0, 0},
		{"a VAT rate off the steps", `{"vat_rate_bp":1905}`, codeInvalidVATRate, "vat_rate_bp", 0, 0},
		{"a service date that is not a date", `{"service_date_from":"14.07.2026"}`, codeInvalidDate, "service_date_from", 0, 0},
		{"an unknown client", `{"client_id":999}`, codeNoSuchClient, "client_id", 0, 0},
		{"a client id that is not one", `{"client_id":-3}`, codeInvalidID, "client_id", 0, 0},
	} {
		w := invoicingDo(f.r, "PUT", path, tc.body)
		var e refusalEnvelope
		var item struct {
			Error struct {
				Item int `json:"item"`
			} `json:"error"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		_ = json.Unmarshal(w.Body.Bytes(), &item)
		if w.Code != http.StatusBadRequest || e.Error.Code != tc.code || e.Error.Field != tc.field || e.Error.Limit != tc.limit || item.Error.Item != tc.item {
			t.Errorf("%s: %d %s; want %s %s limit %d item %d", tc.name, w.Code, w.Body.String(), tc.code, tc.field, tc.limit, tc.item)
		}
	}
}

// A PayPal file's value that cannot be read names its line as ze finds it
// in the file, its value and its cause (c77188 group 4).
func TestPayPalImport_AValueNamesItsLineValueAndCode(t *testing.T) {
	db := setupPaymentsDB(t)
	r := setupPaymentsRouter(db, 7)
	head := "Date,Time,TimeZone,Name,Type,Status,Currency,Gross,Fee,Net,Transaction ID,Note\n"
	good := func(id string) string {
		return "14.07.2026,10:00:00,CET,Galerie Nord,General Payment,Completed,EUR,10.00,0.00,10.00," + id + ",\n"
	}
	// The header is line 1 and T1 line 2. T2's note runs over lines 3 and
	// 4, so the bad record, the third, starts on line 5; counting records
	// would call it line 4.
	twoLine := "14.07.2026,10:00:00,CET,Galerie Nord,General Payment,Completed,EUR,10.00,0.00,10.00,T2,\"Wand\nbild\"\n"
	for _, tc := range []struct {
		name, bad, code, value string
	}{
		{"three decimals", "14.07.2026,10:00:00,CET,Ada,General Payment,Completed,EUR,12.345,0.00,12.35,T9,\n", codeAmountDecimals, "12.345"},
		{"not a date", "31.02.2026,10:00:00,CET,Ada,General Payment,Completed,EUR,10.00,0.00,10.00,T9,\n", codeInvalidDate, "31.02.2026"},
		{"not a currency", "14.07.2026,10:00:00,CET,Ada,General Payment,Completed,EURO,10.00,0.00,10.00,T9,\n", codeInvalidCurrency, "EURO"},
		{"not a number", "14.07.2026,10:00:00,CET,Ada,General Payment,Completed,EUR,zehn,0.00,10.00,T9,\n", codeInvalidAmount, "zehn"},
	} {
		w := uploadCSV(t, r, head+good("T1")+twoLine+tc.bad, "")
		var e struct {
			Error struct {
				Code, Field, Value string
				Line               int
			} `json:"error"`
		}
		if json.Unmarshal(w.Body.Bytes(), &e) != nil || w.Code != http.StatusBadRequest ||
			e.Error.Code != tc.code || e.Error.Line != 5 || e.Error.Value != tc.value || e.Error.Field != "file" {
			t.Errorf("%s: %d %s; want %s on line 5, value %q", tc.name, w.Code, w.Body.String(), tc.code, tc.value)
		}
	}
	// Two date orders in one file.
	mixed := head + "13/07/2026,10:00:00,CET,A,General Payment,Completed,EUR,1.00,0.00,1.00,T1,\n" +
		"07/14/2026,10:00:00,CET,B,General Payment,Completed,EUR,1.00,0.00,1.00,T2,\n"
	if w := uploadCSV(t, r, mixed, ""); !strings.Contains(w.Body.String(), `"code":"`+codeDateOrdersDisagree+`"`) {
		t.Errorf("two date orders: %d %s", w.Code, w.Body.String())
	}
	// A sheet of someone else's columns is not PayPal's export, apart from
	// a file that could not be read at all.
	if w := uploadCSV(t, r, "Datum,Betrag,Verwendungszweck\n14.07.2026,10.00,Miete\n", ""); !strings.Contains(w.Body.String(), `"code":"`+codeNotPayPalExport+`"`) ||
		!strings.Contains(w.Body.String(), "not a PayPal activity export: missing column(s)") {
		t.Errorf("another bank's columns: %d %s", w.Code, w.Body.String())
	}
	if w := uploadCSV(t, r, head+"\"unclosed,quote\n\"x\"y,1\n", ""); strings.Contains(w.Body.String(), codeNotPayPalExport) {
		t.Errorf("a malformed file read as another export: %s", w.Body.String())
	}
	// A long value is clipped in value. The message quotes the whole cell,
	// as it always has (#10583 keeps it out of the log).
	long := strings.Repeat("9", 80) + ".001"
	w := uploadCSV(t, r, head+"14.07.2026,10:00:00,CET,A,General Payment,Completed,EUR,"+long+",0.00,1.00,T1,\n", "")
	var e struct {
		Error struct {
			Value string
			Line  int
		} `json:"error"`
	}
	if json.Unmarshal(w.Body.Bytes(), &e) != nil || e.Error.Line != 2 || utf8.RuneCountInString(e.Error.Value) != maxEchoedValueRunes || !strings.HasSuffix(e.Error.Value, "…") {
		t.Errorf("a long value: %s", w.Body.String())
	}
}

// A sheet leaves its empty rows out; the line is the row number the
// spreadsheet shows, read from the row's r attribute.
func TestParsePayPalXLSX_AValueNamesItsSheetRow(t *testing.T) {
	cell := func(s string) string { return `<c t="inlineStr"><is><t>` + s + `</t></is></c>` }
	headRow := `<row r="1">` + cell("Date") + cell("Name") + cell("Type") + cell("Status") + cell("Currency") + cell("Gross") + cell("Transaction ID") + `</row>`
	sheet := sheetXML(headRow,
		`<row r="2">`+cell("14.07.2026")+cell("Shop")+cell("General Payment")+cell("Completed")+cell("EUR")+cell("10.00")+cell("M1")+`</row>`,
		`<row r="14">`+cell("14.07.2026")+cell("Shop")+cell("General Payment")+cell("Completed")+cell("EURO")+cell("10.00")+cell("M2")+`</row>`,
	)
	_, err := ParsePayPalActivityXLSX(zipParts(t, workbookParts(sheet, "", false)), "")
	var ve *paypalValueError
	if !errors.As(err, &ve) || ve.Line != 14 || ve.Code != codeInvalidCurrency || ve.Value != "EURO" {
		t.Fatalf("got %v (%+v), want row 14's currency", err, ve)
	}
}

// A confirm's refused row answers its 1-based position, so the client
// names it from what it sent (c77188 point 9); a cap names its number.
func TestN26Confirm_ARefusedRowAnswersItsPosition(t *testing.T) {
	db := setupImportFailuresDB(t)
	r := setupN26Router(db, 7)
	row := func(kind string) string {
		return `{"document_kind":"` + kind + `","transaction_id":"N1","paid_on":"2026-07-14","gross_cents":1000,"currency":"EUR","sender_name":"Mara Esempio"}`
	}
	w := invoicingDo(r, "POST", "/invoicing/payments/import-n26/confirm", `{"rows":[`+row(n26KindAccountStatement)+`,`+row("fax")+`]}`)
	var e refusalEnvelope
	var item struct {
		Error struct {
			Item int `json:"item"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	_ = json.Unmarshal(w.Body.Bytes(), &item)
	if w.Code != http.StatusBadRequest || e.Error.Code != "invalid_row" || e.Error.Field != "rows" || item.Error.Item != 2 {
		t.Errorf("the second row: %d %s", w.Code, w.Body.String())
	}
	// A row whose payer is PayPal has its own code, at its position.
	paypal := strings.Replace(row(n26KindAccountStatement), "Mara Esempio", "PayPal Europe S.a.r.l. et Cie S.C.A", 1)
	w = invoicingDo(r, "POST", "/invoicing/payments/import-n26/confirm", `{"rows":[`+row(n26KindAccountStatement)+`,`+paypal+`]}`)
	e, item.Error.Item = refusalEnvelope{}, 0
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	_ = json.Unmarshal(w.Body.Bytes(), &item)
	if w.Code != http.StatusBadRequest || e.Error.Code != codePayPalTransferRow || item.Error.Item != 2 || !strings.HasPrefix(e.Error.Message, "row 1: the payer is PayPal") {
		t.Errorf("a PayPal payer: %d %s", w.Code, w.Body.String())
	}
	many := strings.TrimSuffix(strings.Repeat(row(n26KindAccountStatement)+",", maxN26ConfirmRows+1), ",")
	w = invoicingDo(r, "POST", "/invoicing/payments/import-n26/confirm", `{"rows":[`+many+`]}`)
	e = refusalEnvelope{}
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if e.Error.Code != "too_many_rows" || e.Error.Limit != maxN26ConfirmRows {
		t.Errorf("too many rows: %d %s", w.Code, w.Body.String())
	}
}

// A preset's name answers one code per cause at the name, and the built-in's
// name is told apart from a name ze already used (system_3 #12300 ruling 1).
func TestBookPresetName_EachCauseHasItsOwnCode(t *testing.T) {
	_, r := newBookSelectionFixture(t)
	if w := invoicingDo(r, "POST", "/invoicing/book/presets", `{"name":"Jobcenter","income":["paid_work"],"expense_all":false,"expense":[]}`); w.Code != http.StatusCreated {
		t.Fatalf("seed: %d %s", w.Code, w.Body.String())
	}
	preset := func(name string) string {
		return `{"name":"` + name + `","income":[],"expense_all":false,"expense":[]}`
	}
	for _, tc := range []struct {
		name, body string
		status     int
		code       string
		limit      int64
	}{
		{"no name", `{"income":[],"expense_all":false,"expense":[]}`, http.StatusBadRequest, codeRequired, 0},
		{"an empty name", preset(" "), http.StatusBadRequest, codeRequired, 0},
		{"a name too long", preset(strings.Repeat("n", maxBookPresetNameRunes+1)), http.StatusBadRequest, codeTooLong, maxBookPresetNameRunes},
		{"a name on two lines", preset(`Job\ncenter`), http.StatusBadRequest, codeTooLong, maxBookPresetNameRunes},
		{"a zero-width space", preset("Job\u200bcenter"), http.StatusBadRequest, codeHiddenCharacters, 0},
		{"the built-in's name", preset("work only"), http.StatusConflict, codeBuiltinPresetName, 0},
		{"a name already used", preset("JOBCENTER"), http.StatusConflict, "duplicate_preset", 0},
	} {
		w := invoicingDo(r, "POST", "/invoicing/book/presets", tc.body)
		var e refusalEnvelope
		json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code != tc.status || e.Error.Code != tc.code || e.Error.Field != "name" || e.Error.Limit != tc.limit {
			t.Errorf("%s: %d %s, want %d %q at name, limit %d", tc.name, w.Code, w.Body.String(), tc.status, tc.code, tc.limit)
		}
	}
}

// A payer's new name answers its cause and which payer it is, 1-based.
func TestAddPayers_ARefusedNameAnswersItsCauseAndPayer(t *testing.T) {
	f := newPayerFixture(t)
	u := uint(invoiceFixtureUser)
	a := f.pay(t, u, "n26", "Mara Esempio", "", "uncategorised", 10000, 1)
	b := f.pay(t, u, "n26", "Lia Beispiel", "", "uncategorised", 10000, 2)
	for _, tc := range []struct {
		name, newName, code string
		limit               int64
	}{
		{"a name too long", strings.Repeat("m", maxManualSenderRunes+1), codeTooLong, maxManualSenderRunes},
		{"a name of only punctuation", "...", codeNoLetterOrDigit, 0},
	} {
		w := invoicingDo(f.r, "POST", "/invoicing/payers", fmt.Sprintf(`{"payers":[
			{"payment_ids":[%d],"new_client_name":"Mara Esempio"},
			{"payment_ids":[%d],"new_client_name":%q}]}`, a, b, tc.newName))
		var e refusalEnvelope
		json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code != http.StatusBadRequest || e.Error.Code != tc.code || e.Error.Field != "new_client_name" || e.Error.Item != 2 || e.Error.Limit != tc.limit {
			t.Errorf("%s: %d %s", tc.name, w.Code, w.Body.String())
		}
		if !strings.HasPrefix(e.Error.Message, "payers[1].new_client_name ") {
			t.Errorf("%s: the log's message changed: %q", tc.name, e.Error.Message)
		}
	}
}
