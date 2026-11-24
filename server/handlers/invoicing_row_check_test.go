package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"aspirant-online/server/data_models"
	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// system_3 #12265: the three import confirms refuse what a hand entry would
// refuse about a row's day, amount and text, and name a refused row by its
// day and payer. Every name is invented.

type confirmRoute struct {
	name, path, nameField, amountField, prep string
	latestAhead                              int // days after today in Berlin a row may carry
	good                                     func(id string) map[string]interface{}
}

var confirmRoutes = []confirmRoute{
	{
		name: "N26 payments", path: "/invoicing/payments/import-n26/confirm", nameField: "sender_name", amountField: "gross_cents", prep: "from",
		latestAhead: 1, // the route's tolerance, kept (#12265 review point 5)
		good: func(id string) map[string]interface{} {
			return map[string]interface{}{"document_kind": n26KindTransferConfirmation, "transaction_id": "N26ROW" + id, "paid_on": "2026-10-03",
				"currency": "EUR", "gross_cents": 12000, "fee_cents": 0, "sender_name": "Mara Esempio", "note": "Wandbild"}
		},
	},
	{
		name: "N26 expenses", path: "/invoicing/expenses/import-n26/confirm", nameField: "payee_name", amountField: "amount_cents", prep: "to",
		good: func(id string) map[string]interface{} {
			return map[string]interface{}{"transaction_id": "stmt-row" + id, "paid_on": "2026-10-03", "currency": "EUR",
				"amount_cents": 4567, "payee_name": "Papeterie Rossi", "note": "Papier"}
		},
	},
	{
		name: "PayPal expenses", path: "/invoicing/expenses/import-paypal/confirm", nameField: "payee_name", amountField: "amount_cents", prep: "to",
		good: func(id string) map[string]interface{} {
			return map[string]interface{}{"transaction_id": "PPROW" + id, "paid_on": "2026-10-03", "currency": "EUR",
				"amount_cents": 4567, "payee_name": "Papeterie Rossi", "note": "Papier"}
		},
	},
}

func confirmRowRouter(t *testing.T) (*gorm.DB, *gin.Engine) {
	t.Helper()
	db := setupN26ExpensesDB(t)
	r := setupN26ExpensesRouter(db, 5)
	r.POST("/invoicing/expenses/import-paypal/confirm", ImportPayPalExpensesConfirmHandler)
	return db, r
}

func postRows(r *gin.Engine, path string, rows ...map[string]interface{}) (int, string) {
	body, _ := json.Marshal(map[string]interface{}{"rows": rows})
	w := invoicingDo(r, "POST", path, string(body))
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Error.Code != "" {
		return w.Code, resp.Error.Code + ": " + resp.Error.Message
	}
	return w.Code, w.Body.String()
}

func storedRows(t *testing.T, db *gorm.DB) int {
	t.Helper()
	var p, e int
	db.Model(&data_models.InvoicingPayment{}).Count(&p)
	db.Model(&data_models.InvoicingExpense{}).Count(&e)
	return p + e
}

func TestImportConfirms_RefuseWhatAHandEntryWould(t *testing.T) {
	today := berlinToday()
	for _, route := range confirmRoutes {
		db, r := confirmRowRouter(t)
		ahead := today.AddDate(0, 0, route.latestAhead+1).Format(invoicingDateLayout)
		for _, c := range []struct {
			what  string
			field string
			value interface{}
		}{
			{"the largest int64 amount", route.amountField, int64(9223372036854775807)},
			{"one cent past 100 million euros", route.amountField, int64(10_000_000_001)},
			{"a day past the route's latest", "paid_on", ahead},
			{"0001-01-01", "paid_on", "0001-01-01"},
			{"1999-12-31", "paid_on", "1999-12-31"},
			{"a line break in the name", route.nameField, "Mara\nEsempio"},
			{"a bell in the name", route.nameField, "Mara\aEsempio"},
			{"a right-to-left override in the name", route.nameField, "Mara ‮oipmesE"},
			{"a soft hyphen in the name", route.nameField, "Ma­ra Esempio"},
			{"a bell in the note", "note", "Wand\abild"},
			{"a right-to-left override in the note", "note", "Wand‮bild"},
			{"an isolate in the note", "note", "Wand⁦bild⁩"},
		} {
			row := route.good("bad")
			row[c.field] = c.value
			code, msg := postRows(r, route.path, row)
			if code != http.StatusBadRequest || !strings.HasPrefix(msg, "invalid_row: the row ") {
				t.Errorf("%s, %s: %d %s; want 400 invalid_row naming the row", route.name, c.what, code, msg)
			}
			if strings.ContainsAny(msg, "\a‮⁦­") {
				t.Errorf("%s, %s: the refusal prints the refused character: %q", route.name, c.what, msg)
			}
		}
		if n := storedRows(t, db); n != 0 {
			t.Fatalf("%s: refused rows stored %d rows", route.name, n)
		}

		// A row is named by its day and its payer or payee, not its place.
		row := route.good("named")
		row[route.amountField] = int64(10_000_000_001)
		_, msg := postRows(r, route.path, route.good("first"), row)
		want := fmt.Sprintf("invalid_row: the row of 2026-10-03 %s %s: %s must be above zero and at most 100000000 euros", route.prep, row[route.nameField], route.amountField)
		if msg != want {
			t.Errorf("%s: message %q, want %q", route.name, msg, want)
		}
		// All or nothing: the good row beside it was not stored either.
		if n := storedRows(t, db); n != 0 {
			t.Fatalf("%s: a refused confirm stored %d rows", route.name, n)
		}

		// What passes: the bounds themselves, a line break and a compound
		// emoji in a note, and the latest day the route takes.
		for i, mutate := range []func(map[string]interface{}){
			func(map[string]interface{}) {},
			func(m map[string]interface{}) { m[route.amountField] = int64(10_000_000_000) },
			func(m map[string]interface{}) { m["paid_on"] = "2000-01-01" },
			func(m map[string]interface{}) {
				m["paid_on"] = today.AddDate(0, 0, route.latestAhead).Format(invoicingDateLayout)
			},
			func(m map[string]interface{}) { m["note"] = "Wandbild\nfür das Atelier 👩‍🎨" },
			func(m map[string]interface{}) { m[route.nameField] = "Anna-Lena O'Neil" },
		} {
			row := route.good(fmt.Sprintf("ok%d", i))
			mutate(row)
			if code, msg := postRows(r, route.path, row); code != http.StatusOK {
				t.Errorf("%s, passing case %d: %d %s", route.name, i, code, msg)
			}
		}
	}
}

// The hand entries take the same note rule: a direction override is refused,
// a compound emoji is not.
func TestHandEntries_NoteRefusesADirectionOverride(t *testing.T) {
	f := newInvoiceFixture(t)
	er := setupExpensesRouter(setupExpensesDB(t), 5)
	for _, c := range []struct {
		r    *gin.Engine
		path string
		body string
	}{
		{f.r, "/invoicing/payments/manual", `{"paid_on":"2026-07-14","gross_cents":4200,"method":"bank_transfer","note":%q}`},
		{er, "/invoicing/expenses/manual", `{"paid_on":"2026-07-14","amount_cents":4200,"note":%q}`},
	} {
		w := invoicingDo(c.r, "POST", c.path, fmt.Sprintf(c.body, "Wand‮bild"))
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "direction-override") {
			t.Errorf("%s with an override: %d %s", c.path, w.Code, w.Body.String())
		}
		if w := invoicingDo(c.r, "POST", c.path, fmt.Sprintf(c.body, "Wandbild 👩‍🎨")); w.Code != http.StatusCreated {
			t.Errorf("%s with a compound emoji: %d %s", c.path, w.Code, w.Body.String())
		}
	}
}

// Every row the N26 parses offer from the fixture files passes the confirm,
// and so does a statement row whose payer and note hold a soft hyphen and a
// zero-width space; that row keeps the id it had before the parse cleaned it
// (#12265 review point 1).
func TestN26Parses_EveryOfferedRowPassesTheConfirm(t *testing.T) {
	payments := map[string][]n26Candidate{}
	expenses := map[string][]n26ExpenseCandidate{}
	conf, err := parseN26TransferConfirmation(readN26Fixture(t, "n26_confirmation.txt"))
	if err != nil {
		t.Fatalf("confirmation: %v", err)
	}
	payments["n26_confirmation.txt"] = conf
	dirty := strings.Replace(readN26Fixture(t, "n26_statement.txt"), "Mara Esempio", "Ma­ra​ Esempio", 1)
	dirty = strings.Replace(dirty, "Illustrazione logo", "Illustrazione­ logo‮", 1)
	for _, name := range []string{"n26_statement.txt", "n26_statement_de.txt", "n26_kontoaktivitaet.txt", "dirty"} {
		text := dirty
		if name != "dirty" {
			text = readN26Fixture(t, name)
		}
		in, out, _, err := scanN26Statement(text)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		payments[name], expenses[name] = in, out
	}
	csv := readN26Fixture(t, "n26_export_en.csv")
	if payments["n26_export_en.csv"], err = parseN26CSVPayments(csv); err != nil {
		t.Fatalf("csv payments: %v", err)
	}
	if expenses["n26_export_en.csv"], err = parseN26CSVExpenses(csv); err != nil {
		t.Fatalf("csv expenses: %v", err)
	}

	// What the parse handler offers: a transfer from PayPal is dropped first.
	offered := 0
	for name, cands := range payments {
		_, r := confirmRowRouter(t)
		kept, _ := dropPayPalTransfers(cands)
		for _, c := range kept {
			if w := confirmN26(t, r, []n26Candidate{c}); w.Code != http.StatusOK {
				t.Errorf("%s: payment row %s %q refused: %s", name, c.PaidOn, c.SenderName, w.Body.String())
			}
			offered++
		}
		if len(expenses[name]) > 0 {
			if w := confirmN26Expenses(t, r, asExpenseConfirmRows(expenses[name], "")); w.Code != http.StatusOK {
				t.Errorf("%s: expense rows refused: %s", name, w.Body.String())
			}
			offered += len(expenses[name])
		}
	}
	if offered < 10 {
		t.Fatalf("only %d offered rows were checked", offered)
	}

	// The dirty row is offered clean, under the id its printed text gives.
	var row *n26Candidate
	for i := range payments["dirty"] {
		if payments["dirty"][i].GrossCents == 25000 {
			row = &payments["dirty"][i]
		}
	}
	if row == nil || row.SenderName != "Mara Esempio" || row.Note != "Bonifico ricevuto Illustrazione logo" {
		t.Fatalf("dirty row offered as %+v", row)
	}
	key := "2026-08-01|25000|Ma­ra​ Esempio|Bonifico ricevuto Illustrazione­ logo‮"
	sum := sha256.Sum256([]byte(key + "|0"))
	if want := "stmt-" + hex.EncodeToString(sum[:])[:24]; row.TransactionID != want {
		t.Fatalf("dirty row id %s, want %s (made from the printed text)", row.TransactionID, want)
	}
}
