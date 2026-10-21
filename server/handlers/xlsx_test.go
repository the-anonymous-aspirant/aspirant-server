package handlers

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"testing"
)

// #8367-E1: the stdlib .xlsx writer.

type xlsxReadCell struct {
	Ref    string `xml:"r,attr"`
	Type   string `xml:"t,attr"`
	Value  string `xml:"v"`
	Inline string `xml:"is>t"`
	Fn     string `xml:"f"`
}

type xlsxReadSheet struct {
	Rows []struct {
		Cells []xlsxReadCell `xml:"c"`
	} `xml:"sheetData>row"`
}

func unzipXLSX(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = b
	}
	return out
}

func readSheet(t *testing.T, files map[string][]byte, n string) xlsxReadSheet {
	t.Helper()
	var s xlsxReadSheet
	if err := xml.Unmarshal(files["xl/worksheets/sheet"+n+".xml"], &s); err != nil {
		t.Fatalf("sheet %s: %v", n, err)
	}
	return s
}

func sheetNames(t *testing.T, files map[string][]byte) []string {
	t.Helper()
	var wb struct {
		Sheets []struct {
			Name string `xml:"name,attr"`
		} `xml:"sheets>sheet"`
	}
	if err := xml.Unmarshal(files["xl/workbook.xml"], &wb); err != nil {
		t.Fatalf("workbook.xml: %v", err)
	}
	var names []string
	for _, s := range wb.Sheets {
		names = append(names, s.Name)
	}
	return names
}

func TestWriteXLSX_StructureCellsAndEscaping(t *testing.T) {
	data, err := writeXLSX([]xlsxSheet{
		{Name: "A sheet name that is much longer than thirty-one characters", Header: []string{"Text", "Amount"}, Rows: [][]xlsxCell{
			{xlsxText(`Müller & Söhne <GmbH> "quoted"`), xlsxMoney(123456)},
			{xlsxText("=1+1"), xlsxNumber(2.5)},
		}},
		{Name: "Sheet/2", Header: []string{"X"}},
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	files := unzipXLSX(t, data)
	for _, part := range []string{"[Content_Types].xml", "_rels/.rels", "xl/workbook.xml", "xl/_rels/workbook.xml.rels", "xl/styles.xml", "xl/worksheets/sheet1.xml", "xl/worksheets/sheet2.xml"} {
		if _, ok := files[part]; !ok {
			t.Errorf("missing part %s", part)
		}
	}
	names := sheetNames(t, files)
	if len(names) != 2 || len([]rune(names[0])) != 31 || names[1] != "Sheet_2" {
		t.Errorf("sheet names: %q", names)
	}
	s := readSheet(t, files, "1")
	if len(s.Rows) != 3 {
		t.Fatalf("rows: %d", len(s.Rows))
	}
	text, amount := s.Rows[1].Cells[0], s.Rows[1].Cells[1]
	if text.Type != "inlineStr" || text.Inline != `Müller & Söhne <GmbH> "quoted"` || text.Ref != "A2" {
		t.Errorf("text cell: %+v", text)
	}
	if amount.Type != "" || amount.Value != "1234.56" || amount.Ref != "B2" {
		t.Errorf("number cell: %+v", amount)
	}
	formula := s.Rows[2].Cells[0]
	if formula.Fn != "" || formula.Type != "inlineStr" || formula.Inline != "'=1+1" {
		t.Errorf("a formula-looking text became %+v", formula)
	}
	if strings.Contains(string(files["xl/worksheets/sheet1.xml"]), "<f>") {
		t.Errorf("the sheet contains a formula element")
	}
}

func TestXLSXColumn(t *testing.T) {
	for i, want := range map[int]string{0: "A", 25: "Z", 26: "AA", 27: "AB", 701: "ZZ", 702: "AAA"} {
		if got := xlsxColumn(i); got != want {
			t.Errorf("xlsxColumn(%d) = %q, want %q", i, got, want)
		}
	}
}
