package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"aspirant-online/server/data_models"
)

// system_3 #12262: one long payer or note in an N26 file made the whole
// confirm fail. Every parser now cuts an offered row's text to the confirm's
// limits, in characters (payer 200, payment note 500, expense note 1,000,
// payee 100), after the row's id is made from the uncut text, and the
// confirm counts characters, as its message says. "Ä" is two bytes, so each
// limit is tested past its byte count too.

const longNoteLine = "Rechnung Wandbild Ä " // 20 characters, 21 bytes

// statementWithLongRows is the German statement fixture's layout with one
// credit whose payer and note run past the limits, one credit that differs
// from it only past the cut, and one debit whose payee and note do.
func statementWithLongRows(payer, note2Tail string) string {
	noteLines := ""
	for i := 0; i < 30; i++ { // 30 lines, joined with spaces: 629 characters
		noteLines += longNoteLine + "\n"
	}
	payee := strings.Repeat("Ä", 150)
	var expenseNote string
	for i := 0; i < 60; i++ { // 60 lines: 1,259 characters
		expenseNote += longNoteLine + "\n"
	}
	row := func(name, date, amount string) string {
		return name + strings.Repeat(" ", 4) + date + "       " + amount + "\n"
	}
	return "Kontoauszug Nr. 11/2026\n01.11.2026 bis 30.11.2026\n\n" +
		"Zahlungsempfänger/Auftraggeber                                        Verbuchungsdatum        Betrag\n\n" +
		row(payer, "03.11.2026", "250,00€") + "Gutschriften\n" + noteLines + "Wertstellung 04.11.2026\n\n" +
		row(payer, "03.11.2026", "250,00€") + "Gutschriften\n" + noteLines + note2Tail + "\nWertstellung 04.11.2026\n\n" +
		row(payee, "05.11.2026", "-45,67€") + "Lastschriften\n" + expenseNote + "Wertstellung 05.11.2026\n\n" +
		"N26 Bank Beispiel GmbH Musterstraße 1, 10000 Berlin, Germany\n"
}

func TestN26StatementLongTextIsOfferedCutAndConfirms(t *testing.T) {
	payer := "Mara " + strings.Repeat("Ä", 225) // 230 characters, 455 bytes
	text := statementWithLongRows(payer, "Ende")
	in, out, _, err := scanN26Statement(text)
	if err != nil || len(in) != 2 || len(out) != 1 {
		t.Fatalf("parse: %v, %d in, %d out", err, len(in), len(out))
	}
	for i, c := range in {
		if n := utf8.RuneCountInString(c.SenderName); n != 200 {
			t.Errorf("in[%d] payer: %d characters, want 200", i, n)
		}
		if n := utf8.RuneCountInString(c.Note); n > 500 || n < 495 {
			t.Errorf("in[%d] note: %d characters, want the 500-character cut", i, n)
		}
		if !utf8.ValidString(c.SenderName) || !utf8.ValidString(c.Note) {
			t.Errorf("in[%d] was cut inside a character", i)
		}
		if c.Note != strings.TrimSpace(c.Note) {
			t.Errorf("in[%d] note ends in a space: %q", i, c.Note[len(c.Note)-3:])
		}
	}
	// The two credits differ only past the cut: they read the same, and
	// still have two ids, made from the uncut text.
	if in[0].Note != in[1].Note || in[0].TransactionID == in[1].TransactionID {
		t.Errorf("ids: %q %q", in[0].TransactionID, in[1].TransactionID)
	}
	// The id is the one the uncut row always had, so a statement imported
	// before this change still reads "already imported".
	if want := statementIDFor(t, text); in[0].TransactionID != want {
		t.Errorf("id %q, want the uncut row's %q", in[0].TransactionID, want)
	}
	if n := utf8.RuneCountInString(out[0].PayeeName); n != 100 {
		t.Errorf("payee: %d characters, want 100", n)
	}
	if n := utf8.RuneCountInString(out[0].Note); n > 1000 || n < 995 {
		t.Errorf("expense note: %d characters, want the 1,000-character cut", n)
	}

	// Each confirm takes the rows as offered.
	db := setupN26ExpensesDB(t)
	r := setupN26ExpensesRouter(db, 5)
	if w := confirmN26(t, r, in); w.Code != http.StatusOK {
		t.Fatalf("payments confirm: %d %s", w.Code, w.Body.String())
	}
	if w := confirmN26Expenses(t, r, asExpenseConfirmRows(out, "")); w.Code != http.StatusOK {
		t.Fatalf("expenses confirm: %d %s", w.Code, w.Body.String())
	}
	var stored []data_models.InvoicingPayment
	db.Find(&stored)
	if len(stored) != 2 || stored[0].SenderName != in[0].SenderName || stored[0].Note != in[0].Note {
		t.Errorf("stored: %d payments", len(stored))
	}
}

// statementIDFor is the first credit's id as the parser made it before this
// change: from the date, the amount and the uncut payer and note.
func statementIDFor(t *testing.T, text string) string {
	t.Helper()
	lines := strings.Split(text, "\n")
	var noteParts []string
	payer := ""
	for i, l := range lines {
		if strings.Contains(l, "250,00€") {
			payer = strings.TrimSpace(l[:strings.Index(l, "03.11.2026")])
			for j := i + 2; j < len(lines) && !strings.HasPrefix(lines[j], "Wertstellung"); j++ {
				noteParts = append(noteParts, strings.TrimSpace(lines[j]))
			}
			break
		}
	}
	key := "2026-11-04|25000|" + payer + "|" + strings.Join(noteParts, " ")
	return stmtIDOf(key, 0)
}

func TestN26TransferConfirmationLongNoteIsOfferedCut(t *testing.T) {
	text := readN26Fixture(t, "n26_confirmation.txt")
	long := strings.TrimSpace(strings.Repeat(longNoteLine, 40)) // 799 characters
	if !strings.Contains(text, "Illustrazione copertina album") {
		t.Fatal("fixture drifted: no Causale line to replace")
	}
	text = strings.Replace(text, "Illustrazione copertina album", long, 1)
	cands, err := parseN26TransferConfirmation(text)
	if err != nil || len(cands) != 1 {
		t.Fatalf("parse: %v %+v", err, cands)
	}
	if n := utf8.RuneCountInString(cands[0].Note); n > 500 || n < 495 {
		t.Errorf("note: %d characters", n)
	}
	r := setupN26Router(setupPaymentsDB(t), 5)
	if w := confirmN26(t, r, cands); w.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", w.Code, w.Body.String())
	}
}

// The CSV path's cut moved from bytes to characters: a payer of 150 "Ä"
// (300 bytes) is offered whole, as from a statement.
func TestN26CSVCutsInCharacters(t *testing.T) {
	header := strings.SplitN(csvFixture(t), "\n", 2)[0]
	name := strings.Repeat("Ä", 150)
	note := strings.Repeat("Ä", 450) // 900 bytes
	rows := header + "\n" +
		`"2026-10-02","2026-10-02","` + name + `","","Credit Transfer","` + note + `","Hauptkonto","10.00","","",""` + "\n"
	in, _, _, err := scanN26CSV(rows)
	if err != nil || len(in) != 1 {
		t.Fatalf("parse: %v %+v", err, in)
	}
	if in[0].SenderName != name || in[0].Note != note {
		t.Errorf("cut in bytes: payer %d, note %d characters", utf8.RuneCountInString(in[0].SenderName), utf8.RuneCountInString(in[0].Note))
	}
}

// The confirm counts characters: 200 and 500 "Ä" pass (400 and 1,000
// bytes); one more of each is refused, naming the limit.
func TestN26ConfirmCountsCharacters(t *testing.T) {
	r := setupN26Router(setupPaymentsDB(t), 5)
	row := func(id, payer, note string) n26Candidate {
		return n26Candidate{DocumentKind: n26KindAccountStatement, TransactionID: id, PaidOn: "2026-11-04", Currency: "EUR",
			GrossCents: 1000, NetCents: 1000, SenderName: payer, Note: note}
	}
	if w := confirmN26(t, r, []n26Candidate{row("stmt-a", strings.Repeat("Ä", 200), strings.Repeat("Ä", 500))}); w.Code != http.StatusOK {
		t.Fatalf("at the limits: %d %s", w.Code, w.Body.String())
	}
	for _, c := range []struct {
		row  n26Candidate
		want string
	}{
		{row("stmt-b", strings.Repeat("Ä", 201), ""), "row 0: sender_name is missing or longer than 200 characters"},
		{row("stmt-c", "Mara", strings.Repeat("Ä", 501)), "row 0: note is longer than 500 characters"},
	} {
		w := confirmN26(t, r, []n26Candidate{c.row})
		var resp struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(w.Body.Bytes(), &resp)
		if w.Code != http.StatusBadRequest || resp.Error.Message != c.want {
			t.Errorf("%d %s, want %q", w.Code, w.Body.String(), c.want)
		}
	}
}

// stmtIDOf is the statement parser's id for the n-th row of a key, written
// out here so the test pins the formula rather than calling it.
func stmtIDOf(key string, n int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d", key, n)))
	return "stmt-" + hex.EncodeToString(sum[:])[:24]
}
