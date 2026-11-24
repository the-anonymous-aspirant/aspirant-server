package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"aspirant-online/server/data_models"
)

// system_3 #12261: a draft shows what issuing would print, worked out from
// the profile's current VAT setting on every read (ruling 1), and a draft
// made from payments is priced from what was received, so its total equals
// the payments to the cent with VAT on or off (ruling 2).

func setVat(t *testing.T, f *invoiceFixture, on bool) {
	t.Helper()
	body := strings.Replace(validProfileBody, `"is_kleinunternehmer":true`, fmt.Sprintf(`"is_kleinunternehmer":%v`, !on), 1)
	body = strings.Replace(body, `"next_invoice_number":1`, fmt.Sprintf(`"next_invoice_number":%d`, f.profileNext(t)), 1)
	if w := invoicingDo(f.r, "PUT", "/invoicing/profile", body); w.Code != http.StatusOK {
		t.Fatalf("save profile: %d %s", w.Code, w.Body.String())
	}
}

func getInvoice(t *testing.T, f *invoiceFixture, id uint) invoiceDetail {
	t.Helper()
	w := invoicingDo(f.r, "GET", fmt.Sprintf("/invoicing/invoices/%d", id), "")
	if w.Code != http.StatusOK {
		t.Fatalf("get invoice %d: %d %s", id, w.Code, w.Body.String())
	}
	return decodeDetail(t, w.Body.Bytes())
}

func listedInvoice(t *testing.T, f *invoiceFixture, id uint) data_models.InvoicingInvoice {
	t.Helper()
	var resp struct {
		Invoices []invoiceListRow `json:"invoices"`
	}
	w := invoicingDo(f.r, "GET", "/invoicing/invoices", "")
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("list: %v (%s)", err, w.Body.String())
	}
	for _, r := range resp.Invoices {
		if r.ID == id {
			return r.InvoicingInvoice
		}
	}
	t.Fatalf("invoice %d not listed", id)
	return data_models.InvoicingInvoice{}
}

type figures struct{ net, vat, total int64 }

func figuresOf(inv data_models.InvoicingInvoice) figures {
	return figures{inv.NetCents, inv.VatCents, inv.TotalCents}
}

// The review's sequence (C3-6): a draft from a 100,00 payment as
// Kleinunternehmerin, then the profile saved with VAT on, then issue.
func TestDraftVat_TheDraftShowsWhatIssuePrintsAfterTheSettingChanged(t *testing.T) {
	f := newInvoiceFixture(t)
	id := f.draft(t, 0) // 100,00
	if got := figuresOf(getInvoice(t, f, id).Invoice); got != (figures{10000, 0, 10000}) {
		t.Fatalf("as Kleinunternehmerin: %+v", got)
	}

	setVat(t, f, true)
	want := figures{8403, 1597, 10000}
	d := getInvoice(t, f, id)
	if got := figuresOf(d.Invoice); got != want {
		t.Errorf("draft after VAT on: %+v, want %+v", got, want)
	}
	if len(d.Warnings) != 0 {
		t.Errorf("a draft that asks for what was paid warns: %v", d.Warnings)
	}
	if got := figuresOf(listedInvoice(t, f, id)); got != want {
		t.Errorf("listed draft after VAT on: %+v, want %+v", got, want)
	}
	// Reading wrote nothing: the stored row is as the draft was saved.
	stored, _ := data_models.GetInvoicingInvoice(f.db, invoiceFixtureUser, id)
	if got := figuresOf(*stored); got != (figures{10000, 0, 10000}) {
		t.Errorf("a read rewrote the stored draft: %+v", got)
	}

	// And back: the setting decides at every read.
	setVat(t, f, false)
	if got := figuresOf(getInvoice(t, f, id).Invoice); got != (figures{10000, 0, 10000}) {
		t.Errorf("draft after VAT off again: %+v", got)
	}

	setVat(t, f, true)
	code, issued, body := f.issue(id)
	if code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, body)
	}
	if got := figuresOf(issued.Invoice); got != want {
		t.Errorf("issued: %+v, want what the draft showed, %+v", got, want)
	}

	// The PDF prints the same, its lines under "(brutto)".
	w := invoicingDo(f.r, "GET", fmt.Sprintf("/invoicing/invoices/%d/pdf", id), "")
	if w.Code != http.StatusOK {
		t.Fatalf("pdf: %d", w.Code)
	}
	pdfPath := filepath.Join(t.TempDir(), "invoice.pdf")
	if err := os.WriteFile(pdfPath, w.Body.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("pdftotext", "-q", "-layout", pdfPath, "-").Output()
	if err != nil {
		t.Fatalf("pdftotext: %v", err)
	}
	text := strings.Join(strings.Fields(string(out)), " ")
	for _, s := range []string{"Nettobetrag: 84,03 €", "Umsatzsteuer 19 %: 15,97 €", "Gesamtbetrag (brutto): 100,00 €", "(brutto) (brutto)"} {
		if !strings.Contains(text, s) {
			t.Errorf("PDF lacks %q: %s", s, text)
		}
	}
	if strings.Contains(text, "119,00") {
		t.Errorf("PDF asks for more than was paid: %s", text)
	}
}

// Ruling 2: a generated draft's total is its payment, to the cent, with VAT
// on and off, including totals no whole-cent net reaches at 19 % (100,01).
func TestDraftVat_AGeneratedDraftTotalsItsPaymentWithVatOnAndOff(t *testing.T) {
	for _, vat := range []bool{false, true} {
		t.Run(fmt.Sprintf("vat=%v", vat), func(t *testing.T) {
			f := newInvoiceFixture(t)
			setVat(t, f, vat)
			amounts := []int64{10001, 9999, 99}
			wantVat := map[int64]int64{10001: 1597, 9999: 1596, 99: 16}
			for i, a := range amounts {
				if err := f.db.Model(&f.payments[i]).Update("gross_cents", a).Error; err != nil {
					t.Fatal(err)
				}
			}
			code, resp, body := generate(t, f, `{"month":"2026-07"}`)
			if code != http.StatusCreated || len(resp.Invoices) != 3 {
				t.Fatalf("generate: %d %s", code, body)
			}
			for i, d := range resp.Invoices {
				a := amounts[i]
				want := figures{a, 0, a}
				if vat {
					want = figures{a - wantVat[a], wantVat[a], a}
				}
				if got := figuresOf(d.Invoice); got != want {
					t.Errorf("payment %d: generated %+v, want %+v", a, got, want)
				}
				if got := figuresOf(getInvoice(t, f, d.Invoice.ID).Invoice); got != want {
					t.Errorf("payment %d: read back %+v, want %+v", a, got, want)
				}
				if len(d.Warnings) != 0 {
					t.Errorf("payment %d: warnings %v", a, d.Warnings)
				}
			}
		})
	}
}

// A draft over three payments totals them, with VAT off, on, switched either
// way, and at a typed rate; a typed price on it holds its VAT too.
func TestDraftVat_AThreePaymentDraftTotalsItsPayments(t *testing.T) {
	f := newInvoiceFixture(t)
	id := f.draft(t, 0, 1, 2) // 100,00 + 200,00 + 300,00
	check := func(step string, want figures) {
		t.Helper()
		d := getInvoice(t, f, id)
		if got := figuresOf(d.Invoice); got != want {
			t.Errorf("%s: %+v, want %+v", step, got, want)
		}
		if got := figuresOf(listedInvoice(t, f, id)); got != want {
			t.Errorf("%s, listed: %+v, want %+v", step, got, want)
		}
		if len(d.Warnings) != 0 && want.total == 60000 {
			t.Errorf("%s: warnings %v", step, d.Warnings)
		}
	}
	check("VAT off", figures{60000, 0, 60000})
	setVat(t, f, true)
	check("VAT on", figures{50420, 9580, 60000}) // 9579,83
	if w := invoicingDo(f.r, "PUT", fmt.Sprintf("/invoicing/invoices/%d", id), `{"vat_rate_bp":700}`); w.Code != http.StatusOK {
		t.Fatalf("rate: %d %s", w.Code, w.Body.String())
	}
	check("VAT at 7 %", figures{56075, 3925, 60000}) // 3925,23
	setVat(t, f, false)
	check("VAT off again", figures{60000, 0, 60000})
	setVat(t, f, true)
	body := `{"line_items":[{"description":"Portrait","quantity_milli":2000,"unit_price_cents":11900}]}`
	if w := invoicingDo(f.r, "PUT", fmt.Sprintf("/invoicing/invoices/%d", id), body); w.Code != http.StatusOK {
		t.Fatalf("lines: %d %s", w.Code, w.Body.String())
	}
	check("a typed price, 7 %", figures{22243, 1557, 23800}) // 1557,01
}

func TestVatShareOfGross_RoundsHalfAwayFromZero(t *testing.T) {
	for _, c := range []struct{ gross, bp, want int64 }{
		{10000, 1900, 1597}, // 1596,64
		{11900, 1900, 1900},
		{1, 1900, 0},
		{-10000, 1900, -1597},
		{10700, 700, 700},
		{5000, 0, 0},
		{119, 1900, 19},
		{-119, 1900, -19},
		{3, 1900, 0},    // 0,479
		{122, 2000, 20}, // 20,33
	} {
		if got := vatShareOfGross(c.gross, c.bp); got != c.want {
			t.Errorf("vatShareOfGross(%d, %d) = %d, want %d", c.gross, c.bp, got, c.want)
		}
	}
}

// An issued invoice keeps what it was issued with when the setting changes.
func TestDraftVat_AnIssuedInvoiceKeepsItsFigures(t *testing.T) {
	f := newInvoiceFixture(t)
	id := f.draft(t, 0)
	if code, _, body := f.issue(id); code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, body)
	}
	setVat(t, f, true)
	if got := figuresOf(getInvoice(t, f, id).Invoice); got != (figures{10000, 0, 10000}) {
		t.Errorf("issued after VAT on: %+v", got)
	}
	if got := figuresOf(listedInvoice(t, f, id)); got != (figures{10000, 0, 10000}) {
		t.Errorf("listed issued after VAT on: %+v", got)
	}
}

// Validation reads an invoice priced from the gross as adding up, and still
// reads one issued before as adding up from its net.
func TestDraftVat_ValidationAcceptsBothPricings(t *testing.T) {
	f := newInvoiceFixture(t)
	setVat(t, f, true)
	gross := f.draft(t, 0)
	if code, _, body := f.issue(gross); code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, body)
	}
	net := f.draft(t, 1) // 200,00
	if code, _, body := f.issue(net); code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, body)
	}
	// As the earlier pricing issued it: 200,00 net, 19 % on top.
	if err := f.db.Model(&data_models.InvoicingInvoice{}).Where("id = ?", net).
		Updates(map[string]interface{}{"net_cents": 20000, "vat_cents": 3800, "total_cents": 23800}).Error; err != nil {
		t.Fatal(err)
	}
	for _, v := range f.validation(t).Violations {
		if v.Code == "total_mismatch_items" {
			t.Errorf("violation: %+v", v)
		}
	}
	// A total that is neither is still caught.
	if err := f.db.Model(&data_models.InvoicingInvoice{}).Where("id = ?", net).
		Updates(map[string]interface{}{"net_cents": 20001, "vat_cents": 3800, "total_cents": 23801}).Error; err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range f.validation(t).Violations {
		found = found || v.Code == "total_mismatch_items"
	}
	if !found {
		t.Errorf("a total that adds up under neither pricing is not reported")
	}
}

// Ruling 3: the profile answer counts the drafts the setting prices.
func TestDraftVat_ProfileCountsTheDrafts(t *testing.T) {
	f := newInvoiceFixture(t)
	count := func() int {
		t.Helper()
		var resp struct {
			DraftCount *int `json:"draft_count"`
		}
		w := invoicingDo(f.r, "GET", "/invoicing/profile", "")
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.DraftCount == nil {
			t.Fatalf("profile: %s", w.Body.String())
		}
		return *resp.DraftCount
	}
	if n := count(); n != 0 {
		t.Errorf("no drafts: %d", n)
	}
	f.draft(t, 0)
	issued := f.draft(t, 1)
	f.draft(t, 2)
	if code, _, body := f.issue(issued); code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, body)
	}
	if n := count(); n != 2 {
		t.Errorf("two drafts and one issued: %d, want 2", n)
	}
}
