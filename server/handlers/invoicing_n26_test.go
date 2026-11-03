package handlers

import (
	"bytes"
	"encoding/json"
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
		"no amount":    strings.Replace(full, "1.234,56 EUR", "", 1),
		"no uuid":      strings.Replace(full, "a1b2c3d4-0e15-4a6f-9bd0-123456789abc", "not-an-id", 1),
		"no sender":    strings.Replace(full, "DETTAGLI MITTENTE", "QUALCOS'ALTRO", 1),
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
		Source: "manual", TransactionID: "M-1", Kind: data_models.PaymentKindIncoming,
		PaidOn: paidOn, Currency: "EUR", GrossCents: 123456, NetCents: 123456,
		Category: data_models.PaymentCategoryUncategorised, Entry: data_models.PaymentEntryManual,
	}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	cands := []n26Candidate{
		{TransactionID: "a1b2c3d4-0e15-4a6f-9bd0-123456789abc", PaidOn: "2026-07-14", GrossCents: 123456},
		{TransactionID: "stmt-0000", PaidOn: "2026-09-01", GrossCents: 555},
	}
	if err := flagN26Candidates(db, cands); err != nil {
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
	for _, line := range strings.Split(readN26Fixture(t, "n26_confirmation.txt"), "\n") {
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
