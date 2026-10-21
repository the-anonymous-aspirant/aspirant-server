package handlers

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
)

// A minimal .xlsx writer for the invoicing exports (system_3 #8367-E1). An
// xlsx file is a zip of a handful of SpreadsheetML parts; four plain tables
// need nothing more, so this avoids a spreadsheet dependency.
//
// Text is always written as an inline string (t="inlineStr"), which Excel and
// LibreOffice never evaluate as a formula — text written by an outside payer
// stays text. Numbers are numeric cells so the sheet can sum them.

type xlsxCell struct {
	Text   string
	Number float64
	IsNum  bool
}

func xlsxText(s string) xlsxCell     { return xlsxCell{Text: spreadsheetSafe(s)} }
func xlsxNumber(f float64) xlsxCell  { return xlsxCell{Number: f, IsNum: true} }
func xlsxMoney(cents int64) xlsxCell { return xlsxNumber(float64(cents) / 100) }

type xlsxSheet struct {
	Name   string
	Header []string
	Rows   [][]xlsxCell
}

func xmlEscape(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// xlsxColumn turns a 0-based index into a column name: 0 → A, 26 → AA.
func xlsxColumn(i int) string {
	name := ""
	for i++; i > 0; i = (i - 1) / 26 {
		name = string(rune('A'+(i-1)%26)) + name
	}
	return name
}

// xlsxSheetName applies Excel's rules: at most 31 characters, none of : \ / ? * [ ].
func xlsxSheetName(s string) string {
	s = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`:\/?*[]`, r) {
			return '_'
		}
		return r
	}, s)
	if r := []rune(s); len(r) > 31 {
		s = string(r[:31])
	}
	return s
}

func writeXLSXSheet(sh xlsxSheet) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	cell := func(row, col int, c xlsxCell, style int) {
		ref := fmt.Sprintf("%s%d", xlsxColumn(col), row)
		styleAttr := ""
		if style > 0 {
			styleAttr = fmt.Sprintf(` s="%d"`, style)
		}
		if c.IsNum {
			fmt.Fprintf(&b, `<c r="%s"%s><v>%s</v></c>`, ref, styleAttr, strconv.FormatFloat(c.Number, 'f', -1, 64))
			return
		}
		fmt.Fprintf(&b, `<c r="%s" t="inlineStr"%s><is><t xml:space="preserve">%s</t></is></c>`, ref, styleAttr, xmlEscape(c.Text))
	}
	b.WriteString(`<row r="1">`)
	for i, h := range sh.Header {
		cell(1, i, xlsxCell{Text: h}, 1)
	}
	b.WriteString(`</row>`)
	for r, row := range sh.Rows {
		fmt.Fprintf(&b, `<row r="%d">`, r+2)
		for i, c := range row {
			cell(r+2, i, c, 0)
		}
		b.WriteString(`</row>`)
	}
	b.WriteString(`</sheetData></worksheet>`)
	return b.String()
}

// writeXLSX packs the sheets into a workbook.
func writeXLSX(sheets []xlsxSheet) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name, content string) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write([]byte(content))
		return err
	}

	var overrides, workbookSheets, rels strings.Builder
	for i, sh := range sheets {
		n := i + 1
		fmt.Fprintf(&overrides, `<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, n)
		fmt.Fprintf(&workbookSheets, `<sheet name="%s" sheetId="%d" r:id="rId%d"/>`, xmlEscape(xlsxSheetName(sh.Name)), n, n)
		fmt.Fprintf(&rels, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, n, n)
	}
	styleRel := len(sheets) + 1
	parts := []struct{ name, content string }{
		{"[Content_Types].xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
			`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
			`<Default Extension="xml" ContentType="application/xml"/>` +
			`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
			`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>` +
			overrides.String() + `</Types>`},
		{"_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>` +
			`</Relationships>`},
		{"xl/workbook.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">` +
			`<sheets>` + workbookSheets.String() + `</sheets></workbook>`},
		{"xl/_rels/workbook.xml.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` + rels.String() +
			fmt.Sprintf(`<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>`, styleRel) +
			`</Relationships>`},
		// Style 0 is the default; style 1 is the bold header row.
		{"xl/styles.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
			`<fonts count="2"><font><sz val="11"/><name val="Calibri"/></font><font><b/><sz val="11"/><name val="Calibri"/></font></fonts>` +
			`<fills count="2"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill></fills>` +
			`<borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders>` +
			`<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>` +
			`<cellXfs count="2"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/><xf numFmtId="0" fontId="1" fillId="0" borderId="0" xfId="0" applyFont="1"/></cellXfs>` +
			`</styleSheet>`},
	}
	for _, p := range parts {
		if err := add(p.name, p.content); err != nil {
			return nil, err
		}
	}
	for i, sh := range sheets {
		if err := add(fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1), writeXLSXSheet(sh)); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
