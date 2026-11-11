package handlers

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	"github.com/signintech/gopdf"
)

// #9812: N26 PDF import — parse candidates, confirm-each-row, dedupe. The
// fixtures are look-alike documents with invented values; the group gate is
// covered by the ValidateUserOrAdmin gate tests, handler tests inject the
// user like the sibling invoicing tests do.

func readN26Fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return string(b)
}

func setupN26Router(db *gorm.DB, userID uint) *gin.Engine {
	r := setupInvoicingRouter(db, userID)
	r.POST("/invoicing/payments/import-n26/parse", ImportN26ParseHandler)
	r.POST("/invoicing/payments/import-n26/confirm", ImportN26ConfirmHandler)
	r.GET("/invoicing/payments", ListInvoicingPaymentsHandler)
	return r
}

// --- unit: amounts, detection, strategies ---

func TestN26AmountCents(t *testing.T) {
	cases := map[string]int64{
		"1.234,56": 123456,
		"0,00":     0,
		"400,00":   40000,
		"-45,67":   -4567,
	}
	for in, want := range cases {
		got, err := parseN26AmountCents(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got != want {
			t.Errorf("%s: got %d, want %d", in, got, want)
		}
	}
}

func TestN26DetectDocumentKind(t *testing.T) {
	if s, ok := detectN26Document(readN26Fixture(t, "n26_confirmation.txt")); !ok || s.name != n26KindTransferConfirmation {
		t.Fatalf("confirmation fixture: got (%q, %v)", s.name, ok)
	}
	if s, ok := detectN26Document(readN26Fixture(t, "n26_statement.txt")); !ok || s.name != n26KindAccountStatement {
		t.Fatalf("statement fixture: got (%q, %v)", s.name, ok)
	}
	// The wrong-document controls: a PayPal CSV and plain prose match nothing.
	if _, ok := detectN26Document("Date,Name,Type,Status,Currency,Gross\n01/07/2026,Someone,Payment,Completed,EUR,100.00"); ok {
		t.Fatal("a PayPal CSV must not detect as an N26 document")
	}
	if _, ok := detectN26Document("An unrelated letter about something else entirely."); ok {
		t.Fatal("prose must not detect as an N26 document")
	}
}

func TestN26ParseTransferConfirmation(t *testing.T) {
	cands, err := parseN26TransferConfirmation(readN26Fixture(t, "n26_confirmation.txt"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1", len(cands))
	}
	c := cands[0]
	if c.TransactionID != "a1b2c3d4-0e15-4a6f-9bd0-123456789abc" {
		t.Errorf("transaction id: %q", c.TransactionID)
	}
	if c.PaidOn != "2026-07-14" { // Data valuta, the left date
		t.Errorf("paid_on: %q", c.PaidOn)
	}
	if c.GrossCents != 123456 || c.FeeCents != 0 || c.NetCents != 123456 {
		t.Errorf("amounts: gross %d fee %d net %d", c.GrossCents, c.FeeCents, c.NetCents)
	}
	if c.SenderName != "Mara Esempio" {
		t.Errorf("sender: %q", c.SenderName)
	}
	if c.Note != "Illustrazione copertina album" {
		t.Errorf("note: %q", c.Note)
	}
	if c.Currency != "EUR" || c.DocumentKind != n26KindTransferConfirmation {
		t.Errorf("currency %q kind %q", c.Currency, c.DocumentKind)
	}
}

func TestN26ParseTransferConfirmationMalformed(t *testing.T) {
	full := readN26Fixture(t, "n26_confirmation.txt")
	for name, text := range map[string]string{
		"no amount": strings.Replace(full, "1.234,56 EUR", "", 1),
		"no uuid":   strings.Replace(full, "a1b2c3d4-0e15-4a6f-9bd0-123456789abc", "not-an-id", 1),
		"no sender": strings.Replace(full, "DETTAGLI MITTENTE", "QUALCOS'ALTRO", 1),
		// Security review F2 (c60279): with the name line gone, the block's
		// first content is the IBAN value — the parser must refuse, never
		// store the IBAN as the sender.
		"name missing": strings.Replace(full, "Mara Esempio\n", "", 1),
		"fee >= gross": strings.Replace(full, "0,00 EUR", "2.000,00 EUR", 1),
	} {
		if _, err := parseN26TransferConfirmation(text); err == nil {
			t.Errorf("%s: parsed without error", name)
		}
	}
}

func TestN26ParseStatement(t *testing.T) {
	text := readN26Fixture(t, "n26_statement.txt")
	cands, err := parseN26AccountStatement(text)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("got %d candidates, want 2 (money out is skipped)", len(cands))
	}
	first, second := cands[0], cands[1]
	if first.PaidOn != "2026-08-01" || first.GrossCents != 25000 || first.SenderName != "Mara Esempio" {
		t.Errorf("first row: %+v", first)
	}
	if first.Note != "Bonifico ricevuto Illustrazione logo" {
		t.Errorf("first note: %q", first.Note)
	}
	if second.PaidOn != "2026-08-05" || second.GrossCents != 100000 || second.SenderName != "Cliente Due SRL" {
		t.Errorf("second row: %+v", second)
	}
	for _, c := range cands {
		if !strings.HasPrefix(c.TransactionID, "stmt-") {
			t.Errorf("synthesized id: %q", c.TransactionID)
		}
	}
	if first.TransactionID == second.TransactionID {
		t.Error("distinct rows share a transaction id")
	}
	// Determinism is the dedupe: the same file parses to the same ids.
	again, err := parseN26AccountStatement(text)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	for i := range cands {
		if cands[i].TransactionID != again[i].TransactionID {
			t.Errorf("row %d id not stable: %q vs %q", i, cands[i].TransactionID, again[i].TransactionID)
		}
	}
}

// Two genuinely separate but field-identical rows in one statement are two
// payments, not a duplicate — and stay deterministic across a re-parse
// (security review F1, c60279).
func TestN26ParseStatementIdenticalRows(t *testing.T) {
	block := "01.08.2026    Mara Esempio                                          +250,00 EUR\n" +
		"              Bonifico ricevuto Illustrazione logo\n"
	text := readN26Fixture(t, "n26_statement.txt")
	if !strings.Contains(text, block) {
		t.Fatal("fixture drifted: the doubled block is not in it verbatim")
	}
	doubled := strings.Replace(text, block, block+block, 1)
	cands, err := parseN26AccountStatement(doubled)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ids := map[string]int{}
	for _, c := range cands {
		ids[c.TransactionID]++
	}
	if len(cands) != 3 || len(ids) != 3 {
		t.Fatalf("got %d candidates with %d distinct ids, want 3 and 3: %+v", len(cands), len(ids), cands)
	}
	if cands[0].Note != cands[1].Note || cands[0].GrossCents != cands[1].GrossCents {
		t.Fatalf("the two copies must be field-identical for this test to bite: %+v vs %+v", cands[0], cands[1])
	}
	again, _ := parseN26AccountStatement(doubled)
	for i := range cands {
		if cands[i].TransactionID != again[i].TransactionID {
			t.Errorf("row %d id not stable: %q vs %q", i, cands[i].TransactionID, again[i].TransactionID)
		}
	}
}

func TestN26FlagCandidates(t *testing.T) {
	db := setupPaymentsDB(t)
	paidOn, _ := time.Parse("2006-01-02", "2026-07-14")
	if err := db.Create(&data_models.InvoicingPayment{
		OwnerUserID: 5, Source: "manual", TransactionID: "M-1", Kind: data_models.PaymentKindIncoming,
		PaidOn: paidOn, Currency: "EUR", GrossCents: 123456, NetCents: 123456,
		Category: data_models.PaymentCategoryUncategorised, Entry: data_models.PaymentEntryManual,
	}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	cands := []n26Candidate{
		{TransactionID: "a1b2c3d4-0e15-4a6f-9bd0-123456789abc", PaidOn: "2026-07-14", GrossCents: 123456},
		{TransactionID: "stmt-0000", PaidOn: "2026-09-01", GrossCents: 555},
	}
	// Another book's payment is never named as a twin (system_3 #10054).
	other := []n26Candidate{{TransactionID: "a1b2c3d4-0e15-4a6f-9bd0-123456789abc", PaidOn: "2026-07-14", GrossCents: 123456}}
	if err := flagN26Candidates(db, 6, other); err != nil {
		t.Fatalf("flag as another owner: %v", err)
	}
	if other[0].PossibleDuplicateOf != 0 {
		t.Errorf("another owner's candidate names payment %d as its twin", other[0].PossibleDuplicateOf)
	}
	if err := flagN26Candidates(db, 5, cands); err != nil {
		t.Fatalf("flag: %v", err)
	}
	if cands[0].PossibleDuplicateOf == 0 {
		t.Error("date+amount twin not flagged")
	}
	if cands[1].PossibleDuplicateOf != 0 || cands[1].AlreadyImported {
		t.Errorf("clean candidate flagged: %+v", cands[1])
	}
}

// --- handlers ---

type n26ParseResponse struct {
	Files []n26ParsedFile `json:"files"`
}

func uploadN26(t *testing.T, r *gin.Engine, files map[string][]byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, content := range files {
		fw, err := mw.CreateFormFile("files", name)
		if err != nil {
			t.Fatalf("form file: %v", err)
		}
		fw.Write(content)
	}
	mw.Close()
	req, _ := http.NewRequest("POST", "/invoicing/payments/import-n26/parse", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decodeN26Parse(t *testing.T, w *httptest.ResponseRecorder) n26ParseResponse {
	t.Helper()
	var resp n26ParseResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	return resp
}

func confirmN26(t *testing.T, r *gin.Engine, rows []n26Candidate) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(n26ConfirmRequest{Rows: rows})
	return invoicingDo(r, "POST", "/invoicing/payments/import-n26/confirm", string(body))
}

// buildN26LookalikePDF renders the confirmation fixture's lines into a real
// PDF with the repo's own font, so the pipeline test exercises genuine
// pdftotext extraction. Library-written bytes, never pasted ones.
func buildN26LookalikePDF(t *testing.T) []byte {
	t.Helper()
	return buildN26PDFFromText(t, readN26Fixture(t, "n26_confirmation.txt"))
}

// buildN26PDFFromText renders the given lines the same way, for a variant of
// the fixture.
func buildN26PDFFromText(t *testing.T, text string) []byte {
	t.Helper()
	pdf := gopdf.GoPdf{}
	pdf.Start(gopdf.Config{PageSize: *gopdf.PageSizeA4})
	pdf.AddPage()
	if err := pdf.AddTTFFont("regular", filepath.Join("..", "invoicing_pdf", "fonts", "LiberationSans-Regular.ttf")); err != nil {
		t.Fatalf("font: %v", err)
	}
	if err := pdf.SetFont("regular", "", 10); err != nil {
		t.Fatalf("set font: %v", err)
	}
	y := 40.0
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			pdf.SetXY(40, y)
			if err := pdf.Cell(nil, line); err != nil {
				t.Fatalf("cell: %v", err)
			}
		}
		y += 13
		if y > 800 {
			pdf.AddPage()
			y = 40
		}
	}
	return pdf.GetBytesPdf()
}

func requirePdftotext(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Fatalf("pdftotext is not installed; the N26 pipeline tests need poppler-utils (the verify surface installs it)")
	}
}

func TestN26ImportPipeline(t *testing.T) {
	requirePdftotext(t)
	db := setupPaymentsDB(t)
	r := setupN26Router(db, 7)

	pdf := buildN26LookalikePDF(t)
	w := uploadN26(t, r, map[string][]byte{"conferma.pdf": pdf})
	if w.Code != http.StatusOK {
		t.Fatalf("parse: %d %s", w.Code, w.Body.String())
	}
	resp := decodeN26Parse(t, w)
	if len(resp.Files) != 1 {
		t.Fatalf("files: %+v", resp.Files)
	}
	fe := resp.Files[0]
	if fe.Error != nil {
		t.Fatalf("file error: %+v", fe.Error)
	}
	if fe.DocumentKind != n26KindTransferConfirmation || len(fe.Candidates) != 1 {
		t.Fatalf("kind %q candidates %d", fe.DocumentKind, len(fe.Candidates))
	}
	cand := fe.Candidates[0]
	if cand.TransactionID != "a1b2c3d4-0e15-4a6f-9bd0-123456789abc" || cand.GrossCents != 123456 ||
		cand.PaidOn != "2026-07-14" || cand.SenderName != "Mara Esempio" {
		t.Fatalf("candidate through the real pipeline: %+v", cand)
	}
	if cand.AlreadyImported {
		t.Fatal("nothing is stored yet")
	}

	// Parsing stores nothing.
	var n int
	db.Model(&data_models.InvoicingPayment{}).Count(&n)
	if n != 0 {
		t.Fatalf("parse persisted %d payments", n)
	}

	// Confirm stores the row once.
	w = confirmN26(t, r, fe.Candidates)
	if w.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", w.Code, w.Body.String())
	}
	var cresp struct {
		Summary n26ConfirmSummary `json:"summary"`
	}
	json.Unmarshal(w.Body.Bytes(), &cresp)
	if cresp.Summary.Confirmed != 1 || cresp.Summary.Duplicates != 0 {
		t.Fatalf("summary: %+v", cresp.Summary)
	}
	payments := listPayments(t, r, "")
	if len(payments) != 1 {
		t.Fatalf("payments: %d", len(payments))
	}
	p := payments[0]
	if p.Source != invoicingPaymentSourceN26 || p.Kind != data_models.PaymentKindIncoming ||
		p.Category != data_models.PaymentCategoryUncategorised || p.Entry != data_models.PaymentEntryImport {
		t.Fatalf("stored payment: %+v", p)
	}

	// Re-parse flags the stored row; re-confirm dedupes.
	w = uploadN26(t, r, map[string][]byte{"conferma.pdf": pdf})
	resp = decodeN26Parse(t, w)
	if !resp.Files[0].Candidates[0].AlreadyImported {
		t.Fatal("already_imported not flagged on re-parse")
	}
	w = confirmN26(t, r, resp.Files[0].Candidates)
	json.Unmarshal(w.Body.Bytes(), &cresp)
	if cresp.Summary.Confirmed != 0 || cresp.Summary.Duplicates != 1 {
		t.Fatalf("re-confirm summary: %+v", cresp.Summary)
	}
	db.Model(&data_models.InvoicingPayment{}).Count(&n)
	if n != 1 {
		t.Fatalf("re-confirm stored %d payments", n)
	}

	// The confirm is audit-logged.
	var events int
	db.Model(&data_models.InvoicingAuditEvent{}).Where("action = ? AND details LIKE ?", invoicingActionImport, "%n26%").Count(&events)
	if events == 0 {
		t.Fatal("no audit event for the confirm")
	}
}

func TestN26ParsePerFileErrors(t *testing.T) {
	db := setupPaymentsDB(t)
	r := setupN26Router(db, 7)

	w := uploadN26(t, r, map[string][]byte{
		"not-a-pdf.txt": []byte("just text"),
		"too-big.pdf":   append([]byte("%PDF-1.7 "), make([]byte, maxN26PDFBytes+1)...),
	})
	if w.Code != http.StatusOK {
		t.Fatalf("parse: %d %s", w.Code, w.Body.String())
	}
	resp := decodeN26Parse(t, w)
	codes := map[string]string{}
	for _, f := range resp.Files {
		if f.Error == nil {
			t.Fatalf("%s: expected a per-file error", f.Filename)
		}
		codes[f.Filename] = f.Error.Code
	}
	if codes["not-a-pdf.txt"] != n26ErrNotAPDF {
		t.Errorf("not-a-pdf.txt: %q", codes["not-a-pdf.txt"])
	}
	if codes["too-big.pdf"] != n26ErrTooLarge {
		t.Errorf("too-big.pdf: %q", codes["too-big.pdf"])
	}
}

func TestN26ParseNoFiles(t *testing.T) {
	db := setupPaymentsDB(t)
	r := setupN26Router(db, 7)
	w := invoicingDo(r, "POST", "/invoicing/payments/import-n26/parse", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("no files: %d", w.Code)
	}
}

func TestN26ConfirmValidation(t *testing.T) {
	db := setupPaymentsDB(t)
	r := setupN26Router(db, 7)

	good := n26Candidate{
		DocumentKind: n26KindTransferConfirmation, TransactionID: "a1b2c3d4-0e15-4a6f-9bd0-123456789abc",
		PaidOn: "2026-07-14", Currency: "EUR", GrossCents: 1000, FeeCents: 0, SenderName: "Mara Esempio",
	}
	bad := []func(c *n26Candidate){
		func(c *n26Candidate) { c.DocumentKind = "paypal" },
		func(c *n26Candidate) { c.TransactionID = "" },
		func(c *n26Candidate) { c.PaidOn = "2030-01-01" },
		func(c *n26Candidate) { c.PaidOn = "14.07.2026" },
		func(c *n26Candidate) { c.Currency = "USD" },
		func(c *n26Candidate) { c.GrossCents = 0 },
		func(c *n26Candidate) { c.FeeCents = 1000 },
		func(c *n26Candidate) { c.SenderName = "" },
		// #9881: the parse never offers a PayPal transfer; a client that
		// posts one back anyway is refused.
		func(c *n26Candidate) { c.SenderName = "PAYPAL" },
	}
	for i, mutate := range bad {
		row := good
		mutate(&row)
		w := confirmN26(t, r, []n26Candidate{row})
		if w.Code != http.StatusBadRequest {
			t.Errorf("bad row %d: got %d (%s)", i, w.Code, w.Body.String())
		}
	}
	// Security review N3 (c60279): the batch is bounded.
	many := make([]n26Candidate, maxN26ConfirmRows+1)
	for i := range many {
		many[i] = good
	}
	if w := confirmN26(t, r, many); w.Code != http.StatusBadRequest {
		t.Errorf("oversize batch: got %d", w.Code)
	}

	// The unmutated row is the positive control for the shapes above.
	if w := confirmN26(t, r, []n26Candidate{good}); w.Code != http.StatusOK {
		t.Fatalf("good row: %d %s", w.Code, w.Body.String())
	}
}

// --- system_3 #9881: a credit whose payer is PayPal is her own withdrawal ---

func TestN26PayPalSender(t *testing.T) {
	for name, want := range map[string]bool{
		"PAYPAL":                              true, // her real confirmations' sender line
		"PayPal Europe S.a.r.l. et Cie S.C.A": true, // the company name a statement prints
		"paypal":                              true,
		"Mara Esempio":                        false,
		"Cliente Due SRL":                     false,
		"":                                    false,
	} {
		if got := isPayPalSender(name); got != want {
			t.Errorf("isPayPalSender(%q) = %v, want %v", name, got, want)
		}
	}
}

// A statement keeps its other rows and their ids, drops the PayPal row with
// a count, and keeps a customer's row whose note merely mentions PayPal.
func TestN26DropPayPalTransfers(t *testing.T) {
	text := readN26Fixture(t, "n26_statement.txt")
	plain, err := parseN26AccountStatement(text)
	if err != nil || len(plain) != 2 {
		t.Fatalf("fixture parse: %d rows, %v", len(plain), err)
	}
	const company = "PayPal Europe S.a.r.l. et Cie S.C.A"
	const mention = "Bonifico ricevuto Illustrazione logo, saldo via PayPal"
	mixed := strings.Replace(text, "Cliente Due SRL                    ", company, 1)
	mixed = strings.Replace(mixed, "Bonifico ricevuto Illustrazione logo", mention, 1)
	if mixed == text || !strings.Contains(mixed, company) || !strings.Contains(mixed, mention) {
		t.Fatal("fixture drifted: the replacements did not apply")
	}
	all, err := parseN26AccountStatement(mixed)
	if err != nil || len(all) != 2 || all[1].SenderName != company {
		t.Fatalf("mixed parse must still read both rows before the filter: %+v, %v", all, err)
	}
	kept, skipped := dropPayPalTransfers(all)
	if len(kept) != 1 || kept[0].SenderName != "Mara Esempio" || kept[0].Note != mention {
		t.Fatalf("kept: %+v", kept)
	}
	if skipped[n26SkipPayPalTransfer] != 1 || len(skipped) != 1 {
		t.Fatalf("skipped: %+v", skipped)
	}
	if kept[0].TransactionID != all[0].TransactionID {
		t.Error("a kept row's id changed when a sibling row was dropped")
	}
	// Positive control: nothing to drop leaves the rows and no skipped map.
	if k, s := dropPayPalTransfers(plain); len(k) != 2 || s != nil {
		t.Fatalf("clean statement: kept %d, skipped %+v", len(k), s)
	}
}

// Through the real pipeline: one ordinary confirmation and one whose sender
// is PAYPAL, as in her files. The ordinary one is offered; the PayPal one is
// counted under its file and offered nowhere.
func TestN26ImportPipelineLeavesOutPayPalTransfers(t *testing.T) {
	requirePdftotext(t)
	db := setupPaymentsDB(t)
	r := setupN26Router(db, 7)

	fixture := readN26Fixture(t, "n26_confirmation.txt")
	paypalText := strings.Replace(fixture, "Mara Esempio\n", "PAYPAL\n", 1)
	paypalText = strings.Replace(paypalText, "a1b2c3d4-0e15-4a6f-9bd0-123456789abc", "f0e1d2c3-0b0a-4999-8888-777766665555", 1)
	if paypalText == fixture || !strings.Contains(paypalText, "PAYPAL\n") {
		t.Fatal("fixture drifted: the sender replacement did not apply")
	}
	w := uploadN26(t, r, map[string][]byte{
		"conferma.pdf":        buildN26LookalikePDF(t),
		"conferma-paypal.pdf": buildN26PDFFromText(t, paypalText),
	})
	if w.Code != http.StatusOK {
		t.Fatalf("parse: %d %s", w.Code, w.Body.String())
	}
	byName := map[string]n26ParsedFile{}
	for _, f := range decodeN26Parse(t, w).Files {
		byName[f.Filename] = f
	}
	ordinary, pp := byName["conferma.pdf"], byName["conferma-paypal.pdf"]
	if ordinary.Error != nil || len(ordinary.Candidates) != 1 || ordinary.Skipped != nil {
		t.Fatalf("ordinary file: %+v", ordinary)
	}
	if pp.Error != nil || pp.DocumentKind != n26KindTransferConfirmation || len(pp.Candidates) != 0 {
		t.Fatalf("PayPal file must read cleanly and offer nothing: %+v", pp)
	}
	if pp.Skipped[n26SkipPayPalTransfer] != 1 {
		t.Fatalf("PayPal file skipped: %+v", pp.Skipped)
	}
	// The wire shape the client reads.
	if !strings.Contains(w.Body.String(), `"skipped":{"paypal_transfer":1}`) {
		t.Errorf("response lacks the skipped count: %s", w.Body.String())
	}
}

// A PayPal transfer stored before the import rule cannot become paid work,
// can move to any other category, and cannot go on a draft; an ordinary N26
// row and the PayPal import's own rows stay invoiceable.
func TestPayPalTransferIntoN26_NeverPaidWorkNeverInvoiced(t *testing.T) {
	f := newInvoiceFixture(t)
	seed := func(txn, sender, category string) data_models.InvoicingPayment {
		t.Helper()
		cid := f.clientID
		p := data_models.InvoicingPayment{
			OwnerUserID: invoiceFixtureUser,
			Source:      invoicingPaymentSourceN26, TransactionID: txn, Kind: data_models.PaymentKindIncoming,
			PaidOn: time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC), Currency: "EUR", GrossCents: 50000, NetCents: 50000,
			SenderName: sender, Category: category, Entry: data_models.PaymentEntryImport, ClientID: &cid,
		}
		if err := f.db.Create(&p).Error; err != nil {
			t.Fatalf("seed %s: %v", txn, err)
		}
		return p
	}
	fresh := seed("stmt-fresh", "PayPal Europe S.a.r.l. et Cie S.C.A", data_models.PaymentCategoryUncategorised)
	stored := seed("stmt-stored", "PAYPAL", data_models.PaymentCategoryPaidWork)
	ordinary := seed("stmt-ordinary", "Mara Esempio", data_models.PaymentCategoryPaidWork)
	path := func(p data_models.InvoicingPayment) string { return fmt.Sprintf("/invoicing/payments/%d", p.ID) }
	before := countAuditEvents(t, f.db)

	w := invoicingDo(f.r, "PATCH", path(fresh), `{"category":"paid_work"}`)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "paypal_transfer_not_paid_work") {
		t.Fatalf("paid_work on a PayPal transfer: %d %s", w.Code, w.Body.String())
	}
	if got, _ := data_models.GetInvoicingPayment(f.db, invoiceFixtureUser, fresh.ID); got.Category != data_models.PaymentCategoryUncategorised {
		t.Fatalf("refused patch changed the category: %q", got.Category)
	}
	if after := countAuditEvents(t, f.db); after != before {
		t.Fatalf("refused patch wrote %d audit rows", after-before)
	}

	w = invoicingDo(f.r, "POST", "/invoicing/invoices", fmt.Sprintf(`{"payment_ids":[%d]}`, stored.ID))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "payment_paypal_transfer") {
		t.Fatalf("draft over a stored PayPal transfer: %d %s", w.Code, w.Body.String())
	}

	// The way off paid work stays open, for the row stored before the rule.
	if w := invoicingDo(f.r, "PATCH", path(stored), `{"category":"other"}`); w.Code != http.StatusOK {
		t.Fatalf("move the stored row to other: %d %s", w.Code, w.Body.String())
	}
	if w := invoicingDo(f.r, "PATCH", path(fresh), `{"category":"gift"}`); w.Code != http.StatusOK {
		t.Fatalf("another category on a PayPal transfer: %d %s", w.Code, w.Body.String())
	}

	// Controls: an ordinary N26 row and a PayPal-import row draft as before.
	w = invoicingDo(f.r, "POST", "/invoicing/invoices", fmt.Sprintf(`{"payment_ids":[%d,%d]}`, ordinary.ID, f.payments[0].ID))
	if w.Code != http.StatusCreated {
		t.Fatalf("control draft: %d %s", w.Code, w.Body.String())
	}
}

// --- #10414-B: outgoing statement rows become expense candidates ---

func setupN26ExpensesRouter(db *gorm.DB, userID uint) *gin.Engine {
	r := setupN26Router(db, userID)
	r.POST("/invoicing/expenses/import-n26/confirm", ImportN26ExpensesConfirmHandler)
	r.GET("/invoicing/expenses", ListInvoicingExpensesHandler)
	r.POST("/invoicing/expense-categories", CreateExpenseCategoryHandler)
	return r
}

func setupN26ExpensesDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := setupPaymentsDB(t)
	db.AutoMigrate(&data_models.InvoicingExpense{}, &data_models.InvoicingExpenseCategory{})
	return db
}

func confirmN26Expenses(t *testing.T, r *gin.Engine, rows []n26ExpenseConfirmRow) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(n26ExpenseConfirmRequest{Rows: rows})
	return invoicingDo(r, "POST", "/invoicing/expenses/import-n26/confirm", string(body))
}

func asExpenseConfirmRows(cands []n26ExpenseCandidate, category string) []n26ExpenseConfirmRow {
	out := make([]n26ExpenseConfirmRow, 0, len(cands))
	for _, c := range cands {
		out = append(out, n26ExpenseConfirmRow{
			TransactionID: c.TransactionID, PaidOn: c.PaidOn, Currency: c.Currency,
			AmountCents: c.AmountCents, PayeeName: c.PayeeName, Note: c.Note, Category: category,
		})
	}
	return out
}

// The fixture's one outgoing row is now an expense candidate, and the incoming
// rows are exactly what they were: same fields, and the same ids the
// pre-#10414-B key produced, so a statement imported before this change
// still dedupes on re-upload.
func TestN26StatementOutgoingRowsBecomeExpenseCandidates(t *testing.T) {
	text := readN26Fixture(t, "n26_statement.txt")
	expenses, err := parseN26StatementExpenses(text)
	if err != nil {
		t.Fatalf("parse expenses: %v", err)
	}
	if len(expenses) != 1 {
		t.Fatalf("got %d expense candidates, want 1: %+v", len(expenses), expenses)
	}
	e := expenses[0]
	if e.PaidOn != "2026-08-03" || e.AmountCents != 4567 || e.PayeeName != "Supermercato Esempio" ||
		e.Currency != "EUR" || e.Note != "" || !strings.HasPrefix(e.TransactionID, "stmt-") {
		t.Errorf("expense candidate: %+v", e)
	}

	cands, err := parseN26AccountStatement(text)
	if err != nil {
		t.Fatalf("parse payments: %v", err)
	}
	legacyID := func(paidOn string, cents int64, sender, note string, occurrence int) string {
		key := paidOn + "|" + fmt.Sprint(cents) + "|" + sender + "|" + note
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d", key, occurrence)))
		return "stmt-" + hex.EncodeToString(sum[:])[:24]
	}
	want := []string{
		legacyID("2026-08-01", 25000, "Mara Esempio", "Bonifico ricevuto Illustrazione logo", 0),
		legacyID("2026-08-05", 100000, "Cliente Due SRL", "Acconto progetto murale", 0),
	}
	if len(cands) != 2 {
		t.Fatalf("payment candidates: %d", len(cands))
	}
	for i := range cands {
		if cands[i].TransactionID != want[i] {
			t.Errorf("payment %d id changed: %q, want %q (re-uploads would stop deduping)", i, cands[i].TransactionID, want[i])
		}
		if cands[i].TransactionID == e.TransactionID {
			t.Errorf("payment %d shares the expense's id", i)
		}
	}

	again, _ := parseN26StatementExpenses(text)
	if again[0].TransactionID != e.TransactionID {
		t.Errorf("expense id not stable: %q vs %q", e.TransactionID, again[0].TransactionID)
	}
}

// Mixed directions with continuation lines: each continuation extends the row
// directly above it, whichever direction that row is, and never leaks across
// a direction change or a blank line. A zero-amount row is neither, and its
// continuation goes with it.
func TestN26StatementContinuationLinesFollowEitherDirection(t *testing.T) {
	text := "Estratto conto\n\n" +
		"Data          Descrizione                                           Importo\n\n" +
		"01.08.2026    Mara Esempio                                          +250,00 EUR\n" +
		"              Bonifico ricevuto Illustrazione logo\n" +
		"02.08.2026    Colorificio Esempio                                    -89,90 EUR\n" +
		"              Pigmenti e tele\n" +
		"              ordine 4411\n" +
		"03.08.2026    Cliente Due SRL                                       1.000,00 EUR\n" +
		"              Acconto progetto murale\n" +
		"04.08.2026    Rettifica                                                0,00 EUR\n" +
		"              nota della rettifica\n" +
		"05.08.2026    Tipografia Esempio                                    -120,00 EUR\n\n" +
		"              riga staccata dopo una riga vuota\n"
	in, out, skipped, err := scanN26Statement(text)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if skipped != 0 {
		t.Errorf("the Italian layout has no internal-transfer skip concept: got %d", skipped)
	}
	if len(in) != 2 || len(out) != 2 {
		t.Fatalf("got %d in, %d out, want 2 and 2: %+v %+v", len(in), len(out), in, out)
	}
	if in[0].Note != "Bonifico ricevuto Illustrazione logo" || in[1].Note != "Acconto progetto murale" {
		t.Errorf("incoming notes: %q, %q", in[0].Note, in[1].Note)
	}
	if out[0].PayeeName != "Colorificio Esempio" || out[0].AmountCents != 8990 || out[0].Note != "Pigmenti e tele ordine 4411" {
		t.Errorf("first expense (two continuation lines): %+v", out[0])
	}
	if out[1].PayeeName != "Tipografia Esempio" || out[1].AmountCents != 12000 || out[1].Note != "" {
		t.Errorf("second expense (a blank line ends it): %+v", out[1])
	}
	for _, c := range in {
		if strings.Contains(c.Note, "rettifica") || strings.Contains(c.Note, "Pigmenti") {
			t.Errorf("a note leaked onto a payment: %+v", c)
		}
	}
}

func TestN26FlagExpenseCandidates(t *testing.T) {
	db := setupN26ExpensesDB(t)
	paidOn, _ := time.Parse("2006-01-02", "2026-08-03")
	for _, e := range []data_models.InvoicingExpense{
		{OwnerUserID: 5, Source: "manual", TransactionID: "M-1", PaidOn: paidOn, Currency: "EUR", AmountCents: 4567, Category: "other", Entry: "manual"},
		{OwnerUserID: 5, Source: invoicingPaymentSourceN26, TransactionID: "stmt-seen", PaidOn: paidOn, Currency: "EUR", AmountCents: 1, Category: "other", Entry: "import"},
	} {
		if err := db.Create(&e).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	cands := []n26ExpenseCandidate{
		{TransactionID: "stmt-new", PaidOn: "2026-08-03", AmountCents: 4567},
		{TransactionID: "stmt-seen", PaidOn: "2026-08-03", AmountCents: 1},
		{TransactionID: "stmt-clean", PaidOn: "2026-09-01", AmountCents: 555},
	}
	other := []n26ExpenseCandidate{{TransactionID: "stmt-seen", PaidOn: "2026-08-03", AmountCents: 4567}}
	if err := flagN26ExpenseCandidates(db, 6, other); err != nil {
		t.Fatalf("flag as another owner: %v", err)
	}
	if other[0].AlreadyImported || other[0].PossibleDuplicateOf != 0 {
		t.Errorf("another owner's expenses leaked into the flags: %+v", other[0])
	}
	if err := flagN26ExpenseCandidates(db, 5, cands); err != nil {
		t.Fatalf("flag: %v", err)
	}
	if cands[0].PossibleDuplicateOf == 0 || cands[0].AlreadyImported {
		t.Errorf("date+amount twin of a hand-entered expense: %+v", cands[0])
	}
	if !cands[1].AlreadyImported {
		t.Errorf("already-imported row: %+v", cands[1])
	}
	if cands[2].AlreadyImported || cands[2].PossibleDuplicateOf != 0 {
		t.Errorf("clean candidate flagged: %+v", cands[2])
	}
}

// Confirm stores the owner's chosen outgoing rows as imported expenses, once,
// in her own book, and never as payments.
func TestN26ExpensesConfirm(t *testing.T) {
	db := setupN26ExpensesDB(t)
	r := setupN26ExpensesRouter(db, 7)
	text := readN26Fixture(t, "n26_statement.txt")
	cands, err := parseN26StatementExpenses(text)
	if err != nil || len(cands) != 1 {
		t.Fatalf("parse: %v %+v", err, cands)
	}

	w := confirmN26Expenses(t, r, asExpenseConfirmRows(cands, "materials and supplies"))
	if w.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", w.Code, w.Body.String())
	}
	var cresp struct {
		Summary n26ConfirmSummary `json:"summary"`
	}
	json.Unmarshal(w.Body.Bytes(), &cresp)
	if cresp.Summary.Confirmed != 1 || cresp.Summary.Duplicates != 0 {
		t.Fatalf("summary: %+v", cresp.Summary)
	}
	var stored []data_models.InvoicingExpense
	db.Where("owner_user_id = ?", 7).Find(&stored)
	if len(stored) != 1 {
		t.Fatalf("stored %d expenses", len(stored))
	}
	e := stored[0]
	if e.Source != invoicingPaymentSourceN26 || e.Entry != data_models.ExpenseEntryImport || e.AmountCents != 4567 ||
		e.Category != "materials and supplies" || e.Reference != "Supermercato Esempio" || e.TransactionID != cands[0].TransactionID ||
		e.PaidOn.Format("2006-01-02") != "2026-08-03" {
		t.Fatalf("stored expense: %+v", e)
	}
	var payments int
	db.Model(&data_models.InvoicingPayment{}).Count(&payments)
	if payments != 0 {
		t.Fatalf("an expense confirm stored %d payments", payments)
	}

	// Re-confirm dedupes; the re-flag sees it as imported.
	w = confirmN26Expenses(t, r, asExpenseConfirmRows(cands, ""))
	json.Unmarshal(w.Body.Bytes(), &cresp)
	if cresp.Summary.Confirmed != 0 || cresp.Summary.Duplicates != 1 {
		t.Fatalf("re-confirm summary: %+v", cresp.Summary)
	}
	if err := flagN26ExpenseCandidates(db, 7, cands); err != nil || !cands[0].AlreadyImported {
		t.Fatalf("re-flag: %v %+v", err, cands[0])
	}

	// Another owner confirming the same row stores it in her own book.
	r8 := setupN26ExpensesRouter(db, 8)
	w = confirmN26Expenses(t, r8, asExpenseConfirmRows(cands, ""))
	json.Unmarshal(w.Body.Bytes(), &cresp)
	if w.Code != http.StatusOK || cresp.Summary.Confirmed != 1 {
		t.Fatalf("another owner's confirm: %d %+v", w.Code, cresp.Summary)
	}
	var theirs data_models.InvoicingExpense
	db.Where("owner_user_id = ?", 8).First(&theirs)
	if theirs.Category != data_models.ExpenseCategoryOther {
		t.Errorf("default category: %q", theirs.Category)
	}

	// The confirm is audit-logged.
	var events int
	db.Model(&data_models.InvoicingAuditEvent{}).Where("action = ? AND entity_type = ?", invoicingActionImport, invoicingEntityExpense).Count(&events)
	if events == 0 {
		t.Fatal("no audit event for the expense confirm")
	}
}

func TestN26ExpensesConfirmValidation(t *testing.T) {
	db := setupN26ExpensesDB(t)
	r := setupN26ExpensesRouter(db, 7)
	// Owner 8's own category must not validate for owner 7.
	r8 := setupN26ExpensesRouter(db, 8)
	if w := invoicingDo(r8, "POST", "/invoicing/expense-categories", `{"name":"pigments","counts_as":"materials and supplies"}`); w.Code != http.StatusCreated {
		t.Fatalf("seed owner 8's category: %d %s", w.Code, w.Body.String())
	}
	good := n26ExpenseConfirmRow{
		TransactionID: "stmt-0123456789abcdef01234567", PaidOn: "2026-08-03", Currency: "EUR",
		AmountCents: 4567, PayeeName: "Supermercato Esempio",
	}
	cases := map[string]func(*n26ExpenseConfirmRow){
		"payment-style id":         func(r *n26ExpenseConfirmRow) { r.TransactionID = "a1b2c3d4-0e15-4a6f-9bd0-123456789abc" },
		"missing payee":            func(r *n26ExpenseConfirmRow) { r.PayeeName = " " },
		"multi-line payee":         func(r *n26ExpenseConfirmRow) { r.PayeeName = "a\nb" },
		"non-EUR":                  func(r *n26ExpenseConfirmRow) { r.Currency = "USD" },
		"bad date":                 func(r *n26ExpenseConfirmRow) { r.PaidOn = "03.08.2026" },
		"future date":              func(r *n26ExpenseConfirmRow) { r.PaidOn = time.Now().AddDate(0, 0, 3).Format("2006-01-02") },
		"zero amount":              func(r *n26ExpenseConfirmRow) { r.AmountCents = 0 },
		"negative amount":          func(r *n26ExpenseConfirmRow) { r.AmountCents = -4567 },
		"unknown category":         func(r *n26ExpenseConfirmRow) { r.Category = "holidays" },
		"another owner's category": func(r *n26ExpenseConfirmRow) { r.Category = "pigments" },
	}
	for name, mutate := range cases {
		row := good
		mutate(&row)
		w := confirmN26Expenses(t, r, []n26ExpenseConfirmRow{row})
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_row") {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	if w := confirmN26Expenses(t, r, nil); w.Code != http.StatusBadRequest {
		t.Errorf("empty rows: %d", w.Code)
	}
	var n int
	db.Model(&data_models.InvoicingExpense{}).Count(&n)
	if n != 0 {
		t.Fatalf("a refused confirm stored %d expenses", n)
	}
	if w := confirmN26Expenses(t, r, []n26ExpenseConfirmRow{good}); w.Code != http.StatusOK {
		t.Fatalf("the good row itself: %d %s", w.Code, w.Body.String())
	}
}

// The parse response carries the outgoing rows in their own field, so the
// client's payment review (which posts every `candidates` row to the payment
// confirm) never sees them. Exercised through the real pdftotext pipeline.
func TestN26ParseResponseCarriesExpenseCandidatesSeparately(t *testing.T) {
	requirePdftotext(t)
	db := setupN26ExpensesDB(t)
	r := setupN26ExpensesRouter(db, 7)
	w := uploadN26(t, r, map[string][]byte{"estratto.pdf": buildN26PDFFromText(t, readN26Fixture(t, "n26_statement.txt"))})
	if w.Code != http.StatusOK {
		t.Fatalf("parse: %d %s", w.Code, w.Body.String())
	}
	resp := decodeN26Parse(t, w)
	if len(resp.Files) != 1 || resp.Files[0].Error != nil {
		t.Fatalf("files: %+v", resp.Files)
	}
	f := resp.Files[0]
	if f.DocumentKind != n26KindAccountStatement || len(f.Candidates) != 2 || len(f.ExpenseCandidates) != 1 {
		t.Fatalf("kind %q, %d payment and %d expense candidates", f.DocumentKind, len(f.Candidates), len(f.ExpenseCandidates))
	}
	for _, c := range f.Candidates {
		if c.GrossCents <= 0 {
			t.Errorf("an outgoing row is among the payment candidates: %+v", c)
		}
	}
	if e := f.ExpenseCandidates[0]; e.AmountCents != 4567 || e.PayeeName != "Supermercato Esempio" {
		t.Errorf("expense candidate through the real pipeline: %+v", e)
	}
	if !strings.Contains(w.Body.String(), `"expense_candidates":[`) {
		t.Errorf("wire shape lacks expense_candidates: %s", w.Body.String())
	}
}

// --- system_3 #10534: the real N26 German layout, alongside the Italian one above ---
//
// The operator's own N26 export is German (statement + Kontoaktivität); the
// Italian layout above stays as its own path (operator, comment 65185 on
// #9656) since no real Italian statement exists to retire it against.

func TestN26DetectKontoaktivitaet(t *testing.T) {
	if s, ok := detectN26Document(readN26Fixture(t, "n26_kontoaktivitaet.txt")); !ok || s.name != n26KindAccountStatement {
		t.Fatalf("Kontoaktivität fixture: got (%q, %v)", s.name, ok)
	}
}

func TestN26ParseStatementDE(t *testing.T) {
	text := readN26Fixture(t, "n26_statement_de.txt")
	cands, err := parseN26AccountStatement(text)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("got %d candidates, want 2 (money out and the internal Space transfer are not payments)", len(cands))
	}
	first, second := cands[0], cands[1]
	if first.GrossCents != 25000 || first.SenderName != "Mara Beispiel" {
		t.Errorf("first row: %+v", first)
	}
	if first.Note != "Vorschuss Wandbild" {
		t.Errorf("first note: %q", first.Note)
	}
	if second.GrossCents != 100000 || second.SenderName != "Kunde Zwei GmbH" {
		t.Errorf("second row: %+v", second)
	}
	// PaidOn is the value date (Wertstellung), not the booking date
	// (Verbuchungsdatum) printed on the row itself: the fixture's first row
	// books on 03.11.2026 but values on 04.11.2026.
	if !strings.Contains(text, "03.11.2026       250,00€") {
		t.Fatal("fixture drifted: the booking-date row is not in it verbatim")
	}
	if first.PaidOn != "2026-11-04" {
		t.Errorf("first PaidOn: %q, want the Wertstellung date, not the booking date", first.PaidOn)
	}
	if second.PaidOn != "2026-11-11" {
		t.Errorf("second PaidOn: %q, want the Wertstellung date", second.PaidOn)
	}
	for _, c := range cands {
		if !strings.HasPrefix(c.TransactionID, "stmt-") {
			t.Errorf("synthesized id: %q", c.TransactionID)
		}
	}
	if first.TransactionID == second.TransactionID {
		t.Error("distinct rows share a transaction id")
	}
	again, err := parseN26AccountStatement(text)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	for i := range cands {
		if cands[i].TransactionID != again[i].TransactionID {
			t.Errorf("row %d id not stable: %q vs %q", i, cands[i].TransactionID, again[i].TransactionID)
		}
	}
}

func TestN26StatementOutgoingRowsBecomeExpenseCandidatesDE(t *testing.T) {
	text := readN26Fixture(t, "n26_statement_de.txt")
	expenses, err := parseN26StatementExpenses(text)
	if err != nil {
		t.Fatalf("parse expenses: %v", err)
	}
	if len(expenses) != 2 {
		t.Fatalf("got %d expense candidates, want 2: %+v", len(expenses), expenses)
	}
	first, second := expenses[0], expenses[1]
	if first.PaidOn != "2026-11-05" || first.AmountCents != 4567 || first.PayeeName != "Beispiel Supermarkt" ||
		first.Currency != "EUR" || first.Note != "" || !strings.HasPrefix(first.TransactionID, "stmt-") {
		t.Errorf("first expense candidate: %+v", first)
	}
	// A Mastercard purchase has no IBAN/BIC line, and still parses cleanly.
	if second.PaidOn != "2026-11-07" || second.AmountCents != 1250 || second.PayeeName != "BEISPIEL SPORTHALLE" ||
		second.Note != "" || !strings.HasPrefix(second.TransactionID, "stmt-") {
		t.Errorf("second expense candidate (Mastercard, no IBAN): %+v", second)
	}

	cands, err := parseN26AccountStatement(text)
	if err != nil {
		t.Fatalf("parse payments: %v", err)
	}
	for _, c := range cands {
		for _, e := range expenses {
			if c.TransactionID == e.TransactionID {
				t.Errorf("a payment and an expense share an id: %q", c.TransactionID)
			}
		}
	}

	again, err := parseN26StatementExpenses(text)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	for i := range expenses {
		if expenses[i].TransactionID != again[i].TransactionID {
			t.Errorf("expense %d id not stable: %q vs %q", i, expenses[i].TransactionID, again[i].TransactionID)
		}
	}
}

// The Kontoaktivität export's per-Space summary page carries no dated rows:
// its four aggregate lines (Eingänge/Ausgänge/Gebühren/Saldo) never match
// the statement row pattern, so only the main account page's one real row
// is read.
func TestN26ParseKontoaktivitaetSkipsSpaceSummaryPage(t *testing.T) {
	text := readN26Fixture(t, "n26_kontoaktivitaet.txt")
	in, out, skipped, err := scanN26Statement(text)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(in) != 1 || len(out) != 0 || skipped != 0 {
		t.Fatalf("got %d in, %d out, %d skipped, want 1, 0, 0: %+v", len(in), len(out), skipped, in)
	}
	if in[0].SenderName != "Mara Beispiel" || in[0].GrossCents != 25000 || in[0].PaidOn != "2026-11-04" {
		t.Errorf("the one real row: %+v", in[0])
	}
}

// A German row's detail block carries structural lines (the transaction
// type, IBAN/BIC, the value date) alongside any genuine reference lines;
// only the latter become the row's note, and multiple reference lines join
// into one. A zero-amount row is neither a payment nor an expense. A row
// with no type line (an N26 Space's own internal transfer, e.g. into
// "Investments") is reported as a skip, not a candidate — the shape such a
// row takes in the operator's real statement.
func TestN26StatementMixedDetailBlocksDE(t *testing.T) {
	text := "Kontoauszug Nr. 1/2026\n01.01.2026 bis 31.01.2026\n\n\n\n" +
		"Zahlungsempfänger/Auftraggeber                            Verbuchungsdatum     Betrag\n\n" +
		"Mara Beispiel                                             01.01.2026    250,00€\n" +
		"Gutschriften\n" +
		"IBAN: DE00111122223333444455 • BIC: BANKDEFFXXX\n" +
		"Vorschuss Wandbild\n" +
		"Wertstellung 02.01.2026\n\n" +
		"Beispiel Farbenhandel                                     03.01.2026    -89,90€\n" +
		"Lastschriften\n" +
		"IBAN: DE00555566667777888899 • BIC: BANKDEFFXXX\n" +
		"Pigmente und Leinwand\n" +
		"Bestellung 4411\n" +
		"Wertstellung 03.01.2026\n\n" +
		"Kunde Zwei GmbH                                            05.01.2026   1.000,00€\n" +
		"Gutschriften\n" +
		"IBAN: DE00999988887777666655 • BIC: BANKDEFFXXX\n" +
		"Wertstellung 06.01.2026\n\n" +
		"Korrektur                                                  07.01.2026      0,00€\n" +
		"Wertstellung 07.01.2026\n\n" +
		"zu Investments                                             08.01.2026    -100,00€\n" +
		"Wertstellung 08.01.2026\n\n"

	in, out, skipped, err := scanN26Statement(text)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(in) != 2 || len(out) != 1 || skipped != 1 {
		t.Fatalf("got %d in, %d out, %d skipped, want 2, 1, 1: %+v %+v", len(in), len(out), skipped, in, out)
	}
	if in[0].Note != "Vorschuss Wandbild" || in[1].Note != "" {
		t.Errorf("incoming notes: %q, %q", in[0].Note, in[1].Note)
	}
	if out[0].PayeeName != "Beispiel Farbenhandel" || out[0].AmountCents != 8990 || out[0].Note != "Pigmente und Leinwand Bestellung 4411" {
		t.Errorf("the outgoing row's two reference lines: %+v", out[0])
	}
	for _, c := range in {
		if strings.Contains(c.Note, "IBAN") || strings.Contains(c.Note, "Gutschriften") {
			t.Errorf("a structural line leaked into a note: %+v", c)
		}
	}
	if in[0].PaidOn != "2026-01-02" {
		t.Errorf("first row PaidOn: %q, want the Wertstellung date", in[0].PaidOn)
	}
}

// The type vocabulary of N26's real German statement (system_3 #10534,
// checked against the operator's October statement and N26's CSV export of
// the same month; every name, IBAN and amount below is synthetic). Real
// money: Gutschriften, Lastschriften, Belastungen, Mastercard with or
// without a "• <category>" suffix, MoneyBeam — in either direction, and on a
// per-Space page as much as on the main one (a Belastungen paid out of a
// Space is still money leaving the account holder). Internal: a Round-up, a
// Monthly Rule, and a "zu/von <Space>" row with no type line. A type the
// parser has never seen is offered as a candidate, never dropped unseen.
func TestN26StatementRealTypeVocabularyDE(t *testing.T) {
	row := func(name, date, amount string, block ...string) string {
		s := fmt.Sprintf("%-58s%s    %s€\n", name, date, amount)
		for _, l := range block {
			s += l + "\n"
		}
		return s + "\n"
	}
	text := "Kontoauszug Nr. 2/2026\n01.02.2026 bis 28.02.2026\n\n\n\n" +
		"Zahlungsempfänger/Auftraggeber                            Verbuchungsdatum     Betrag\n\n" +
		row("Beispiel Vermieter", "01.02.2026", "-700,00", "Belastungen", "IBAN: DE00111122223333444466 • BIC: BANKDEFFXXX", "Miete Atelier", "Wertstellung 01.02.2026") +
		row("Lia Beispiel", "02.02.2026", "40,00", "MoneyBeam", "Farben geteilt", "Wertstellung 02.02.2026") +
		row("Lia Beispiel", "03.02.2026", "-15,00", "MoneyBeam", "Wertstellung 03.02.2026") +
		row("BEISPIEL KINO", "04.02.2026", "-11,00", "Mastercard • Freizeit", "Wertstellung 05.02.2026") +
		row("BEISPIEL KINO", "06.02.2026", "11,00", "Mastercard • Freizeit", "Wertstellung 06.02.2026") +
		row("Beispiel Neu", "07.02.2026", "-3,00", "Neue Buchungsart", "Wertstellung 07.02.2026") +
		row("zu Investments", "08.02.2026", "-1,20", "2x Round-up", "Wertstellung 08.02.2026") +
		row("zu Investments", "09.02.2026", "-50,00", "Monthly Rule", "Wertstellung 09.02.2026") +
		row("von Investments", "10.02.2026", "80,00", "Wertstellung 10.02.2026") +
		"N26 Bank Beispiel GmbH Musterstraße 1, 10000 Berlin, Germany\n" +
		"Kontoauszug\nSpace: Investments\n\n" +
		row("von Hauptkonto", "08.02.2026", "1,20", "2x Round-up", "Wertstellung 08.02.2026") +
		row("Beispiel Versicherung", "12.02.2026", "-20,00", "Belastungen", "IBAN: DE00222233334444555566 • BIC: BANKDEFFXXX", "Police 77", "Wertstellung 12.02.2026") +
		row("zu Hauptkonto", "10.02.2026", "-80,00", "Wertstellung 10.02.2026")

	in, out, skipped, err := scanN26Statement(text)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	var inNames, outNames []string
	for _, c := range in {
		inNames = append(inNames, c.SenderName)
	}
	for _, e := range out {
		outNames = append(outNames, e.PayeeName)
	}
	wantIn := []string{"Lia Beispiel", "BEISPIEL KINO"}
	wantOut := []string{"Beispiel Vermieter", "Lia Beispiel", "BEISPIEL KINO", "Beispiel Neu", "Beispiel Versicherung"}
	if fmt.Sprint(inNames) != fmt.Sprint(wantIn) || fmt.Sprint(outNames) != fmt.Sprint(wantOut) || skipped != 5 {
		t.Fatalf("in %v (want %v), out %v (want %v), skipped %d (want 5)", inNames, wantIn, outNames, wantOut, skipped)
	}
	if out[0].Note != "Miete Atelier" || out[4].Note != "Police 77" || in[0].Note != "Farben geteilt" {
		t.Errorf("a type line or IBAN leaked into a note, or a reference was lost: %q %q %q", out[0].Note, out[4].Note, in[0].Note)
	}
	for _, e := range out {
		if strings.Contains(e.Note, "Belastungen") || strings.Contains(e.Note, "MoneyBeam") || strings.Contains(e.Note, "Mastercard") || strings.Contains(e.Note, "Buchungsart") {
			t.Errorf("type line in note: %+v", e)
		}
	}
	if out[2].PaidOn != "2026-02-05" {
		t.Errorf("card purchase PaidOn: %q, want the Wertstellung date", out[2].PaidOn)
	}
}

func TestN26DropPayPalTransfersDE(t *testing.T) {
	text := readN26Fixture(t, "n26_statement_de.txt")
	plain, err := parseN26AccountStatement(text)
	if err != nil || len(plain) != 2 {
		t.Fatalf("fixture parse: %d rows, %v", len(plain), err)
	}
	const company = "PayPal Europe S.a.r.l. et Cie S.C.A"
	const mention = "Vorschuss Wandbild via PayPal"
	mixed := strings.Replace(text, "Kunde Zwei GmbH", company, 1)
	mixed = strings.Replace(mixed, "Vorschuss Wandbild", mention, 1)
	if mixed == text || !strings.Contains(mixed, company) || !strings.Contains(mixed, mention) {
		t.Fatal("fixture drifted: the replacements did not apply")
	}
	all, err := parseN26AccountStatement(mixed)
	if err != nil || len(all) != 2 || all[1].SenderName != company {
		t.Fatalf("mixed parse must still read both rows before the filter: %+v, %v", all, err)
	}
	kept, skipped := dropPayPalTransfers(all)
	if len(kept) != 1 || kept[0].SenderName != "Mara Beispiel" || kept[0].Note != mention {
		t.Fatalf("kept: %+v", kept)
	}
	if skipped[n26SkipPayPalTransfer] != 1 || len(skipped) != 1 {
		t.Fatalf("skipped: %+v", skipped)
	}
	if kept[0].TransactionID != all[0].TransactionID {
		t.Error("a kept row's id changed when a sibling row was dropped")
	}
	if k, s := dropPayPalTransfers(plain); len(k) != 2 || s != nil {
		t.Fatalf("clean statement: kept %d, skipped %+v", len(k), s)
	}
}

// Through the real pipeline: the German fixture's statement rows, its
// outgoing rows as expense candidates, and its one internal Space transfer
// reported as a skip — all against real pdftotext extraction of a rendered
// PDF, not the unit-level scan.
func TestN26ParseResponseCarriesExpenseCandidatesSeparatelyDE(t *testing.T) {
	requirePdftotext(t)
	db := setupN26ExpensesDB(t)
	r := setupN26ExpensesRouter(db, 7)
	w := uploadN26(t, r, map[string][]byte{"kontoauszug.pdf": buildN26PDFFromText(t, readN26Fixture(t, "n26_statement_de.txt"))})
	if w.Code != http.StatusOK {
		t.Fatalf("parse: %d %s", w.Code, w.Body.String())
	}
	resp := decodeN26Parse(t, w)
	if len(resp.Files) != 1 || resp.Files[0].Error != nil {
		t.Fatalf("files: %+v", resp.Files)
	}
	f := resp.Files[0]
	if f.DocumentKind != n26KindAccountStatement || len(f.Candidates) != 2 || len(f.ExpenseCandidates) != 2 {
		t.Fatalf("kind %q, %d payment and %d expense candidates", f.DocumentKind, len(f.Candidates), len(f.ExpenseCandidates))
	}
	for _, c := range f.Candidates {
		if c.GrossCents <= 0 {
			t.Errorf("an outgoing row is among the payment candidates: %+v", c)
		}
	}
	if e := f.ExpenseCandidates[0]; e.AmountCents != 4567 || e.PayeeName != "Beispiel Supermarkt" {
		t.Errorf("expense candidate through the real pipeline: %+v", e)
	}
	if !strings.Contains(w.Body.String(), `"expense_candidates":[`) {
		t.Errorf("wire shape lacks expense_candidates: %s", w.Body.String())
	}
	if f.Skipped[n26SkipInternalTransfer] != 1 {
		t.Errorf("internal-transfer skip not reported through the real pipeline: %+v", f.Skipped)
	}
}
