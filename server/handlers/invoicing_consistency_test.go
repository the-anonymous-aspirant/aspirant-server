package handlers

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jinzhu/gorm"

	"aspirant-online/server/data_models"
)

// The six inconsistencies of system_3 #12273, one test each.

// ibanWithCheckDigits builds a German IBAN from bban whose mod-97 sum passes
// but whose check digits are cd. 00, 01 and 99 pass the sum as 97, 98 and
// 02 do, so the bban is searched for one whose real check digits are those.
func ibanWithCheckDigits(t *testing.T, cd string) string {
	t.Helper()
	real := map[string]string{"00": "97", "01": "98", "99": "02"}[cd]
	for n := 0; n < 100000; n++ {
		bban := fmt.Sprintf("37040044%010d", n)
		// DE = 13 14; the check digits are 98 - (bban + "131400") mod 97.
		v, _ := new(big.Int).SetString(bban+"131400", 10)
		got := 98 - new(big.Int).Mod(v, big.NewInt(97)).Int64()
		if fmt.Sprintf("%02d", got) == real {
			return "DE" + cd + bban
		}
	}
	t.Fatalf("no bban gives check digits %s", real)
	return ""
}

func TestIBAN_CheckDigits00_01And99AreRefused(t *testing.T) {
	for _, cd := range []string{"00", "01", "99"} {
		iban := ibanWithCheckDigits(t, cd)
		real := map[string]string{"00": "97", "01": "98", "99": "02"}[cd]
		if !data_models.ValidIBAN("DE" + real + iban[4:]) {
			t.Fatalf("%s: the same account with its real check digits must pass", cd)
		}
		if data_models.ValidIBAN(iban) {
			t.Errorf("%s is accepted; check digits %s are refused", iban, cd)
		}
	}
	if !data_models.ValidIBAN("DE89370400440532013000") {
		t.Errorf("a real IBAN is refused")
	}

	db := setupInvoicingDB(t)
	r := setupInvoicingRouter(db, 5)
	bad := ibanWithCheckDigits(t, "00")
	if w := invoicingDo(r, "POST", "/invoicing/clients", `{"name":"Mara Esempio","iban":"`+bad+`"}`); w.Code != http.StatusBadRequest || errorCode(t, w) != "invalid_iban" {
		t.Fatalf("a new client with check digits 00: %d %s", w.Code, w.Body.String())
	}

	// A client stored with such an IBAN before this rule keeps saving its
	// other fields, the IBAN sent unchanged (the form sends it every time).
	cl := data_models.InvoicingClient{OwnerUserID: 5, Name: "Mara Esempio", IBAN: bad}
	if err := db.Create(&cl).Error; err != nil {
		t.Fatal(err)
	}
	spaced := bad[:4] + " " + bad[4:8] + " " + strings.ToLower(bad[8:])
	if w := invoicingDo(r, "PUT", fmt.Sprintf("/invoicing/clients/%d", cl.ID), `{"name":"Mara Esempio","notes":"Atelier","iban":"`+spaced+`"}`); w.Code != http.StatusOK {
		t.Fatalf("saving a stored client with its IBAN unchanged: %d %s", w.Code, w.Body.String())
	}
	other := ibanWithCheckDigits(t, "99")
	if w := invoicingDo(r, "PUT", fmt.Sprintf("/invoicing/clients/%d", cl.ID), `{"name":"Mara Esempio","iban":"`+other+`"}`); w.Code != http.StatusBadRequest || errorCode(t, w) != "invalid_iban" {
		t.Fatalf("changing it to another IBAN with check digits 99: %d %s", w.Code, w.Body.String())
	}
	var stored data_models.InvoicingClient
	db.First(&stored, cl.ID)
	if stored.IBAN != bad || stored.Notes != "Atelier" {
		t.Errorf("stored client: iban %q notes %q", stored.IBAN, stored.Notes)
	}
}

func TestPaymentFee_AnsweredAsAChargeInOneSign(t *testing.T) {
	db := setupPaymentsDB(t)
	r := setupPaymentsRouter(db, 5)
	if w := uploadCSV(t, r, paypalFixtureEN, ""); w.Code != http.StatusOK {
		t.Fatalf("import: %d %s", w.Code, w.Body.String())
	}
	day := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	seed := []data_models.InvoicingPayment{
		// A fee PayPal gives back is printed positive.
		{OwnerUserID: 5, Source: "paypal", TransactionID: "RF1", Kind: "incoming", PaidOn: day, Currency: "EUR", GrossCents: 2000, FeeCents: 50, NetCents: 2050, Category: "refund"},
		{OwnerUserID: 5, Source: data_models.PaymentSourceN26, TransactionID: "op-1", Kind: "incoming", PaidOn: day, Currency: "EUR", GrossCents: 5000, FeeCents: 120, NetCents: 4880, Category: "uncategorised"},
	}
	for i := range seed {
		if err := db.Create(&seed[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]int64{"1AA11111AA1111111": 1050, "RF1": -50, "op-1": 120}
	var galerie uint
	for _, p := range listPayments(t, r, "") {
		if w, ok := want[p.TransactionID]; ok && p.FeeCents != w {
			t.Errorf("%s answers fee %d, want %d", p.TransactionID, p.FeeCents, w)
		}
		if p.TransactionID == "1AA11111AA1111111" {
			galerie = p.ID
		}
	}

	// The stored row keeps the file's sign, and an edit of another field
	// does not write the answered one back.
	w := invoicingDo(r, "PATCH", fmt.Sprintf("/invoicing/payments/%d", galerie), `{"note":"Wandbild Juli, Rahmen"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("edit note: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"fee_cents":1050`) {
		t.Errorf("the edit's answer: %s", w.Body.String())
	}
	var stored data_models.InvoicingPayment
	db.First(&stored, galerie)
	if stored.FeeCents != -1050 || stored.NetCents != 33950 {
		t.Errorf("stored fee %d net %d, want the file's -1050 and 33950", stored.FeeCents, stored.NetCents)
	}
}

func TestMergedPDFAudit_CountsTheInvoicesMerged(t *testing.T) {
	f := newInvoiceFixture(t)
	setupMergeRouter(f)
	id := f.draft(t, 0)
	if code, _, body := f.issue(id); code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, body)
	}
	if w := invoicingDo(f.r, "GET", fmt.Sprintf("/invoicing/export/invoices-merged.pdf?ids=%d,%d,%d", id, id, id), ""); w.Code != http.StatusOK {
		t.Fatalf("merge: %d %s", w.Code, w.Body.String())
	}
	events, _ := data_models.ListInvoicingAuditEvents(f.db, invoiceFixtureUser, invoicingEntityExport, 0)
	if len(events) != 1 {
		t.Fatalf("export events: %+v", events)
	}
	var details map[string]interface{}
	json.Unmarshal([]byte(events[0].Details), &details)
	if details["kind"] != "invoices_merged_pdf" || details["invoices"] != float64(1) {
		t.Errorf("audit details %v, want 1 invoice merged", details)
	}
}

func TestExpenseClient_ChangedAndClearedByAnEdit(t *testing.T) {
	f := newEditFixture(t)
	code, e, body := createExpense(t, f.r, fmt.Sprintf(`{"paid_on":"2026-07-01","amount_cents":1500,"client_id":%d}`, f.clientID))
	if (code != http.StatusCreated && code != http.StatusOK) || e.ClientID == nil || *e.ClientID != f.clientID {
		t.Fatalf("create with a client: %d %s", code, body)
	}
	patch := func(b string) (int, data_models.InvoicingExpense, string) {
		w := invoicingDo(f.r, "PATCH", fmt.Sprintf("/invoicing/expenses/%d", e.ID), b)
		var resp struct {
			Expense data_models.InvoicingExpense `json:"expense"`
		}
		json.Unmarshal(w.Body.Bytes(), &resp)
		return w.Code, resp.Expense, w.Body.String()
	}
	if code, got, b := patch(`{"client_id":null}`); code != http.StatusOK || got.ClientID != nil {
		t.Fatalf("clear the client: %d %s", code, b)
	}
	if code, got, b := patch(fmt.Sprintf(`{"client_id":%d}`, f.clientID)); code != http.StatusOK || got.ClientID == nil || *got.ClientID != f.clientID {
		t.Fatalf("set it again: %d %s", code, b)
	}
	if code, _, b := patch(`{"client_id":99999}`); code != http.StatusBadRequest || !strings.Contains(b, "does not name a client") {
		t.Fatalf("a client not in the book: %d %s", code, b)
	}
	var stored data_models.InvoicingExpense
	f.db.First(&stored, e.ID)
	if stored.ClientID == nil || *stored.ClientID != f.clientID {
		t.Errorf("stored client %v", stored.ClientID)
	}
	edits := 0
	for _, a := range auditActions(t, f, invoicingEntityExpense, e.ID) {
		if a == invoicingActionEdit {
			edits++
		}
	}
	if edits != 2 {
		t.Errorf("edit audit rows %d, want 2", edits)
	}
}

func TestRemovedTransaction_RefusesChangesUntilRestored(t *testing.T) {
	f := newRemoveFixture(t)
	f.r.PUT("/invoicing/expenses/:id/receipt", PutInvoicingExpenseReceiptHandler)
	f.r.DELETE("/invoicing/expenses/:id/receipt", DeleteInvoicingExpenseReceiptHandler)
	e := mustCreateExpense(t, f.r)
	if w := uploadReceipt(f.r, int(e.ID), "beleg.png", testReceiptPNG(t), ""); w.Code != http.StatusOK {
		t.Fatalf("receipt: %d %s", w.Code, w.Body.String())
	}
	payment := f.payments[3].ID
	if code, _, b := rowAction(f, "expenses", "remove", e.ID); code != http.StatusOK {
		t.Fatalf("remove expense: %d %s", code, b)
	}
	if code, _, b := rowAction(f, "payments", "remove", payment); code != http.StatusOK {
		t.Fatalf("remove payment: %d %s", code, b)
	}

	const msg = "This transaction was removed. Restore it to change it."
	for _, tc := range []struct {
		what string
		do   func() (int, string)
	}{
		{"expense edit", func() (int, string) {
			w := invoicingDo(f.r, "PATCH", fmt.Sprintf("/invoicing/expenses/%d", e.ID), `{"note":"Farbe"}`)
			return w.Code, w.Body.String()
		}},
		{"payment edit", func() (int, string) {
			w := invoicingDo(f.r, "PATCH", fmt.Sprintf("/invoicing/payments/%d", payment), `{"note":"Geschenk"}`)
			return w.Code, w.Body.String()
		}},
		{"receipt replace", func() (int, string) {
			w := uploadReceipt(f.r, int(e.ID), "beleg2.png", testReceiptPNG(t), "")
			return w.Code, w.Body.String()
		}},
		{"receipt delete", func() (int, string) {
			w := invoicingDo(f.r, "DELETE", fmt.Sprintf("/invoicing/expenses/%d/receipt", e.ID), "")
			return w.Code, w.Body.String()
		}},
	} {
		code, body := tc.do()
		got := errOf(t, []byte(body))
		if code != http.StatusConflict || got.Error.Code != "transaction_removed" || got.Error.Message != msg {
			t.Errorf("%s of a removed row: %d %s", tc.what, code, body)
		}
	}
	var stored data_models.InvoicingExpense
	f.db.First(&stored, e.ID)
	if stored.Note != "" {
		t.Errorf("the removed expense was changed: note %q", stored.Note)
	}
	if r, _ := data_models.GetInvoicingExpenseReceipt(f.db, invoiceFixtureUser, e.ID); r == nil {
		t.Errorf("the removed expense's receipt was deleted")
	}

	// Restored, it changes again.
	if code, _, b := rowAction(f, "expenses", "restore", e.ID); code != http.StatusOK {
		t.Fatalf("restore: %d %s", code, b)
	}
	if w := invoicingDo(f.r, "PATCH", fmt.Sprintf("/invoicing/expenses/%d", e.ID), `{"note":"Farbe"}`); w.Code != http.StatusOK {
		t.Errorf("edit after restore: %d %s", w.Code, w.Body.String())
	}
}

// payPalFile is a PayPal CSV of n incoming payments T0001… from three
// payers, the first of whom is a client.
func payPalFile(n int) string {
	var b strings.Builder
	b.WriteString("Date,Name,Type,Status,Currency,Gross,Transaction ID,From Email Address\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "14.07.2026,Payer %d,Payment,Completed,EUR,10.00,T%04d,payer%d@example.test\n", i%3, i, i%3)
	}
	return b.String()
}

func TestImportPayPal_ALargeFileReadsTheBookOnceAndStoresAllOrNothing(t *testing.T) {
	const rows = 2500
	db := setupPaymentsDB(t)
	r := setupPaymentsRouter(db, 5)
	if w := invoicingDo(r, "POST", "/invoicing/clients", `{"name":"Mara Esempio","paypal_email":"payer0@example.test"}`); w.Code != http.StatusCreated {
		t.Fatalf("client: %d %s", w.Code, w.Body.String())
	}
	// One stored, one removed, one deleted for good and one entered by hand.
	day := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	now := day
	for _, p := range []data_models.InvoicingPayment{
		{OwnerUserID: 5, Source: "paypal", TransactionID: "T0010", Kind: "incoming", PaidOn: day, Currency: "EUR", GrossCents: 1000, Category: "uncategorised"},
		{OwnerUserID: 5, Source: "paypal", TransactionID: "T1500", Kind: "incoming", PaidOn: day, Currency: "EUR", GrossCents: 1000, Category: "uncategorised", RemovedAt: &now},
		{OwnerUserID: 5, Source: "manual", Entry: "manual", Method: "paypal", Reference: "T2499", TransactionID: "M-1", Kind: "incoming", PaidOn: day, Currency: "EUR", GrossCents: 1000, Category: "uncategorised"},
	} {
		if err := db.Create(&p).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&data_models.InvoicingPaymentTombstone{OwnerUserID: 5, Source: "paypal", TransactionID: "T2000"}).Error; err != nil {
		t.Fatal(err)
	}

	// A row past the first thousand fails to store: nothing of the file is
	// kept, and no import is recorded.
	if err := db.Exec(`CREATE TRIGGER fail_one BEFORE INSERT ON invoicing_payments WHEN NEW.transaction_id = 'T2400' BEGIN SELECT RAISE(ABORT, 'refused'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if w := uploadCSV(t, r, payPalFile(rows), ""); w.Code != http.StatusInternalServerError {
		t.Fatalf("a failing row: %d %s", w.Code, w.Body.String())
	}
	var n int
	db.Model(&data_models.InvoicingPayment{}).Count(&n)
	if n != 3 {
		t.Fatalf("payments after a failed file: %d, want the 3 seeded", n)
	}
	if events, _ := data_models.ListInvoicingAuditEvents(db, 5, "import", 0); len(events) != 0 {
		t.Fatalf("a failed file recorded an import: %+v", events)
	}
	db.Exec(`DROP TRIGGER fail_one`)

	// The book is read a fixed number of times for the file, not per row.
	var reads int64
	count := func(*gorm.Scope) { atomic.AddInt64(&reads, 1) }
	db.Callback().Query().Register("test:count_query", count)
	db.Callback().RowQuery().Register("test:count_row_query", count)
	db.Callback().Create().Register("test:count_create", count)
	w := uploadCSV(t, r, payPalFile(rows), "")
	db.Callback().Query().Remove("test:count_query")
	db.Callback().RowQuery().Remove("test:count_row_query")
	db.Callback().Create().Remove("test:count_create")
	if w.Code != http.StatusOK {
		t.Fatalf("import: %d %s", w.Code, w.Body.String())
	}
	if reads > 40 {
		t.Errorf("the import ran %d reads for %d rows; the book is read once for the file", reads, rows)
	}
	s := decodeSummary(t, w)
	if s.Imported != rows-4 || s.Duplicates != 4 || s.RowsRead != s.Imported+s.Duplicates+s.Skipped {
		t.Errorf("summary %+v", s)
	}
	db.Model(&data_models.InvoicingPayment{}).Count(&n)
	if n != 3+rows-4 {
		t.Errorf("payments stored %d, want %d", n, 3+rows-4)
	}
	var linked, payer0 int
	db.Model(&data_models.InvoicingPayment{}).Where("source = 'paypal' AND client_id IS NOT NULL").Count(&linked)
	db.Model(&data_models.InvoicingPayment{}).Where("source = 'paypal' AND sender_email = 'payer0@example.test' AND transaction_id NOT IN ('T0010','T1500')").Count(&payer0)
	if linked != payer0 || linked == 0 {
		t.Errorf("payments linked to the client %d, want every new one of payer0 (%d)", linked, payer0)
	}
	var one data_models.InvoicingPayment
	db.Where("transaction_id = 'T0001'").First(&one)
	if one.Entry != data_models.PaymentEntryImport || one.CreatedAt.IsZero() || one.GrossCents != 1000 || one.NetCents != 1000 || one.PaidOn.Format("2006-01-02") != "2026-07-14" {
		t.Errorf("a stored row: %+v", one)
	}
}
