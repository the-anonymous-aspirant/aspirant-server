package invoicing_pdf

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"aspirant-online/server/data_models"
)

// pageTexts extracts each page's text with pdftotext (poppler-utils, which
// the verify image installs), in the order the page draws it: the mark is
// drawn on a slant, and reading order would scatter its words among the
// invoice's own.
func pageTexts(t *testing.T, pdf []byte, pages int) []string {
	t.Helper()
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Fatalf("pdftotext is not installed; this test reads the marked PDF (the verify surface installs poppler-utils)")
	}
	var out []string
	for p := 1; p <= pages; p++ {
		cmd := exec.Command("pdftotext", "-raw", "-f", fmt.Sprint(p), "-l", fmt.Sprint(p), "-", "-")
		cmd.Stdin = bytes.NewReader(pdf)
		text, err := cmd.Output()
		if err != nil {
			t.Fatalf("pdftotext page %d: %v", p, err)
		}
		out = append(out, strings.Join(strings.Fields(string(text)), " "))
	}
	return out
}

// pageCount reads the page count with pdfinfo.
func pageCount(t *testing.T, pdf []byte) int {
	t.Helper()
	path := filepath.Join(t.TempDir(), "p.pdf")
	if err := os.WriteFile(path, pdf, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("pdfinfo", path).Output()
	if err != nil {
		t.Fatalf("pdfinfo: %v", err)
	}
	m := regexp.MustCompile(`(?m)^Pages:\s+(\d+)`).FindSubmatch(out)
	if m == nil {
		t.Fatalf("pdfinfo: no page count in %s", out)
	}
	n, _ := strconv.Atoi(string(m[1]))
	return n
}

func longInvoice(t *testing.T) []byte {
	t.Helper()
	in := issuedInput(true)
	in.Items = nil
	for i := 0; i < 40; i++ {
		in.Items = append(in.Items, data_models.InvoicingLineItem{Description: fmt.Sprintf("Position %02d with a description long enough to wrap onto a second line in the table", i+1), QuantityMilli: 1000, UnitPriceCents: 100, LineTotalCents: 100})
	}
	d, _ := Build(in)
	pdf, err := Render(d)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return pdf
}

func TestMarkCancelled_EveryPageCarriesTheMarkAndTheInvoice(t *testing.T) {
	stored := longInvoice(t)
	kept := append([]byte(nil), stored...)
	n := pageCount(t, stored)
	if n < 2 {
		t.Fatalf("the long invoice has %d page(s)", n)
	}
	storedPages := pageTexts(t, stored, n)
	if strings.Contains(strings.Join(storedPages, " "), "STORNIERT") {
		t.Fatalf("the stored invoice already reads STORNIERT")
	}

	marked, err := MarkCancelled(stored, "24.11.2026")
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	if !bytes.Equal(stored, kept) {
		t.Fatalf("marking changed the stored bytes")
	}
	if !bytes.HasPrefix(marked, []byte("%PDF-")) {
		t.Fatalf("not a PDF: %q", marked[:16])
	}
	if got := pageCount(t, marked); got != n {
		t.Fatalf("the marked copy has %d pages, the stored PDF %d", got, n)
	}
	pages := pageTexts(t, marked, n)
	for i, text := range pages {
		// pdftotext spaces a slanted line's letters apart, so the mark is
		// compared with the spaces taken out.
		if !strings.Contains(strings.ReplaceAll(text, " ", ""), "STORNIERTam24.11.2026") {
			t.Errorf("page %d has no mark: %.200s", i+1, text)
		}
		// The invoice under the mark is still all there.
		for _, word := range strings.Fields(storedPages[i]) {
			if !strings.Contains(text, word) {
				t.Errorf("page %d lost %q under the mark", i+1, word)
				break
			}
		}
	}
}

// renderPages rasterises every page with pdftoppm into P6 images.
func renderPages(t *testing.T, pdf []byte) [][]byte {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "m.pdf")
	if err := os.WriteFile(path, pdf, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("pdftoppm", "-r", "40", path, filepath.Join(dir, "p")).CombinedOutput(); err != nil {
		t.Fatalf("pdftoppm: %v %s", err, out)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "p-*.ppm"))
	var pages [][]byte
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		// P6 <w> <h> <max> then RGB bytes.
		fields := 0
		i := 0
		for fields < 4 && i < len(b) {
			for i < len(b) && (b[i] == ' ' || b[i] == '\n') {
				i++
			}
			for i < len(b) && b[i] != ' ' && b[i] != '\n' {
				i++
			}
			fields++
		}
		pages = append(pages, b[i+1:])
	}
	return pages
}

func near(a, b byte) bool { return a >= b-24 && a <= b+24 }

// The mark is see-through: on white it is a pale red, and nowhere is it the
// full red, which would hide the amounts it crosses.
func TestMarkCancelled_TheInvoiceStaysLegibleUnderTheMark(t *testing.T) {
	marked, err := MarkCancelled(longInvoice(t), "24.11.2026")
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	pages := renderPages(t, marked)
	if len(pages) < 2 {
		t.Fatalf("rendered %d pages", len(pages))
	}
	for n, rgb := range pages {
		tint, full := 0, 0
		for i := 0; i+2 < len(rgb); i += 3 {
			r, g, b := rgb[i], rgb[i+1], rgb[i+2]
			if near(r, 178) && near(g, 24) && near(b, 24) {
				full++
			}
			if r > 200 && g > 120 && g < 190 && b > 120 && b < 190 && r > g+30 {
				tint++
			}
		}
		if tint < 200 {
			t.Errorf("page %d: no pale red mark (%d pixels)", n+1, tint)
		}
		if full > 0 {
			t.Errorf("page %d: %d pixels of the mark are opaque", n+1, full)
		}
	}
}

func TestMarkCancelled_RefusesWhenTheCopyCannotBeMade(t *testing.T) {
	if _, err := MarkCancelled([]byte("not a pdf"), "24.11.2026"); err == nil {
		t.Errorf("a file that is not a PDF was marked")
	}
	d, _ := Build(issuedInput(true))
	pdf, err := Render(d)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if out, err := MarkCancelled(pdf, ""); err == nil || out != nil {
		t.Errorf("marked with no day: %v", err)
	}
}
