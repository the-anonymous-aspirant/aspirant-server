package invoicing_pdf

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"aspirant-online/server/data_models"
)

// recordingCanvas captures what layout draws, so the test asserts on the
// printed text without parsing a PDF.
type recordingCanvas struct {
	texts []string
	pages int
	size  float64
}

func (r *recordingCanvas) SetFont(bold bool, size float64) error { r.size = size; return nil }
func (r *recordingCanvas) Text(x, y float64, s string) error {
	if x < 0 || x > pageW || y < 0 || y > pageH {
		return fmt.Errorf("text %q drawn off the page at (%.0f, %.0f)", s, x, y)
	}
	r.texts = append(r.texts, s)
	return nil
}
func (r *recordingCanvas) TextWidth(s string) (float64, error) {
	return float64(len([]rune(s))) * r.size * 0.5, nil
}
func (r *recordingCanvas) Wrap(s string, width float64) ([]string, error) {
	var lines []string
	cur := ""
	for _, w := range strings.Fields(s) {
		next := strings.TrimSpace(cur + " " + w)
		if float64(len([]rune(next)))*r.size*0.5 > width && cur != "" {
			lines = append(lines, cur)
			next = w
		}
		cur = next
	}
	return append(lines, cur), nil
}
func (r *recordingCanvas) Line(x1, y1, x2, y2 float64) {}
func (r *recordingCanvas) NewPage()                    { r.pages++ }

func TestLayout_DrawsEveryDocumentLine(t *testing.T) {
	d, err := Build(issuedInput(true))
	if err != nil {
		t.Fatal(err)
	}
	rc := &recordingCanvas{}
	rc.NewPage()
	if err := layout(rc, d); err != nil {
		t.Fatalf("layout: %v", err)
	}
	drawn := strings.Join(rc.texts, "\n")
	var want []string
	want = append(want, d.Issuer...)
	want = append(want, d.Recipient...)
	want = append(want, d.Title)
	for _, f := range d.Meta {
		want = append(want, f.Label+":", f.Value)
	}
	for _, it := range d.Items {
		want = append(want, it.Pos, it.Quantity, it.UnitPrice, it.Total)
	}
	want = append(want, ItemHeaders[:]...)
	for _, f := range d.Totals {
		want = append(want, f.Label+":", f.Value)
	}
	for _, w := range want {
		if !strings.Contains(drawn, w) {
			t.Errorf("not drawn: %q", w)
		}
	}
	// Wrapped notes reassemble to the exact sentence.
	joined := strings.Join(rc.texts, " ")
	for _, n := range d.Notes {
		if !strings.Contains(joined, n) {
			t.Errorf("legal sentence not drawn intact: %q", n)
		}
	}
	if rc.pages != 1 {
		t.Errorf("a short invoice took %d pages", rc.pages)
	}
}

func TestLayout_LongInvoiceBreaksPagesAndKeepsEveryItem(t *testing.T) {
	in := issuedInput(true)
	in.Items = nil
	for i := 0; i < 40; i++ {
		in.Items = append(in.Items, data_models.InvoicingLineItem{Description: fmt.Sprintf("Position %02d with a description long enough to wrap onto a second line in the table", i+1), QuantityMilli: 1000, UnitPriceCents: 100, LineTotalCents: 100})
	}
	d, _ := Build(in)
	rc := &recordingCanvas{}
	rc.NewPage()
	if err := layout(rc, d); err != nil {
		t.Fatalf("layout: %v", err)
	}
	if rc.pages < 2 {
		t.Fatalf("40 items fit on %d page(s)", rc.pages)
	}
	joined := strings.Join(rc.texts, " ")
	for i := 1; i <= 40; i++ {
		if !strings.Contains(joined, fmt.Sprintf("Position %02d", i)) {
			t.Errorf("item %d lost across the page break", i)
		}
	}
}

func TestRender_ProducesARealA4PDF(t *testing.T) {
	d, _ := Build(issuedInput(true))
	pdf, err := Render(d)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) || !bytes.Contains(pdf, []byte("%%EOF")) {
		t.Fatalf("not a PDF: %q…", pdf[:16])
	}
	pages := regexp.MustCompile(`/Type\s*/Page[^s]`).FindAll(pdf, -1)
	if len(pages) != 1 {
		t.Errorf("pages: %d", len(pages))
	}
	if !bytes.Contains(pdf, []byte("/MediaBox [ 0 0 595.00 842.00 ]")) {
		t.Errorf("no A4 media box")
	}
	// The TTF is embedded with a ToUnicode map, so the text is extractable
	// (a caseworker can search and copy it).
	if !bytes.Contains(pdf, []byte("/FontFile2")) || !bytes.Contains(pdf, []byte("/ToUnicode")) {
		t.Errorf("the font is not embedded with a ToUnicode map")
	}
}
