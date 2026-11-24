package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"aspirant-online/server/data_models"
)

// system_3 #12259: a reversal or a refund in a later PayPal file names the
// payment in the book it undoes, and the import changes nothing. The names
// and ids are invented.

const undoPayment = "03.10.2026,Mara Esempio,Express Checkout Payment,Completed,EUR,120.00,PPUNDO00001,\n"

func decodeUndone(t *testing.T, w *httptest.ResponseRecorder) []paypalUndone {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("import: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Undone *[]paypalUndone `json:"undone"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if resp.Undone == nil {
		t.Fatalf("the answer has no undone list: %s", w.Body.String())
	}
	return *resp.Undone
}

func storedPayment(t *testing.T, r interface {
	ServeHTTP(http.ResponseWriter, *http.Request)
}, txn string) data_models.InvoicingPayment {
	t.Helper()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/invoicing/payments", nil)
	r.ServeHTTP(w, req)
	var resp struct {
		Payments []data_models.InvoicingPayment `json:"payments"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	for _, p := range resp.Payments {
		if p.TransactionID == txn {
			return p
		}
	}
	t.Fatalf("payment %s is not listed", txn)
	return data_models.InvoicingPayment{}
}

// The review's case (C3-4): file 1 holds the payment and a partial refund
// naming it; file 2 holds only the reversal of the whole payment.
func TestImportPayPal_ALaterReversalNamesTheStoredPayment(t *testing.T) {
	db, r := setupPayPalExpensesRouter(t, 7)

	// File 1: the refund is found though its payment is stored by the same
	// import, and it is offered as an expense candidate.
	w := uploadCSV(t, r, reversalHeader+undoPayment+
		"05.10.2026,Mara Esempio,Payment Refund,Completed,EUR,-30.00,PPUNDO00002,PPUNDO00001\n", "")
	undone := decodeUndone(t, w)
	s := decodeSummary(t, w)
	if s.Imported != 1 || s.SkippedReasons[paypalSkipOutgoing] != 1 || s.RowsRead != s.Imported+s.Duplicates+s.Skipped {
		t.Fatalf("file 1 summary: %+v", s)
	}
	paid := storedPayment(t, r, "PPUNDO00001")
	if len(undone) != 1 {
		t.Fatalf("file 1 undone: %+v", undone)
	}
	u := undone[0]
	if u.Kind != paypalUndoRefund || u.Full || u.AmountCents != 3000 || u.TransactionID != "PPUNDO00002" || u.PaidOn != "2026-10-05" || u.Invoice != nil {
		t.Errorf("file 1 entry: %+v", u)
	}
	if u.Payment != (paypalUndonePayment{ID: paid.ID, TransactionID: "PPUNDO00001", PaidOn: "2026-10-03", SenderName: "Mara Esempio", GrossCents: 12000, Currency: "EUR"}) {
		t.Errorf("file 1 payment: %+v", u.Payment)
	}
	cands := decodePayPalExpenseCandidates(t, w)
	if len(cands) != 1 || cands[0].TransactionID != "PPUNDO00002" || cands[0].AmountCents != 3000 ||
		cands[0].RefundOf != paid.ID || cands[0].UndoKind != paypalUndoRefund || cands[0].PayeeName != "Mara Esempio" {
		t.Fatalf("file 1 candidates: %+v", cands)
	}
	// Confirmed, the refund is money out in the book.
	if w := confirmPayPalExpenses(r, cands, ""); w.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", w.Code, w.Body.String())
	}
	var e data_models.InvoicingExpense
	if err := db.Where("owner_user_id = ? AND transaction_id = ?", 7, "PPUNDO00002").First(&e).Error; err != nil || e.AmountCents != 3000 {
		t.Fatalf("the refund as an expense: %+v %v", e, err)
	}

	// File 2: the reversal alone. Named, in full, and counted as before.
	w = uploadCSV(t, r, reversalHeader+"20.11.2026,Mara Esempio,Payment Reversal,Completed,EUR,-120.00,PPUNDO00003,PPUNDO00001\n", "")
	undone = decodeUndone(t, w)
	s = decodeSummary(t, w)
	if s.RowsRead != 1 || s.Imported != 0 || s.Skipped != 1 || s.SkippedReasons[paypalSkipReversal] != 1 {
		t.Fatalf("file 2 summary: %+v", s)
	}
	if len(undone) != 1 || undone[0].Kind != paypalUndoReversal || !undone[0].Full || undone[0].AmountCents != 12000 || undone[0].Payment.ID != paid.ID {
		t.Fatalf("file 2 undone: %+v", undone)
	}
	if cands := decodePayPalExpenseCandidates(t, w); len(cands) != 0 {
		t.Errorf("a full reversal is no candidate: %+v", cands)
	}
	// Nothing changed without zir press.
	if after := storedPayment(t, r, "PPUNDO00001"); after.GrossCents != 12000 || after.RemovedAt != nil {
		t.Errorf("the payment changed: %+v", after)
	}
}

// Undone in full, on an issued invoice: the answer names the invoice.
func TestImportPayPal_AReversalNamesTheIssuedInvoiceItsPaymentIsOn(t *testing.T) {
	f := newInvoiceFixture(t)
	draft := f.draft(t, 1) // TX2, €200.00
	if code, _, body := f.issue(draft); code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, body)
	}
	f.draft(t, 2) // TX3, on a draft only
	undone := decodeUndone(t, uploadCSV(t, f.r, reversalHeader+
		"20.10.2026,Galerie Nord,Payment Reversal,Completed,EUR,-200.00,R2,TX2\n"+
		"20.10.2026,Galerie Nord,Payment Reversal,Completed,EUR,-300.00,R3,TX3\n", ""))
	if len(undone) != 2 {
		t.Fatalf("undone: %+v", undone)
	}
	byPayment := map[string]paypalUndone{}
	for _, u := range undone {
		byPayment[u.Payment.TransactionID] = u
	}
	issued := byPayment["TX2"]
	if issued.Invoice == nil || issued.Invoice.ID != draft || issued.Invoice.InvoiceNumber == "" || !issued.Full {
		t.Errorf("on an issued invoice: %+v", issued)
	}
	// A draft is not an issued invoice: removing the payment is the step.
	if onDraft := byPayment["TX3"]; onDraft.Invoice != nil || !onDraft.Full {
		t.Errorf("on a draft: %+v", onDraft)
	}
}

// A full refund in a later file is named in full and is no candidate.
func TestImportPayPal_AFullRefundInALaterFileIsNamedAndNotOffered(t *testing.T) {
	_, r := setupPayPalExpensesRouter(t, 7)
	decodeUndone(t, uploadCSV(t, r, reversalHeader+undoPayment, ""))
	w := uploadCSV(t, r, reversalHeader+"06.10.2026,Mara Esempio,Payment Refund,Completed,EUR,-120.00,PPUNDO00004,PPUNDO00001\n", "")
	undone := decodeUndone(t, w)
	if len(undone) != 1 || undone[0].Kind != paypalUndoRefund || !undone[0].Full {
		t.Fatalf("undone: %+v", undone)
	}
	if cands := decodePayPalExpenseCandidates(t, w); len(cands) != 0 {
		t.Errorf("candidates: %+v", cands)
	}
}

// A reversal in the same file as its payment cancels it, as before, and
// names nothing: the payment was never stored.
func TestImportPayPal_ASameFileReversalBehavesAsBefore(t *testing.T) {
	_, r := setupPayPalExpensesRouter(t, 7)
	w := uploadCSV(t, r, reversalHeader+undoPayment+
		"20.10.2026,Mara Esempio,Payment Reversal,Completed,EUR,-120.00,PPUNDO00003,PPUNDO00001\n", "")
	undone := decodeUndone(t, w)
	s := decodeSummary(t, w)
	if len(undone) != 0 || s.Imported != 0 || s.SkippedReasons[paypalSkipReversed] != 1 || s.SkippedReasons[paypalSkipReversal] != 1 {
		t.Fatalf("summary %+v, undone %+v", s, undone)
	}
}

// What matches nothing stays a counted skip: another book's payment, a
// removed payment, a withdrawal, another currency, more than the payment,
// and an id that exists nowhere.
func TestImportPayPal_AnUndoThatMatchesNothingStaysACountedSkip(t *testing.T) {
	db, r := setupPayPalExpensesRouter(t, 7)
	// Another owner's payment, with the id this owner's file names.
	if err := db.Create(&data_models.InvoicingPayment{OwnerUserID: 8, Source: "paypal", TransactionID: "PPOTHER0001", Kind: "incoming",
		Currency: "EUR", GrossCents: 12000, Category: "paid_work"}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	decodeUndone(t, uploadCSV(t, r, reversalHeader+undoPayment+
		"03.10.2026,Mara Esempio,Express Checkout Payment,Completed,EUR,50.00,PPGONE00001,\n"+
		"04.10.2026,,User Initiated Withdrawal,Completed,EUR,-80.00,PPWD00001,\n", ""))
	gone := storedPayment(t, r, "PPGONE00001")
	if w := invoicingDo(r, "POST", "/invoicing/payments/"+jsonID(gone.ID)+"/remove", ""); w.Code != http.StatusOK {
		t.Fatalf("remove: %d %s", w.Code, w.Body.String())
	}
	w := uploadCSV(t, r, reversalHeader+
		"20.10.2026,X,Payment Reversal,Completed,EUR,-120.00,R1,PPOTHER0001\n"+
		"20.10.2026,X,Payment Reversal,Completed,EUR,-50.00,R2,PPGONE00001\n"+
		"20.10.2026,X,Payment Reversal,Completed,EUR,-80.00,R3,PPWD00001\n"+
		"20.10.2026,X,Payment Reversal,Completed,USD,-120.00,R4,PPUNDO00001\n"+
		"20.10.2026,X,Payment Refund,Completed,EUR,-150.00,R5,PPUNDO00001\n"+
		"20.10.2026,X,Payment Reversal,Completed,EUR,-10.00,R6,PPNOWHERE01\n", "")
	undone := decodeUndone(t, w)
	s := decodeSummary(t, w)
	if len(undone) != 0 {
		t.Errorf("undone: %+v", undone)
	}
	if s.SkippedReasons[paypalSkipReversal] != 5 || s.SkippedReasons[paypalSkipOutgoing] != 1 || s.RowsRead != s.Skipped {
		t.Errorf("summary: %+v", s)
	}
	if cands := decodePayPalExpenseCandidates(t, w); len(cands) != 0 {
		t.Errorf("candidates: %+v", cands)
	}
}

func jsonID(id uint) string {
	b, _ := json.Marshal(id)
	return string(b)
}
