package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"aspirant-online/server/data_models"
)

// system_3 #10548 (design §3.142 Decisions 4-6): payers from payments into
// the client book. The invoice fixture's own four payments are all linked to
// "Galerie Nord", so every candidate below is one these tests seed.

type payerFixture struct {
	*invoiceFixture
	seq int
}

func newPayerFixture(t *testing.T) *payerFixture {
	f := newInvoiceFixture(t)
	f.r.GET("/invoicing/payers/candidates", ListPayerCandidatesHandler)
	f.r.POST("/invoicing/payers", AddPayersHandler)
	return &payerFixture{invoiceFixture: f}
}

// pay seeds one unlinked incoming payment and returns its id.
func (f *payerFixture) pay(t *testing.T, owner uint, source, name, email, category string, cents int64, day int) uint {
	t.Helper()
	f.seq++
	p := data_models.InvoicingPayment{
		OwnerUserID: owner, Source: source, TransactionID: fmt.Sprintf("P%d", f.seq), Kind: "incoming",
		PaidOn: time.Date(2026, 8, day, 0, 0, 0, 0, time.UTC), Currency: "EUR", GrossCents: cents, NetCents: cents,
		SenderName: name, SenderEmail: email, Category: category,
	}
	if err := f.db.Create(&p).Error; err != nil {
		t.Fatalf("seed payment: %v", err)
	}
	return p.ID
}

type candidatesResp struct {
	Payers  []payerCandidate `json:"payers"`
	LeftOut map[string]int   `json:"left_out"`
}

func (f *payerFixture) candidates(t *testing.T) candidatesResp {
	t.Helper()
	w := invoicingDo(f.r, "GET", "/invoicing/payers/candidates", "")
	if w.Code != http.StatusOK {
		t.Fatalf("candidates: %d %s", w.Code, w.Body.String())
	}
	var r candidatesResp
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return r
}

func TestTidyPayerName(t *testing.T) {
	for in, want := range map[string]string{
		"MARA ESEMPIO":       "Mara Esempio",
		"mara esempio":       "Mara Esempio",
		"  MARA   ESEMPIO ":  "Mara Esempio",
		"ESEMPIO-ROSSI MARA": "Esempio-Rossi Mara",
		"O'NEILL":            "O'Neill",
		"Mara de Esempio":    "Mara de Esempio",
		"mARA eSEMPIO":       "mARA eSEMPIO",
		"Galerie NORD GmbH":  "Galerie NORD GmbH",
		"ÉLISE ÇELIK":        "Élise Çelik",
		"":                   "",
		"1234":               "1234",
	} {
		if got := tidyPayerName(in); got != want {
			t.Errorf("tidyPayerName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPayerCandidates_GroupingLeftOutAndOrder(t *testing.T) {
	f := newPayerFixture(t)
	u := uint(invoiceFixtureUser)
	// Mara by name, three ways of writing it: one row.
	m1 := f.pay(t, u, "n26", "MARA ESEMPIO", "", "uncategorised", 10000, 1)
	m2 := f.pay(t, u, "n26", "Mara Esempio.", "", "paid_work", 20000, 5)
	m3 := f.pay(t, u, "manual", "mára  esempio", "", "paid_work", 5000, 3)
	// A PayPal sender groups by email, whatever the name says.
	p1 := f.pay(t, u, "paypal", "Lia B.", "Lia@Example.test", "uncategorised", 3000, 2)
	p2 := f.pay(t, u, "paypal", "Lia Beispiel", "lia@example.test", "paid_work", 4000, 4)
	// Left out by rule.
	f.pay(t, u, "n26", "PayPal Europe S.a.r.l. et Cie S.C.A", "", "uncategorised", 9900, 6)
	f.pay(t, u, "n26", "Tante Beispiel", "", "gift", 5000, 7)
	f.pay(t, u, "n26", "Bank Beispiel", "", "loan", 5000, 8)
	f.pay(t, u, "manual", "  ", "", "paid_work", 100, 9)
	// Another owner's payment never appears.
	f.pay(t, u+1, "n26", "Mara Esempio", "", "paid_work", 7000, 1)

	r := f.candidates(t)
	if len(r.Payers) != 2 {
		t.Fatalf("rows: %+v", r.Payers)
	}
	mara, lia := r.Payers[0], r.Payers[1]
	if mara.PaymentCount != 3 || lia.PaymentCount != 2 {
		t.Fatalf("order is most payments first: %+v", r.Payers)
	}
	if fmt.Sprint(mara.PaymentIDs) != fmt.Sprint([]uint{m1, m3, m2}) {
		t.Errorf("mara's payments (oldest first): %v", mara.PaymentIDs)
	}
	if mara.Payer != "MARA ESEMPIO" || mara.TidiedName != "Mara Esempio" || mara.PayPalEmail != "" {
		t.Errorf("mara row: %+v", mara)
	}
	if mara.FirstPaidOn != "2026-08-01" || mara.LastPaidOn != "2026-08-05" || len(mara.Totals) != 1 || mara.Totals[0].Cents != 35000 {
		t.Errorf("mara dates/totals: %+v", mara)
	}
	if lia.PayPalEmail != "lia@example.test" || fmt.Sprint(lia.PaymentIDs) != fmt.Sprint([]uint{p1, p2}) {
		t.Errorf("lia row groups by email: %+v", lia)
	}
	if r.LeftOut["paypal_to_n26"] != 1 || r.LeftOut["other_category"] != 2 || r.LeftOut["no_name"] != 1 {
		t.Errorf("left out: %+v", r.LeftOut)
	}
	if mara.SameClient != nil || lia.SameClient != nil {
		t.Errorf("no book client matches yet: %+v %+v", mara.SameClient, lia.SameClient)
	}
}

func TestPayerCandidates_SameClientIsSurfaced(t *testing.T) {
	f := newPayerFixture(t) // the book holds "Galerie Nord" with pay@galerie.test
	u := uint(invoiceFixtureUser)
	f.pay(t, u, "n26", "GALERIE-NORD", "", "uncategorised", 1000, 1)
	f.pay(t, u, "paypal", "Someone Else", "PAY@galerie.test", "uncategorised", 1000, 2)
	r := f.candidates(t)
	if len(r.Payers) != 2 {
		t.Fatalf("rows: %+v", r.Payers)
	}
	for _, row := range r.Payers {
		if row.SameClient == nil || row.SameClient.ID != f.clientID {
			t.Errorf("%q should point at Galerie Nord: %+v", row.Payer, row.SameClient)
		}
	}
}

func addPayers(f *payerFixture, body string) (int, string) {
	w := invoicingDo(f.r, "POST", "/invoicing/payers", body)
	return w.Code, w.Body.String()
}

func clientOf(t *testing.T, f *payerFixture, paymentID uint) *uint {
	t.Helper()
	var p data_models.InvoicingPayment
	if err := f.db.First(&p, paymentID).Error; err != nil {
		t.Fatalf("read payment: %v", err)
	}
	return p.ClientID
}

func TestAddPayers_NewAndExistingClientsLinkInOneGo(t *testing.T) {
	f := newPayerFixture(t)
	u := uint(invoiceFixtureUser)
	m1 := f.pay(t, u, "n26", "MARA ESEMPIO", "", "uncategorised", 10000, 1)
	m2 := f.pay(t, u, "n26", "Mara Esempio", "", "paid_work", 10000, 2)
	l1 := f.pay(t, u, "paypal", "Lia", "lia@example.test", "uncategorised", 3000, 3)
	g1 := f.pay(t, u, "paypal", "Galerie Nord", "other@galerie.test", "uncategorised", 3000, 4)
	clients0 := countRows(t, f.invoiceFixture, &data_models.InvoicingClient{})
	audits0 := countRows(t, f.invoiceFixture, &data_models.InvoicingAuditEvent{})

	code, body := addPayers(f, fmt.Sprintf(`{"payers":[
		{"payment_ids":[%d,%d],"new_client_name":"Mara Esempio"},
		{"payment_ids":[%d],"new_client_name":"Lia Beispiel"},
		{"payment_ids":[%d],"client_id":%d}]}`, m1, m2, l1, g1, f.clientID))
	if code != http.StatusOK {
		t.Fatalf("add: %d %s", code, body)
	}
	var resp struct {
		ClientsAdded   int                           `json:"clients_added"`
		PaymentsLinked int                           `json:"payments_linked"`
		Clients        []data_models.InvoicingClient `json:"clients"`
	}
	json.Unmarshal([]byte(body), &resp)
	if resp.ClientsAdded != 2 || resp.PaymentsLinked != 4 || len(resp.Clients) != 2 {
		t.Fatalf("response: %s", body)
	}
	mara, lia := resp.Clients[0], resp.Clients[1]
	if c := clientOf(t, f, m1); c == nil || *c != mara.ID {
		t.Errorf("m1 not linked to Mara")
	}
	if c := clientOf(t, f, m2); c == nil || *c != mara.ID {
		t.Errorf("m2 not linked to Mara")
	}
	if c := clientOf(t, f, g1); c == nil || *c != f.clientID {
		t.Errorf("g1 not linked to Galerie Nord")
	}
	// A new client from a PayPal row takes the sender's email; one from an
	// N26 row has none to take.
	if lia.PayPalEmail != "lia@example.test" || mara.PayPalEmail != "" {
		t.Errorf("paypal emails: lia %q, mara %q", lia.PayPalEmail, mara.PayPalEmail)
	}
	// Galerie Nord already has an email, so it keeps it.
	g, _ := data_models.GetInvoicingClient(f.db, u, f.clientID)
	if g.PayPalEmail != "pay@galerie.test" {
		t.Errorf("an existing client's PayPal email was overwritten: %q", g.PayPalEmail)
	}
	if got := countRows(t, f.invoiceFixture, &data_models.InvoicingClient{}); got != clients0+2 {
		t.Errorf("clients: %d, want %d", got, clients0+2)
	}
	// Two client creates and four payment links.
	if got := countRows(t, f.invoiceFixture, &data_models.InvoicingAuditEvent{}); got != audits0+6 {
		t.Errorf("audit events: %d, want %d", got, audits0+6)
	}
	if r := f.candidates(t); len(r.Payers) != 0 {
		t.Errorf("linked payments are still candidates: %+v", r.Payers)
	}
}

func TestAddPayers_ExistingClientGetsTheEmailOnlyWhenItHasNone(t *testing.T) {
	f := newPayerFixture(t)
	u := uint(invoiceFixtureUser)
	bare := data_models.InvoicingClient{OwnerUserID: u, Name: "Lia Beispiel"}
	f.db.Create(&bare)
	l1 := f.pay(t, u, "paypal", "Lia", "lia@example.test", "uncategorised", 3000, 3)
	if code, body := addPayers(f, fmt.Sprintf(`{"payers":[{"payment_ids":[%d],"client_id":%d}]}`, l1, bare.ID)); code != http.StatusOK {
		t.Fatalf("add: %d %s", code, body)
	}
	got, _ := data_models.GetInvoicingClient(f.db, u, bare.ID)
	if got.PayPalEmail != "lia@example.test" {
		t.Errorf("an existing client with no email should take the row's: %q", got.PayPalEmail)
	}
}

// One bad row refuses the whole add: nothing is written.
func TestAddPayers_AllOrNothing(t *testing.T) {
	f := newPayerFixture(t)
	u := uint(invoiceFixtureUser)
	m1 := f.pay(t, u, "n26", "Mara Esempio", "", "uncategorised", 10000, 1)
	other := f.pay(t, u+1, "n26", "Mara Esempio", "", "uncategorised", 10000, 1)
	gift := f.pay(t, u, "n26", "Tante", "", "gift", 10000, 2)
	clients0 := countRows(t, f.invoiceFixture, &data_models.InvoicingClient{})
	for name, id := range map[string]uint{
		"another owner's payment":    other,
		"a payment left out by rule": gift,
		"an already linked payment":  f.payments[0].ID,
		"a missing payment":          999999,
	} {
		code, body := addPayers(f, fmt.Sprintf(`{"payers":[{"payment_ids":[%d],"new_client_name":"Mara Esempio"},{"payment_ids":[%d],"new_client_name":"Someone"}]}`, m1, id))
		if code != http.StatusConflict || !strings.Contains(body, "payer_stale") {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
	if got := countRows(t, f.invoiceFixture, &data_models.InvoicingClient{}); got != clients0 {
		t.Errorf("a refused add created clients: %d, want %d", got, clients0)
	}
	if c := clientOf(t, f, m1); c != nil {
		t.Errorf("a refused add linked a payment")
	}
	if c := clientOf(t, f, other); c != nil {
		t.Errorf("another owner's payment was touched")
	}
}

func TestAddPayers_Refusals(t *testing.T) {
	f := newPayerFixture(t)
	u := uint(invoiceFixtureUser)
	m1 := f.pay(t, u, "n26", "Mara Esempio", "", "uncategorised", 10000, 1)
	m2 := f.pay(t, u, "n26", "Lia", "", "uncategorised", 10000, 2)
	for name, tc := range map[string]struct {
		body string
		code int
		key  string
	}{
		"empty batch":           {`{"payers":[]}`, 400, "invalid_field"},
		"neither field":         {fmt.Sprintf(`{"payers":[{"payment_ids":[%d]}]}`, m1), 400, "invalid_field"},
		"both fields":           {fmt.Sprintf(`{"payers":[{"payment_ids":[%d],"client_id":%d,"new_client_name":"X"}]}`, m1, f.clientID), 400, "invalid_field"},
		"no payments":           {`{"payers":[{"payment_ids":[],"new_client_name":"X"}]}`, 400, "invalid_field"},
		"repeated payment":      {fmt.Sprintf(`{"payers":[{"payment_ids":[%d],"new_client_name":"A"},{"payment_ids":[%d],"new_client_name":"B"}]}`, m1, m1), 400, "invalid_field"},
		"punctuation-only name": {fmt.Sprintf(`{"payers":[{"payment_ids":[%d],"new_client_name":"..."}]}`, m1), 400, "invalid_field"},
		"newline in name":       {fmt.Sprintf(`{"payers":[{"payment_ids":[%d],"new_client_name":"A\nB"}]}`, m1), 400, "invalid_field"},
		"one new client twice":  {fmt.Sprintf(`{"payers":[{"payment_ids":[%d],"new_client_name":"Mara"},{"payment_ids":[%d],"new_client_name":"MARA."}]}`, m1, m2), 400, "invalid_field"},
		"unknown client":        {fmt.Sprintf(`{"payers":[{"payment_ids":[%d],"client_id":999}]}`, m1), 400, "invalid_field"},
		"name already in book":  {fmt.Sprintf(`{"payers":[{"payment_ids":[%d],"new_client_name":"galerie nord"}]}`, m1), 409, "client_exists"},
	} {
		code, body := addPayers(f, tc.body)
		if code != tc.code || !strings.Contains(body, tc.key) {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
	if c := clientOf(t, f, m1); c != nil {
		t.Errorf("a refusal linked a payment")
	}
}

// Two identical adds at once: one links, the other finds the payments no
// longer waiting. sqlite :memory: serialises them on its one connection;
// on Postgres the issuing lock taken before the re-read does the ordering.
func TestAddPayers_ConcurrentAddsLinkOnce(t *testing.T) {
	f := newPayerFixture(t)
	f.db.DB().SetMaxOpenConns(1)
	m1 := f.pay(t, uint(invoiceFixtureUser), "n26", "Mara Esempio", "", "uncategorised", 10000, 1)
	clients0 := countRows(t, f.invoiceFixture, &data_models.InvoicingClient{})
	body := fmt.Sprintf(`{"payers":[{"payment_ids":[%d],"new_client_name":"Mara Esempio"}]}`, m1)
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = addPayers(f, body)
		}(i)
	}
	wg.Wait()
	ok, conflict := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		}
	}
	if ok != 1 || conflict != 1 {
		t.Errorf("codes %v: want one 200 and one 409", codes)
	}
	if got := countRows(t, f.invoiceFixture, &data_models.InvoicingClient{}); got != clients0+1 {
		t.Errorf("clients: %d, want %d", got, clients0+1)
	}
}
