package handlers

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// system_3 #12255: issue, update, delete and cancel answer 404 for an invoice
// id the caller has no invoice under, the same as for an id that exists
// nowhere, and each request ends its transaction.
//
// The fixture's database gets one connection. A request that left its
// transaction open would keep that connection, and the next request would
// wait for it forever; `within` turns that wait into a failure.

// unknownInvoiceCaller signs in to the second router. The fixture's invoices
// are unknown to it.
const unknownInvoiceCaller uint = 10

// callerInvoiceRouter mounts the invoice routes behind RecoverPanics, as
// main.go does, signed in as unknownInvoiceCaller.
func callerInvoiceRouter(db *gorm.DB) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RecoverPanics())
	r.Use(func(c *gin.Context) {
		c.Set("db", db)
		c.Set("user_id", unknownInvoiceCaller)
		c.Next()
	})
	r.PUT("/invoicing/profile", PutInvoicingProfileHandler)
	r.PUT("/invoicing/invoices/:id", UpdateInvoiceDraftHandler)
	r.DELETE("/invoicing/invoices/:id", DeleteInvoiceDraftHandler)
	r.POST("/invoicing/invoices/:id/issue", IssueInvoiceHandler)
	r.POST("/invoicing/invoices/:id/cancel", CancelInvoiceHandler)
	return r
}

// within runs one request and fails the test if it has not answered in 5s.
func within(t *testing.T, what string, do func() *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- do() }()
	select {
	case w := <-done:
		return w
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not answer within 5s", what)
		return nil
	}
}

func oneConnectionFixture(t *testing.T) (*invoiceFixture, *gin.Engine) {
	t.Helper()
	f := newInvoiceFixture(t)
	f.db.DB().SetMaxOpenConns(1)
	return f, callerInvoiceRouter(f.db)
}

const lineItemEdit = `{"line_items":[{"description":"Invented work","quantity_milli":1000,"unit_price_cents":2500}]}`

// expectNotFound checks a 404 "Invoice not found" in the usual envelope. It
// does not stop the test, so the checks after it still say whether the next
// requests answer.
func expectNotFound(t *testing.T, what string, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "Invoice not found") {
		t.Errorf("%s: got %d %s, want 404 Invoice not found", what, w.Code, w.Body.String())
	}
}

// thenStillWorks checks, each within the timeout, that the given draft can
// still be edited through the fixture's router and that the caller can still
// save its profile.
func thenStillWorks(t *testing.T, f *invoiceFixture, caller *gin.Engine, draft uint) {
	t.Helper()
	path := fmt.Sprintf("/invoicing/invoices/%d", draft)
	if w := within(t, "the next edit", func() *httptest.ResponseRecorder { return invoicingDo(f.r, "PUT", path, lineItemEdit) }); w.Code != http.StatusOK {
		t.Fatalf("the next edit: %d %s", w.Code, w.Body.String())
	}
	if w := within(t, "the caller's profile save", func() *httptest.ResponseRecorder {
		return invoicingDo(caller, "PUT", "/invoicing/profile", validProfileBody)
	}); w.Code != http.StatusOK {
		t.Fatalf("the caller's profile save: %d %s", w.Code, w.Body.String())
	}
}

func TestIssue_UnknownInvoiceIs404AndTheNextRequestsAnswer(t *testing.T) {
	f, caller := oneConnectionFixture(t)
	draft := f.draft(t, 0)

	// The caller has no issuer profile yet: the 404 comes before the profile
	// check, so it reads the same as for an id that exists nowhere.
	issue := func(id uint) *httptest.ResponseRecorder {
		return invoicingDo(caller, "POST", fmt.Sprintf("/invoicing/invoices/%d/issue", id), "")
	}
	known := within(t, "issue", func() *httptest.ResponseRecorder { return issue(draft) })
	expectNotFound(t, "issue", known)
	nowhere := within(t, "issue of an id that exists nowhere", func() *httptest.ResponseRecorder { return issue(draft + 1000) })
	expectNotFound(t, "issue of an id that exists nowhere", nowhere)
	if known.Body.String() != nowhere.Body.String() {
		t.Errorf("the two 404s differ:\n%s\n%s", known.Body.String(), nowhere.Body.String())
	}

	thenStillWorks(t, f, caller, draft)
	code, _, body := 0, invoiceDetail{}, ""
	within(t, "the next issue", func() *httptest.ResponseRecorder {
		code, _, body = f.issue(draft)
		return nil
	})
	if code != http.StatusOK {
		t.Fatalf("the next issue: %d %s", code, body)
	}
}

// The same with an issuer profile saved, so every check before the invoice
// read passes.
func TestIssue_UnknownInvoiceIs404WithAProfileAndTheNextRequestsAnswer(t *testing.T) {
	f, caller := oneConnectionFixture(t)
	draft := f.draft(t, 0)
	if w := invoicingDo(caller, "PUT", "/invoicing/profile", validProfileBody); w.Code != http.StatusOK {
		t.Fatalf("seed the caller's profile: %d %s", w.Code, w.Body.String())
	}
	expectNotFound(t, "issue", within(t, "issue", func() *httptest.ResponseRecorder {
		return invoicingDo(caller, "POST", fmt.Sprintf("/invoicing/invoices/%d/issue", draft), "")
	}))
	thenStillWorks(t, f, caller, draft)
}

func TestUpdateDraft_UnknownInvoiceIs404AndTheNextRequestsAnswer(t *testing.T) {
	f, caller := oneConnectionFixture(t)
	draft := f.draft(t, 0)
	expectNotFound(t, "update", within(t, "update", func() *httptest.ResponseRecorder {
		return invoicingDo(caller, "PUT", fmt.Sprintf("/invoicing/invoices/%d", draft), lineItemEdit)
	}))
	thenStillWorks(t, f, caller, draft)
}

func TestDeleteDraft_UnknownInvoiceIs404AndTheNextRequestsAnswer(t *testing.T) {
	f, caller := oneConnectionFixture(t)
	draft := f.draft(t, 0)
	expectNotFound(t, "delete", within(t, "delete", func() *httptest.ResponseRecorder {
		return invoicingDo(caller, "DELETE", fmt.Sprintf("/invoicing/invoices/%d", draft), "")
	}))
	// The draft is still there to edit.
	thenStillWorks(t, f, caller, draft)
}

func TestCancel_UnknownInvoiceIs404AndTheNextRequestsAnswer(t *testing.T) {
	f, caller := oneConnectionFixture(t)
	issued := f.draft(t, 0)
	if code, _, body := f.issue(issued); code != http.StatusOK {
		t.Fatalf("seed issue: %d %s", code, body)
	}
	cancel := fmt.Sprintf("/invoicing/invoices/%d/cancel", issued)
	expectNotFound(t, "cancel", within(t, "cancel", func() *httptest.ResponseRecorder {
		return invoicingDo(caller, "POST", cancel, `{"reason":"Invented reason"}`)
	}))
	thenStillWorks(t, f, caller, f.draft(t, 1))
	if w := within(t, "the next cancel", func() *httptest.ResponseRecorder {
		return invoicingDo(f.r, "POST", cancel, `{"reason":"Invented reason"}`)
	}); w.Code != http.StatusOK {
		t.Fatalf("the next cancel: %d %s", w.Code, w.Body.String())
	}
}
