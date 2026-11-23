package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// system_3 #12085 (#11551; security c75108): an N26 CSV import keeps the
// payer's IBAN on the payment while it has no client, links later payments
// to the client that has it, and "Add payers…" carries it to a new client.
// The copy on the payment goes when the payment is linked or removed (C4),
// and no IBAN reaches a log line, an error, a summary, the payments API or
// an audit row (C5). Every IBAN here is a published example (C6).

const thirdExampleIBAN = "NL91ABNA0417164300" // the Dutch published example

const n26CSVHeaderRow = `"Booking Date","Value Date","Partner Name","Partner Iban","Type","Payment Reference","Account Name","Amount (EUR)","Original Amount","Original Currency","Exchange Rate"`

// n26CSVLine is one money-in row of an invented export.
func n26CSVLine(day, name, iban, amount string) string {
	return fmt.Sprintf(`"%s","%s","%s","%s","Credit Transfer","Rechnung","Hauptkonto","%s","","",""`, day, day, name, iban, amount)
}

func n26CSV(lines ...string) []byte {
	return []byte(strings.Join(append([]string{n26CSVHeaderRow}, lines...), "\n") + "\n")
}

func setupN26IBANRouter(db *gorm.DB, userID uint) *gin.Engine {
	r := setupN26Router(db, userID)
	r.PATCH("/invoicing/payments/:id", PatchInvoicingPaymentHandler)
	r.GET("/invoicing/payers/candidates", ListPayerCandidatesHandler)
	r.POST("/invoicing/payers", AddPayersHandler)
	registerPaymentRowActions(r)
	return r
}

// noIBANIn fails when text holds any of the example IBANs, in the stored
// form or in the spaced form an export prints.
func noIBANIn(t *testing.T, what, text string) {
	t.Helper()
	for _, iban := range []string{exampleIBAN, otherExampleIBAN, thirdExampleIBAN, badChecksumIBAN} {
		if strings.Contains(strings.ToUpper(strings.ReplaceAll(text, " ", "")), iban) {
			t.Errorf("%s carries an IBAN (%s…): %s", what, iban[:4], text)
		}
	}
}

func noIBANInAudit(t *testing.T, db *gorm.DB) {
	t.Helper()
	var events []data_models.InvoicingAuditEvent
	if err := db.Order("id asc").Find(&events).Error; err != nil {
		t.Fatalf("audit rows: %v", err)
	}
	if len(events) == 0 {
		t.Fatalf("no audit rows at all: the check below would pass on nothing")
	}
	for _, e := range events {
		noIBANIn(t, fmt.Sprintf("audit row %d (%s %s)", e.ID, e.Action, e.EntityType), e.Details)
	}
}

func loadPayment(t *testing.T, db *gorm.DB, id uint) data_models.InvoicingPayment {
	t.Helper()
	var p data_models.InvoicingPayment
	if err := db.First(&p, id).Error; err != nil {
		t.Fatalf("payment %d: %v", id, err)
	}
	return p
}

func paymentByName(t *testing.T, db *gorm.DB, owner uint, name string) data_models.InvoicingPayment {
	t.Helper()
	var p data_models.InvoicingPayment
	if err := db.Where("owner_user_id = ? AND sender_name = ?", owner, name).First(&p).Error; err != nil {
		t.Fatalf("payment of %s: %v", name, err)
	}
	return p
}

func TestN26IBAN_TheExamplesAreValid(t *testing.T) {
	for _, iban := range []string{exampleIBAN, otherExampleIBAN, thirdExampleIBAN} {
		if !data_models.ValidIBAN(iban) {
			t.Fatalf("%s… fails mod-97: the tests below would test nothing", iban[:4])
		}
	}
	if data_models.ValidIBAN(badChecksumIBAN) {
		t.Fatal("the bad-checksum example passes")
	}
}

func TestN26IBAN_ParseCarriesTheNormalisedIBAN(t *testing.T) {
	in, _, _, err := scanN26CSV(string(n26CSV(
		n26CSVLine("2026-10-02", "Mara Esempio", exampleIBANSpaced, "100.00"),
		n26CSVLine("2026-10-03", "Luca Prova", "", "50.00"),
		n26CSVLine("2026-10-04", "Nora Beispiel", badChecksumIBAN, "70.00"),
	)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(in) != 3 {
		t.Fatalf("candidates: %d, want 3 (a bad IBAN must not drop its row)", len(in))
	}
	if in[0].SenderIBAN != exampleIBAN {
		t.Errorf("Mara's IBAN %q, want the normalised example", in[0].SenderIBAN)
	}
	if in[1].SenderIBAN != "" || in[2].SenderIBAN != "" {
		t.Errorf("an empty or invalid IBAN came through: %q, %q", in[1].SenderIBAN, in[2].SenderIBAN)
	}

	// The PDFs never carry one.
	cands, err := parseN26TransferConfirmation(readN26Fixture(t, "n26_confirmation.txt"))
	if err != nil {
		t.Fatalf("confirmation: %v", err)
	}
	for _, c := range cands {
		if c.SenderIBAN != "" {
			t.Errorf("a PDF candidate has an IBAN: %+v", c)
		}
	}
}

func TestN26IBAN_ConfirmStoresItAndNothingServesIt(t *testing.T) {
	db := setupPaymentsDB(t)
	r := setupN26IBANRouter(db, 7)
	file := n26CSV(
		n26CSVLine("2026-10-02", "Mara Esempio", exampleIBANSpaced, "100.00"),
		n26CSVLine("2026-10-03", "Luca Prova", "", "50.00"),
	)
	w := uploadN26(t, r, map[string][]byte{"export.csv": file})
	if w.Code != http.StatusOK {
		t.Fatalf("parse: %d %s", w.Code, w.Body.String())
	}
	cands := decodeN26Parse(t, w).Files[0].Candidates
	w = confirmN26(t, r, cands)
	if w.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", w.Code, w.Body.String())
	}
	noIBANIn(t, "the confirm response", w.Body.String())

	if got := paymentByName(t, db, 7, "Mara Esempio").SenderIBAN; got != exampleIBAN {
		t.Errorf("stored IBAN %q, want the normalised example", got)
	}
	if got := paymentByName(t, db, 7, "Luca Prova").SenderIBAN; got != "" {
		t.Errorf("a row with no IBAN stored %q", got)
	}

	w = invoicingDo(r, "GET", "/invoicing/payments", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Mara Esempio") {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "sender_iban") {
		t.Errorf("the payments list has a sender_iban key: %s", w.Body.String())
	}
	noIBANIn(t, "the payments list", w.Body.String())
	noIBANInAudit(t, db)

	// A re-upload of the same file is a no-op: nothing is stored again and
	// nothing is backfilled.
	again := decodeN26Parse(t, uploadN26(t, r, map[string][]byte{"export.csv": file})).Files[0].Candidates
	for _, c := range again {
		if !c.AlreadyImported {
			t.Errorf("not read as already imported: %+v", c)
		}
	}
	w = confirmN26(t, r, again)
	var resp struct {
		Summary n26ConfirmSummary `json:"summary"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Summary.Confirmed != 0 || resp.Summary.Duplicates != 2 {
		t.Errorf("re-confirm: %+v", resp.Summary)
	}
}

func TestN26IBAN_ConfirmLinksToTheOwnersClientOnly(t *testing.T) {
	db := setupPaymentsDB(t)
	// Another book's client with the same IBAN comes first by id, so a
	// lookup that forgot the owner would pick it.
	other := data_models.InvoicingClient{OwnerUserID: 8, Name: "Fremde Firma", IBAN: exampleIBAN}
	mine := data_models.InvoicingClient{OwnerUserID: 7, Name: "Mara Esempio", IBAN: exampleIBAN}
	for _, cl := range []*data_models.InvoicingClient{&other, &mine} {
		if err := db.Create(cl).Error; err != nil {
			t.Fatalf("seed client: %v", err)
		}
	}
	file := n26CSV(n26CSVLine("2026-10-02", "M. Esempio", exampleIBAN, "100.00"))

	r := setupN26IBANRouter(db, 7)
	w := confirmN26(t, r, decodeN26Parse(t, uploadN26(t, r, map[string][]byte{"export.csv": file})).Files[0].Candidates)
	var resp struct {
		Summary n26ConfirmSummary `json:"summary"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != http.StatusOK || resp.Summary.Confirmed != 1 || resp.Summary.Linked != 1 {
		t.Fatalf("confirm: %d %s", w.Code, w.Body.String())
	}
	p := paymentByName(t, db, 7, "M. Esempio")
	if p.ClientID == nil || *p.ClientID != mine.ID {
		t.Errorf("linked to %v, want the owner's client %d", p.ClientID, mine.ID)
	}
	if p.SenderIBAN != "" {
		t.Errorf("a linked payment kept its IBAN %q (C4)", p.SenderIBAN)
	}

	// A book with no such client stores the payment unlinked, with its IBAN.
	r9 := setupN26IBANRouter(db, 9)
	w = confirmN26(t, r9, decodeN26Parse(t, uploadN26(t, r9, map[string][]byte{"export.csv": file})).Files[0].Candidates)
	if w.Code != http.StatusOK {
		t.Fatalf("confirm as 9: %d %s", w.Code, w.Body.String())
	}
	p9 := paymentByName(t, db, 9, "M. Esempio")
	if p9.ClientID != nil || p9.SenderIBAN != exampleIBAN {
		t.Errorf("owner 9's payment: client %v, IBAN %q", p9.ClientID, p9.SenderIBAN)
	}
}

func TestN26IBAN_ConfirmRefusesABadOneWithoutRepeatingIt(t *testing.T) {
	db := setupPaymentsDB(t)
	r := setupN26IBANRouter(db, 7)
	cands := decodeN26Parse(t, uploadN26(t, r, map[string][]byte{
		"export.csv": n26CSV(n26CSVLine("2026-10-02", "Mara Esempio", exampleIBAN, "100.00")),
	})).Files[0].Candidates
	if len(cands) != 1 {
		t.Fatalf("candidates: %+v", cands)
	}

	bad := cands[0]
	bad.SenderIBAN = badChecksumIBAN
	w := confirmN26(t, r, []n26Candidate{bad})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "sender_iban is not a valid IBAN") {
		t.Errorf("bad checksum: %d %s", w.Code, w.Body.String())
	}
	noIBANIn(t, "the refusal", w.Body.String())

	pdf := cands[0]
	pdf.DocumentKind = n26KindAccountStatement
	pdf.TransactionID = "stmt-000000000000000000000001"
	w = confirmN26(t, r, []n26Candidate{pdf})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "only on CSV export rows") {
		t.Errorf("IBAN on a PDF row: %d %s", w.Code, w.Body.String())
	}
	noIBANIn(t, "the refusal", w.Body.String())

	var n int
	db.Model(&data_models.InvoicingPayment{}).Count(&n)
	if n != 0 {
		t.Errorf("a refused confirm stored %d payments", n)
	}
}

// seedN26Payment stores one unlinked incoming N26 payment with an IBAN.
func seedN26Payment(t *testing.T, db *gorm.DB, owner uint, name, iban string, day int) uint {
	t.Helper()
	p := data_models.InvoicingPayment{
		OwnerUserID: owner, Source: invoicingPaymentSourceN26, TransactionID: fmt.Sprintf("csv-%s-%d", strings.ReplaceAll(name, " ", ""), day),
		Kind: data_models.PaymentKindIncoming, PaidOn: time.Date(2026, 10, day, 0, 0, 0, 0, time.UTC), Currency: "EUR",
		GrossCents: 10000, NetCents: 10000, SenderName: name, SenderIBAN: iban,
		Category: data_models.PaymentCategoryUncategorised, Entry: data_models.PaymentEntryImport,
	}
	if err := db.Create(&p).Error; err != nil {
		t.Fatalf("seed payment: %v", err)
	}
	return p.ID
}

func TestN26IBAN_AddPayersCarriesItToANewClient(t *testing.T) {
	db := setupPaymentsDB(t)
	r := setupN26IBANRouter(db, 7)
	taken := data_models.InvoicingClient{OwnerUserID: 7, Name: "Alter Kunde", IBAN: thirdExampleIBAN}
	if err := db.Create(&taken).Error; err != nil {
		t.Fatalf("seed client: %v", err)
	}
	mara1 := seedN26Payment(t, db, 7, "Mara Esempio", exampleIBAN, 2)
	mara2 := seedN26Payment(t, db, 7, "Mara Esempio", exampleIBAN, 3)
	luca1 := seedN26Payment(t, db, 7, "Luca Prova", otherExampleIBAN, 4)
	luca2 := seedN26Payment(t, db, 7, "Luca Prova", "", 5)
	nora := seedN26Payment(t, db, 7, "Nora Beispiel", thirdExampleIBAN, 6)

	w := invoicingDo(r, "GET", "/invoicing/payers/candidates", "")
	if w.Code != http.StatusOK {
		t.Fatalf("candidates: %d %s", w.Code, w.Body.String())
	}
	noIBANIn(t, "the payer candidates", w.Body.String())

	body := fmt.Sprintf(`{"payers":[
		{"payment_ids":[%d,%d],"new_client_name":"Mara Esempio"},
		{"payment_ids":[%d,%d],"new_client_name":"Luca Prova"},
		{"payment_ids":[%d],"new_client_name":"Nora Beispiel"}]}`, mara1, mara2, luca1, luca2, nora)
	w = invoicingDo(r, "POST", "/invoicing/payers", body)
	if w.Code != http.StatusOK {
		t.Fatalf("add payers: %d %s", w.Code, w.Body.String())
	}

	clients, err := data_models.ListInvoicingClients(db, 7)
	if err != nil {
		t.Fatalf("clients: %v", err)
	}
	want := map[string]string{
		"Mara Esempio":  exampleIBAN, // every payment carries it
		"Luca Prova":    "",          // two different values: none
		"Nora Beispiel": "",          // another client already has it
		"Alter Kunde":   thirdExampleIBAN,
	}
	for _, cl := range clients {
		if w, ok := want[cl.Name]; !ok || cl.IBAN != w {
			t.Errorf("client %s has IBAN %q, want %q", cl.Name, cl.IBAN, w)
		}
	}
	if len(clients) != len(want) {
		t.Errorf("clients: %d, want %d", len(clients), len(want))
	}
	for _, id := range []uint{mara1, mara2, luca1, luca2, nora} {
		p := loadPayment(t, db, id)
		if p.ClientID == nil {
			t.Errorf("payment %d not linked", id)
		}
		if p.SenderIBAN != "" {
			t.Errorf("payment %d kept its IBAN after the link (C4)", id)
		}
	}
	noIBANInAudit(t, db)
}

func TestN26IBAN_ClearedWhenLinkedByAnEditOrRemoved(t *testing.T) {
	db := setupPaymentsDB(t)
	r := setupN26IBANRouter(db, 7)
	cl := data_models.InvoicingClient{OwnerUserID: 7, Name: "Mara Esempio"}
	if err := db.Create(&cl).Error; err != nil {
		t.Fatalf("seed client: %v", err)
	}

	edited := seedN26Payment(t, db, 7, "Mara Esempio", exampleIBAN, 2)
	w := invoicingDo(r, "PATCH", fmt.Sprintf("/invoicing/payments/%d", edited), fmt.Sprintf(`{"client_id":%d}`, cl.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("link by edit: %d %s", w.Code, w.Body.String())
	}
	noIBANIn(t, "the edit response", w.Body.String())
	if p := loadPayment(t, db, edited); p.ClientID == nil || p.SenderIBAN != "" {
		t.Errorf("after the link: client %v, IBAN kept %v", p.ClientID, p.SenderIBAN != "")
	}

	// A note edit on an unlinked payment leaves the IBAN where it is.
	kept := seedN26Payment(t, db, 7, "Luca Prova", otherExampleIBAN, 3)
	if w := invoicingDo(r, "PATCH", fmt.Sprintf("/invoicing/payments/%d", kept), `{"note":"Oktober"}`); w.Code != http.StatusOK {
		t.Fatalf("note edit: %d %s", w.Code, w.Body.String())
	}
	if p := loadPayment(t, db, kept); p.SenderIBAN != otherExampleIBAN {
		t.Errorf("a note edit changed the IBAN")
	}

	removed := seedN26Payment(t, db, 7, "Nora Beispiel", thirdExampleIBAN, 4)
	if w := invoicingDo(r, "POST", fmt.Sprintf("/invoicing/payments/%d/remove", removed), ""); w.Code != http.StatusOK {
		t.Fatalf("remove: %d %s", w.Code, w.Body.String())
	}
	if p := loadPayment(t, db, removed); p.RemovedAt == nil || p.SenderIBAN != "" {
		t.Errorf("after the remove: removed %v, IBAN kept %v", p.RemovedAt != nil, p.SenderIBAN != "")
	}
	if w := invoicingDo(r, "POST", fmt.Sprintf("/invoicing/payments/%d/restore", removed), ""); w.Code != http.StatusOK {
		t.Fatalf("restore: %d %s", w.Code, w.Body.String())
	}
	if p := loadPayment(t, db, removed); p.RemovedAt != nil || p.SenderIBAN != "" {
		t.Errorf("a restore brought the IBAN back")
	}

	// A purge records the whole row as it was; json:"-" keeps the IBAN out
	// even for a removed row that still had one (removed before this change).
	legacy := seedN26Payment(t, db, 7, "Ines Muster", exampleIBAN, 5)
	if err := db.Model(&data_models.InvoicingPayment{}).Where("id = ?", legacy).Update("removed_at", time.Now().UTC()).Error; err != nil {
		t.Fatalf("mark removed: %v", err)
	}
	if w := invoicingDo(r, "POST", fmt.Sprintf("/invoicing/payments/%d/purge", legacy), ""); w.Code != http.StatusOK {
		t.Fatalf("purge: %d %s", w.Code, w.Body.String())
	}
	var purges int
	db.Model(&data_models.InvoicingAuditEvent{}).Where("action = ? AND entity_id = ?", invoicingActionDelete, legacy).Count(&purges)
	if purges != 1 {
		t.Fatalf("purge audit rows: %d, want 1", purges)
	}
	noIBANInAudit(t, db)
}

func TestN26IBAN_ErrorsAndLogsNeverQuoteIt(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	db := setupPaymentsDB(t)
	db.AutoMigrate(&data_models.InvoicingImportFailure{})
	r := setupN26IBANRouter(db, 7)
	header := n26CSVHeaderRow + "\n"
	files := map[string][]byte{
		// No header row: the first data row is read as the header.
		"no-header.csv": []byte(n26CSVLine("2026-10-02", "Mara Esempio", exampleIBANSpaced, "100.00") + "\n"),
		// Columns shifted by a hand edit: an IBAN where the amount belongs.
		"iban-amount.csv": []byte(header + `"2026-10-02","2026-10-02","Mara Esempio","","Credit Transfer","","Hauptkonto","` + exampleIBANSpaced + `","","",""` + "\n"),
		"iban-date.csv":   []byte(header + `"2026-10-02","` + otherExampleIBAN + `","Mara Esempio","","Credit Transfer","","Hauptkonto","1.00","","",""` + "\n"),
	}
	w := uploadN26(t, r, files)
	if w.Code != http.StatusOK {
		t.Fatalf("parse: %d %s", w.Code, w.Body.String())
	}
	got := decodeN26Parse(t, w).Files
	if len(got) != len(files) {
		t.Fatalf("files: %d", len(got))
	}
	for _, f := range got {
		if f.Error == nil {
			t.Errorf("%s: no error, got %+v", f.Filename, f)
			continue
		}
		if !strings.Contains(f.Error.Message, "a value shaped like an IBAN") {
			t.Errorf("%s: the message does not say what it hid: %q", f.Filename, f.Error.Message)
		}
	}
	noIBANIn(t, "the parse response", w.Body.String())
	if strings.Contains(w.Body.String(), "3704") {
		t.Errorf("a cut IBAN reached the response: %s", w.Body.String())
	}
	noIBANIn(t, "the log", logs.String())

	// Each failure is recorded with its code's fixed message, never the
	// parse error's text.
	var failures []data_models.InvoicingImportFailure
	if err := db.Find(&failures).Error; err != nil {
		t.Fatalf("failure records: %v", err)
	}
	if len(failures) != len(files) {
		t.Errorf("failure records: %d, want %d", len(failures), len(files))
	}
	for _, f := range failures {
		noIBANIn(t, "a failure record", f.ErrorMessage+" "+f.ErrorCode+" "+f.Filename)
	}

	// The PDF parser's amount error goes through the same quoting.
	if _, err := parseN26AmountCents(otherExampleIBAN); err == nil {
		t.Error("an IBAN parsed as an amount")
	} else {
		noIBANIn(t, "the PDF amount error", err.Error())
	}
}

func TestN26IBAN_QuoteKeepsOrdinaryValues(t *testing.T) {
	for in, want := range map[string]string{
		"2026-13-01":   `"2026-13-01"`,
		"12,34":        `"12,34"`,
		"Booking Date": `"Booking Date"`,
		"Rechnung 7":   `"Rechnung 7"`,
	} {
		if got := quoteN26Value(in, 20); got != want {
			t.Errorf("quoteN26Value(%q) = %s, want %s", in, got, want)
		}
	}
	for _, in := range []string{exampleIBAN, exampleIBANSpaced, "Amount " + otherExampleIBAN} {
		if got := quoteN26Value(in, 20); got != "(a value shaped like an IBAN)" {
			t.Errorf("an IBAN was quoted: %s", got)
		}
	}
}

// C7 (c75240): the IBAN is kept for payers only. An outgoing row's
// Partner Iban reaches no expense candidate, as a key or as a value.
func TestN26IBAN_ExpenseCandidatesCarryNone(t *testing.T) {
	db := setupN26ExpensesDB(t)
	r := setupN26ExpensesRouter(db, 7)
	w := uploadN26(t, r, map[string][]byte{"export.csv": n26CSV(
		n26CSVLine("2026-10-02", "Mara Esempio", exampleIBAN, "100.00"),
		n26CSVLine("2026-10-03", "Buero Bedarf", otherExampleIBAN, "-30.00"),
	)})
	if w.Code != http.StatusOK {
		t.Fatalf("parse: %d %s", w.Code, w.Body.String())
	}
	var raw struct {
		Files []struct {
			Candidates        []map[string]interface{} `json:"candidates"`
			ExpenseCandidates []map[string]interface{} `json:"expense_candidates"`
		} `json:"files"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil || len(raw.Files) != 1 {
		t.Fatalf("decode: %v %s", err, w.Body.String())
	}
	f := raw.Files[0]
	if len(f.Candidates) != 1 || f.Candidates[0]["sender_iban"] != exampleIBAN {
		t.Fatalf("the payer row lost its IBAN, so the check below proves nothing: %+v", f.Candidates)
	}
	if len(f.ExpenseCandidates) != 1 {
		t.Fatalf("expense candidates: %+v", f.ExpenseCandidates)
	}
	for k := range f.ExpenseCandidates[0] {
		if strings.Contains(strings.ToLower(k), "iban") {
			t.Errorf("an expense candidate has the key %q", k)
		}
	}
	if strings.Contains(w.Body.String(), otherExampleIBAN) {
		t.Errorf("the payee's IBAN reached the parse response")
	}
}
