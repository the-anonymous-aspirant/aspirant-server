package invoicing_pdf

import (
	"errors"
	"fmt"
	"math"

	"github.com/signintech/gopdf"
)

// The mark a cancelled invoice carries when it leaves the app (system_3
// #12267, from the #11498 review's C3-12). Its stored PDF is the document as
// it was issued and has no sign of the Storno, so served as it is it reads as
// a valid invoice (§3.132 Decision 1: "A visible "cancelled" stamp on every
// page would be the route"). MarkCancelled draws the mark over a copy of every
// page; the stored bytes are never changed.
const (
	cancelledMarkSize  = 54.0
	cancelledMarkAlpha = 0.45
)

// CancelledMarkText is the mark's words; day is the Berlin day of the Storno,
// written as the invoice writes its dates ("24.11.2026").
func CancelledMarkText(day string) string { return "STORNIERT am " + day }

// MarkCancelled returns the stored PDF with the mark drawn diagonally across
// every page. It returns an error, never the unmarked file, when the copy
// cannot be made: a missing mark must not read as a valid invoice.
func MarkCancelled(stored []byte, day string) (out []byte, err error) {
	if day == "" {
		return nil, errors.New("mark cancelled invoice: no cancellation day")
	}
	// The page importer panics on a file it cannot parse.
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("mark cancelled invoice: %v", r)
		}
	}()
	pdf := &gopdf.GoPdf{}
	pdf.Start(gopdf.Config{PageSize: *gopdf.PageSizeA4})
	if err := pdf.AddTTFFontData("bold", fontBold); err != nil {
		return nil, fmt.Errorf("mark cancelled invoice: load font: %w", err)
	}
	if err := pdf.ImportPagesFromSource(stored, "/MediaBox"); err != nil {
		return nil, fmt.Errorf("mark cancelled invoice: import pages: %w", err)
	}
	pages := pdf.GetNumberOfPages()
	if pages == 0 {
		return nil, errors.New("mark cancelled invoice: the stored PDF has no pages")
	}
	text := CancelledMarkText(day)
	if err := pdf.SetFont("bold", "", cancelledMarkSize); err != nil {
		return nil, fmt.Errorf("mark cancelled invoice: %w", err)
	}
	width, err := pdf.MeasureTextWidth(text)
	if err != nil {
		return nil, fmt.Errorf("mark cancelled invoice: %w", err)
	}
	// Along the diagonal from the lower left to the upper right, centred.
	angle := math.Atan2(pageH, pageW) * 180 / math.Pi
	cx, cy := pageW/2, pageH/2
	for p := 1; p <= pages; p++ {
		if err := pdf.SetPage(p); err != nil {
			return nil, fmt.Errorf("mark cancelled invoice: page %d: %w", p, err)
		}
		if err := pdf.SetTransparency(gopdf.Transparency{Alpha: cancelledMarkAlpha, BlendModeType: gopdf.NormalBlendMode}); err != nil {
			return nil, fmt.Errorf("mark cancelled invoice: page %d: %w", p, err)
		}
		pdf.SetTextColor(178, 24, 24)
		pdf.Rotate(angle, cx, cy)
		pdf.SetXY(cx-width/2, cy+cancelledMarkSize*0.35)
		if err := pdf.Text(text); err != nil {
			return nil, fmt.Errorf("mark cancelled invoice: page %d: %w", p, err)
		}
		pdf.RotateReset()
		pdf.ClearTransparency()
	}
	return pdf.GetBytesPdfReturnErr()
}
