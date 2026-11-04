package handlers

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
)

// A minimal .xlsx reader for the PayPal import (system_3 #9862-A1). PayPal
// offers its activity as an Excel file as well as CSV, and a CSV opened and
// re-saved in Excel becomes one. Only the first worksheet's cell values are
// read; styles, formulas and every other part are ignored. Standard library
// only, like the xlsx writer beside it.
//
// The file comes from an upload, so every size is capped: the zip's entry
// count, the bytes each part unpacks to, the shared-string count, the rows
// and the column a cell may name. A deflate stream can unpack to a thousand
// times its size, and a cell reference like XFD1 must not size a 16k-wide row.
// encoding/xml does not resolve external entities or expand DTD entities.

const (
	xlsxMaxEntries       = 256
	xlsxMaxPartBytes     = 32 << 20
	xlsxMaxSharedStrings = 100000
	xlsxMaxRows          = 20000
	xlsxMaxColumns       = 200
)

// errNotXLSX is returned for a zip that is not a SpreadsheetML workbook.
var errNotXLSX = errors.New("not an Excel workbook (.xlsx)")

type xlsxInCell struct {
	Text  string
	Num   float64
	IsNum bool
}

type xlsxInSheet struct {
	Rows     [][]xlsxInCell
	Date1904 bool
}

// cappedReader fails once more than n bytes have been read, instead of
// silently truncating the way io.LimitReader does.
type cappedReader struct {
	r    io.Reader
	n    int64
	name string
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.n <= 0 {
		return 0, fmt.Errorf("the workbook part %s unpacks to more than %d MB", c.name, xlsxMaxPartBytes>>20)
	}
	if int64(len(p)) > c.n {
		p = p[:c.n]
	}
	k, err := c.r.Read(p)
	c.n -= int64(k)
	return k, err
}

func openXLSXPart(f *zip.File) (*xml.Decoder, io.Closer, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, nil, errNotXLSX
	}
	// One byte over the cap is what tells an oversized part from one that is
	// exactly at it.
	d := xml.NewDecoder(&cappedReader{r: rc, n: xlsxMaxPartBytes + 1, name: f.Name})
	return d, rc, nil
}

func attr(se xml.StartElement, local string) (string, bool) {
	for _, a := range se.Attr {
		if a.Name.Local == local {
			return a.Value, true
		}
	}
	return "", false
}

// readXLSXWorkbook returns the first sheet's relationship id and whether the
// workbook counts dates from 1904.
func readXLSXWorkbook(f *zip.File) (string, bool, error) {
	d, c, err := openXLSXPart(f)
	if err != nil {
		return "", false, err
	}
	defer c.Close()
	relID, date1904 := "", false
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", false, fmt.Errorf("could not read the workbook: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "workbookPr":
			if v, ok := attr(se, "date1904"); ok {
				date1904 = v == "1" || strings.EqualFold(v, "true")
			}
		case "sheet":
			if relID == "" {
				for _, a := range se.Attr {
					if a.Name.Local == "id" && strings.HasSuffix(a.Name.Space, "/relationships") {
						relID = a.Value
					}
				}
			}
		}
	}
	if relID == "" {
		return "", false, errors.New("the workbook has no worksheet")
	}
	return relID, date1904, nil
}

// readXLSXRels maps relationship ids to part names, and returns the shared
// strings part if one is declared.
func readXLSXRels(f *zip.File) (map[string]string, string, error) {
	d, c, err := openXLSXPart(f)
	if err != nil {
		return nil, "", err
	}
	defer c.Close()
	targets, shared := map[string]string{}, ""
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("could not read the workbook relationships: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "Relationship" {
			continue
		}
		id, _ := attr(se, "Id")
		target, _ := attr(se, "Target")
		typ, _ := attr(se, "Type")
		// Targets are relative to xl/ unless they start with a slash.
		if strings.HasPrefix(target, "/") {
			target = strings.TrimPrefix(target, "/")
		} else {
			target = path.Join("xl", target)
		}
		targets[id] = target
		if strings.HasSuffix(typ, "/sharedStrings") {
			shared = target
		}
	}
	return targets, shared, nil
}

// readXLSXSharedStrings reads every <si>, joining its rich-text runs and
// leaving out phonetic hints (<rPh>), which are not part of the cell's text.
func readXLSXSharedStrings(f *zip.File) ([]string, error) {
	d, c, err := openXLSXPart(f)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	var out []string
	var cur strings.Builder
	inSI, inT, phonetic := false, false, 0
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("could not read the shared strings: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				inSI = true
				cur.Reset()
			case "rPh":
				phonetic++
			case "t":
				inT = inSI && phonetic == 0
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "si":
				if len(out) >= xlsxMaxSharedStrings {
					return nil, fmt.Errorf("the workbook has more than %d distinct texts", xlsxMaxSharedStrings)
				}
				out = append(out, cur.String())
				inSI = false
			case "rPh":
				phonetic--
			case "t":
				inT = false
			}
		case xml.CharData:
			if inT {
				cur.Write(t)
			}
		}
	}
	return out, nil
}

// xlsxColumnIndex turns the letters of a cell reference ("B7") into a 0-based
// column index.
func xlsxColumnIndex(ref string) (int, bool) {
	n, letters := 0, 0
	for _, r := range ref {
		if r < 'A' || r > 'Z' {
			break
		}
		n = n*26 + int(r-'A'+1)
		letters++
		if n > xlsxMaxColumns {
			return n - 1, true
		}
	}
	if letters == 0 {
		return 0, false
	}
	return n - 1, true
}

// readXLSXSheet reads the cell values of one worksheet, row by row.
func readXLSXSheet(f *zip.File, shared []string) ([][]xlsxInCell, error) {
	d, c, err := openXLSXPart(f)
	if err != nil {
		return nil, err
	}
	defer c.Close()

	var rows [][]xlsxInCell
	var row []xlsxInCell
	inRow := false
	// The cell being read.
	col, typ := -1, ""
	var val, inline strings.Builder
	inV, inIS, inT, phonetic := false, false, false, 0

	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("could not read the worksheet: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				if len(rows) >= xlsxMaxRows {
					return nil, fmt.Errorf("the worksheet has more than %d rows", xlsxMaxRows)
				}
				inRow, row = true, nil
			case "c":
				if !inRow {
					continue
				}
				next := len(row)
				if ref, ok := attr(t, "r"); ok {
					idx, ok := xlsxColumnIndex(ref)
					if !ok {
						return nil, fmt.Errorf("cell reference %q is not valid", ref)
					}
					next = idx
				}
				if next >= xlsxMaxColumns {
					return nil, fmt.Errorf("the worksheet uses more than %d columns", xlsxMaxColumns)
				}
				col = next
				typ, _ = attr(t, "t")
				val.Reset()
				inline.Reset()
			case "v":
				inV = col >= 0
			case "is":
				inIS = col >= 0
			case "rPh":
				phonetic++
			case "t":
				inT = inIS && phonetic == 0
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "row":
				if inRow {
					rows = append(rows, row)
				}
				inRow = false
			case "c":
				if col < 0 {
					continue
				}
				cell, err := xlsxCellValue(typ, val.String(), inline.String(), shared)
				if err != nil {
					return nil, err
				}
				for len(row) <= col {
					row = append(row, xlsxInCell{})
				}
				row[col] = cell
				col = -1
			case "v":
				inV = false
			case "is":
				inIS = false
			case "rPh":
				phonetic--
			case "t":
				inT = false
			}
		case xml.CharData:
			if inV {
				val.Write(t)
			} else if inT {
				inline.Write(t)
			}
		}
	}
	return rows, nil
}

// xlsxCellValue decodes one cell by its type attribute (ECMA-376 §18.18.11).
func xlsxCellValue(typ, v, inline string, shared []string) (xlsxInCell, error) {
	switch typ {
	case "s":
		i, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || i < 0 || i >= len(shared) {
			return xlsxInCell{}, fmt.Errorf("a cell points at shared text %q, which does not exist", v)
		}
		return xlsxInCell{Text: shared[i]}, nil
	case "inlineStr":
		return xlsxInCell{Text: inline}, nil
	case "str", "e", "b", "d":
		// A formula's text result, an error value, a boolean and an ISO date
		// string are all read as their text.
		return xlsxInCell{Text: v}, nil
	case "", "n":
		s := strings.TrimSpace(v)
		if s == "" {
			return xlsxInCell{}, nil
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return xlsxInCell{}, fmt.Errorf("a number cell holds %q", v)
		}
		return xlsxInCell{Text: s, Num: f, IsNum: true}, nil
	default:
		return xlsxInCell{}, fmt.Errorf("unknown cell type %q", typ)
	}
}

// readXLSXFirstSheet reads the cell values of a workbook's first worksheet.
func readXLSXFirstSheet(data []byte) (*xlsxInSheet, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, errNotXLSX
	}
	if len(zr.File) > xlsxMaxEntries {
		return nil, fmt.Errorf("the file holds more than %d parts; it is not a plain workbook", xlsxMaxEntries)
	}
	files := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		files[f.Name] = f
	}
	wb, rels := files["xl/workbook.xml"], files["xl/_rels/workbook.xml.rels"]
	if wb == nil || rels == nil {
		return nil, errNotXLSX
	}
	relID, date1904, err := readXLSXWorkbook(wb)
	if err != nil {
		return nil, err
	}
	targets, sharedName, err := readXLSXRels(rels)
	if err != nil {
		return nil, err
	}
	sheetFile := files[targets[relID]]
	if sheetFile == nil {
		return nil, errors.New("the workbook's first worksheet is missing")
	}
	var shared []string
	if sharedName == "" {
		sharedName = "xl/sharedStrings.xml"
	}
	if sf := files[sharedName]; sf != nil {
		if shared, err = readXLSXSharedStrings(sf); err != nil {
			return nil, err
		}
	}
	rows, err := readXLSXSheet(sheetFile, shared)
	if err != nil {
		return nil, err
	}
	return &xlsxInSheet{Rows: rows, Date1904: date1904}, nil
}
