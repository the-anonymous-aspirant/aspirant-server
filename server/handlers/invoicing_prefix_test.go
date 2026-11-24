package handlers

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"aspirant-online/server/data_models"
)

// The invoice number prefix: its hyphens, and its case (system_3 #12272, the
// #11498 review's C3-18).

func profileWithPrefix(prefix string, next int) string {
	body := strings.Replace(validProfileBody, `"invoice_number_prefix":"2026"`, fmt.Sprintf(`"invoice_number_prefix":%q`, prefix), 1)
	return strings.Replace(body, `"next_invoice_number":1`, fmt.Sprintf(`"next_invoice_number":%d`, next), 1)
}

func TestPrefix_HyphensAreRefusedAtTheField(t *testing.T) {
	for prefix, want := range map[string]struct{ code, message string }{
		"2026-":  {codePrefixEndsWithHyphen, "The prefix cannot end with a hyphen: the app adds one before the number."},
		"-":      {codePrefixEndsWithHyphen, "The prefix cannot end with a hyphen: the app adds one before the number."},
		"-2026":  {codePrefixStartsWithHyphen, "The prefix cannot start with a hyphen."},
		"20--26": {codePrefixDoubleHyphen, "The prefix cannot hold two hyphens in a row."},
		"RE":     {},
		"RE-26":  {},
		"a-b-c":  {},
	} {
		r := setupInvoicingRouter(setupInvoicingDB(t), 42)
		w := invoicingDo(r, "PUT", "/invoicing/profile", profileWithPrefix(prefix, 1))
		if want.code == "" {
			if w.Code != http.StatusOK {
				t.Errorf("%q: want 200, got %d %s", prefix, w.Code, w.Body.String())
			}
			continue
		}
		if w.Code != http.StatusBadRequest || errorCode(t, w) != want.code || !strings.Contains(w.Body.String(), want.message) {
			t.Errorf("%q: want 400 %s, got %d %s", prefix, want.code, w.Code, w.Body.String())
		}
	}
}

// A prefix stored before the rule keeps saving the profile's other fields:
// only a new or changed prefix is checked (the review's point 1).
func TestPrefix_AStoredPrefixWithAHyphenStillSaves(t *testing.T) {
	db := setupInvoicingDB(t)
	r := setupInvoicingRouter(db, 42)
	if w := invoicingDo(r, "PUT", "/invoicing/profile", validProfileBody); w.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", w.Code, w.Body.String())
	}
	if err := db.Model(&data_models.InvoicingProfile{}).Where("owner_user_id = ?", 42).
		UpdateColumn("invoice_number_prefix", "2026-").Error; err != nil {
		t.Fatalf("store the old prefix: %v", err)
	}
	body := strings.Replace(profileWithPrefix("2026-", 1), "flower@example.test", "new@example.test", 1)
	if w := invoicingDo(r, "PUT", "/invoicing/profile", body); w.Code != http.StatusOK {
		t.Fatalf("a Save of the email with the stored prefix: %d %s", w.Code, w.Body.String())
	}
	if p, _ := data_models.GetInvoicingProfile(db, 42); p == nil || p.Email != "new@example.test" || p.InvoiceNumberPrefix != "2026-" {
		t.Fatalf("the Save did not land: %+v", p)
	}
	if w := invoicingDo(r, "PUT", "/invoicing/profile", profileWithPrefix("2027-", 1)); w.Code != http.StatusBadRequest || errorCode(t, w) != codePrefixEndsWithHyphen {
		t.Errorf("a changed prefix is checked: %d %s", w.Code, w.Body.String())
	}
	if w := invoicingDo(r, "PUT", "/invoicing/profile", profileWithPrefix("2026", 1)); w.Code != http.StatusOK {
		t.Errorf("the hyphen dropped: %d %s", w.Code, w.Body.String())
	}
}

// Changing "r" to "R" continues the series: the save reads the numbers
// issued under either case.
func TestPrefix_ChangingCaseContinuesTheSeries(t *testing.T) {
	f := newInvoiceFixture(t)
	if w := invoicingDo(f.r, "PUT", "/invoicing/profile", profileWithPrefix("r", 1)); w.Code != http.StatusOK {
		t.Fatalf("prefix r: %d %s", w.Code, w.Body.String())
	}
	for i, want := range []string{"r-001", "r-002"} {
		if _, d, body := f.issue(f.draft(t, i)); d.Invoice.InvoiceNumber == nil || *d.Invoice.InvoiceNumber != want {
			t.Fatalf("issue %d: %s", i, body)
		}
	}
	w := invoicingDo(f.r, "PUT", "/invoicing/profile", profileWithPrefix("R", 1))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "next_number_server_owned") || !strings.Contains(w.Body.String(), "the next number is 3") {
		t.Fatalf(`"R" starting again at 1: %d %s`, w.Code, w.Body.String())
	}
	if w := invoicingDo(f.r, "PUT", "/invoicing/profile", profileWithPrefix("R", 3)); w.Code != http.StatusOK {
		t.Fatalf(`"R" at 3: %d %s`, w.Code, w.Body.String())
	}
	if _, d, body := f.issue(f.draft(t, 2)); d.Invoice.InvoiceNumber == nil || *d.Invoice.InvoiceNumber != "R-003" {
		t.Fatalf("the series continued: %s", body)
	}
	if rep := f.validation(t); !rep.OK || len(rep.Violations) != 0 || len(rep.Warnings) != 0 {
		t.Errorf("r-001, r-002, R-003 is one clean series: %+v", rep)
	}
}

// A book that already holds r-001 and R-001 (issued before the fold) is not
// rewritten. Its check reads a warning, not a violation, so it can still
// export; and "R" continued past "r"'s numbers reads no gap.
func TestValidation_TwoCasesOfOneNumberReadAWarning(t *testing.T) {
	f := newInvoiceFixture(t)
	f.db.Model(&data_models.InvoicingPayment{}).Where("id = ?", f.payments[3].ID).Update("category", "paid_work")
	if w := invoicingDo(f.r, "PUT", "/invoicing/profile", profileWithPrefix("r", 1)); w.Code != http.StatusOK {
		t.Fatalf("prefix r: %d %s", w.Code, w.Body.String())
	}
	var ids []uint
	for i := 0; i < 3; i++ {
		id := f.draft(t, i)
		if code, _, body := f.issue(id); code != http.StatusOK {
			t.Fatalf("issue %d: %s", i, body)
		}
		ids = append(ids, id)
	}
	// The third was issued as R-001, as a book switched to "R" before the
	// fold would have: R's own series started again at 1.
	f.db.Model(&data_models.InvoicingInvoice{}).Where("id = ?", ids[2]).
		Updates(map[string]interface{}{"prefix": "R", "number": 1, "invoice_number": "R-001"})
	if w := invoicingDo(f.r, "PUT", "/invoicing/profile", profileWithPrefix("R", 3)); w.Code != http.StatusOK {
		t.Fatalf(`"R" at 3: %d %s`, w.Code, w.Body.String())
	}
	if _, d, body := f.issue(f.draft(t, 3)); d.Invoice.InvoiceNumber == nil || *d.Invoice.InvoiceNumber != "R-003" {
		t.Fatalf("the next one: %s", body)
	}

	rep := f.validation(t)
	if !rep.OK || len(rep.Violations) != 0 {
		t.Errorf("R-001 then R-003, with r-002 between, is no gap: %+v", rep)
	}
	if got := codes(rep.Warnings); len(got) != 1 || got[0] != "number_case_twin" ||
		rep.Warnings[0].Message != "Invoices R-001 and r-001 have the same number; their prefixes differ only in capital letters." {
		t.Errorf("warnings: %+v", rep.Warnings)
	}
}
