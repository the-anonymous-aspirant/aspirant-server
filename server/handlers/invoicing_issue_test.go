package handlers

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// #8367-B2: drafts, issuing, lock and Storno, with the spec §10 guardrails
// enforced in the backend. Each guardrail test starts from a draft that
// issues cleanly and breaks exactly one thing.

type invoiceFixture struct {
	db       *gorm.DB
	r        *gin.Engine
	clientID uint
	payments []data_models.InvoicingPayment
}

func setupInvoiceRouter(db *gorm.DB, userID uint) *gin.Engine {
	r := setupPaymentsRouter(db, userID)
	r.GET("/invoicing/invoices", ListInvoicesHandler)
	r.POST("/invoicing/invoices", CreateInvoiceDraftHandler)
	r.GET("/invoicing/invoices/:id", GetInvoiceHandler)
	r.PUT("/invoicing/invoices/:id", UpdateInvoiceDraftHandler)
	r.DELETE("/invoicing/invoices/:id", DeleteInvoiceDraftHandler)
	r.POST("/invoicing/invoices/:id/issue", IssueInvoiceHandler)
	r.POST("/invoicing/invoices/:id/cancel", CancelInvoiceHandler)
	r.GET("/invoicing/invoices/:id/pdf", GetInvoicePDFHandler)
	return r
}

// withInvoicingClock pins the issuing clock.
func withInvoicingClock(t *testing.T, at time.Time) {
	t.Helper()
	orig := invoicingNow
	invoicingNow = func() time.Time { return at }
	t.Cleanup(func() { invoicingNow = orig })
}

func newInvoiceFixture(t *testing.T) *invoiceFixture {
	t.Helper()
	db := setupPaymentsDB(t)
	r := setupInvoiceRouter(db, 9)
	withInvoicingClock(t, time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	if w := invoicingDo(r, "PUT", "/invoicing/profile", validProfileBody); w.Code != http.StatusOK {
		t.Fatalf("seed profile: %d %s", w.Code, w.Body.String())
	}
	w := invoicingDo(r, "POST", "/invoicing/clients", `{"name":"Galerie Nord","address":"Nordweg 2\n13353 Berlin","paypal_email":"pay@galerie.test"}`)
	var cl struct {
		Client data_models.InvoicingClient `json:"client"`
	}
	json.Unmarshal(w.Body.Bytes(), &cl)
	f := &invoiceFixture{db: db, r: r, clientID: cl.Client.ID}
	for i, cat := range []string{"paid_work", "paid_work", "paid_work", "gift"} {
		cid := cl.Client.ID
		p := data_models.InvoicingPayment{
			Source: "paypal", TransactionID: fmt.Sprintf("TX%d", i+1), Kind: "incoming",
			PaidOn: time.Date(2026, 7, 10+i, 0, 0, 0, 0, time.UTC), Currency: "EUR",
			GrossCents: int64(10000 * (i + 1)), Note: fmt.Sprintf("Work %d", i+1), Category: cat, ClientID: &cid,
		}
		if err := db.Create(&p).Error; err != nil {
			t.Fatalf("seed payment: %v", err)
		}
		f.payments = append(f.payments, p)
	}
	return f
}

func decodeDetail(t *testing.T, body []byte) invoiceDetail {
	t.Helper()
	var d invoiceDetail
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatalf("decode invoice: %v (%s)", err, body)
	}
	return d
}

// draft creates a draft over the given payments and returns its id.
func (f *invoiceFixture) draft(t *testing.T, paymentIdx ...int) uint {
	t.Helper()
	ids := make([]string, 0, len(paymentIdx))
	for _, i := range paymentIdx {
		ids = append(ids, fmt.Sprint(f.payments[i].ID))
	}
	w := invoicingDo(f.r, "POST", "/invoicing/invoices", `{"payment_ids":[`+strings.Join(ids, ",")+`]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create draft: %d %s", w.Code, w.Body.String())
	}
	return decodeDetail(t, w.Body.Bytes()).Invoice.ID
}

func (f *invoiceFixture) issue(id uint) (int, invoiceDetail, string) {
	w := invoicingDo(f.r, "POST", fmt.Sprintf("/invoicing/invoices/%d/issue", id), "")
	var d invoiceDetail
	json.Unmarshal(w.Body.Bytes(), &d)
	return w.Code, d, w.Body.String()
}

func (f *invoiceFixture) profileNext(t *testing.T) int {
	t.Helper()
	p, err := data_models.GetInvoicingProfile(f.db)
	if err != nil || p == nil {
		t.Fatalf("profile: %v", err)
	}
	return p.NextInvoiceNumber
}

func TestCreateDraft_PrefilledFromPaymentsOnly(t *testing.T) {
	f := newInvoiceFixture(t)
	f.db.Model(&data_models.InvoicingPayment{}).Where("id = ?", f.payments[1].ID).Update("note", "")
	w := invoicingDo(f.r, "POST", "/invoicing/invoices", fmt.Sprintf(`{"payment_ids":[%d,%d]}`, f.payments[0].ID, f.payments[1].ID))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	d := decodeDetail(t, w.Body.Bytes())
	inv := d.Invoice
	if inv.Status != "draft" || inv.Number != nil || inv.InvoiceNumber != nil || inv.IssueDate != nil {
		t.Errorf("a draft carries no number or issue date: %+v", inv)
	}
	if inv.ClientID == nil || *inv.ClientID != f.clientID {
		t.Errorf("client not prefilled from the payments: %v", inv.ClientID)
	}
	if !inv.ServiceDateFrom.Equal(f.payments[0].PaidOn) || !inv.ServiceDateTo.Equal(f.payments[1].PaidOn) {
		t.Errorf("service period: %v..%v", inv.ServiceDateFrom, inv.ServiceDateTo)
	}
	if len(d.LineItems) != 2 || d.LineItems[0].Description != "Work 1" || d.LineItems[1].Description != "" ||
		d.LineItems[0].UnitPriceCents != 10000 || d.LineItems[0].QuantityMilli != 1000 {
		t.Errorf("line items (an empty note must stay an empty description, not an invented one): %+v", d.LineItems)
	}
	if inv.TotalCents != 30000 || len(d.Warnings) != 0 || len(d.Payments) != 2 {
		t.Errorf("total %d warnings %v payments %d", inv.TotalCents, d.Warnings, len(d.Payments))
	}
}

func TestCreateDraft_RefusesNonPaidWorkAndWithdrawals(t *testing.T) {
	f := newInvoiceFixture(t)
	gift := f.payments[3]
	w := invoicingDo(f.r, "POST", "/invoicing/invoices", fmt.Sprintf(`{"payment_ids":[%d]}`, gift.ID))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "payment_not_paid_work") {
		t.Errorf("gift payment: %d %s", w.Code, w.Body.String())
	}
	cases := map[string]string{
		"empty list":      `{"payment_ids":[]}`,
		"unknown payment": `{"payment_ids":[999]}`,
		"repeated id":     fmt.Sprintf(`{"payment_ids":[%d,%d]}`, f.payments[0].ID, f.payments[0].ID),
	}
	for name, body := range cases {
		if w := invoicingDo(f.r, "POST", "/invoicing/invoices", body); w.Code < 400 || w.Code >= 500 {
			t.Errorf("%s: got %d", name, w.Code)
		}
	}
	var n int
	f.db.Model(&data_models.InvoicingInvoice{}).Count(&n)
	if n != 0 {
		t.Errorf("refused creates left %d invoices", n)
	}
}

func TestDraft_ServerOwnedFieldsAreRefusedByName(t *testing.T) {
	f := newInvoiceFixture(t)
	id := f.draft(t, 0)
	path := fmt.Sprintf("/invoicing/invoices/%d", id)
	for _, field := range []string{"issue_date", "invoice_number", "number", "status", "prefix"} {
		body := fmt.Sprintf(`{%q:"2026-07-01"}`, field)
		w := invoicingDo(f.r, "PUT", path, body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "server_owned_field") || !strings.Contains(w.Body.String(), field) {
			t.Errorf("PUT %s: %d %s", field, w.Code, w.Body.String())
		}
		if w := invoicingDo(f.r, "POST", "/invoicing/invoices", fmt.Sprintf(`{"payment_ids":[%d],%q:"x"}`, f.payments[1].ID, field)); w.Code != http.StatusBadRequest {
			t.Errorf("POST with %s: %d", field, w.Code)
		}
		if w := invoicingDo(f.r, "POST", path+"/issue", body); w.Code != http.StatusBadRequest {
			t.Errorf("issue with %s in the body: %d %s", field, w.Code, w.Body.String())
		}
	}
	if code, d, _ := f.issue(id); code != http.StatusOK || d.Invoice.IssueDate.Format(invoicingDateLayout) != "2026-10-21" {
		t.Fatalf("the refusals must not block a clean issue: %d %+v", code, d.Invoice)
	}
}

func TestDraft_EditLineItemsRoundsAndRecomputes(t *testing.T) {
	f := newInvoiceFixture(t)
	id := f.draft(t, 0)
	body := `{"line_items":[{"description":"Portrait, 2,5 Stunden","quantity_milli":2500,"unit_price_cents":1999},{"description":"Material","quantity_milli":1000,"unit_price_cents":5003}],"service_date_from":"2026-07-01","service_date_to":"2026-07-03"}`
	w := invoicingDo(f.r, "PUT", fmt.Sprintf("/invoicing/invoices/%d", id), body)
	if w.Code != http.StatusOK {
		t.Fatalf("edit: %d %s", w.Code, w.Body.String())
	}
	d := decodeDetail(t, w.Body.Bytes())
	// 2.5 × 19.99 = 49.975 → 49.98 (half away from zero).
	if len(d.LineItems) != 2 || d.LineItems[0].LineTotalCents != 4998 || d.Invoice.TotalCents != 4998+5003 {
		t.Fatalf("totals: %+v total %d", d.LineItems, d.Invoice.TotalCents)
	}
	if len(d.Warnings) != 1 || !strings.Contains(d.Warnings[0], "total_mismatch") {
		t.Errorf("a total that differs from the linked payments is a warning: %v", d.Warnings)
	}
	if w := invoicingDo(f.r, "PUT", fmt.Sprintf("/invoicing/invoices/%d", id), `{"line_items":[{"description":"x","quantity_milli":-1,"unit_price_cents":1}]}`); w.Code != http.StatusBadRequest {
		t.Errorf("negative quantity: %d", w.Code)
	}
}

func TestDraft_DeleteDraftOnly(t *testing.T) {
	f := newInvoiceFixture(t)
	id := f.draft(t, 0)
	if w := invoicingDo(f.r, "DELETE", fmt.Sprintf("/invoicing/invoices/%d", id), ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete draft: %d", w.Code)
	}
	var n int
	f.db.Model(&data_models.InvoicingInvoicePayment{}).Count(&n)
	if n != 0 {
		t.Errorf("draft delete left %d payment links", n)
	}
}

func TestIssue_GoldenPath(t *testing.T) {
	f := newInvoiceFixture(t)
	id := f.draft(t, 0, 1)
	code, d, body := f.issue(id)
	if code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, body)
	}
	inv := d.Invoice
	if inv.Status != "issued" || inv.InvoiceNumber == nil || *inv.InvoiceNumber != "2026-001" || inv.Number == nil || *inv.Number != 1 {
		t.Fatalf("number: %+v", inv)
	}
	if inv.IssueDate.Format(invoicingDateLayout) != "2026-10-21" || inv.IssuedBy != 9 || inv.IssuedAt == nil {
		t.Errorf("issue date/by: %+v", inv)
	}
	if !strings.Contains(inv.ProfileSnapshot, "12/345/67890") || !strings.Contains(inv.ClientSnapshot, "Galerie Nord") {
		t.Errorf("snapshots: %q / %q", inv.ProfileSnapshot, inv.ClientSnapshot)
	}
	if f.profileNext(t) != 2 {
		t.Errorf("counter after one issue: %d, want 2", f.profileNext(t))
	}
	events, _ := data_models.ListInvoicingAuditEvents(f.db, "invoice", id)
	if len(events) != 2 || events[0].Action != "issue" || !strings.Contains(events[0].Details, "2026-001") {
		t.Errorf("issue audit: %+v", events)
	}

	// The snapshot does not follow later profile edits.
	invoicingDo(f.r, "PUT", "/invoicing/profile", strings.Replace(validProfileBody, "Flower Example", "Renamed", 1))
	got, _ := data_models.GetInvoicingInvoice(f.db, id)
	if strings.Contains(got.ProfileSnapshot, "Renamed") {
		t.Errorf("issued snapshot changed with the profile")
	}
}

func TestIssue_DateIsBerlinNotUTC(t *testing.T) {
	f := newInvoiceFixture(t)
	// 23:30 UTC on 21 Oct is 01:30 on 22 Oct in Berlin (CEST, UTC+2).
	withInvoicingClock(t, time.Date(2026, 10, 21, 23, 30, 0, 0, time.UTC))
	_, d, body := f.issue(f.draft(t, 0))
	if d.Invoice.IssueDate == nil || d.Invoice.IssueDate.Format(invoicingDateLayout) != "2026-10-22" {
		t.Fatalf("issue date: %v (%s)", d.Invoice.IssueDate, body)
	}
}

func TestIssue_Guardrails(t *testing.T) {
	cases := []struct {
		name   string
		code   int
		want   string
		break_ func(t *testing.T, f *invoiceFixture, id uint)
	}{
		{"empty Steuernummer", 422, "profile_incomplete", func(t *testing.T, f *invoiceFixture, id uint) {
			f.db.Model(&data_models.InvoicingProfile{}).Where("id = 1").Update("steuernummer", "")
		}},
		{"no profile", 422, "profile_incomplete", func(t *testing.T, f *invoiceFixture, id uint) {
			f.db.Delete(&data_models.InvoicingProfile{}, 1)
		}},
		{"no client", 422, "client_incomplete", func(t *testing.T, f *invoiceFixture, id uint) {
			invoicingDo(f.r, "PUT", fmt.Sprintf("/invoicing/invoices/%d", id), `{"client_id":null}`)
		}},
		{"client without address", 422, "client_incomplete", func(t *testing.T, f *invoiceFixture, id uint) {
			f.db.Model(&data_models.InvoicingClient{}).Where("id = ?", f.clientID).Update("address", "")
		}},
		{"no line items", 422, "line_items_incomplete", func(t *testing.T, f *invoiceFixture, id uint) {
			invoicingDo(f.r, "PUT", fmt.Sprintf("/invoicing/invoices/%d", id), `{"line_items":[]}`)
		}},
		{"empty description", 422, "line_items_incomplete", func(t *testing.T, f *invoiceFixture, id uint) {
			invoicingDo(f.r, "PUT", fmt.Sprintf("/invoicing/invoices/%d", id), `{"line_items":[{"description":" ","quantity_milli":1000,"unit_price_cents":100}]}`)
		}},
		{"zero price", 422, "line_items_incomplete", func(t *testing.T, f *invoiceFixture, id uint) {
			invoicingDo(f.r, "PUT", fmt.Sprintf("/invoicing/invoices/%d", id), `{"line_items":[{"description":"x","quantity_milli":1000,"unit_price_cents":0}]}`)
		}},
		{"no service date", 422, "service_date_invalid", func(t *testing.T, f *invoiceFixture, id uint) {
			invoicingDo(f.r, "PUT", fmt.Sprintf("/invoicing/invoices/%d", id), `{"service_date_from":null,"service_date_to":null}`)
		}},
		{"service period ends before it starts", 422, "service_date_invalid", func(t *testing.T, f *invoiceFixture, id uint) {
			invoicingDo(f.r, "PUT", fmt.Sprintf("/invoicing/invoices/%d", id), `{"service_date_from":"2026-07-10","service_date_to":"2026-07-01"}`)
		}},
		{"service date after the issue date", 422, "service_date_invalid", func(t *testing.T, f *invoiceFixture, id uint) {
			invoicingDo(f.r, "PUT", fmt.Sprintf("/invoicing/invoices/%d", id), `{"service_date_from":"2026-10-22","service_date_to":null}`)
		}},
		{"payment recategorised after drafting", 422, "payment_not_paid_work", func(t *testing.T, f *invoiceFixture, id uint) {
			f.db.Model(&data_models.InvoicingPayment{}).Where("id = ?", f.payments[0].ID).Update("category", "gift")
		}},
		{"payment already on another issued invoice", 422, "payment_already_invoiced", func(t *testing.T, f *invoiceFixture, id uint) {
			other := f.draft(t, 0)
			if code, _, body := f.issue(other); code != http.StatusOK {
				t.Fatalf("issue the other invoice: %d %s", code, body)
			}
		}},
		{"clock before the previous issue date", 422, "issue_date_before_previous", func(t *testing.T, f *invoiceFixture, id uint) {
			other := f.draft(t, 1)
			if code, _, body := f.issue(other); code != http.StatusOK {
				t.Fatalf("issue the other invoice: %d %s", code, body)
			}
			withInvoicingClock(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
		}},
		{"already issued", 409, "not_a_draft", func(t *testing.T, f *invoiceFixture, id uint) {
			if code, _, body := f.issue(id); code != http.StatusOK {
				t.Fatalf("first issue: %d %s", code, body)
			}
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			f := newInvoiceFixture(t)
			id := f.draft(t, 0)
			tc.break_(t, f, id)
			nextBefore := 0
			if p, _ := data_models.GetInvoicingProfile(f.db); p != nil {
				nextBefore = p.NextInvoiceNumber
			}
			auditBefore := countAuditEvents(t, f.db)

			code, _, body := f.issue(id)
			if code != tc.code || !strings.Contains(body, tc.want) {
				t.Fatalf("got %d %s, want %d %s", code, body, tc.code, tc.want)
			}
			if p, _ := data_models.GetInvoicingProfile(f.db); p != nil && p.NextInvoiceNumber != nextBefore {
				t.Errorf("a refused issue consumed a number: %d → %d", nextBefore, p.NextInvoiceNumber)
			}
			if after := countAuditEvents(t, f.db); after != auditBefore {
				t.Errorf("a refused issue wrote %d audit rows", after-auditBefore)
			}
			if inv, _ := data_models.GetInvoicingInvoice(f.db, id); tc.want != "not_a_draft" && (inv.Status != "draft" || inv.InvoiceNumber != nil) {
				t.Errorf("a refused issue changed the draft: %+v", inv)
			}
		})
	}
}

func TestIssue_SequentialNumbersHaveNoGapAfterARefusal(t *testing.T) {
	f := newInvoiceFixture(t)
	a, b, c := f.draft(t, 0), f.draft(t, 1), f.draft(t, 2)
	if code, _, _ := f.issue(a); code != 200 {
		t.Fatal("issue a")
	}
	// b fails a guardrail in between; it must not use up 002.
	invoicingDo(f.r, "PUT", fmt.Sprintf("/invoicing/invoices/%d", b), `{"line_items":[]}`)
	if code, _, _ := f.issue(b); code != 422 {
		t.Fatal("b should be refused")
	}
	_, dc, _ := f.issue(c)
	if dc.Invoice.InvoiceNumber == nil || *dc.Invoice.InvoiceNumber != "2026-002" {
		t.Fatalf("after a refusal the next number is %v, want 2026-002", dc.Invoice.InvoiceNumber)
	}
}

func TestIssue_ConcurrentIssuesGetDistinctGapFreeNumbers(t *testing.T) {
	f := newInvoiceFixture(t)
	// sqlite :memory: is one database per connection; pin one connection so
	// every goroutine shares it. Transactions then serialise on the
	// connection, which exercises the counter logic; on Postgres the
	// profile-row lock taken by the UPDATE does the same across connections.
	f.db.DB().SetMaxOpenConns(1)
	var ids []uint
	for i := 0; i < 3; i++ {
		ids = append(ids, f.draft(t, i))
	}
	var wg sync.WaitGroup
	numbers := make(chan string, len(ids))
	for _, id := range ids {
		wg.Add(1)
		go func(id uint) {
			defer wg.Done()
			if code, d, body := f.issue(id); code == 200 {
				numbers <- *d.Invoice.InvoiceNumber
			} else {
				t.Errorf("issue %d: %d %s", id, code, body)
			}
		}(id)
	}
	wg.Wait()
	close(numbers)
	var got []string
	for n := range numbers {
		got = append(got, n)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "2026-001,2026-002,2026-003" {
		t.Fatalf("numbers: %v", got)
	}
}

func TestIssued_IsImmutable(t *testing.T) {
	f := newInvoiceFixture(t)
	id := f.draft(t, 0)
	f.issue(id)
	path := fmt.Sprintf("/invoicing/invoices/%d", id)
	if w := invoicingDo(f.r, "PUT", path, `{"service_date_from":"2026-07-01"}`); w.Code != http.StatusConflict {
		t.Errorf("PUT issued: %d", w.Code)
	}
	if w := invoicingDo(f.r, "DELETE", path, ""); w.Code != http.StatusConflict {
		t.Errorf("DELETE issued: %d", w.Code)
	}
	// The payment under it keeps its paid-work category.
	w := invoicingDo(f.r, "PATCH", fmt.Sprintf("/invoicing/payments/%d", f.payments[0].ID), `{"category":"gift"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "payment_on_issued_invoice") {
		t.Errorf("recategorise an invoiced payment: %d %s", w.Code, w.Body.String())
	}
	// Re-sending the same category is not a change and passes.
	if w := invoicingDo(f.r, "PATCH", fmt.Sprintf("/invoicing/payments/%d", f.payments[0].ID), `{"category":"paid_work"}`); w.Code != http.StatusOK {
		t.Errorf("same category: %d %s", w.Code, w.Body.String())
	}
}

func TestStorno_KeepsTheInvoiceAndFreesItsPayments(t *testing.T) {
	f := newInvoiceFixture(t)
	id := f.draft(t, 0)
	f.issue(id)
	cancel := fmt.Sprintf("/invoicing/invoices/%d/cancel", id)
	if w := invoicingDo(f.r, "POST", cancel, `{"reason":""}`); w.Code != http.StatusBadRequest {
		t.Errorf("cancel without reason: %d", w.Code)
	}
	w := invoicingDo(f.r, "POST", cancel, `{"reason":"Falsche Adresse"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	d := decodeDetail(t, w.Body.Bytes())
	if d.Invoice.Status != "cancelled" || d.Invoice.InvoiceNumber == nil || *d.Invoice.InvoiceNumber != "2026-001" || d.Invoice.CancelReason != "Falsche Adresse" {
		t.Fatalf("cancelled invoice: %+v", d.Invoice)
	}
	if w := invoicingDo(f.r, "POST", cancel, `{"reason":"again"}`); w.Code != http.StatusConflict {
		t.Errorf("second cancel: %d", w.Code)
	}
	if w := invoicingDo(f.r, "DELETE", fmt.Sprintf("/invoicing/invoices/%d", id), ""); w.Code != http.StatusConflict {
		t.Errorf("delete cancelled: %d", w.Code)
	}
	// The corrected invoice covers the same payment and takes the next number.
	_, d2, body := f.issue(f.draft(t, 0))
	if d2.Invoice.InvoiceNumber == nil || *d2.Invoice.InvoiceNumber != "2026-002" {
		t.Fatalf("replacement: %s", body)
	}
	draft := f.draft(t, 1)
	if w := invoicingDo(f.r, "POST", fmt.Sprintf("/invoicing/invoices/%d/cancel", draft), `{"reason":"x"}`); w.Code != http.StatusConflict {
		t.Errorf("cancel a draft: %d", w.Code)
	}
}

func TestProfile_NextNumberIsServerOwnedOnceIssued(t *testing.T) {
	f := newInvoiceFixture(t)
	// Before any issue the counter may be set, e.g. to continue an outside
	// sequence.
	if w := invoicingDo(f.r, "PUT", "/invoicing/profile", strings.Replace(validProfileBody, `"next_invoice_number":1`, `"next_invoice_number":7`, 1)); w.Code != http.StatusOK {
		t.Fatalf("set next before issuing: %d %s", w.Code, w.Body.String())
	}
	_, d, _ := f.issue(f.draft(t, 0))
	if *d.Invoice.InvoiceNumber != "2026-007" {
		t.Fatalf("continued sequence: %v", *d.Invoice.InvoiceNumber)
	}
	w := invoicingDo(f.r, "PUT", "/invoicing/profile", strings.Replace(validProfileBody, `"next_invoice_number":1`, `"next_invoice_number":3`, 1))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "next_number_server_owned") {
		t.Errorf("rewind after issuing: %d %s", w.Code, w.Body.String())
	}
	if w := invoicingDo(f.r, "PUT", "/invoicing/profile", strings.Replace(validProfileBody, `"next_invoice_number":1`, `"next_invoice_number":8`, 1)); w.Code != http.StatusOK {
		t.Errorf("re-saving the server's own next number: %d %s", w.Code, w.Body.String())
	}
	newPrefix := strings.Replace(strings.Replace(validProfileBody, `"2026"`, `"2027"`, 1), `"next_invoice_number":1`, `"next_invoice_number":1`, 1)
	if w := invoicingDo(f.r, "PUT", "/invoicing/profile", newPrefix); w.Code != http.StatusOK {
		t.Errorf("a new prefix starts its own sequence: %d %s", w.Code, w.Body.String())
	}
}

func TestListInvoices_Filters(t *testing.T) {
	f := newInvoiceFixture(t)
	issued := f.draft(t, 0)
	f.issue(issued)
	f.draft(t, 1)
	list := func(q string) []invoiceListRow {
		w := invoicingDo(f.r, "GET", "/invoicing/invoices"+q, "")
		if w.Code != http.StatusOK {
			t.Fatalf("list %s: %d %s", q, w.Code, w.Body.String())
		}
		var resp struct {
			Invoices []invoiceListRow `json:"invoices"`
		}
		json.Unmarshal(w.Body.Bytes(), &resp)
		return resp.Invoices
	}
	if n := len(list("")); n != 2 {
		t.Errorf("all: %d", n)
	}
	if got := list("?status=issued"); len(got) != 1 || got[0].ID != issued || got[0].ClientName != "Galerie Nord" {
		t.Errorf("issued: %+v", got)
	}
	if n := len(list("?issued_from=2026-10-21&issued_to=2026-10-21")); n != 1 {
		t.Errorf("issue-date range: %d", n)
	}
	if n := len(list("?issued_to=2026-10-20")); n != 0 {
		t.Errorf("before the issue date: %d", n)
	}
	if n := len(list("?q=galerie")); n != 2 {
		t.Errorf("client search: %d", n)
	}
	if n := len(list("?q=nobody")); n != 0 {
		t.Errorf("client search miss: %d", n)
	}
	if w := invoicingDo(f.r, "GET", "/invoicing/invoices?status=paid", ""); w.Code != http.StatusBadRequest {
		t.Errorf("bad status: %d", w.Code)
	}
}

// @security F1 (c51467 on #8371): a second issue of the same invoice is
// refused and leaves the counter alone. On Postgres the profile- and
// invoice-row locks taken before any read serialise concurrent issues; this
// pins the sequential half of that contract.
func TestIssue_SecondIssueOfTheSameInvoiceIs409AndKeepsTheCounter(t *testing.T) {
	f := newInvoiceFixture(t)
	id := f.draft(t, 0)
	if code, _, body := f.issue(id); code != http.StatusOK {
		t.Fatalf("first issue: %d %s", code, body)
	}
	next := f.profileNext(t)
	code, _, body := f.issue(id)
	if code != http.StatusConflict || !strings.Contains(body, "not_a_draft") {
		t.Fatalf("second issue: %d %s", code, body)
	}
	if f.profileNext(t) != next {
		t.Errorf("a refused second issue moved the counter %d → %d", next, f.profileNext(t))
	}
	inv, _ := data_models.GetInvoicingInvoice(f.db, id)
	if *inv.InvoiceNumber != "2026-001" {
		t.Errorf("the second issue renumbered the invoice: %s", *inv.InvoiceNumber)
	}
}

// Two drafts sharing one payment: whichever issues first wins, the other is
// refused, and the payment ends up on exactly one issued invoice.
func TestIssue_TwoDraftsSharingAPaymentIssueOnlyOnce(t *testing.T) {
	f := newInvoiceFixture(t)
	f.db.DB().SetMaxOpenConns(1)
	a, b := f.draft(t, 0), f.draft(t, 0)
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for _, id := range []uint{a, b} {
		wg.Add(1)
		go func(id uint) { defer wg.Done(); c, _, _ := f.issue(id); codes <- c }(id)
	}
	wg.Wait()
	close(codes)
	ok, refused := 0, 0
	for c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusUnprocessableEntity:
			refused++
		}
	}
	if ok != 1 || refused != 1 {
		t.Fatalf("issued %d, refused %d; want exactly one of each", ok, refused)
	}
	if f.profileNext(t) != 2 {
		t.Errorf("counter: %d, want 2 (one number used, no gap)", f.profileNext(t))
	}
}

func TestIssue_ChunkedBodyIsRefused(t *testing.T) {
	f := newInvoiceFixture(t)
	id := f.draft(t, 0)
	req, _ := http.NewRequest("POST", fmt.Sprintf("/invoicing/invoices/%d/issue", id), strings.NewReader(`{"issue_date":"2026-07-01"}`))
	req.ContentLength = -1 // what net/http sets for a chunked request body
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "issue_date") {
		t.Fatalf("chunked body naming issue_date: %d %s", w.Code, w.Body.String())
	}
}

func TestDraft_LineItemBoundsKeepTheProductInsideInt64(t *testing.T) {
	f := newInvoiceFixture(t)
	path := fmt.Sprintf("/invoicing/invoices/%d", f.draft(t, 0))
	for _, body := range []string{
		`{"line_items":[{"description":"x","quantity_milli":10000001,"unit_price_cents":1}]}`,
		`{"line_items":[{"description":"x","quantity_milli":1000,"unit_price_cents":10000000001}]}`,
		`{"line_items":[{"description":"x","quantity_milli":9223372036854775807,"unit_price_cents":9223372036854775807}]}`,
	} {
		if w := invoicingDo(f.r, "PUT", path, body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	if w := invoicingDo(f.r, "PUT", path, `{"line_items":[{"description":"x","quantity_milli":10000000,"unit_price_cents":10000000000}]}`); w.Code != http.StatusOK {
		t.Errorf("the bounds themselves are allowed: %d %s", w.Code, w.Body.String())
	}
}

// #8367-C1: the PDF is rendered inside the issue and served from storage.
func TestInvoicePDF_StoredAtIssueAndServedUnchanged(t *testing.T) {
	f := newInvoiceFixture(t)
	draft := f.draft(t, 1)
	if w := invoicingDo(f.r, "GET", fmt.Sprintf("/invoicing/invoices/%d/pdf", draft), ""); w.Code != http.StatusConflict {
		t.Errorf("a draft's PDF: %d, want 409", w.Code)
	}

	id := f.draft(t, 0)
	if code, _, body := f.issue(id); code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, body)
	}
	var stored []data_models.InvoicingInvoicePDF
	f.db.Where("invoice_id = ?", id).Find(&stored)
	if len(stored) != 1 || len(stored[0].Content) == 0 {
		t.Fatalf("issue stored %d PDFs", len(stored))
	}

	get := func() *httptest.ResponseRecorder {
		return invoicingDo(f.r, "GET", fmt.Sprintf("/invoicing/invoices/%d/pdf", id), "")
	}
	w := get()
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/pdf" ||
		w.Header().Get("Content-Disposition") != `attachment; filename="Rechnung-2026-001.pdf"` {
		t.Fatalf("serve: %d %v", w.Code, w.Header())
	}
	if !bytes.HasPrefix(w.Body.Bytes(), []byte("%PDF-")) || !bytes.Equal(w.Body.Bytes(), stored[0].Content) {
		t.Fatalf("served bytes are not the stored PDF")
	}
	sum := sha256.Sum256(w.Body.Bytes())
	if hex.EncodeToString(sum[:]) != stored[0].SHA256 || w.Header().Get("X-Content-SHA256") != stored[0].SHA256 {
		t.Errorf("sha256 mismatch")
	}
	events, _ := data_models.ListInvoicingAuditEvents(f.db, "invoice", id)
	if !strings.Contains(events[0].Details, stored[0].SHA256) {
		t.Errorf("issue audit does not record the PDF hash: %s", events[0].Details)
	}

	// A later profile edit never reaches an issued document.
	invoicingDo(f.r, "PUT", "/invoicing/profile", strings.Replace(validProfileBody, "Flower Example", "Renamed", 1))
	if again := get(); !bytes.Equal(again.Body.Bytes(), stored[0].Content) {
		t.Errorf("the served PDF changed after a profile edit")
	}

	// Cancelled invoices keep their PDF.
	invoicingDo(f.r, "POST", fmt.Sprintf("/invoicing/invoices/%d/cancel", id), `{"reason":"x"}`)
	if w := get(); w.Code != http.StatusOK {
		t.Errorf("a cancelled invoice's PDF: %d", w.Code)
	}
}

func TestInvoicePDF_RefusedIssueStoresNoPDF(t *testing.T) {
	f := newInvoiceFixture(t)
	id := f.draft(t, 0)
	f.db.Model(&data_models.InvoicingProfile{}).Where("id = 1").Update("steuernummer", "")
	if code, _, _ := f.issue(id); code != http.StatusUnprocessableEntity {
		t.Fatalf("issue should be refused, got %d", code)
	}
	var n int
	f.db.Model(&data_models.InvoicingInvoicePDF{}).Count(&n)
	if n != 0 {
		t.Errorf("a refused issue stored %d PDFs", n)
	}
}
