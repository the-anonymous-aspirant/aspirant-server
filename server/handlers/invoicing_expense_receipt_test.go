package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// system_3 #10537-A1: one receipt attached to one work expense, the same
// shape as invoicing_logo_test.go's coverage of the logo.

func setupReceiptDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := setupExpensesDB(t)
	db.AutoMigrate(&data_models.InvoicingExpenseReceipt{})
	return db
}

func setupReceiptRouter(db *gorm.DB, userID uint) *gin.Engine {
	r := setupExpensesRouter(db, userID)
	r.PUT("/invoicing/expenses/:id/receipt", PutInvoicingExpenseReceiptHandler)
	r.GET("/invoicing/expenses/:id/receipt", GetInvoicingExpenseReceiptHandler)
	r.DELETE("/invoicing/expenses/:id/receipt", DeleteInvoicingExpenseReceiptHandler)
	return r
}

func testReceiptPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	img.SetNRGBA(1, 1, color.NRGBA{10, 20, 30, 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// A minimal byte sequence Go's http.DetectContentType recognizes as a PDF
// (the "%PDF-" magic prefix) -- not a structurally valid PDF, which the
// sniffer does not require.
func testReceiptPDF() []byte {
	return []byte("%PDF-1.4\n%%EOF\n")
}

func uploadReceipt(r *gin.Engine, id int, filename string, content []byte, declaredType string) *httptest.ResponseRecorder {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if filename != "" {
		h := make(map[string][]string)
		h["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name="receipt"; filename=%q`, filename)}
		if declaredType != "" {
			h["Content-Type"] = []string{declaredType}
		}
		part, _ := mw.CreatePart(h)
		part.Write(content)
	}
	mw.Close()
	req, _ := http.NewRequest("PUT", fmt.Sprintf("/invoicing/expenses/%d/receipt", id), &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func mustCreateExpense(t *testing.T, r *gin.Engine) data_models.InvoicingExpense {
	t.Helper()
	code, e, body := createExpense(t, r, `{"paid_on":"2026-07-01","amount_cents":1500}`)
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("create expense: %d %s", code, body)
	}
	return e
}

func TestInvoicingExpenseReceipt_UploadServeReplaceDelete(t *testing.T) {
	db := setupReceiptDB(t)
	r := setupReceiptRouter(db, 42)
	e := mustCreateExpense(t, r)

	if w := invoicingDo(r, "GET", fmt.Sprintf("/invoicing/expenses/%d/receipt", e.ID), ""); w.Code != http.StatusNotFound {
		t.Fatalf("GET before upload: %d, want 404", w.Code)
	}

	png := testReceiptPNG(t)
	w := uploadReceipt(r, int(e.ID), "Kvitto.png", png, "")
	if w.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	var res struct {
		Receipt struct {
			ContentType string `json:"content_type"`
			Filename    string `json:"filename"`
			SHA256      string `json:"sha256"`
			SizeBytes   int64  `json:"size_bytes"`
		} `json:"receipt"`
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	if res.Receipt.ContentType != "image/png" || res.Receipt.Filename != "Kvitto.png" || res.Receipt.SHA256 == "" || res.Receipt.SizeBytes != int64(len(png)) {
		t.Errorf("stored receipt %+v, want image/png Kvitto.png with a sha256 and size %d", res.Receipt, len(png))
	}
	if bytes.Contains(w.Body.Bytes(), png) {
		t.Errorf("the upload response carries the raw bytes")
	}

	w = invoicingDo(r, "GET", fmt.Sprintf("/invoicing/expenses/%d/receipt", e.ID), "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET after upload: %d", w.Code)
	}
	if w.Header().Get("Content-Type") != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", w.Header().Get("Content-Type"))
	}
	if !bytes.Equal(w.Body.Bytes(), png) {
		t.Errorf("served bytes do not round-trip byte-for-byte")
	}
	if w.Header().Get("X-Content-SHA256") != res.Receipt.SHA256 {
		t.Errorf("X-Content-SHA256 = %q, want %q", w.Header().Get("X-Content-SHA256"), res.Receipt.SHA256)
	}

	// Replace with a PDF -- one receipt per expense, a second upload wins.
	pdf := testReceiptPDF()
	w = uploadReceipt(r, int(e.ID), "receipt.pdf", pdf, "")
	if w.Code != http.StatusOK {
		t.Fatalf("replace: %d %s", w.Code, w.Body.String())
	}
	w = invoicingDo(r, "GET", fmt.Sprintf("/invoicing/expenses/%d/receipt", e.ID), "")
	if w.Header().Get("Content-Type") != "application/pdf" || !bytes.Equal(w.Body.Bytes(), pdf) {
		t.Errorf("after replace: Content-Type=%q bytes-match=%v", w.Header().Get("Content-Type"), bytes.Equal(w.Body.Bytes(), pdf))
	}
	var count int
	db.Model(&data_models.InvoicingExpenseReceipt{}).Where("expense_id = ?", e.ID).Count(&count)
	if count != 1 {
		t.Errorf("rows for this expense = %d, want 1 (replace, not a second row)", count)
	}

	w = invoicingDo(r, "DELETE", fmt.Sprintf("/invoicing/expenses/%d/receipt", e.ID), "")
	if w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if w := invoicingDo(r, "GET", fmt.Sprintf("/invoicing/expenses/%d/receipt", e.ID), ""); w.Code != http.StatusNotFound {
		t.Fatalf("GET after delete: %d, want 404", w.Code)
	}
	if w := invoicingDo(r, "DELETE", fmt.Sprintf("/invoicing/expenses/%d/receipt", e.ID), ""); w.Code != http.StatusNotFound {
		t.Fatalf("delete again: %d, want 404", w.Code)
	}
}

func TestInvoicingExpenseReceipt_TwoExpensesIndependent(t *testing.T) {
	db := setupReceiptDB(t)
	r := setupReceiptRouter(db, 42)
	e1 := mustCreateExpense(t, r)
	e2 := mustCreateExpense(t, r)

	uploadReceipt(r, int(e1.ID), "a.png", testReceiptPNG(t), "")

	if w := invoicingDo(r, "GET", fmt.Sprintf("/invoicing/expenses/%d/receipt", e1.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("e1 GET: %d", w.Code)
	}
	if w := invoicingDo(r, "GET", fmt.Sprintf("/invoicing/expenses/%d/receipt", e2.ID), ""); w.Code != http.StatusNotFound {
		t.Fatalf("e2 GET (should have none): %d, want 404", w.Code)
	}
}

// A receipt exists on the expense before the other owner ever touches it
// (security review of #10602, c65909): with no receipt at all, GET/DELETE
// would 404 whether or not ownership were checked, so that order could not
// have caught a regression. Uploading first, then checking the owner's own
// GET is unchanged afterward, proves the 404s below are ownership and
// nothing else got through.
func TestInvoicingExpenseReceipt_AnotherOwnersExpenseIsNotFound(t *testing.T) {
	db := setupReceiptDB(t)
	owner := setupReceiptRouter(db, 42)
	other := setupReceiptRouter(db, 99)
	e := mustCreateExpense(t, owner)

	if w := uploadReceipt(owner, int(e.ID), "x.png", testReceiptPNG(t), ""); w.Code != http.StatusOK {
		t.Fatalf("owner's own upload: %d %s", w.Code, w.Body.String())
	}
	own := invoicingDo(owner, "GET", fmt.Sprintf("/invoicing/expenses/%d/receipt", e.ID), "")
	ownSHA := own.Header().Get("X-Content-SHA256")
	if own.Code != http.StatusOK || ownSHA == "" {
		t.Fatalf("owner's own GET before the other owner touches it: %d sha256=%q", own.Code, ownSHA)
	}

	if w := uploadReceipt(other, int(e.ID), "evil.png", testReceiptPNG(t), ""); w.Code != http.StatusNotFound {
		t.Fatalf("other owner replace: %d, want 404", w.Code)
	}
	if w := invoicingDo(other, "GET", fmt.Sprintf("/invoicing/expenses/%d/receipt", e.ID), ""); w.Code != http.StatusNotFound {
		t.Fatalf("other owner GET: %d, want 404", w.Code)
	}
	if w := invoicingDo(other, "DELETE", fmt.Sprintf("/invoicing/expenses/%d/receipt", e.ID), ""); w.Code != http.StatusNotFound {
		t.Fatalf("other owner DELETE: %d, want 404", w.Code)
	}

	// The other owner's refused calls changed nothing: the owner's own GET
	// still answers with the same receipt.
	if w := invoicingDo(owner, "GET", fmt.Sprintf("/invoicing/expenses/%d/receipt", e.ID), ""); w.Code != http.StatusOK || w.Header().Get("X-Content-SHA256") != ownSHA {
		t.Fatalf("owner's own GET after the other owner's refused calls: %d sha256=%q, want 200 %q", w.Code, w.Header().Get("X-Content-SHA256"), ownSHA)
	}
}

func TestInvoicingExpenseReceipt_RejectsOversizeAndUnsupportedType(t *testing.T) {
	db := setupReceiptDB(t)
	r := setupReceiptRouter(db, 42)
	e := mustCreateExpense(t, r)

	big := make([]byte, maxReceiptBytes+1)
	if w := uploadReceipt(r, int(e.ID), "big.pdf", big, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("oversize upload: %d, want 400", w.Code)
	}

	if w := uploadReceipt(r, int(e.ID), "note.txt", []byte("not a receipt"), ""); w.Code != http.StatusBadRequest {
		t.Fatalf("text/plain upload: %d, want 400", w.Code)
	}

	// Declaring a false Content-Type on the multipart part must not launder
	// a disallowed type past the server's own sniff of the bytes.
	if w := uploadReceipt(r, int(e.ID), "note.txt", []byte("not a receipt"), "image/png"); w.Code != http.StatusBadRequest {
		t.Fatalf("mislabeled text/plain upload: %d, want 400", w.Code)
	}

	// Positive control: nothing got through any of the refused uploads.
	if w := invoicingDo(r, "GET", fmt.Sprintf("/invoicing/expenses/%d/receipt", e.ID), ""); w.Code != http.StatusNotFound {
		t.Fatalf("GET after only-refused uploads: %d, want 404", w.Code)
	}
}

func TestInvoicingExpenseReceipt_NoExpenseIsNotFound(t *testing.T) {
	db := setupReceiptDB(t)
	r := setupReceiptRouter(db, 42)

	if w := uploadReceipt(r, 99999, "x.png", testReceiptPNG(t), ""); w.Code != http.StatusNotFound {
		t.Fatalf("upload to a nonexistent expense: %d, want 404", w.Code)
	}
}

// Security review of #10602 (c65885 point 4): the download is forced as an
// attachment, named from the expense id and sniffed type alone, never from
// the uploader's own filename, and carries a sandboxing CSP so a direct
// navigation runs no script whatever the file holds.
func TestInvoicingExpenseReceipt_ServingHeaders(t *testing.T) {
	db := setupReceiptDB(t)
	r := setupReceiptRouter(db, 42)
	e := mustCreateExpense(t, r)

	evilName := "<script>evil</script>.png"
	if w := uploadReceipt(r, int(e.ID), evilName, testReceiptPNG(t), ""); w.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}

	w := invoicingDo(r, "GET", fmt.Sprintf("/invoicing/expenses/%d/receipt", e.ID), "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET: %d", w.Code)
	}
	want := fmt.Sprintf(`attachment; filename="receipt-%d.png"`, e.ID)
	if got := w.Header().Get("Content-Disposition"); got != want {
		t.Errorf("Content-Disposition = %q, want %q", got, want)
	}
	if strings.Contains(w.Header().Get("Content-Disposition"), "script") {
		t.Errorf("Content-Disposition carries the uploader's filename: %q", w.Header().Get("Content-Disposition"))
	}
	if got := w.Header().Get("Content-Security-Policy"); !strings.Contains(got, "sandbox") {
		t.Errorf("Content-Security-Policy = %q, want it to carry sandbox", got)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", w.Header().Get("X-Content-Type-Options"))
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", w.Header().Get("Cache-Control"))
	}
}

// Security review of #10602 (c65885 point 5): the audit row carries sha256,
// size and sniffed type, never the uploader's filename or any receipt bytes.
func TestInvoicingExpenseReceipt_AuditCarriesNoFilename(t *testing.T) {
	db := setupReceiptDB(t)
	r := setupReceiptRouter(db, 42)
	e := mustCreateExpense(t, r)

	secretName := "Steuerbescheid-2026-SECRET.pdf"
	if w := uploadReceipt(r, int(e.ID), secretName, testReceiptPDF(), ""); w.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}

	events, err := data_models.ListInvoicingAuditEvents(db, 42, invoicingEntityExpenseReceipt, e.ID)
	if err != nil {
		t.Fatalf("ListInvoicingAuditEvents: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("no audit event recorded for the upload")
	}
	for _, ev := range events {
		if strings.Contains(ev.Details, secretName) || strings.Contains(ev.Details, "filename") {
			t.Errorf("audit details carry the filename: %s", ev.Details)
		}
		if !strings.Contains(ev.Details, "sha256") || !strings.Contains(ev.Details, "content_type") {
			t.Errorf("audit details missing sha256/content_type: %s", ev.Details)
		}
	}
}

// Security review of #10602 (c65885 point 6): deleting the expense deletes
// its receipt in the same transaction, with an audit row for each, and
// leaves no orphan.
func TestInvoicingExpenseReceipt_DeletedWithItsExpense(t *testing.T) {
	db := setupReceiptDB(t)
	r := setupReceiptRouter(db, 42)
	e := mustCreateExpense(t, r)

	if w := uploadReceipt(r, int(e.ID), "x.pdf", testReceiptPDF(), ""); w.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	var before int
	db.Model(&data_models.InvoicingExpenseReceipt{}).Where("expense_id = ?", e.ID).Count(&before)
	if before != 1 {
		t.Fatalf("receipt rows before delete = %d, want 1", before)
	}

	// Delete is remove then purge (system_3 #10647-A2).
	if code, body := deleteForGood(r, "expenses", e.ID); code != http.StatusOK {
		t.Fatalf("delete expense: %d %s", code, body)
	}

	var after int
	db.Model(&data_models.InvoicingExpenseReceipt{}).Where("expense_id = ?", e.ID).Count(&after)
	if after != 0 {
		t.Errorf("receipt rows after expense delete = %d, want 0 (orphan left behind)", after)
	}
	if w := invoicingDo(r, "GET", fmt.Sprintf("/invoicing/expenses/%d/receipt", e.ID), ""); w.Code != http.StatusNotFound {
		t.Errorf("GET receipt after expense delete: %d, want 404", w.Code)
	}

	// entity_id means the same thing on every expense_receipt audit row
	// (security review c65909): the expense id, matching the PUT and the
	// standalone DELETE, never the receipt row's own id.
	events, err := data_models.ListInvoicingAuditEvents(db, 42, invoicingEntityExpenseReceipt, e.ID)
	if err != nil {
		t.Fatalf("ListInvoicingAuditEvents: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("no expense_receipt audit event found keyed on the expense id")
	}
}

// Security review of #10602 (c65885 point 2): a multipart part that *claims*
// application/pdf but is not one by its bytes is refused, confirming the
// sniff -- not the client's header -- decides the type.
func TestInvoicingExpenseReceipt_MislabeledPDFClaimRefused(t *testing.T) {
	db := setupReceiptDB(t)
	r := setupReceiptRouter(db, 42)
	e := mustCreateExpense(t, r)

	if w := uploadReceipt(r, int(e.ID), "fake.pdf", []byte("not actually a pdf"), "application/pdf"); w.Code != http.StatusBadRequest {
		t.Fatalf("mislabeled application/pdf upload: %d, want 400", w.Code)
	}
}
