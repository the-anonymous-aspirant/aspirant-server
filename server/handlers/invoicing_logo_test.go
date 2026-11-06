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
	"aspirant-online/server/invoicing_pdf"

	"github.com/gin-gonic/gin"
)

// system_3 #9858: the issuer's logo, uploaded through the profile and printed
// on every invoice issued while it is set.

func withLogoRoutes(r *gin.Engine) *gin.Engine {
	r.GET("/invoicing/profile/logo", GetInvoicingLogoHandler)
	r.PUT("/invoicing/profile/logo", PutInvoicingLogoHandler)
	r.DELETE("/invoicing/profile/logo", DeleteInvoicingLogoHandler)
	return r
}

// testLogoPNG is a w×h transparent canvas with an opaque black ink box and
// one part-transparent pixel, so the normalized logo keeps its alpha.
func testLogoPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := h / 4; y < 3*h/4; y++ {
		for x := w / 4; x < 3*w/4; x++ {
			img.SetNRGBA(x, y, color.NRGBA{0, 0, 0, 255})
		}
	}
	img.SetNRGBA(w/4, h/4, color.NRGBA{0, 0, 0, 90})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func uploadLogo(r *gin.Engine, field string, content []byte) *httptest.ResponseRecorder {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if field != "" {
		fw, _ := mw.CreateFormFile(field, "Logo novo.png")
		fw.Write(content)
	}
	mw.Close()
	req, _ := http.NewRequest("PUT", "/invoicing/profile/logo", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestInvoicingLogo_UploadServeReplaceDelete(t *testing.T) {
	db := setupInvoicingDB(t)
	r := withLogoRoutes(setupInvoicingRouter(db, 42))

	if w := invoicingDo(r, "GET", "/invoicing/profile/logo", ""); w.Code != http.StatusNotFound {
		t.Fatalf("GET before upload: %d, want 404", w.Code)
	}
	if hasLogo(t, r) {
		t.Fatalf("has_logo before upload")
	}

	w := uploadLogo(r, "logo", testLogoPNG(t, 1600, 800))
	if w.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	var res struct {
		Logo data_models.InvoicingProfileLogo `json:"logo"`
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	// The ink box is 800×400 of the canvas, scaled to 600 on the long side.
	if res.Logo.Width != 600 || res.Logo.Height != 300 || res.Logo.SHA256 == "" {
		t.Errorf("stored logo %+v, want 600×300 with a sha256", res.Logo)
	}
	if strings.Contains(w.Body.String(), "content") {
		t.Errorf("the upload response carries the image bytes: %s", w.Body.String())
	}

	got := invoicingDo(r, "GET", "/invoicing/profile/logo", "")
	if got.Code != http.StatusOK || got.Header().Get("Content-Type") != "image/png" ||
		got.Header().Get("Cache-Control") != "no-store" || got.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("serve: %d %v", got.Code, got.Header())
	}
	img, err := png.Decode(bytes.NewReader(got.Body.Bytes()))
	if err != nil || img.Bounds().Dx() != 600 || img.Bounds().Dy() != 300 {
		t.Fatalf("served logo is not the normalized PNG: %v %v", err, img)
	}
	if !hasLogo(t, r) {
		t.Errorf("has_logo false after upload")
	}

	// Replacing keeps one row.
	if w := uploadLogo(r, "logo", testLogoPNG(t, 200, 200)); w.Code != http.StatusOK {
		t.Fatalf("replace: %d %s", w.Code, w.Body.String())
	}
	var n int
	db.Model(&data_models.InvoicingProfileLogo{}).Count(&n)
	if n != 1 {
		t.Errorf("%d logo rows after a replace, want 1", n)
	}

	if w := invoicingDo(r, "DELETE", "/invoicing/profile/logo", ""); w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if w := invoicingDo(r, "GET", "/invoicing/profile/logo", ""); w.Code != http.StatusNotFound {
		t.Errorf("GET after delete: %d, want 404", w.Code)
	}
	if w := invoicingDo(r, "DELETE", "/invoicing/profile/logo", ""); w.Code != http.StatusNotFound {
		t.Errorf("second delete: %d, want 404", w.Code)
	}
	if hasLogo(t, r) {
		t.Errorf("has_logo true after delete")
	}

	// Upload, replace and delete are audited, with hashes and never bytes.
	events, err := data_models.ListInvoicingAuditEvents(db, "profile_logo", 0)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, e := range events {
		actions = append(actions, e.Action)
		if e.UserID != 42 || !strings.Contains(e.Details, "sha256") {
			t.Errorf("audit row %+v lacks the user or the hash", e)
		}
		if len(e.Details) > 2000 {
			t.Errorf("audit row carries %d bytes of details", len(e.Details))
		}
	}
	if strings.Join(actions, ",") != "delete,upload,upload" {
		t.Errorf("audit actions newest first: %v", actions)
	}
	if !strings.Contains(events[1].Details, `"before"`) || strings.Contains(events[2].Details, `"before"`) {
		t.Errorf("the replace should record the logo it replaced, the first upload none")
	}
}

func hasLogo(t *testing.T, r *gin.Engine) bool {
	t.Helper()
	w := invoicingDo(r, "GET", "/invoicing/profile", "")
	var res struct {
		HasLogo *bool `json:"has_logo"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.HasLogo == nil {
		t.Fatalf("profile GET has no has_logo: %s", w.Body.String())
	}
	return *res.HasLogo
}

func TestInvoicingLogo_RefusalsWriteNothing(t *testing.T) {
	cases := []struct {
		name    string
		field   string
		content []byte
		want    string
	}{
		{"no file", "", nil, "No logo file"},
		{"wrong field", "image", []byte("x"), "No logo file"},
		{"not an image", "logo", []byte("%PDF-1.7 not a logo"), "PNG or JPEG"},
		{"over the byte cap", "logo", append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, invoicing_pdf.MaxLogoBytes)...), "larger than 5 MB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := setupInvoicingDB(t)
			r := withLogoRoutes(setupInvoicingRouter(db, 42))
			w := uploadLogo(r, tc.field, tc.content)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("got %d %s, want 400 naming %q", w.Code, w.Body.String(), tc.want)
			}
			var n int
			db.Model(&data_models.InvoicingProfileLogo{}).Count(&n)
			if n != 0 || countAuditEvents(t, db) != 0 {
				t.Errorf("a refused upload wrote %d logo rows and %d audit events", n, countAuditEvents(t, db))
			}
		})
	}
}

func TestInvoicingLogo_PrintedOnIssueAndFrozenInTheStoredPDF(t *testing.T) {
	f := newInvoiceFixture(t)
	withLogoRoutes(f.r)

	// Issued without a logo: no image in the document.
	plain := f.draft(t, 0)
	if code, _, body := f.issue(plain); code != http.StatusOK {
		t.Fatalf("issue without a logo: %d %s", code, body)
	}
	plainPDF, _ := data_models.GetInvoicePDF(f.db, plain)
	if bytes.Contains(plainPDF.Content, []byte("/Subtype /Image")) {
		t.Errorf("an invoice issued without a logo carries an image")
	}

	if w := uploadLogo(f.r, "logo", testLogoPNG(t, 1600, 800)); w.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	logo, _ := data_models.GetInvoicingProfileLogo(f.db, invoiceFixtureUser)

	id := f.draft(t, 1)
	if code, _, body := f.issue(id); code != http.StatusOK {
		t.Fatalf("issue with a logo: %d %s", code, body)
	}
	stored, _ := data_models.GetInvoicePDF(f.db, id)
	for _, want := range []string{"/Subtype /Image", "/SMask"} {
		if !bytes.Contains(stored.Content, []byte(want)) {
			t.Errorf("the issued PDF has no %s", want)
		}
	}
	events, _ := data_models.ListInvoicingAuditEvents(f.db, "invoice", id)
	if !strings.Contains(events[0].Details, fmt.Sprintf(`"logo_sha256":%q`, logo.SHA256)) {
		t.Errorf("the issue audit does not record the logo's hash: %s", events[0].Details)
	}

	// Removing the logo afterwards changes neither issued document.
	invoicingDo(f.r, "DELETE", "/invoicing/profile/logo", "")
	again, _ := data_models.GetInvoicePDF(f.db, id)
	if !bytes.Equal(again.Content, stored.Content) {
		t.Errorf("removing the logo changed an issued PDF")
	}
	plainAgain, _ := data_models.GetInvoicePDF(f.db, plain)
	if !bytes.Equal(plainAgain.Content, plainPDF.Content) {
		t.Errorf("the logo reached an invoice issued before it was set")
	}
}
