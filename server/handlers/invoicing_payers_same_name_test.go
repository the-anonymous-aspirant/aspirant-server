package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"aspirant-online/server/data_models"
)

// system_3 #12319: one person who paid by PayPal and by bank transfer is two
// rows in "Add payers". Ticked with one new name, they are one client
// (§3.142 Decision 4), unless a row asks for a second one.

type addPayersResp struct {
	ClientsAdded   int                           `json:"clients_added"`
	PaymentsLinked int                           `json:"payments_linked"`
	Clients        []data_models.InvoicingClient `json:"clients"`
}

func mustAddPayers(t *testing.T, f *payerFixture, body string) addPayersResp {
	t.Helper()
	code, out := addPayers(f, body)
	if code != http.StatusOK {
		t.Fatalf("add: %d %s", code, out)
	}
	var r addPayersResp
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return r
}

// payIBAN seeds an N26 payment with its payer's IBAN.
func (f *payerFixture) payIBAN(t *testing.T, name, iban string, day int) uint {
	t.Helper()
	id := f.pay(t, uint(invoiceFixtureUser), "n26", name, "", "uncategorised", 5000, day)
	if err := f.db.Model(&data_models.InvoicingPayment{}).Where("id = ?", id).UpdateColumn("sender_iban", iban).Error; err != nil {
		t.Fatalf("seed iban: %v", err)
	}
	return id
}

const (
	sameNameIBAN  = "DE89370400440532013000" // the published example IBAN
	sameNameEmail = "mara@example.test"
)

func TestAddPayers_TwoRowsOfOneNameAreOneClient(t *testing.T) {
	for _, order := range []string{"paypal first", "bank first"} {
		t.Run(order, func(t *testing.T) {
			f := newPayerFixture(t)
			u := uint(invoiceFixtureUser)
			pp := f.pay(t, u, "paypal", "Mara Esempio", sameNameEmail, "uncategorised", 3000, 1)
			bank := f.payIBAN(t, "MARA ESEMPIO", sameNameIBAN, 2)
			clients0 := countRows(t, f.invoiceFixture, &data_models.InvoicingClient{})
			audits0 := countRows(t, f.invoiceFixture, &data_models.InvoicingAuditEvent{})

			rows := []string{
				fmt.Sprintf(`{"payment_ids":[%d],"new_client_name":"Mara Esempio"}`, pp),
				fmt.Sprintf(`{"payment_ids":[%d],"new_client_name":"MARA ESEMPIO"}`, bank),
			}
			first := "Mara Esempio"
			if order == "bank first" {
				rows[0], rows[1] = rows[1], rows[0]
				first = "MARA ESEMPIO"
			}
			r := mustAddPayers(t, f, `{"payers":[`+strings.Join(rows, ",")+`]}`)
			if r.ClientsAdded != 1 || r.PaymentsLinked != 2 || len(r.Clients) != 1 {
				t.Fatalf("response: %+v", r)
			}
			cl := r.Clients[0]
			// The first ticked row's name, as typed there; the PayPal row's
			// email and the bank row's IBAN, whichever came first.
			if cl.Name != first || cl.PayPalEmail != sameNameEmail || cl.Email != sameNameEmail || cl.IBAN != sameNameIBAN {
				t.Errorf("client: name %q, paypal %q, email %q, iban %q", cl.Name, cl.PayPalEmail, cl.Email, cl.IBAN)
			}
			stored, _ := data_models.GetInvoicingClient(f.db, u, cl.ID)
			if stored == nil || stored.PayPalEmail != sameNameEmail || stored.IBAN != sameNameIBAN {
				t.Errorf("stored client: %+v", stored)
			}
			for _, id := range []uint{pp, bank} {
				if c := clientOf(t, f, id); c == nil || *c != cl.ID {
					t.Errorf("payment %d not linked to the one client", id)
				}
			}
			if got := countRows(t, f.invoiceFixture, &data_models.InvoicingClient{}); got != clients0+1 {
				t.Errorf("clients: %d, want %d", got, clients0+1)
			}
			// One create, with the final fields, and two payment links.
			if got := countRows(t, f.invoiceFixture, &data_models.InvoicingAuditEvent{}); got != audits0+3 {
				t.Errorf("audit events: %d, want %d", got, audits0+3)
			}
		})
	}
}

// Three rows of one person, the third an exact repeat: still one client.
func TestAddPayers_ThreeRowsOfOneNameAreOneClient(t *testing.T) {
	f := newPayerFixture(t)
	u := uint(invoiceFixtureUser)
	a := f.pay(t, u, "paypal", "Mara Esempio", sameNameEmail, "uncategorised", 3000, 1)
	b := f.payIBAN(t, "MARA ESEMPIO", sameNameIBAN, 2)
	c := f.pay(t, u, "n26", "Mara  Esempio", "", "uncategorised", 2000, 3)
	r := mustAddPayers(t, f, fmt.Sprintf(`{"payers":[
		{"payment_ids":[%d],"new_client_name":"Mara Esempio"},
		{"payment_ids":[%d],"new_client_name":"MARA ESEMPIO"},
		{"payment_ids":[%d],"new_client_name":"mara esempio."}]}`, a, b, c))
	if r.ClientsAdded != 1 || r.PaymentsLinked != 3 {
		t.Fatalf("response: %+v", r)
	}
}

// Two rows with two different PayPal emails: the first row's is kept.
func TestAddPayers_TheFirstRowsEmailIsKept(t *testing.T) {
	f := newPayerFixture(t)
	u := uint(invoiceFixtureUser)
	a := f.pay(t, u, "paypal", "Mara Esempio", "mara@example.test", "uncategorised", 3000, 1)
	b := f.pay(t, u, "paypal", "Mara Esempio", "m.esempio@example.test", "uncategorised", 3000, 2)
	r := mustAddPayers(t, f, fmt.Sprintf(`{"payers":[
		{"payment_ids":[%d],"new_client_name":"Mara Esempio"},
		{"payment_ids":[%d],"new_client_name":"Mara Esempio"}]}`, a, b))
	if r.ClientsAdded != 1 || r.Clients[0].PayPalEmail != "mara@example.test" || r.Clients[0].Email != "mara@example.test" {
		t.Fatalf("response: %+v", r)
	}
}

// A later row fills an email the first row could not give: the first's is
// already another client's, so it is left out.
func TestAddPayers_ALaterRowFillsAnEmptyEmail(t *testing.T) {
	f := newPayerFixture(t)
	u := uint(invoiceFixtureUser)
	a := f.pay(t, u, "paypal", "Mara Esempio", "pay@galerie.test", "uncategorised", 3000, 1)
	b := f.pay(t, u, "paypal", "Mara Esempio", "mara@example.test", "uncategorised", 3000, 2)
	r := mustAddPayers(t, f, fmt.Sprintf(`{"payers":[
		{"payment_ids":[%d],"new_client_name":"Mara Esempio"},
		{"payment_ids":[%d],"new_client_name":"Mara Esempio"}]}`, a, b))
	if r.ClientsAdded != 1 || r.Clients[0].PayPalEmail != "mara@example.test" {
		t.Fatalf("response: %+v", r)
	}
}

// "Add as a second Mara Esempio": that row is its own client, and a third
// row of the name joins the first.
func TestAddPayers_ASecondClientRowIsItsOwnClient(t *testing.T) {
	f := newPayerFixture(t)
	u := uint(invoiceFixtureUser)
	a := f.pay(t, u, "paypal", "Mara Esempio", sameNameEmail, "uncategorised", 3000, 1)
	b := f.payIBAN(t, "Mara Esempio", sameNameIBAN, 2)
	c := f.pay(t, u, "n26", "Mara Esempio", "", "uncategorised", 2000, 3)
	r := mustAddPayers(t, f, fmt.Sprintf(`{"payers":[
		{"payment_ids":[%d],"new_client_name":"Mara Esempio"},
		{"payment_ids":[%d],"new_client_name":"Mara Esempio","second_client":true},
		{"payment_ids":[%d],"new_client_name":"Mara Esempio"}]}`, a, b, c))
	if r.ClientsAdded != 2 || r.PaymentsLinked != 3 || len(r.Clients) != 2 {
		t.Fatalf("response: %+v", r)
	}
	first, second := r.Clients[0], r.Clients[1]
	if first.Name != "Mara Esempio" || second.Name != "Mara Esempio" || first.ID == second.ID {
		t.Fatalf("clients: %+v", r.Clients)
	}
	// Each takes only its own rows' fields.
	if first.PayPalEmail != sameNameEmail || first.IBAN != "" || second.PayPalEmail != "" || second.IBAN != sameNameIBAN {
		t.Errorf("fields: first %+v, second %+v", first, second)
	}
	for id, want := range map[uint]uint{a: first.ID, b: second.ID, c: first.ID} {
		if got := clientOf(t, f, id); got == nil || *got != want {
			t.Errorf("payment %d linked to %v, want %d", id, got, want)
		}
	}
}

// A new name that is a client already in the book is still refused, also on
// a second_client row: whether a second client may share a book client's
// name is not this request's to answer.
func TestAddPayers_ABookNameIsStillRefused(t *testing.T) {
	f := newPayerFixture(t)
	u := uint(invoiceFixtureUser)
	a := f.pay(t, u, "n26", "Mara Esempio", "", "uncategorised", 3000, 1)
	b := f.pay(t, u, "n26", "Galerie Nord", "", "uncategorised", 3000, 2)
	clients0 := countRows(t, f.invoiceFixture, &data_models.InvoicingClient{})
	for _, row := range []string{`"new_client_name":"GALERIE NORD"`, `"new_client_name":"Galerie Nord","second_client":true`} {
		code, body := addPayers(f, fmt.Sprintf(`{"payers":[
			{"payment_ids":[%d],"new_client_name":"Mara Esempio"},
			{"payment_ids":[%d],%s}]}`, a, b, row))
		if code != http.StatusConflict || !strings.Contains(body, "client_exists") {
			t.Errorf("%s: %d %s", row, code, body)
		}
	}
	if got := countRows(t, f.invoiceFixture, &data_models.InvoicingClient{}); got != clients0 {
		t.Errorf("a refused add created clients: %d, want %d", got, clients0)
	}
}

// Two new clients of one request never both take one IBAN (security c76401
// on #12319): the second finds the IBAN on the client the first just made,
// as it would find it on a client already in the book.
func TestAddPayers_TwoNewClientsNeverShareAnIBAN(t *testing.T) {
	f := newPayerFixture(t)
	a := f.payIBAN(t, "Mara Esempio", sameNameIBAN, 1)
	b := f.payIBAN(t, "Mara Esempio", sameNameIBAN, 2)
	r := mustAddPayers(t, f, fmt.Sprintf(`{"payers":[
		{"payment_ids":[%d],"new_client_name":"Mara Esempio"},
		{"payment_ids":[%d],"new_client_name":"Mara Esempio","second_client":true}]}`, a, b))
	if r.ClientsAdded != 2 || len(r.Clients) != 2 {
		t.Fatalf("response: %+v", r)
	}
	with := 0
	for _, cl := range r.Clients {
		stored, _ := data_models.GetInvoicingClient(f.db, uint(invoiceFixtureUser), cl.ID)
		if stored != nil && stored.IBAN == sameNameIBAN {
			with++
		}
	}
	if with != 1 {
		t.Errorf("%d of the 2 new clients hold the IBAN, want exactly 1", with)
	}
	if r.Clients[0].IBAN != sameNameIBAN {
		t.Errorf("the first row's client should hold it: %+v", r.Clients)
	}
}
