package invoicing_pdf

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

// system_3 #9858: the issuer's logo, normalized on upload and printed top
// right. Every fixture is encoded by the standard library, so the bytes are
// valid files with correct checksums.

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// inkOnCanvas is a transparent w×h canvas (white under zero alpha, as design
// tools export it) with an opaque black rectangle over ink.
func inkOnCanvas(w, h int, ink image.Rectangle) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.NRGBA{255, 255, 255, 0}
			if image.Pt(x, y).In(ink) {
				c = color.NRGBA{0, 0, 0, 255}
			}
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

func decodeLogo(t *testing.T, l Logo) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(l.PNG))
	if err != nil {
		t.Fatalf("normalized logo is not a PNG: %v", err)
	}
	if img.Bounds().Dx() != l.Width || img.Bounds().Dy() != l.Height {
		t.Fatalf("reported %d×%d, PNG is %v", l.Width, l.Height, img.Bounds())
	}
	return img
}

func TestNormalizeLogo_TrimsTheTransparentMarginAndDownscales(t *testing.T) {
	raw := encodePNG(t, inkOnCanvas(2000, 1000, image.Rect(500, 200, 1700, 800)))
	l, err := NormalizeLogo(raw)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	// 1200×600 of ink, scaled to 600 on the long side.
	if l.Width != 600 || l.Height != 300 {
		t.Fatalf("got %d×%d, want 600×300", l.Width, l.Height)
	}
	img := decodeLogo(t, l)
	for _, p := range []image.Point{{0, 0}, {599, 0}, {0, 299}, {599, 299}, {300, 150}} {
		if c := color.NRGBAModel.Convert(img.At(p.X, p.Y)).(color.NRGBA); c != (color.NRGBA{0, 0, 0, 255}) {
			t.Errorf("pixel %v is %v, want opaque black: the margin was not trimmed", p, c)
		}
	}
}

func TestNormalizeLogo_LeavesASmallLogoUnscaled(t *testing.T) {
	l, err := NormalizeLogo(encodePNG(t, inkOnCanvas(120, 80, image.Rect(0, 0, 120, 80))))
	if err != nil {
		t.Fatal(err)
	}
	if l.Width != 120 || l.Height != 80 {
		t.Errorf("got %d×%d, want 120×80", l.Width, l.Height)
	}
}

func TestNormalizeLogo_KeepsTheAspectOfATallLogo(t *testing.T) {
	l, err := NormalizeLogo(encodePNG(t, inkOnCanvas(700, 1400, image.Rect(0, 0, 700, 1400))))
	if err != nil {
		t.Fatal(err)
	}
	if l.Width != 300 || l.Height != 600 {
		t.Errorf("got %d×%d, want 300×600", l.Width, l.Height)
	}
}

func TestNormalizeLogo_AcceptsJPEG(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 800, 400))
	for i := range img.Pix {
		img.Pix[i] = 200
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	l, err := NormalizeLogo(b.Bytes())
	if err != nil {
		t.Fatalf("JPEG refused: %v", err)
	}
	decodeLogo(t, l)
	if l.Width != 600 || l.Height != 300 {
		t.Errorf("got %d×%d, want 600×300", l.Width, l.Height)
	}
}

// Averaging premultiplied pixels: a transparent pixel adds nothing, so the
// edge of black ink stays black at part opacity, not grey.
func TestDownscale_NoLightFringeAtTheInkEdge(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 2, 1))
	// Left: transparent (white under zero alpha, premultiplied to zero).
	// Right: opaque black.
	src.Pix = []uint8{0, 0, 0, 0, 0, 0, 0, 255}
	out := downscale(src, 1)
	c := color.NRGBAModel.Convert(out.At(0, 0)).(color.NRGBA)
	if c.A < 120 || c.A > 135 || c.R != 0 || c.G != 0 || c.B != 0 {
		t.Errorf("edge pixel %v, want black at about half opacity", c)
	}
}

func TestNormalizeLogo_DropsEmbeddedMetadata(t *testing.T) {
	raw := encodePNG(t, inkOnCanvas(10, 10, image.Rect(0, 0, 10, 10)))
	// Insert a tEXt chunk after IHDR (8-byte signature + 25-byte IHDR).
	text := []byte("Comment\x00private-note-xyz")
	raw = append(append(append([]byte{}, raw[:33]...), pngChunk("tEXt", text)...), raw[33:]...)
	if _, err := png.Decode(bytes.NewReader(raw)); err != nil {
		t.Fatalf("fixture is not a valid PNG: %v", err)
	}
	l, err := NormalizeLogo(raw)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(l.PNG, []byte("private-note-xyz")) {
		t.Errorf("the normalized logo still carries the uploaded metadata")
	}
}

func pngChunk(typ string, data []byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint32(len(data)))
	b.WriteString(typ)
	b.Write(data)
	crc := crc32.NewIEEE()
	crc.Write([]byte(typ))
	crc.Write(data)
	binary.Write(&b, binary.BigEndian, crc.Sum32())
	return b.Bytes()
}

// hugeHeaderPNG is a valid 1×1 PNG whose IHDR claims w×h: a decompression
// bomb's shape. Only the header is rewritten, with its CRC recomputed.
func hugeHeaderPNG(t *testing.T, w, h uint32) []byte {
	raw := encodePNG(t, image.NewNRGBA(image.Rect(0, 0, 1, 1)))
	ihdr := append([]byte{}, raw[16:29]...) // IHDR data: 13 bytes after length+type
	binary.BigEndian.PutUint32(ihdr[0:4], w)
	binary.BigEndian.PutUint32(ihdr[4:8], h)
	return append(append(append([]byte{}, raw[:8]...), pngChunk("IHDR", ihdr)...), raw[33:]...)
}

func TestNormalizeLogo_Refusals(t *testing.T) {
	var gifBuf bytes.Buffer
	if err := gif.Encode(&gifBuf, image.NewPaletted(image.Rect(0, 0, 4, 4), []color.Color{color.Black, color.White}), nil); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		{"empty", nil, "empty"},
		{"gif", gifBuf.Bytes(), "PNG or JPEG"},
		{"text", []byte("<svg xmlns='http://www.w3.org/2000/svg'/>"), "PNG or JPEG"},
		{"fully transparent", encodePNG(t, inkOnCanvas(50, 50, image.Rectangle{})), "fully transparent"},
		{"over the pixel cap", hugeHeaderPNG(t, 20000, 20000), "20000×20000"},
		{"over the side cap", hugeHeaderPNG(t, 9000, 10), "9000×10"},
		{"truncated", encodePNG(t, inkOnCanvas(50, 50, image.Rect(0, 0, 50, 50)))[:60], "could not be read"},
		{"over the byte cap", append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, MaxLogoBytes)...), "larger than 5 MB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormalizeLogo(tc.raw)
			if !IsLogoError(err) {
				t.Fatalf("got %v, want a LogoError", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("message %q does not name %q", err.Error(), tc.want)
			}
		})
	}
}

func logoFixture(t *testing.T, w, h int) *Logo {
	l, err := NormalizeLogo(encodePNG(t, inkOnCanvas(w, h, image.Rect(0, 0, w, h))))
	if err != nil {
		t.Fatal(err)
	}
	return &l
}

func TestLayout_LogoSitsTopRightAboveTheRecipient(t *testing.T) {
	for _, tc := range []struct {
		name  string
		w, h  int
		wantW float64
		wantH float64
	}{
		{"wide", 600, 300, 140, 70},
		{"tall", 300, 600, 36, 72},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := issuedInput(true)
			in.Logo = logoFixture(t, tc.w, tc.h)
			d, _ := Build(in)
			rc := &recordingCanvas{}
			rc.NewPage()
			if err := layout(rc, d); err != nil {
				t.Fatal(err)
			}
			if len(rc.images) != 1 {
				t.Fatalf("drew %d images, want the logo once", len(rc.images))
			}
			im := rc.images[0]
			if im.page != 1 || !bytes.Equal(im.png, in.Logo.PNG) {
				t.Errorf("logo on page %d (want 1), or not the stored bytes", im.page)
			}
			if abs(im.w-tc.wantW) > 0.01 || abs(im.h-tc.wantH) > 0.01 {
				t.Errorf("drawn %.1f×%.1f, want %.0f×%.0f", im.w, im.h, tc.wantW, tc.wantH)
			}
			if abs(im.x+im.w-(marginX+contentW)) > 0.01 || im.y != logoTop {
				t.Errorf("logo at (%.1f, %.1f)–(%.1f), want right edge on the margin at %.0f, top %.0f", im.x, im.y, im.x+im.w, marginX+contentW, logoTop)
			}
			if im.y+im.h > recipientTop-20 {
				t.Errorf("logo ends at %.1f, too close to the recipient line at %.0f", im.y+im.h, recipientTop)
			}
			for _, p := range rc.placed {
				if p.s == d.Recipient[0] && p.y != recipientTop {
					t.Errorf("recipient moved to y=%.1f", p.y)
				}
			}
		})
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// A long issuer line wraps short of the logo instead of running under it.
func TestLayout_IssuerTextStaysClearOfTheLogo(t *testing.T) {
	in := issuedInput(true)
	in.Profile.Address = "Hinterhaus, Seitenflügel links, dritter Stock, bei Beispiel-Mustermann\n10117 Berlin"
	in.Logo = logoFixture(t, 600, 300)
	d, _ := Build(in)
	rc := &recordingCanvas{}
	rc.NewPage()
	if err := layout(rc, d); err != nil {
		t.Fatal(err)
	}
	im := rc.images[0]
	joined := ""
	for _, p := range rc.placed {
		if p.s == d.Recipient[0] {
			break
		}
		rc.size = p.size
		w, _ := rc.TextWidth(p.s)
		if p.y < im.y+im.h && p.x+w > im.x-logoGap+0.01 {
			t.Errorf("issuer text %q reaches x=%.1f, under the logo at %.1f", p.s, p.x+w, im.x)
		}
		joined += p.s + " "
	}
	if !strings.Contains(joined, "Hinterhaus, Seitenflügel links,") || !strings.Contains(joined, "Beispiel-Mustermann") {
		t.Errorf("the wrapped address lost words: %q", joined)
	}
}

func TestLayout_NoLogoDrawsNoImageAndKeepsTheHeader(t *testing.T) {
	d, _ := Build(issuedInput(true))
	rc := &recordingCanvas{}
	rc.NewPage()
	if err := layout(rc, d); err != nil {
		t.Fatal(err)
	}
	if len(rc.images) != 0 {
		t.Errorf("drew %d images without a logo", len(rc.images))
	}
	for i, l := range d.Issuer {
		p := rc.placed[i]
		if p.s != l || p.x != marginX || p.y != marginTop+float64(i)*lineGap {
			t.Errorf("issuer line %d: %q at (%.0f, %.0f), want %q at (%.0f, %.0f)", i, p.s, p.x, p.y, l, marginX, marginTop+float64(i)*lineGap)
		}
	}
}

func TestRender_EmbedsTheLogoWithItsTransparency(t *testing.T) {
	in := issuedInput(true)
	// One part-transparent pixel inside the ink, so the normalized PNG keeps
	// an alpha channel (an all-opaque one is encoded as plain RGB).
	soft := inkOnCanvas(40, 20, image.Rect(0, 0, 40, 20))
	soft.SetNRGBA(0, 0, color.NRGBA{0, 0, 0, 100})
	l, err := NormalizeLogo(encodePNG(t, soft))
	if err != nil {
		t.Fatal(err)
	}
	in.Logo = &l
	d, _ := Build(in)
	pdf, err := Render(d)
	if err != nil {
		t.Fatalf("render with a logo: %v", err)
	}
	for _, want := range []string{"/Subtype /Image", "/SMask"} {
		if !bytes.Contains(pdf, []byte(want)) {
			t.Errorf("the PDF has no %s", want)
		}
	}
	plain, _ := Build(issuedInput(true))
	without, err := Render(plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(without, []byte("/Subtype /Image")) {
		t.Errorf("an invoice without a logo carries an image")
	}
}
