package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"aspirant-online/server/data_models"

	"github.com/jinzhu/gorm"
)

// system_3 #10555-B1, §3.143 Decision 6: a re-import meets a row the owner
// removed, and a row she deleted for good, honestly. The review flags a
// removed row (no checkbox) and a deleted one, and every confirm counts both
// apart from plain duplicates, for payments and expenses, on the N26 PDF, N26
// CSV and PayPal paths.

var priorNow = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

func removePaymentRow(t *testing.T, db *gorm.DB, owner uint, source, txn string) {
	t.Helper()
	res := db.Model(&data_models.InvoicingPayment{}).Where("owner_user_id = ? AND source = ? AND transaction_id = ?", owner, source, txn).Update("removed_at", priorNow)
	if res.Error != nil || res.RowsAffected != 1 {
		t.Fatalf("remove payment %s: %v (%d rows)", txn, res.Error, res.RowsAffected)
	}
}

func purgePaymentRow(t *testing.T, db *gorm.DB, owner uint, source, txn string) {
	t.Helper()
	var p data_models.InvoicingPayment
	if err := db.Where("owner_user_id = ? AND source = ? AND transaction_id = ?", owner, source, txn).First(&p).Error; err != nil {
		t.Fatalf("load payment %s: %v", txn, err)
	}
	if err := data_models.TombstoneInvoicingPayment(db, p, owner, priorNow); err != nil {
		t.Fatalf("tombstone payment: %v", err)
	}
	if err := db.Delete(&p).Error; err != nil {
		t.Fatalf("delete payment: %v", err)
	}
}

func removeExpenseRow(t *testing.T, db *gorm.DB, owner uint, source, txn string) {
	t.Helper()
	res := db.Model(&data_models.InvoicingExpense{}).Where("owner_user_id = ? AND source = ? AND transaction_id = ?", owner, source, txn).Update("removed_at", priorNow)
	if res.Error != nil || res.RowsAffected != 1 {
		t.Fatalf("remove expense %s: %v (%d rows)", txn, res.Error, res.RowsAffected)
	}
}

func purgeExpenseRow(t *testing.T, db *gorm.DB, owner uint, source, txn string) {
	t.Helper()
	var e data_models.InvoicingExpense
	if err := db.Where("owner_user_id = ? AND source = ? AND transaction_id = ?", owner, source, txn).First(&e).Error; err != nil {
		t.Fatalf("load expense %s: %v", txn, err)
	}
	if err := data_models.TombstoneInvoicingExpense(db, e, owner, priorNow); err != nil {
		t.Fatalf("tombstone expense: %v", err)
	}
	if err := db.Delete(&e).Error; err != nil {
		t.Fatalf("delete expense: %v", err)
	}
}

func countPriorRows(db *gorm.DB, model interface{}, owner uint, txn string) int {
	var n int
	db.Model(model).Where("owner_user_id = ? AND transaction_id = ?", owner, txn).Count(&n)
	return n
}

type n26PriorCase struct {
	name   string
	upload func(t *testing.T) map[string][]byte
}

func TestImportPrior_N26ReviewFlagsAndConfirmCounts(t *testing.T) {
	cases := []n26PriorCase{
		{"pdf statement", func(t *testing.T) map[string][]byte {
			requirePdftotext(t)
			return map[string][]byte{"estratto.pdf": buildN26PDFFromText(t, readN26Fixture(t, "n26_statement.txt"))}
		}},
		{"csv export", func(t *testing.T) map[string][]byte {
			return map[string][]byte{"export.csv": []byte(csvFixture(t))}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := tc.upload(t)
			db := setupN26ExpensesDB(t)
			r := setupN26ExpensesRouter(db, 7)
			parse := func() n26ParsedFile {
				t.Helper()
				w := uploadN26(t, r, files)
				if w.Code != http.StatusOK {
					t.Fatalf("parse: %d %s", w.Code, w.Body.String())
				}
				f := decodeN26Parse(t, w).Files[0]
				if f.Error != nil || len(f.Candidates) < 2 || len(f.ExpenseCandidates) < 1 {
					t.Fatalf("fixture through the pipeline: %+v", f)
				}
				return f
			}
			confirmPay := func(rows []n26Candidate) n26ConfirmSummary {
				t.Helper()
				w := confirmN26(t, r, rows)
				if w.Code != http.StatusOK {
					t.Fatalf("payment confirm: %d %s", w.Code, w.Body.String())
				}
				var resp struct {
					Summary n26ConfirmSummary `json:"summary"`
				}
				json.Unmarshal(w.Body.Bytes(), &resp)
				return resp.Summary
			}
			confirmExp := func(rows []n26ExpenseCandidate) n26ConfirmSummary {
				t.Helper()
				w := confirmN26Expenses(t, r, asExpenseConfirmRows(rows, ""))
				if w.Code != http.StatusOK {
					t.Fatalf("expense confirm: %d %s", w.Code, w.Body.String())
				}
				var resp struct {
					Summary n26ConfirmSummary `json:"summary"`
				}
				json.Unmarshal(w.Body.Bytes(), &resp)
				return resp.Summary
			}

			f := parse()
			if s := confirmPay(f.Candidates); s.Confirmed != len(f.Candidates) {
				t.Fatalf("first payment confirm: %+v", s)
			}
			if s := confirmExp(f.ExpenseCandidates); s.Confirmed != len(f.ExpenseCandidates) {
				t.Fatalf("first expense confirm: %+v", s)
			}
			pay, exp := f.Candidates[0], f.ExpenseCandidates[0]

			// Stored rows read as plain duplicates, with neither new flag.
			f = parse()
			for _, c := range f.Candidates {
				if !c.AlreadyImported || c.Removed || c.DeletedEarlier {
					t.Errorf("stored payment flags: %+v", c)
				}
			}

			// Removed: flagged in the review, unticked, and the confirm stores
			// nothing and counts it as removed earlier.
			removePaymentRow(t, db, 7, invoicingPaymentSourceN26, pay.TransactionID)
			removeExpenseRow(t, db, 7, invoicingPaymentSourceN26, exp.TransactionID)
			f = parse()
			if c := f.Candidates[0]; !c.AlreadyImported || !c.Removed || c.DeletedEarlier {
				t.Errorf("removed payment in review: %+v", c)
			}
			if !f.Candidates[1].AlreadyImported || f.Candidates[1].Removed {
				t.Errorf("a stored neighbour changed: %+v", f.Candidates[1])
			}
			if e := f.ExpenseCandidates[0]; !e.AlreadyImported || !e.Removed || e.DeletedEarlier {
				t.Errorf("removed expense in review: %+v", e)
			}
			if s := confirmPay(f.Candidates[:1]); s.Confirmed != 0 || s.Duplicates != 1 || s.RemovedEarlier != 1 || s.DeletedEarlier != 0 {
				t.Errorf("payment confirm of a removed row: %+v", s)
			}
			if s := confirmExp(f.ExpenseCandidates[:1]); s.Confirmed != 0 || s.Duplicates != 1 || s.RemovedEarlier != 1 || s.DeletedEarlier != 0 {
				t.Errorf("expense confirm of a removed row: %+v", s)
			}

			// Deleted for good: flagged, and the confirm counts it as deleted
			// earlier and stores nothing. Before #10555-B1 the expense was
			// offered as new and stored again.
			purgePaymentRow(t, db, 7, invoicingPaymentSourceN26, pay.TransactionID)
			purgeExpenseRow(t, db, 7, invoicingPaymentSourceN26, exp.TransactionID)
			f = parse()
			if c := f.Candidates[0]; !c.AlreadyImported || c.Removed || !c.DeletedEarlier {
				t.Errorf("deleted payment in review: %+v", c)
			}
			if e := f.ExpenseCandidates[0]; !e.AlreadyImported || e.Removed || !e.DeletedEarlier {
				t.Errorf("deleted expense in review: %+v", e)
			}
			if s := confirmPay(f.Candidates[:1]); s.Confirmed != 0 || s.Duplicates != 1 || s.DeletedEarlier != 1 || s.RemovedEarlier != 0 {
				t.Errorf("payment confirm of a deleted row: %+v", s)
			}
			if s := confirmExp(f.ExpenseCandidates[:1]); s.Confirmed != 0 || s.Duplicates != 1 || s.DeletedEarlier != 1 || s.RemovedEarlier != 0 {
				t.Errorf("expense confirm of a deleted row: %+v", s)
			}
			if n := countPriorRows(db, &data_models.InvoicingPayment{}, 7, pay.TransactionID); n != 0 {
				t.Errorf("the deleted payment came back: %d rows", n)
			}
			if n := countPriorRows(db, &data_models.InvoicingExpense{}, 7, exp.TransactionID); n != 0 {
				t.Errorf("the deleted expense came back: %d rows", n)
			}

			// Another owner's book holds none of this: nothing is flagged.
			r8 := setupN26ExpensesRouter(db, 8)
			w := uploadN26(t, r8, files)
			other := decodeN26Parse(t, w).Files[0]
			for _, c := range other.Candidates {
				if c.AlreadyImported || c.Removed || c.DeletedEarlier {
					t.Errorf("another owner's row flagged: %+v", c)
				}
			}
			for _, e := range other.ExpenseCandidates {
				if e.AlreadyImported || e.Removed || e.DeletedEarlier {
					t.Errorf("another owner's expense flagged: %+v", e)
				}
			}
		})
	}
}

func TestImportPrior_PayPalImportAndExpenseReview(t *testing.T) {
	db, r := setupPayPalExpensesRouter(t, 7)
	w := uploadCSV(t, r, paypalFixtureEN, "")
	if s := decodeSummary(t, w); s.Imported != 3 {
		t.Fatalf("first import: %+v", s)
	}
	cands := decodePayPalExpenseCandidates(t, w)
	if len(cands) != 1 {
		t.Fatalf("expense candidates: %+v", cands)
	}
	if w := confirmPayPalExpenses(r, cands, "materials and supplies"); w.Code != http.StatusOK {
		t.Fatalf("expense confirm: %d %s", w.Code, w.Body.String())
	}
	var stored []data_models.InvoicingPayment
	db.Where("owner_user_id = ? AND source = ?", 7, invoicingPaymentSourcePayPal).Order("id asc").Find(&stored)
	if len(stored) != 3 {
		t.Fatalf("stored payments: %d", len(stored))
	}
	payTxn, expTxn := stored[0].TransactionID, cands[0].TransactionID

	confirmExp := func(cs []paypalExpenseCandidate) paypalExpenseConfirmSummary {
		t.Helper()
		w := confirmPayPalExpenses(r, cs, "")
		if w.Code != http.StatusOK {
			t.Fatalf("expense confirm: %d %s", w.Code, w.Body.String())
		}
		var resp struct {
			Summary paypalExpenseConfirmSummary `json:"summary"`
		}
		json.Unmarshal(w.Body.Bytes(), &resp)
		return resp.Summary
	}

	// Removed: the import, which has no review, skips it and names it.
	removePaymentRow(t, db, 7, invoicingPaymentSourcePayPal, payTxn)
	removeExpenseRow(t, db, 7, invoicingPaymentSourcePayPal, expTxn)
	w = uploadCSV(t, r, paypalFixtureEN, "")
	if s := decodeSummary(t, w); s.Imported != 0 || s.Duplicates != 3 || s.RemovedEarlier != 1 || s.DeletedEarlier != 0 {
		t.Errorf("import with a removed payment: %+v", s)
	}
	if !strings.Contains(w.Body.String(), `"removed_earlier":1`) || !strings.Contains(w.Body.String(), `"deleted_earlier":0`) {
		t.Errorf("summary wire shape: %s", w.Body.String())
	}
	cands = decodePayPalExpenseCandidates(t, w)
	if c := cands[0]; !c.AlreadyImported || !c.Removed || c.DeletedEarlier {
		t.Errorf("removed expense in review: %+v", c)
	}
	if !strings.Contains(w.Body.String(), `"removed":true`) || strings.Contains(w.Body.String(), `"deleted_earlier":true`) {
		t.Errorf("candidate wire shape: %s", w.Body.String())
	}
	if s := confirmExp(cands); s.Confirmed != 0 || s.Duplicates != 1 || s.RemovedEarlier != 1 {
		t.Errorf("expense confirm of a removed row: %+v", s)
	}

	// Deleted for good: skipped, counted as deleted earlier, never stored again.
	purgePaymentRow(t, db, 7, invoicingPaymentSourcePayPal, payTxn)
	purgeExpenseRow(t, db, 7, invoicingPaymentSourcePayPal, expTxn)
	w = uploadCSV(t, r, paypalFixtureEN, "")
	s := decodeSummary(t, w)
	if s.Imported != 0 || s.Duplicates != 3 || s.DeletedEarlier != 1 || s.RemovedEarlier != 0 {
		t.Errorf("import with a deleted payment: %+v", s)
	}
	if s.RowsRead != s.Imported+s.Duplicates+s.Skipped {
		t.Errorf("rows_read no longer adds up: %+v", s)
	}
	cands = decodePayPalExpenseCandidates(t, w)
	if c := cands[0]; !c.AlreadyImported || c.Removed || !c.DeletedEarlier {
		t.Errorf("deleted expense in review: %+v", c)
	}
	if s := confirmExp(cands); s.Confirmed != 0 || s.Duplicates != 1 || s.DeletedEarlier != 1 {
		t.Errorf("expense confirm of a deleted row: %+v", s)
	}
	if n := countPriorRows(db, &data_models.InvoicingPayment{}, 7, payTxn); n != 0 {
		t.Errorf("the deleted payment came back: %d rows", n)
	}
	if n := countPriorRows(db, &data_models.InvoicingExpense{}, 7, expTxn); n != 0 {
		t.Errorf("the deleted expense came back: %d rows", n)
	}
}

// The prior read is owner-scoped and prefers a stored row to a tombstone.
func TestImportPrior_ModelReads(t *testing.T) {
	db := setupN26ExpensesDB(t)
	if p, err := data_models.InvoicingPaymentPrior(db, 7, "n26", "none"); err != nil || p != data_models.PriorNone || p.Seen() {
		t.Fatalf("empty book: %v %v", p, err)
	}
	pay := data_models.InvoicingPayment{OwnerUserID: 7, Source: "n26", TransactionID: "t1", Kind: data_models.PaymentKindIncoming, PaidOn: priorNow, Currency: "EUR", GrossCents: 100}
	if err := db.Create(&pay).Error; err != nil {
		t.Fatal(err)
	}
	if err := data_models.TombstoneInvoicingPayment(db, pay, 7, priorNow); err != nil {
		t.Fatal(err)
	}
	if p, _ := data_models.InvoicingPaymentPrior(db, 7, "n26", "t1"); p != data_models.PriorStored {
		t.Errorf("a stored row with a stray tombstone reads %v, want stored", p)
	}
	if p, _ := data_models.InvoicingPaymentPrior(db, 8, "n26", "t1"); p != data_models.PriorNone {
		t.Errorf("another owner reads %v, want none", p)
	}
	if seen, _ := data_models.InvoicingPaymentSeen(db, 7, "n26", "t1"); !seen {
		t.Error("InvoicingPaymentSeen lost a stored row")
	}
}
