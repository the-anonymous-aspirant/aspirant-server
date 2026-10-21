package invoicing_pdf

import (
	_ "embed"
	"fmt"
	"strings"

	"github.com/signintech/gopdf"
)

// Liberation Sans (SIL OFL 1.1, fonts/LICENSE-OFL.txt) is embedded so the
// invoice prints any name exactly — umlauts, ß, §, €, and letters outside
// Latin-1 — on every runtime image, with nothing to install.
var (
	//go:embed fonts/LiberationSans-Regular.ttf
	fontRegular []byte
	//go:embed fonts/LiberationSans-Bold.ttf
	fontBold []byte
)

// A4 in points, with the layout's margins and type sizes.
const (
	pageW, pageH   = 595.0, 842.0
	marginX        = 56.0
	marginTop      = 56.0
	marginBottom   = 64.0
	contentW       = pageW - 2*marginX
	bodySize       = 10.0
	smallSize      = 8.5
	titleSize      = 20.0
	lineGap        = 14.0
	recipientTop   = 150.0
	itemTableGap   = 22.0
	colPosW        = 28.0
	colQtyW        = 46.0
	colPriceW      = 80.0
	colTotalW      = 80.0
	colDescW       = contentW - colPosW - colQtyW - colPriceW - colTotalW
	notesWrapWidth = contentW
)

// canvas is the drawing surface Render needs. The gopdf adapter draws the
// real PDF; tests record the calls to assert exactly what is printed.
type canvas interface {
	SetFont(bold bool, size float64) error
	Text(x, y float64, s string) error
	TextWidth(s string) (float64, error)
	Wrap(s string, width float64) ([]string, error)
	Line(x1, y1, x2, y2 float64)
	NewPage()
}

// UnprintableError reports characters the embedded font has no glyph for.
// gopdf would silently drop them, printing a name with letters missing — an
// altered invoice — so Render refuses instead (@security N5, c51481 on #8373).
type UnprintableError struct {
	Runes []rune
}

func (e *UnprintableError) Error() string {
	return fmt.Sprintf("the invoice font cannot print %q", string(e.Runes))
}

// Render lays the document out on A4 and returns the PDF bytes. It fails
// with *UnprintableError when any printed text uses a character the font
// lacks.
func Render(doc Document) ([]byte, error) {
	var missing []rune
	seen := map[rune]bool{}
	onMissing := func(r rune) {
		// Line breaks and tabs are layout, not glyphs; the layout splits on them.
		if r < 0x20 || seen[r] {
			return
		}
		seen[r] = true
		missing = append(missing, r)
	}
	pdf := &gopdf.GoPdf{}
	pdf.Start(gopdf.Config{PageSize: *gopdf.PageSizeA4})
	if err := pdf.AddTTFFontDataWithOption("regular", fontRegular, gopdf.TtfOption{OnGlyphNotFound: onMissing}); err != nil {
		return nil, fmt.Errorf("load regular font: %w", err)
	}
	if err := pdf.AddTTFFontDataWithOption("bold", fontBold, gopdf.TtfOption{OnGlyphNotFound: onMissing}); err != nil {
		return nil, fmt.Errorf("load bold font: %w", err)
	}
	pdf.SetInfo(gopdf.PdfInfo{Title: doc.Title + " " + metaValue(doc, labelRechnungsnummer), Author: first(doc.Issuer)})
	c := &gopdfCanvas{pdf: pdf}
	c.NewPage()
	if err := layout(c, doc); err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		return nil, &UnprintableError{Runes: missing}
	}
	return pdf.GetBytesPdf(), nil
}

func first(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return lines[0]
}

func metaValue(doc Document, label string) string {
	for _, f := range doc.Meta {
		if f.Label == label {
			return f.Value
		}
	}
	return ""
}

// cursor tracks the vertical position and breaks pages.
type cursor struct {
	c canvas
	y float64
}

func (k *cursor) need(h float64) {
	if k.y+h > pageH-marginBottom {
		k.c.NewPage()
		k.y = marginTop
	}
}

func (k *cursor) text(x float64, s string, bold bool, size float64) error {
	if err := k.c.SetFont(bold, size); err != nil {
		return err
	}
	return k.c.Text(x, k.y, s)
}

func (k *cursor) rightText(right float64, s string, bold bool, size float64) error {
	if err := k.c.SetFont(bold, size); err != nil {
		return err
	}
	w, err := k.c.TextWidth(s)
	if err != nil {
		return err
	}
	return k.c.Text(right-w, k.y, s)
}

func layout(c canvas, doc Document) error {
	k := &cursor{c: c, y: marginTop}

	// Issuer, top left, with the Steuernummer.
	for i, l := range doc.Issuer {
		if err := k.text(marginX, l, i == 0, bodySize); err != nil {
			return err
		}
		k.y += lineGap
	}

	// Recipient block where a window envelope shows it.
	if k.y < recipientTop {
		k.y = recipientTop
	}
	for _, l := range doc.Recipient {
		if err := k.text(marginX, l, false, bodySize); err != nil {
			return err
		}
		k.y += lineGap
	}

	// Title, then number and dates.
	k.y += 2 * lineGap
	if err := k.text(marginX, doc.Title, true, titleSize); err != nil {
		return err
	}
	k.y += titleSize + 8
	for _, f := range doc.Meta {
		if err := k.text(marginX, f.Label+":", false, bodySize); err != nil {
			return err
		}
		if err := k.text(marginX+120, f.Value, false, bodySize); err != nil {
			return err
		}
		k.y += lineGap
	}

	// Line items.
	k.y += itemTableGap
	xPos := marginX
	xDesc := xPos + colPosW
	rQty := xDesc + colDescW + colQtyW
	rPrice := rQty + colPriceW
	rTotal := rPrice + colTotalW
	header := func() error {
		if err := k.text(xPos, ItemHeaders[0], true, bodySize); err != nil {
			return err
		}
		if err := k.text(xDesc, ItemHeaders[1], true, bodySize); err != nil {
			return err
		}
		for i, r := range []float64{rQty, rPrice, rTotal} {
			if err := k.rightText(r, ItemHeaders[2+i], true, bodySize); err != nil {
				return err
			}
		}
		k.y += lineGap
		c.Line(marginX, k.y-3, marginX+contentW, k.y-3)
		k.y += 4
		return nil
	}
	k.need(3 * lineGap)
	if err := header(); err != nil {
		return err
	}
	for _, it := range doc.Items {
		if err := c.SetFont(false, bodySize); err != nil {
			return err
		}
		desc, err := wrapLines(c, it.Description, colDescW-8)
		if err != nil {
			return err
		}
		if len(desc) == 0 {
			desc = []string{""}
		}
		before := k.y
		k.need(float64(len(desc)) * lineGap)
		if k.y < before {
			if err := header(); err != nil {
				return err
			}
		}
		if err := k.text(xPos, it.Pos, false, bodySize); err != nil {
			return err
		}
		for i, r := range []float64{rQty, rPrice, rTotal} {
			if err := k.rightText(r, []string{it.Quantity, it.UnitPrice, it.Total}[i], false, bodySize); err != nil {
				return err
			}
		}
		for _, l := range desc {
			if err := k.text(xDesc, l, false, bodySize); err != nil {
				return err
			}
			k.y += lineGap
		}
	}
	c.Line(marginX, k.y-3, marginX+contentW, k.y-3)
	k.y += 6

	// Totals, right-aligned under the table.
	k.need(float64(len(doc.Totals)) * lineGap)
	for i, f := range doc.Totals {
		bold := i == len(doc.Totals)-1
		if err := k.rightText(rPrice, f.Label+":", bold, bodySize); err != nil {
			return err
		}
		if err := k.rightText(rTotal, f.Value, bold, bodySize); err != nil {
			return err
		}
		k.y += lineGap
	}

	// § 19 line, payment confirmations, retroactive note.
	k.y += lineGap
	for _, n := range doc.Notes {
		if err := c.SetFont(false, bodySize); err != nil {
			return err
		}
		lines, err := wrapLines(c, n, notesWrapWidth)
		if err != nil {
			return err
		}
		k.need(float64(len(lines)) * lineGap)
		for _, l := range lines {
			if err := k.text(marginX, l, false, bodySize); err != nil {
				return err
			}
			k.y += lineGap
		}
		k.y += 4
	}
	return nil
}

// wrapLines keeps the author's own line breaks and wraps each line to width.
func wrapLines(c canvas, s string, width float64) ([]string, error) {
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		wrapped, err := c.Wrap(strings.ReplaceAll(line, "\t", " "), width)
		if err != nil {
			return nil, err
		}
		out = append(out, wrapped...)
	}
	return out, nil
}

// gopdfCanvas draws on a gopdf document.
type gopdfCanvas struct {
	pdf *gopdf.GoPdf
}

func (g *gopdfCanvas) SetFont(bold bool, size float64) error {
	family := "regular"
	if bold {
		family = "bold"
	}
	return g.pdf.SetFont(family, "", size)
}

func (g *gopdfCanvas) Text(x, y float64, s string) error {
	g.pdf.SetXY(x, y)
	return g.pdf.Cell(nil, s)
}

func (g *gopdfCanvas) TextWidth(s string) (float64, error) {
	return g.pdf.MeasureTextWidth(s)
}

func (g *gopdfCanvas) Wrap(s string, width float64) ([]string, error) {
	if s == "" {
		return []string{""}, nil
	}
	return g.pdf.SplitTextWithWordWrap(s, width)
}

func (g *gopdfCanvas) Line(x1, y1, x2, y2 float64) {
	g.pdf.SetLineWidth(0.5)
	g.pdf.Line(x1, y1, x2, y2)
}

func (g *gopdfCanvas) NewPage() {
	g.pdf.AddPage()
}
