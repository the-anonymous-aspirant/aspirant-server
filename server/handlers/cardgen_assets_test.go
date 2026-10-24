package handlers

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"aspirant-online/server/data_models"
	"aspirant-online/server/storage"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

// #8642: the card editor's asset store. These tests drive the handlers through
// a router with db, storage, role and user_id set, so the gate is not under
// test here (cardgen_routes_gate_test.go walks that); what is under test is the
// sniff, the cap, the sha256 identity, the headers the content route sets and
// the soft delete.

type cardAssetFixture struct {
	db    *gorm.DB
	store *storage.LocalStorage
	dir   string
	r     *gin.Engine
}

func newCardAssetFixture(t *testing.T) *cardAssetFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	db.AutoMigrate(&data_models.CardAsset{}, &data_models.CardType{}, &data_models.CardTemplate{}, &data_models.CardTemplateVersion{}, &data_models.Card{}, &data_models.CardVersion{})
	t.Cleanup(func() { db.Close() })

	dir := t.TempDir()
	store, err := storage.NewLocalStorage(dir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("db", db)
		c.Set("storage", store)
		c.Set("role", "Admin")
		c.Set("user_id", uint(7))
		c.Next()
	})
	g := r.Group("/cardgen")
	g.POST("/assets", UploadCardAssetHandler)
	g.GET("/assets", ListCardAssetsHandler)
	g.GET("/assets/:id", GetCardAssetHandler)
	g.GET("/assets/:id/content", GetCardAssetContentHandler)
	g.GET("/assets/:id/usage", GetCardAssetUsageHandler)
	g.DELETE("/assets/:id", DeleteCardAssetHandler)
	return &cardAssetFixture{db: db, store: store, dir: dir, r: r}
}

// upload posts content as the multipart field "file" under filename.
func (f *cardAssetFixture) upload(t *testing.T, filename string, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile(cardAssetFormField, filename)
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("write part: %v", err)
	}
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/cardgen/assets", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	f.r.ServeHTTP(w, req)
	return w
}

func (f *cardAssetFixture) call(t *testing.T, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	w := httptest.NewRecorder()
	f.r.ServeHTTP(w, req)
	return w
}

type uploadBody struct {
	Asset        CardAssetResponse `json:"asset"`
	Deduplicated bool              `json:"deduplicated"`
}

func decodeUpload(t *testing.T, w *httptest.ResponseRecorder) uploadBody {
	t.Helper()
	var b uploadBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	return b
}

// pngBytes encodes a 2x3 opaque PNG.
func cardPNGBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 3))
	for y := 0; y < 3; y++ {
		for x := 0; x < 2; x++ {
			img.Set(x, y, color.RGBA{R: uint8(40 * x), G: uint8(60 * y), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

const svgDoc = "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!-- icon -->\n<svg xmlns=\"http://www.w3.org/2000/svg\" viewBox=\"0 0 10 10\"><circle cx=\"5\" cy=\"5\" r=\"4\"/></svg>\n"

func TestCardAssetUpload_PNGIsStoredContentAddressed(t *testing.T) {
	f := newCardAssetFixture(t)
	content := cardPNGBytes(t)

	w := f.upload(t, "art/orion.png", content)
	if w.Code != http.StatusCreated {
		t.Fatalf("upload: want 201, got %d body=%s", w.Code, w.Body.String())
	}
	b := decodeUpload(t, w)
	if b.Deduplicated {
		t.Fatalf("first upload reported deduplicated")
	}
	sum := sha256.Sum256(content)
	wantSHA := hex.EncodeToString(sum[:])
	a := b.Asset
	if a.SHA256 != wantSHA {
		t.Errorf("sha256 = %s, want %s", a.SHA256, wantSHA)
	}
	if a.Kind != data_models.CardAssetKindImage || a.MediaType != "image/png" {
		t.Errorf("kind/type = %s/%s, want image/image/png", a.Kind, a.MediaType)
	}
	if a.PixelWidth != 2 || a.PixelHeight != 3 {
		t.Errorf("pixel size = %dx%d, want 2x3", a.PixelWidth, a.PixelHeight)
	}
	if a.ByteSize != int64(len(content)) {
		t.Errorf("byte_size = %d, want %d", a.ByteSize, len(content))
	}
	if a.OriginalFilename != "orion.png" {
		t.Errorf("original_filename = %q, want the base name orion.png", a.OriginalFilename)
	}
	if a.UploadedByUserID != 7 {
		t.Errorf("uploaded_by_user_id = %d, want 7", a.UploadedByUserID)
	}
	if a.ContentURL != "/api/cardgen/assets/1/content" {
		t.Errorf("content_url = %q", a.ContentURL)
	}
	onDisk, err := os.ReadFile(filepath.Join(f.dir, "cardgen", wantSHA))
	if err != nil {
		t.Fatalf("bytes not at cardgen/<sha256>: %v", err)
	}
	if !bytes.Equal(onDisk, content) {
		t.Errorf("stored bytes differ from the upload")
	}
}

func TestCardAssetUpload_SameBytesTwiceIsOneRow(t *testing.T) {
	f := newCardAssetFixture(t)
	content := cardPNGBytes(t)

	first := decodeUpload(t, f.upload(t, "a.png", content))
	w := f.upload(t, "copy-of-a.png", content)
	if w.Code != http.StatusOK {
		t.Fatalf("second upload: want 200, got %d body=%s", w.Code, w.Body.String())
	}
	second := decodeUpload(t, w)
	if !second.Deduplicated || second.Asset.ID != first.Asset.ID {
		t.Errorf("second upload: deduplicated=%v id=%d, want deduplicated with id %d", second.Deduplicated, second.Asset.ID, first.Asset.ID)
	}
	var n int
	f.db.Model(&data_models.CardAsset{}).Count(&n)
	if n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
}

func TestCardAssetUpload_SniffRejectsTextNamedPNG(t *testing.T) {
	f := newCardAssetFixture(t)
	w := f.upload(t, "not-really.png", []byte("hello, I am plain text pretending to be a picture\n"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", w.Code, w.Body.String())
	}
	entries, _ := os.ReadDir(filepath.Join(f.dir, "cardgen"))
	if len(entries) != 0 {
		t.Errorf("rejected upload left %d file(s) in the store", len(entries))
	}
}

func TestCardAssetUpload_OverCapIsRejectedBeforeStoring(t *testing.T) {
	f := newCardAssetFixture(t)
	big := make([]byte, data_models.MaxCardAssetBytes+1)
	copy(big, cardPNGBytes(t)) // a real PNG header, so only the size can refuse it
	w := f.upload(t, "huge.png", big)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(f.dir, "cardgen")); !os.IsNotExist(err) {
		t.Errorf("over-cap upload wrote into the store")
	}
}

func TestCardAssetUpload_EmptyIsRejected(t *testing.T) {
	f := newCardAssetFixture(t)
	w := f.upload(t, "empty.png", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestCardAssetContent_SVGIsAttachmentWithNosniff(t *testing.T) {
	f := newCardAssetFixture(t)
	b := decodeUpload(t, f.upload(t, "icon.svg", []byte(svgDoc)))
	if b.Asset.Kind != data_models.CardAssetKindSVG || b.Asset.MediaType != "image/svg+xml" {
		t.Fatalf("svg sniffed as %s/%s", b.Asset.Kind, b.Asset.MediaType)
	}
	w := f.call(t, http.MethodGet, "/cardgen/assets/1/content")
	if w.Code != http.StatusOK {
		t.Fatalf("content: want 200, got %d", w.Code)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "image/svg+xml") {
		t.Errorf("Content-Type = %q, want image/svg+xml", got)
	}
	if got := w.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "attachment;") || !strings.Contains(got, "icon.svg") {
		t.Errorf("Content-Disposition = %q, want attachment with the filename", got)
	}
	if got := w.Header().Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'none'") || !strings.Contains(got, "sandbox") {
		t.Errorf("Content-Security-Policy = %q", got)
	}
	if w.Body.String() != svgDoc {
		t.Errorf("body differs from the upload")
	}
}

func TestCardAssetContent_PNGIsInline(t *testing.T) {
	f := newCardAssetFixture(t)
	content := cardPNGBytes(t)
	f.upload(t, "bg.png", content)
	w := f.call(t, http.MethodGet, "/cardgen/assets/1/content")
	if w.Code != http.StatusOK {
		t.Fatalf("content: want 200, got %d", w.Code)
	}
	if got := w.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "inline;") {
		t.Errorf("Content-Disposition = %q, want inline", got)
	}
	if got := w.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", got)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "private, max-age=86400" {
		t.Errorf("Cache-Control = %q", got)
	}
	if !bytes.Equal(w.Body.Bytes(), content) {
		t.Errorf("body differs from the upload")
	}
}

func TestCardAssetUpload_FontsAndPDFAreTypedByMagic(t *testing.T) {
	f := newCardAssetFixture(t)
	cases := []struct {
		name, filename string
		head           []byte
		wantType, want string
	}{
		{"ttf", "display.ttf", []byte{0x00, 0x01, 0x00, 0x00, 0x00, 0x10}, "font/ttf", data_models.CardAssetKindFont},
		{"otf", "display.otf", []byte("OTTO\x00\x10"), "font/otf", data_models.CardAssetKindFont},
		{"woff", "display.woff", []byte("wOFF\x00\x01"), "font/woff", data_models.CardAssetKindFont},
		{"woff2", "display.woff2", []byte("wOF2\x00\x01"), "font/woff2", data_models.CardAssetKindFont},
		{"pdf", "cards.pdf", []byte("%PDF-1.4\n%\xE2\xE3\xCF\xD3\n1 0 obj\n"), "application/pdf", data_models.CardAssetKindPDF},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := append(tc.head, bytes.Repeat([]byte{0x42}, 64)...)
			w := f.upload(t, tc.filename, content)
			if w.Code != http.StatusCreated {
				t.Fatalf("want 201, got %d body=%s", w.Code, w.Body.String())
			}
			b := decodeUpload(t, w)
			if b.Asset.MediaType != tc.wantType || b.Asset.Kind != tc.want {
				t.Errorf("typed %s/%s, want %s/%s", b.Asset.Kind, b.Asset.MediaType, tc.want, tc.wantType)
			}
			if b.Asset.PixelWidth != 0 || b.Asset.PixelHeight != 0 {
				t.Errorf("non-raster carries a pixel size %dx%d", b.Asset.PixelWidth, b.Asset.PixelHeight)
			}
			c := f.call(t, http.MethodGet, "/cardgen/assets/"+cardAssetID(b.Asset.ID)+"/content")
			if got := c.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "attachment;") {
				t.Errorf("Content-Disposition = %q, want attachment", got)
			}
		})
	}
}

func TestCardAssetList_FiltersByKindNewestFirst(t *testing.T) {
	f := newCardAssetFixture(t)
	f.upload(t, "bg.png", cardPNGBytes(t))
	f.upload(t, "display.ttf", append([]byte{0x00, 0x01, 0x00, 0x00}, bytes.Repeat([]byte{1}, 32)...))
	f.upload(t, "icon.svg", []byte(svgDoc))

	var all struct {
		Assets []CardAssetResponse `json:"assets"`
	}
	w := f.call(t, http.MethodGet, "/cardgen/assets")
	if err := json.Unmarshal(w.Body.Bytes(), &all); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(all.Assets) != 3 || all.Assets[0].ID != 3 || all.Assets[2].ID != 1 {
		t.Errorf("list = %+v, want ids 3,2,1", all.Assets)
	}

	w = f.call(t, http.MethodGet, "/cardgen/assets?kind=font")
	if err := json.Unmarshal(w.Body.Bytes(), &all); err != nil {
		t.Fatalf("decode filtered list: %v", err)
	}
	if len(all.Assets) != 1 || all.Assets[0].Kind != data_models.CardAssetKindFont {
		t.Errorf("kind=font list = %+v, want the one font", all.Assets)
	}
}

func TestCardAssetDelete_SoftDeletesAndKeepsBytes(t *testing.T) {
	f := newCardAssetFixture(t)
	content := cardPNGBytes(t)
	b := decodeUpload(t, f.upload(t, "bg.png", content))

	w := f.call(t, http.MethodDelete, "/cardgen/assets/1")
	if w.Code != http.StatusOK {
		t.Fatalf("delete: want 200, got %d body=%s", w.Code, w.Body.String())
	}
	if w := f.call(t, http.MethodGet, "/cardgen/assets/1"); w.Code != http.StatusNotFound {
		t.Errorf("get after delete: want 404, got %d", w.Code)
	}
	if w := f.call(t, http.MethodGet, "/cardgen/assets/1/content"); w.Code != http.StatusNotFound {
		t.Errorf("content after delete: want 404, got %d", w.Code)
	}
	var list struct {
		Assets []CardAssetResponse `json:"assets"`
	}
	json.Unmarshal(f.call(t, http.MethodGet, "/cardgen/assets").Body.Bytes(), &list)
	if len(list.Assets) != 0 {
		t.Errorf("list after delete = %+v, want empty", list.Assets)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "cardgen", b.Asset.SHA256)); err != nil {
		t.Errorf("bytes removed on soft delete: %v", err)
	}

	// Same content again restores the row instead of writing a twin.
	w = f.upload(t, "bg-again.png", content)
	if w.Code != http.StatusOK {
		t.Fatalf("re-upload after delete: want 200, got %d body=%s", w.Code, w.Body.String())
	}
	again := decodeUpload(t, w)
	if !again.Deduplicated || again.Asset.ID != 1 {
		t.Errorf("re-upload = id %d deduplicated=%v, want id 1 restored", again.Asset.ID, again.Deduplicated)
	}
	if w := f.call(t, http.MethodGet, "/cardgen/assets/1"); w.Code != http.StatusOK {
		t.Errorf("get after restore: want 200, got %d", w.Code)
	}
}

func TestCardAssetGet_BadIDs(t *testing.T) {
	f := newCardAssetFixture(t)
	if w := f.call(t, http.MethodGet, "/cardgen/assets/0"); w.Code != http.StatusBadRequest {
		t.Errorf("id 0: want 400, got %d", w.Code)
	}
	if w := f.call(t, http.MethodGet, "/cardgen/assets/abc"); w.Code != http.StatusBadRequest {
		t.Errorf("id abc: want 400, got %d", w.Code)
	}
	if w := f.call(t, http.MethodGet, "/cardgen/assets/99"); w.Code != http.StatusNotFound {
		t.Errorf("id 99: want 404, got %d", w.Code)
	}
}

// Security review items 1 to 3 (#8634 c53354): an SVG with active content is
// refused, a raster over the per-side pixel cap is refused from its header
// alone, and an over-cap body is refused by the body reader before the form
// is parsed.
func TestCardAssetUpload_RefusesActiveSVG(t *testing.T) {
	f := newCardAssetFixture(t)
	cases := map[string]string{
		"script":         `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`,
		"foreignObject":  `<svg xmlns="http://www.w3.org/2000/svg"><foreignObject><div>x</div></foreignObject></svg>`,
		"onload":         `<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"><rect/></svg>`,
		"href-http":      `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink"><use xlink:href="http://evil.test/x.svg#a"/></svg>`,
		"href-js":        `<svg xmlns="http://www.w3.org/2000/svg"><a href="javascript:alert(1)"><text>x</text></a></svg>`,
		"href-data-html": `<svg xmlns="http://www.w3.org/2000/svg"><a href="data:text/html,<script>1</script>"><text>x</text></a></svg>`,
		// Security review of PR 139 (c53387): the five bypasses of the first cut.
		"prefixed-script":        `<svg xmlns="http://www.w3.org/2000/svg" xmlns:s="http://www.w3.org/2000/svg"><s:script>alert(1)</s:script></svg>`,
		"xhtml-script":           `<svg xmlns="http://www.w3.org/2000/svg" xmlns:h="http://www.w3.org/1999/xhtml"><h:script>alert(1)</h:script></svg>`,
		"prefixed-foreignObject": `<svg xmlns="http://www.w3.org/2000/svg" xmlns:s="http://www.w3.org/2000/svg"><s:foreignObject><div>x</div></s:foreignObject></svg>`,
		"animate-href":           `<svg xmlns="http://www.w3.org/2000/svg"><a href="#x"><animate attributeName="href" values="javascript:alert(1)"/><text>x</text></a></svg>`,
		"set-href":               `<svg xmlns="http://www.w3.org/2000/svg"><a href="#x"><set attributeName="xlink:href" to="javascript:alert(1)"/><text>x</text></a></svg>`,
		"xhtml-namespace-only":   `<svg xmlns="http://www.w3.org/2000/svg" xmlns:h="HTTP://WWW.W3.ORG/1999/XHTML"><rect/></svg>`,
		// Security re-review (c53403): character references inside attribute values.
		"charref-animate-href": `<svg xmlns="http://www.w3.org/2000/svg"><a href="#x"><animate attributeName="&#104;ref" values="&#106;avascript:alert(1)"/><text>x</text></a></svg>`,
		"charref-set-href-hex": `<svg xmlns="http://www.w3.org/2000/svg"><a href="#x"><set attributeName="h&#x72;ef" to="&#x6a;avascript:alert(1)"/><text>x</text></a></svg>`,
		"charref-xhtml-iframe": `<svg xmlns="http://www.w3.org/2000/svg" xmlns:h="http://www.w3.org/1999/&#120;html"><h:iframe src="&#106;avascript:alert(1)"/></svg>`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			w := f.upload(t, name+".svg", []byte(doc))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "Refused") {
				t.Errorf("refusal reason missing: %s", w.Body.String())
			}
		})
	}
	// Positive control: fragment and data:image hrefs are fine.
	ok := `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink"><defs><g id="s"><path d="M0 0h1v1H0z"/></g></defs><use xlink:href="#s"/><image href="data:image/png;base64,iVBORw0KGgo="/><animate attributeName="opacity" values="0;1"/></svg>`
	if w := f.upload(t, "fine.svg", []byte(ok)); w.Code != http.StatusCreated {
		t.Errorf("benign svg: want 201, got %d body=%s", w.Code, w.Body.String())
	}
	entries, _ := os.ReadDir(filepath.Join(f.dir, "cardgen"))
	if len(entries) != 1 {
		t.Errorf("store holds %d file(s), want only the benign svg", len(entries))
	}
}

func TestCardAssetUpload_RefusesOversizedRasterFromHeader(t *testing.T) {
	f := newCardAssetFixture(t)
	// A PNG header declaring 20000x20000 with no pixel data: DecodeConfig reads
	// the size, a full Decode would fail or allocate 1.6 GB.
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	ihdr := append([]byte("IHDR"), 0, 0, 0x4e, 0x20, 0, 0, 0x4e, 0x20, 8, 6, 0, 0, 0) // 20000 x 20000, 8-bit RGBA
	buf.Write([]byte{0, 0, 0, 13})
	buf.Write(ihdr)
	crc := make([]byte, 4)
	binary.BigEndian.PutUint32(crc, crc32.ChecksumIEEE(ihdr)) // the png decoder verifies chunk CRCs
	buf.Write(crc)
	w := f.upload(t, "bomb.png", buf.Bytes())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "12000") {
		t.Errorf("refusal does not name the cap: %s", w.Body.String())
	}
}

func TestCardAssetUpload_RefusesUnreadableRasterHeader(t *testing.T) {
	f := newCardAssetFixture(t)
	// PNG signature with a zeroed, CRC-less IHDR: sniffed as image/png, but
	// DecodeConfig cannot read a size from it, so the store refuses it rather
	// than record a picture it cannot measure.
	broken := append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, bytes.Repeat([]byte{0}, 40)...)
	w := f.upload(t, "broken.png", broken)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "header could not be read") {
		t.Errorf("refusal reason: %s", w.Body.String())
	}
}

func TestCardAssetUpload_BodyReaderRefusesBeforeParsing(t *testing.T) {
	f := newCardAssetFixture(t)
	// Well over the cap plus slack, as one multipart body.
	big := bytes.Repeat([]byte{0x42}, data_models.MaxCardAssetBytes+cardAssetFormSlack+1024)
	w := f.upload(t, "huge.bin", big)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "byte limit") {
		t.Errorf("refusal does not name the limit: %s", w.Body.String())
	}
}

func TestDetectCardAssetType_RejectsOutsideAllowlist(t *testing.T) {
	for name, head := range map[string][]byte{
		"html":  []byte("<!DOCTYPE html><html><body>hi</body></html>"),
		"xml":   []byte("<?xml version=\"1.0\"?><root/>"),
		"zip":   []byte("PK\x03\x04\x14\x00\x00\x00"),
		"text":  []byte("plain text"),
		"empty": {},
	} {
		if _, _, ok := data_models.DetectCardAssetType(head); ok {
			t.Errorf("%s was accepted", name)
		}
	}
}

func cardAssetID(v uint) string { return strconv.FormatUint(uint64(v), 10) }

// #8647: usage lookup and the delete refusal while referenced.
func TestCardAssetUsage_AndDeleteRefusalWhileReferenced(t *testing.T) {
	f := newCardAssetFixture(t)
	b := decodeUpload(t, f.upload(t, "bg.png", cardPNGBytes(t)))
	id := b.Asset.ID

	w := f.call(t, http.MethodGet, "/cardgen/assets/"+cardAssetID(id)+"/usage")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"templates":[]`) || !strings.Contains(w.Body.String(), `"cards":[]`) {
		t.Fatalf("unused: %d %s", w.Code, w.Body.String())
	}

	tmpl := &data_models.CardTemplate{Slug: "t", Name: "T"}
	f.db.Create(tmpl)
	data_models.AddTemplateVersion(f.db, tmpl, json.RawMessage(`{"elements": {"art": {"kind": "image", "asset_id": `+cardAssetID(id)+`}}}`), "v1", 7)
	ct := data_models.CardType{Slug: "x", Name: "X"}
	f.db.Create(&ct)
	card := data_models.Card{CardTypeID: ct.ID, Slug: "c", Name: "C"}
	f.db.Create(&card)
	data_models.AddCardVersion(f.db, &card, json.RawMessage(`{"values": {"title": {"font_asset_id": `+cardAssetID(id)+`}}}`), "v1", 7)

	w = f.call(t, http.MethodGet, "/cardgen/assets/"+cardAssetID(id)+"/usage")
	var out struct {
		Usage data_models.AssetUsageReport `json:"usage"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Usage.Templates) != 1 || out.Usage.Templates[0].Slug != "t" || out.Usage.Templates[0].Elements[0] != "art" || len(out.Usage.Cards) != 1 || out.Usage.Cards[0].Elements[0] != "title" {
		t.Errorf("usage = %+v", out.Usage)
	}
	w = f.call(t, http.MethodDelete, "/cardgen/assets/"+cardAssetID(id))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"usage"`) {
		t.Errorf("delete while used: %d %s", w.Code, w.Body.String())
	}
	if w := f.call(t, http.MethodGet, "/cardgen/assets/"+cardAssetID(id)); w.Code != http.StatusOK {
		t.Errorf("asset gone after refused delete: %d", w.Code)
	}

	// Drop both references with new versions; delete goes through.
	data_models.AddTemplateVersion(f.db, tmpl, json.RawMessage(`{"elements": {"art": {"kind": "image"}}}`), "v2", 7)
	data_models.AddCardVersion(f.db, &card, json.RawMessage(`{"values": {}}`), "v2", 7)
	if w := f.call(t, http.MethodDelete, "/cardgen/assets/"+cardAssetID(id)); w.Code != http.StatusOK {
		t.Errorf("delete after references dropped: %d %s", w.Code, w.Body.String())
	}
}
