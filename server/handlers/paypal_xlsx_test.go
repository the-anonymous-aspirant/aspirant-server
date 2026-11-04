package handlers

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// #9862-A1: the .xlsx reader behind the PayPal import. Every workbook here is
// built in the test with archive/zip, never pasted bytes, and one comes from
// the separate xlsx writer, so the reader is checked against a second
// generator.

const testRelsNS = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"

// zipParts writes the given parts into a zip, in name order.
func zipParts(t *testing.T, parts map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range sortedKeys(parts) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		w.Write([]byte(parts[name]))
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// workbookParts is a minimal workbook whose first sheet is sheetXML. The
// sheet is deliberately not named sheet1.xml, so the reader has to follow
// the relationship rather than guess the path.
func workbookParts(sheetXML, sharedXML string, date1904 bool) map[string]string {
	pr := ""
	if date1904 {
		pr = `<workbookPr date1904="1"/>`
	}
	parts := map[string]string{
		"[Content_Types].xml": `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`,
		"xl/workbook.xml": `<?xml version="1.0"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="` + testRelsNS + `">` +
			pr + `<sheets><sheet name="Activity" sheetId="1" r:id="rId7"/><sheet name="Other" sheetId="2" r:id="rId8"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId8" Type="` + testRelsNS + `/worksheet" Target="worksheets/other.xml"/>` +
			`<Relationship Id="rId7" Type="` + testRelsNS + `/worksheet" Target="/xl/worksheets/activity.xml"/>` +
			`<Relationship Id="rId9" Type="` + testRelsNS + `/sharedStrings" Target="strings.xml"/></Relationships>`,
		"xl/worksheets/activity.xml": sheetXML,
		"xl/worksheets/other.xml":    `<worksheet><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>wrong sheet</t></is></c></row></sheetData></worksheet>`,
	}
	if sharedXML != "" {
		parts["xl/strings.xml"] = sharedXML
	}
	return parts
}

func sheetXML(rows ...string) string {
	return `<?xml version="1.0"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>` +
		strings.Join(rows, "") + `</sheetData></worksheet>`
}

func TestReadXLSX_CellKindsAndPlacement(t *testing.T) {
	shared := `<sst><si><t>Name</t></si><si><r><t>Gal</t></r><r><t>erie</t></r><rPh><t>PHONETIC</t></rPh></si></sst>`
	sheet := sheetXML(
		// A shared string, a gap at B, an inline string at C.
		`<row r="1"><c r="A1" t="s"><v>0</v></c><c r="C1" t="inlineStr"><is><t>Gross</t></is></c></row>`,
		// Rich text with a phonetic hint; a number; a cell with no reference
		// follows the one before it; a formula's cached text result.
		`<row r="2"><c r="A2" t="s"><v>1</v></c><c r="C2"><v>17.719999999999999</v></c><c t="str"><f>A2</f><v>Galerie</v></c></row>`,
	)
	got, err := readXLSXFirstSheet(zipParts(t, workbookParts(sheet, shared, false)))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("rows: %+v", got.Rows)
	}
	r0, r1 := got.Rows[0], got.Rows[1]
	if len(r0) != 3 || r0[0].Text != "Name" || r0[1].Text != "" || r0[2].Text != "Gross" {
		t.Errorf("row 1: %+v", r0)
	}
	if len(r1) != 4 || r1[0].Text != "Galerie" || !r1[2].IsNum || r1[2].Num != 17.72 || r1[3].Text != "Galerie" || r1[3].IsNum {
		t.Errorf("row 2: %+v", r1)
	}
	if got.Date1904 {
		t.Error("date1904 set without workbookPr")
	}
}

func TestExcelSerialDate(t *testing.T) {
	cases := []struct {
		serial   float64
		date1904 bool
		want     time.Time
	}{
		{45658, false, time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)},
		{43831.75, false, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}, // 18:00 is still that day
		{45658 - 1462, true, time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		got, err := excelSerialDate(c.serial, c.date1904)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("serial %v (1904=%v): got %v %v, want %v", c.serial, c.date1904, got, err, c.want)
		}
	}
	for _, bad := range []float64{0, 60, -1, 3e6} {
		if _, err := excelSerialDate(bad, false); err == nil {
			t.Errorf("serial %v: want refused", bad)
		}
	}
}

// fixtureAsXLSX turns a CSV fixture into a workbook the way Excel stores one:
// the date column as date serials, the money columns as numbers, and text as
// shared strings in odd columns and inline strings in even ones.
func fixtureAsXLSX(t *testing.T, fixture string, date1904 bool) []byte {
	t.Helper()
	recs, err := csv.NewReader(strings.NewReader(fixture)).ReadAll()
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	header := recs[0]
	idx := map[string]int{}
	for i, h := range header {
		idx[h] = i
	}
	var sst []string
	var rows []string
	epoch := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)
	if date1904 {
		epoch = time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	for r, rec := range recs {
		var cells []string
		for c, v := range rec {
			ref := xlsxColumn(c) + strconv.Itoa(r+1)
			switch {
			case r > 0 && c == idx["Date"]:
				d, err := time.Parse("01/02/2006", v)
				if err != nil {
					t.Fatalf("fixture date %q: %v", v, err)
				}
				serial := d.Sub(epoch).Hours() / 24
				cells = append(cells, fmt.Sprintf(`<c r="%s" s="1"><v>%v</v></c>`, ref, serial))
			case r > 0 && (c == idx["Gross"] || c == idx["Fee"] || c == idx["Net"]):
				f, err := strconv.ParseFloat(strings.ReplaceAll(v, ",", ""), 64)
				if err != nil {
					t.Fatalf("fixture amount %q: %v", v, err)
				}
				cells = append(cells, fmt.Sprintf(`<c r="%s"><v>%s</v></c>`, ref, strconv.FormatFloat(f, 'g', 17, 64)))
			case v == "":
			case c%2 == 1:
				sst = append(sst, v)
				cells = append(cells, fmt.Sprintf(`<c r="%s" t="s"><v>%d</v></c>`, ref, len(sst)-1))
			default:
				cells = append(cells, fmt.Sprintf(`<c r="%s" t="inlineStr"><is><t>%s</t></is></c>`, ref, xmlEscape(v)))
			}
		}
		rows = append(rows, fmt.Sprintf(`<row r="%d">%s</row>`, r+1, strings.Join(cells, "")))
	}
	var sb strings.Builder
	sb.WriteString(`<sst>`)
	for _, s := range sst {
		sb.WriteString(`<si><t>` + xmlEscape(s) + `</t></si>`)
	}
	sb.WriteString(`</sst>`)
	return zipParts(t, workbookParts(sheetXML(rows...), sb.String(), date1904))
}

func assertSameParse(t *testing.T, a, b *paypalParseResult) {
	t.Helper()
	if a.RowsRead != b.RowsRead || len(a.Rows) != len(b.Rows) {
		t.Fatalf("rows read %d/%d, rows %d/%d", a.RowsRead, b.RowsRead, len(a.Rows), len(b.Rows))
	}
	for i := range a.Rows {
		x, y := a.Rows[i], b.Rows[i]
		if x.Kind != y.Kind || x.TransactionID != y.TransactionID || x.GrossCents != y.GrossCents ||
			x.FeeCents != y.FeeCents || x.NetCents != y.NetCents || !x.PaidOn.Equal(y.PaidOn) ||
			x.SenderName != y.SenderName || x.SenderEmail != y.SenderEmail || x.Note != y.Note ||
			x.Currency != y.Currency || x.Category != y.Category {
			t.Errorf("row %d differs:\n %+v\n %+v", i, x, y)
		}
	}
	for reason, n := range a.Skipped {
		if b.Skipped[reason] != n {
			t.Errorf("skipped[%s]: %d vs %d", reason, n, b.Skipped[reason])
		}
	}
}

func TestParsePayPalXLSX_MatchesTheCSV(t *testing.T) {
	want := mustParse(t, paypalFixtureEN, "")
	for _, date1904 := range []bool{false, true} {
		got, err := ParsePayPalActivityXLSX(fixtureAsXLSX(t, paypalFixtureEN, date1904), "")
		if err != nil {
			t.Fatalf("xlsx (1904=%v): %v", date1904, err)
		}
		assertSameParse(t, want, got)
	}
}

func TestParsePayPalXLSX_FloatNoiseRoundsToCents(t *testing.T) {
	sheet := sheetXML(
		`<row><c t="inlineStr"><is><t>Date</t></is></c><c t="inlineStr"><is><t>Name</t></is></c><c t="inlineStr"><is><t>Type</t></is></c><c t="inlineStr"><is><t>Status</t></is></c><c t="inlineStr"><is><t>Currency</t></is></c><c t="inlineStr"><is><t>Gross</t></is></c><c t="inlineStr"><is><t>Transaction ID</t></is></c></row>`,
		`<row><c><v>46218</v></c><c t="inlineStr"><is><t>Shop</t></is></c><c t="inlineStr"><is><t>Mass Pay Payment</t></is></c><c t="inlineStr"><is><t>Completed</t></is></c><c t="inlineStr"><is><t>EUR</t></is></c><c><v>17.719999999999999</v></c><c t="inlineStr"><is><t>M1</t></is></c></row>`,
	)
	res, err := ParsePayPalActivityXLSX(zipParts(t, workbookParts(sheet, "", false)), "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0].GrossCents != 1772 || !res.Rows[0].PaidOn.Equal(time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("got %+v", res.Rows)
	}
}

// The separate writer's workbook: text dates, numeric amounts, inline strings.
func TestParsePayPalXLSX_ReadsTheWritersWorkbook(t *testing.T) {
	data, err := writeXLSX([]xlsxSheet{{
		Name:   "Activity",
		Header: []string{"Datum", "Name", "Typ", "Status", "Währung", "Brutto", "Transaktionscode"},
		Rows: [][]xlsxCell{
			{xlsxText("14.07.2026"), xlsxText("Galerie Nord"), xlsxText("Mobile Zahlung"), xlsxText("Abgeschlossen"), xlsxText("EUR"), xlsxMoney(35000), xlsxText("1AA")},
			{xlsxText("01.08.2026"), xlsxText(""), xlsxText("Allgemeine Abbuchung"), xlsxText("Abgeschlossen"), xlsxText("EUR"), xlsxMoney(-150000), xlsxText("9JJ")},
		},
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	res, err := ParsePayPalActivityXLSX(data, "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res.Rows) != 2 || res.Rows[0].GrossCents != 35000 || res.Rows[0].SenderName != "Galerie Nord" ||
		res.Rows[1].Kind != "withdrawal" || res.Rows[1].GrossCents != 150000 {
		t.Fatalf("got %+v", res.Rows)
	}
}

func TestReadXLSX_Refusals(t *testing.T) {
	ok := sheetXML(`<row><c t="inlineStr"><is><t>x</t></is></c></row>`)
	many := map[string]string{}
	for k, v := range workbookParts(ok, "", false) {
		many[k] = v
	}
	for i := 0; i < xlsxMaxEntries; i++ {
		many[fmt.Sprintf("xl/media/pad%03d.txt", i)] = "x"
	}
	var rows strings.Builder
	for i := 0; i <= xlsxMaxRows; i++ {
		rows.WriteString(`<row/>`)
	}
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"not a zip", []byte("Date,Name\n"), "not an Excel workbook"},
		{"zip without a workbook", zipParts(t, map[string]string{"hello.txt": "hi"}), "not an Excel workbook"},
		{"too many entries", zipParts(t, many), "more than 256 parts"},
		// A deflate bomb: 32 MB of comments zips to a few tens of KB. The
		// comments keep a '<' every 8 bytes, so the part cap, not the run
		// cap, is what refuses it.
		{"part unpacks past the cap", zipParts(t, workbookParts(sheetXML(strings.Repeat("<!--x-->", xlsxMaxPartBytes/8+2)), "", false)), "unpacks to more than 32 MB"},
		// @security F2 probes (c60958 on system_3 #9872): one text run, one
		// tag's attributes, and nesting, each refused before the decoder can
		// grow its buffers or element stack.
		{"text run past 1 MB", zipParts(t, workbookParts(sheetXML(strings.Repeat(" ", xlsxMaxRunBytes+16)), "", false)), "longer than 1 MB"},
		{"attributes past 1 MB", zipParts(t, withPart(workbookParts(ok, "", false), "xl/workbook.xml", `<workbook`+strings.Repeat(` a=""`, xlsxMaxRunBytes/5+16)+`/>`)), "longer than 1 MB"},
		{"nesting past 64", zipParts(t, withPart(workbookParts(ok, "", false), "xl/workbook.xml", strings.Repeat("<a>", 10000)+strings.Repeat("</a>", 10000))), "deeper than 64"},
		{"column past the cap", zipParts(t, workbookParts(sheetXML(`<row><c r="XFD1"><v>1</v></c></row>`), "", false)), "more than 200 columns"},
		{"overlong column letters", zipParts(t, workbookParts(sheetXML(`<row><c r="ZZZZZZZZZZZZZZZZZZZZ1"><v>1</v></c></row>`), "", false)), "more than 200 columns"},
		{"too many rows", zipParts(t, workbookParts(sheetXML(rows.String()), "", false)), "more than 20000 rows"},
		{"shared index out of range", zipParts(t, workbookParts(sheetXML(`<row><c t="s"><v>3</v></c></row>`), `<sst><si><t>a</t></si></sst>`, false)), "does not exist"},
	}
	for _, c := range cases {
		_, err := readXLSXFirstSheet(c.data)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error containing %q", c.name, err, c.want)
		}
	}
	// Positive control: the same small workbook without the padding reads.
	if _, err := readXLSXFirstSheet(zipParts(t, workbookParts(ok, "", false))); err != nil {
		t.Fatalf("control workbook: %v", err)
	}
	if _, err := readXLSXFirstSheet([]byte("x")); !errors.Is(err, errNotXLSX) {
		t.Errorf("not a zip: %v", err)
	}
}

func TestReadXLSX_MissingFirstSheetIsNamed(t *testing.T) {
	parts := workbookParts(sheetXML(), "", false)
	delete(parts, "xl/worksheets/activity.xml")
	if _, err := readXLSXFirstSheet(zipParts(t, parts)); err == nil || !strings.Contains(err.Error(), "first worksheet is missing") {
		t.Fatalf("got %v", err)
	}
}

func withPart(parts map[string]string, name, body string) map[string]string {
	out := map[string]string{}
	for k, v := range parts {
		out[k] = v
	}
	out[name] = body
	return out
}

// fanOutWorkbook is @security's F1 probe (c60958 on system_3 #9872): one
// shared string of n bytes, referenced from Name, Subject, Note and Item
// Title on every one of rows rows, so a small upload carries rows*4*n bytes
// of row text.
func fanOutWorkbook(t *testing.T, n, rows int) []byte {
	t.Helper()
	inl := func(ref, v string) string {
		return fmt.Sprintf(`<c r="%s" t="inlineStr"><is><t>%s</t></is></c>`, ref, v)
	}
	header := []string{"Date", "Name", "Type", "Status", "Currency", "Gross", "Transaction ID", "Subject", "Note", "Item Title"}
	var hdr strings.Builder
	for i, h := range header {
		hdr.WriteString(inl(xlsxColumn(i)+"1", h))
	}
	xmlRows := []string{`<row r="1">` + hdr.String() + `</row>`}
	for r := 2; r <= rows+1; r++ {
		n := strconv.Itoa(r)
		xmlRows = append(xmlRows, `<row r="`+n+`">`+
			`<c r="A`+n+`"><v>46218</v></c><c r="B`+n+`" t="s"><v>0</v></c>`+
			inl("C"+n, "Mobile Payment")+inl("D"+n, "Completed")+inl("E"+n, "EUR")+
			`<c r="F`+n+`"><v>10</v></c>`+inl("G"+n, "T"+n)+
			`<c r="H`+n+`" t="s"><v>0</v></c><c r="I`+n+`" t="s"><v>0</v></c><c r="J`+n+`" t="s"><v>0</v></c></row>`)
	}
	shared := `<sst><si><t>` + strings.Repeat("x", n) + `</t></si></sst>`
	return zipParts(t, workbookParts(sheetXML(xmlRows...), shared, false))
}

func TestParsePayPalXLSX_SharedStringFanOutIsCapped(t *testing.T) {
	for _, c := range []struct{ n, rows int }{{5 << 10, 2000}, {50 << 10, 200}} {
		data := fanOutWorkbook(t, c.n, c.rows)
		if len(data) > 1<<20 {
			t.Fatalf("probe %d x %d: the upload is %d bytes, meant to be under 1 MB", c.n, c.rows, len(data))
		}
		_, err := ParsePayPalActivityXLSX(data, "")
		if err == nil || !strings.Contains(err.Error(), "more than 32 MB of text") {
			t.Errorf("probe %d B x %d rows: got %v, want the text cap", c.n, c.rows, err)
		}
	}
	// Positive control: the same shape at half the text imports every row.
	res, err := ParsePayPalActivityXLSX(fanOutWorkbook(t, 5<<10, 1000), "")
	if err != nil || len(res.Rows) != 1000 {
		t.Fatalf("control: %v, %d rows", err, len(res.Rows))
	}
}
