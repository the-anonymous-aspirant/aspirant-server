package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"aspirant-online/server/data_models"
)

// system_3 #12274: every refusal the issue press can answer carries the
// figures the dialog words its own sentence from, and an invoice that totals
// 0,00 is refused.

type refusalEnvelope struct {
	Error struct {
		Code    string                 `json:"code"`
		Details map[string]interface{} `json:"details"`
	} `json:"error"`
}

func refusalOf(t *testing.T, body string) refusalEnvelope {
	t.Helper()
	var e refusalEnvelope
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("decode refusal: %v (%s)", err, body)
	}
	return e
}

func putDraft(f *invoiceFixture, id uint, body string) {
	invoicingDo(f.r, "PUT", fmt.Sprintf("/invoicing/invoices/%d", id), body)
}

func TestIssue_EachRefusalCarriesItsFigures(t *testing.T) {
	cases := []struct {
		name    string
		code    string
		details map[string]interface{}
		break_  func(t *testing.T, f *invoiceFixture, id uint)
	}{
		{"no profile", "profile_incomplete", map[string]interface{}{"missing": []interface{}{"name", "address", "steuernummer"}}, func(t *testing.T, f *invoiceFixture, id uint) {
			f.db.Delete(&data_models.InvoicingProfile{}, 1)
		}},
		{"the Steuernummer is missing", "profile_incomplete", map[string]interface{}{"missing": []interface{}{"steuernummer"}}, func(t *testing.T, f *invoiceFixture, id uint) {
			f.db.Model(&data_models.InvoicingProfile{}).Where("id = 1").Update("steuernummer", "")
		}},
		{"no client", "client_incomplete", map[string]interface{}{"missing": "client"}, func(t *testing.T, f *invoiceFixture, id uint) {
			putDraft(f, id, `{"client_id":null}`)
		}},
		{"the client has no name", "client_incomplete", map[string]interface{}{"missing": "name"}, func(t *testing.T, f *invoiceFixture, id uint) {
			f.db.Model(&data_models.InvoicingClient{}).Where("id = ?", f.clientID).Update("name", "")
		}},
		{"no address above the limit", "client_address_required", map[string]interface{}{"total_cents": float64(25001), "limit_cents": float64(25000), "currency": "EUR"}, func(t *testing.T, f *invoiceFixture, id uint) {
			f.db.Model(&data_models.InvoicingClient{}).Where("id = ?", f.clientID).Update("address", "")
			putDraft(f, id, `{"line_items":[{"description":"x","quantity_milli":1000,"unit_price_cents":25001}]}`)
		}},
		{"no lines", "line_items_incomplete", map[string]interface{}{"missing": "lines"}, func(t *testing.T, f *invoiceFixture, id uint) {
			putDraft(f, id, `{"line_items":[]}`)
		}},
		{"line 2 lacks a description", "line_items_incomplete", map[string]interface{}{"position": float64(2)}, func(t *testing.T, f *invoiceFixture, id uint) {
			putDraft(f, id, `{"line_items":[{"description":"Wandbild","quantity_milli":1000,"unit_price_cents":100},{"description":"","quantity_milli":1000,"unit_price_cents":100}]}`)
		}},
		{"a total of 0,00", "total_zero", map[string]interface{}{"total_cents": float64(0), "currency": "EUR"}, func(t *testing.T, f *invoiceFixture, id uint) {
			putDraft(f, id, `{"line_items":[{"description":"Probe","quantity_milli":1,"unit_price_cents":1}]}`)
		}},
		{"no service date", "service_date_invalid", map[string]interface{}{"reason": "missing"}, func(t *testing.T, f *invoiceFixture, id uint) {
			putDraft(f, id, `{"service_date_from":null,"service_date_to":null}`)
		}},
		{"until before from", "service_date_invalid", map[string]interface{}{"reason": "ends_before_start"}, func(t *testing.T, f *invoiceFixture, id uint) {
			putDraft(f, id, `{"service_date_from":"2026-07-10","service_date_to":"2026-07-01"}`)
		}},
		{"dated after today", "service_date_invalid", map[string]interface{}{"reason": "after_today", "date": "2026-10-22"}, func(t *testing.T, f *invoiceFixture, id uint) {
			putDraft(f, id, `{"service_date_from":"2026-10-22","service_date_to":null}`)
		}},
		{"a covered payment was removed", "payment_removed", nil, func(t *testing.T, f *invoiceFixture, id uint) {
			now := time.Now()
			f.db.Model(&data_models.InvoicingPayment{}).Where("id = ?", f.payments[0].ID).Update("removed_at", &now)
		}},
		{"a covered payment is not paid work", "payment_not_paid_work", map[string]interface{}{"reason": "not_paid_work", "category": "gift"}, func(t *testing.T, f *invoiceFixture, id uint) {
			f.db.Model(&data_models.InvoicingPayment{}).Where("id = ?", f.payments[0].ID).Update("category", "gift")
		}},
		{"the draft covers no payment", "payment_not_paid_work", map[string]interface{}{"reason": "no_payments"}, func(t *testing.T, f *invoiceFixture, id uint) {
			f.db.Where("invoice_id = ?", id).Delete(&data_models.InvoicingInvoicePayment{})
		}},
		// A payment that no longer exists is not listed on the draft, so the
		// issue reads the draft as covering no payment; payment_not_found is
		// a create's refusal.
		{"its only payment no longer exists", "payment_not_paid_work", map[string]interface{}{"reason": "no_payments"}, func(t *testing.T, f *invoiceFixture, id uint) {
			f.db.Delete(&data_models.InvoicingPayment{}, f.payments[0].ID)
		}},
		{"characters the font lacks", "unprintable_characters", map[string]interface{}{"characters": "王小明"}, func(t *testing.T, f *invoiceFixture, id uint) {
			putDraft(f, id, `{"line_items":[{"description":"王小明","quantity_milli":1000,"unit_price_cents":100}]}`)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newInvoiceFixture(t)
			id := f.draft(t, 0)
			tc.break_(t, f, id)
			code, _, body := f.issue(id)
			if code != http.StatusUnprocessableEntity {
				t.Fatalf("issue: %d %s", code, body)
			}
			e := refusalOf(t, body)
			if e.Error.Code != tc.code {
				t.Fatalf("code %q, want %q: %s", e.Error.Code, tc.code, body)
			}
			for k, want := range tc.details {
				if got := e.Error.Details[k]; !reflect.DeepEqual(got, want) {
					t.Errorf("details[%s] = %#v, want %#v (%s)", k, got, want, body)
				}
			}
			if id, ok := e.Error.Details["payment_id"]; tc.code != "payment_not_paid_work" && tc.code[:8] == "payment_" && (!ok || id != float64(f.payments[0].ID)) {
				t.Errorf("payment_id %v, want %d", id, f.payments[0].ID)
			}
		})
	}
}

func TestIssue_AlreadyInvoicedNamesTheInvoiceNumber(t *testing.T) {
	f := newInvoiceFixture(t)
	id := f.draft(t, 0)
	other := f.draft(t, 1)
	code, issued, body := f.issue(other)
	if code != http.StatusOK || issued.Invoice.InvoiceNumber == nil {
		t.Fatalf("issue the other: %d %s", code, body)
	}
	// The payment of the open draft is put on the issued one behind its back.
	f.db.Create(&data_models.InvoicingInvoicePayment{InvoiceID: other, PaymentID: f.payments[0].ID})
	code, _, body = f.issue(id)
	e := refusalOf(t, body)
	if code != http.StatusUnprocessableEntity || e.Error.Code != "payment_already_invoiced" ||
		e.Error.Details["invoice_number"] != *issued.Invoice.InvoiceNumber || e.Error.Details["invoice_id"] != float64(other) {
		t.Fatalf("%d %s, want payment_already_invoiced naming %s", code, body, *issued.Invoice.InvoiceNumber)
	}
}

func TestIssue_NotADraftAndAFutureLastInvoice(t *testing.T) {
	f := newInvoiceFixture(t)
	id := f.draft(t, 0)
	if code, _, body := f.issue(id); code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, body)
	}
	code, _, body := f.issue(id)
	if e := refusalOf(t, body); code != http.StatusConflict || e.Error.Code != "not_a_draft" || e.Error.Details["status"] != "issued" {
		t.Errorf("issue twice: %d %s", code, body)
	}

	f.db.Model(&data_models.InvoicingInvoice{}).Where("id = ?", id).Update("issue_date", time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	next := f.draft(t, 1)
	code, _, body = f.issue(next)
	if e := refusalOf(t, body); code != http.StatusUnprocessableEntity || e.Error.Code != "issue_date_before_previous" || e.Error.Details["latest"] != "2026-12-01" {
		t.Errorf("a later last invoice: %d %s", code, body)
	}
}

func TestIssue_TotalZeroIsRefusedAndNoNumberIsUsed(t *testing.T) {
	f := newInvoiceFixture(t)
	before := f.profileNext(t)
	id := f.draft(t, 0)
	putDraft(f, id, `{"line_items":[{"description":"Probe","quantity_milli":1,"unit_price_cents":1}]}`)
	code, _, body := f.issue(id)
	if code != http.StatusUnprocessableEntity || refusalOf(t, body).Error.Code != "total_zero" {
		t.Fatalf("a draft of 0.001 x 0,01 €: %d %s", code, body)
	}
	var inv data_models.InvoicingInvoice
	f.db.First(&inv, id)
	if inv.Status != data_models.InvoiceStatusDraft || inv.InvoiceNumber != nil || f.profileNext(t) != before {
		t.Errorf("a refused issue changed the book: status %s number %v next %d (was %d)", inv.Status, inv.InvoiceNumber, f.profileNext(t), before)
	}
	// One cent is enough.
	putDraft(f, id, `{"line_items":[{"description":"Probe","quantity_milli":1000,"unit_price_cents":1}]}`)
	if code, _, body := f.issue(id); code != http.StatusOK {
		t.Errorf("a draft of 0,01 €: %d %s", code, body)
	}
}

func TestDraftSave_QuantityTooLargeNamesTheLine(t *testing.T) {
	f := newInvoiceFixture(t)
	id := f.draft(t, 0)
	w := invoicingDo(f.r, "PUT", fmt.Sprintf("/invoicing/invoices/%d", id),
		`{"line_items":[{"description":"a","quantity_milli":1000,"unit_price_cents":100},{"description":"b","quantity_milli":10000001,"unit_price_cents":100}]}`)
	e := refusalOf(t, w.Body.String())
	if w.Code != http.StatusBadRequest || e.Error.Code != "quantity_too_large" || e.Error.Details["position"] != float64(2) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// A price over the bound has its own code since #12300, with the line's
	// position in details too.
	w = invoicingDo(f.r, "PUT", fmt.Sprintf("/invoicing/invoices/%d", id),
		`{"line_items":[{"description":"a","quantity_milli":1000,"unit_price_cents":10000000001}]}`)
	if e := refusalOf(t, w.Body.String()); w.Code != http.StatusBadRequest || e.Error.Code != "amount_too_large" || e.Error.Details["position"] != float64(1) {
		t.Errorf("price over the bound: %d %s", w.Code, w.Body.String())
	}
}
