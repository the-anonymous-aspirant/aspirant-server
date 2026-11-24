package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"aspirant-online/server/data_models"
)

// system_3 #12260: an amount with more than two decimals, or above the hand
// entry's ceiling, is refused naming its row, and the file stores nothing.
// A lone separator before three digits stays a thousands group unless the
// file's other amounts show that separator is the decimal one.

func TestParseCentsIn_DecimalsAndCeiling(t *testing.T) {
	type tc struct {
		in      string
		decimal byte
		want    int64
		refuse  string // a part of the refusal; "" when it is read
	}
	for _, c := range []tc{
		// The review's cases (C3-5).
		{"20.0000", 0, 0, "more than two decimals"},
		{"12.3456", 0, 0, "more than two decimals"},
		{"92233720368547758,07", 0, 0, "above 100000000 euros"},
		{"1,234.567", 0, 0, "more than two decimals"},
		// A lone three-digit tail: a group unless the file says otherwise.
		{"12,345", 0, 1234500, ""},
		{"12,345", '.', 1234500, ""},
		{"12,345", ',', 0, "more than two decimals"},
		{"12.345", ',', 1234500, ""},
		{"12.345", '.', 0, "more than two decimals"},
		{"1.234.567", ',', 123456700, ""},
		// Four or more digits after one separator, any file.
		{"20,0000", ',', 0, "more than two decimals"},
		{"-1.23456", 0, 0, "more than two decimals"},
		{"1,234,5678", 0, 0, "more than two decimals"},
		// The ceiling: 100 million euros, as hand entry.
		{"100000000.00", '.', 10_000_000_000, ""},
		{"100.000.000,00", ',', 10_000_000_000, ""},
		{"-100000000", 0, -10_000_000_000, ""},
		{"100000000.01", '.', 0, "above 100000000 euros"},
		{"100000001", 0, 0, "above 100000000 euros"},
		{"-100.000.000,01", ',', 0, "above 100000000 euros"},
		{"99999999999999999999", 0, 0, "above 100000000 euros"},
		// Tested on the whole before it is multiplied, and on its absolute
		// value (design_agent c76354): neither wraps past int64.
		{"92233720368547759,00", 0, 0, "above 100000000 euros"},
		{"-200000000,00", 0, 0, "above 100000000 euros"},
		// A second or misplaced sign is no number, and cannot pass the
		// ceiling as a negative (security c76329).
		{"--92233720368547758", 0, 0, "is not a number"},
		{"--100000001", 0, 0, "is not a number"},
		{"--5.10", 0, 0, "is not a number"},
		{"5.-1", 0, 0, "is not a number"},
		{"-+5", 0, 0, "is not a number"},
		{"+-3", 0, 0, "is not a number"},
		// Unchanged.
		{"20.00", '.', 2000, ""},
		{"1.234,56", ',', 123456, ""},
		{"0,5", 0, 50, ""},
		{"", '.', 0, ""},
	} {
		got, err := parseCentsIn(c.in, c.decimal)
		if c.refuse == "" {
			if err != nil || got != c.want {
				t.Errorf("parseCentsIn(%q, %q) = %d, %v; want %d", c.in, c.decimal, got, err, c.want)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.refuse) || !strings.Contains(err.Error(), c.in) {
			t.Errorf("parseCentsIn(%q, %q) = %d, %v; want a refusal naming it, %q", c.in, c.decimal, got, err, c.refuse)
		}
	}
}

func TestPayPalDecimalSeparator(t *testing.T) {
	for _, c := range []struct {
		amounts []string
		want    byte
	}{
		{[]string{"20.00", "-0.35", ""}, '.'},
		{[]string{"1.234,56", "12,345"}, ','},
		{[]string{"1,234.56", "12,345"}, '.'},
		{[]string{"12,345", "20", ""}, 0},
		{[]string{"€ 5,00", "+1.500"}, ','},
		// Amounts that disagree show nothing: a lone group stays a group.
		{[]string{"20.00", "1.234,56"}, 0},
	} {
		if got := paypalDecimalSeparator(c.amounts); got != c.want {
			t.Errorf("paypalDecimalSeparator(%q) = %q, want %q", c.amounts, got, c.want)
		}
	}
}

// Files through the import: each refused one names its line and stores
// nothing; the file with a thousands group in comma-group notation imports.
func TestImportPayPal_AnAmountWithTooManyDecimalsRefusesTheFile(t *testing.T) {
	db := setupPaymentsDB(t)
	r := setupPaymentsRouter(db, 5)
	const header = "Date,Name,Type,Status,Currency,Gross,Fee,Net,Transaction ID\n"
	for _, c := range []struct{ name, file, want string }{
		{"20.0000 beside 1,234.56", header +
			"03.10.2026,Mara Esempio,Payment,Completed,EUR,\"1,234.56\",0.00,\"1,234.56\",PPAMT00001\n" +
			"04.10.2026,Mara Esempio,Payment,Completed,EUR,20.0000,0.00,20.0000,PPAMT00002\n",
			`line 3: amount "20.0000" has more than two decimals`},
		{"a three-digit tail in a dot-decimal file", header +
			"03.10.2026,Mara Esempio,Payment,Completed,EUR,20.00,-0.35,19.65,PPAMT00003\n" +
			"04.10.2026,Mara Esempio,Payment,Completed,EUR,12.345,0.00,12.345,PPAMT00004\n",
			`line 3: amount "12.345" has more than two decimals`},
		{"an amount past the ceiling", header +
			"03.10.2026,Mara Esempio,Payment,Completed,EUR,\"92233720368547758,07\",\"0,00\",\"92233720368547758,07\",PPAMT00005\n",
			`line 2: amount "92233720368547758,07" is above 100000000 euros`},
		{"a fee with too many decimals", header +
			"03.10.2026,Mara Esempio,Payment,Completed,EUR,20.00,-0.3500,19.65,PPAMT00006\n",
			`line 2: amount "-0.3500" has more than two decimals`},
	} {
		w := uploadCSV(t, r, c.file, "")
		var resp struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(w.Body.Bytes(), &resp)
		if w.Code != http.StatusBadRequest || resp.Error.Message != c.want {
			t.Errorf("%s: %d %s, want 400 with %q", c.name, w.Code, w.Body.String(), c.want)
		}
	}
	var rows int
	db.Model(&data_models.InvoicingPayment{}).Count(&rows)
	if rows != 0 || countAuditEvents(t, db) != 0 {
		t.Fatalf("refused files wrote %d payments and %d audit rows", rows, countAuditEvents(t, db))
	}

	// "12,345" beside "1,234.56" is twelve thousand three hundred forty-five.
	w := uploadCSV(t, r, header+
		"03.10.2026,Mara Esempio,Payment,Completed,EUR,\"1,234.56\",0.00,\"1,234.56\",PPAMT00007\n"+
		"04.10.2026,Mara Esempio,Payment,Completed,EUR,\"12,345\",0.00,\"12,345\",PPAMT00008\n", "")
	if s := decodeSummary(t, w); s.Imported != 2 {
		t.Fatalf("summary: %+v (%s)", s, w.Body.String())
	}
	got := map[string]int64{}
	for _, p := range listPayments(t, r, "") {
		got[p.TransactionID] = p.GrossCents
	}
	if got["PPAMT00007"] != 123456 || got["PPAMT00008"] != 1234500 {
		t.Errorf("stored: %v", got)
	}
}

// An .xlsx's numeric money cells are written by the reader itself, with a dot
// and two decimals, so they are no evidence of the separator (design_agent
// c76354): a typed text "12.345" beside them stays a thousands group, as in
// a file of text cells alone.
func TestParsePayPalXLSX_NumericCellsAreNoSeparatorEvidence(t *testing.T) {
	head := `<row><c t="inlineStr"><is><t>Date</t></is></c><c t="inlineStr"><is><t>Name</t></is></c><c t="inlineStr"><is><t>Type</t></is></c><c t="inlineStr"><is><t>Status</t></is></c><c t="inlineStr"><is><t>Currency</t></is></c><c t="inlineStr"><is><t>Gross</t></is></c><c t="inlineStr"><is><t>Transaction ID</t></is></c></row>`
	row := func(gross, id string) string {
		return `<row><c><v>46218</v></c><c t="inlineStr"><is><t>Mara Esempio</t></is></c><c t="inlineStr"><is><t>Payment</t></is></c><c t="inlineStr"><is><t>Completed</t></is></c><c t="inlineStr"><is><t>EUR</t></is></c>` +
			gross + `<c t="inlineStr"><is><t>` + id + `</t></is></c></row>`
	}
	sheet := sheetXML(head,
		row(`<c><v>20.5</v></c>`, "PPXLS00001"),
		row(`<c t="inlineStr"><is><t>12.345</t></is></c>`, "PPXLS00002"),
	)
	res, err := ParsePayPalActivityXLSX(zipParts(t, workbookParts(sheet, "", false)), "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := map[string]int64{}
	for _, r := range res.Rows {
		got[r.TransactionID] = r.GrossCents
	}
	if got["PPXLS00001"] != 2050 || got["PPXLS00002"] != 1234500 {
		t.Fatalf("got %v", got)
	}
}
