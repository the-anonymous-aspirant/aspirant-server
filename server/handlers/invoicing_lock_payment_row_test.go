package handlers

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"aspirant-online/server/data_models"

	"github.com/jinzhu/gorm"
)

// system_3 #10645 (security c66025): lockPaymentRow is scoped to the owner,
// like lockExpenseRow, so another book's payment id locks nothing.
func TestLockPaymentRow_ScopedToTheOwner(t *testing.T) {
	f := newInvoiceFixture(t)
	other := data_models.InvoicingPayment{
		OwnerUserID: invoiceFixtureUser + 1, Source: "paypal", TransactionID: "OTHER-1", Kind: "incoming",
		PaidOn: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), Currency: "EUR", GrossCents: 100, Category: "paid_work",
	}
	if err := f.db.Create(&other).Error; err != nil {
		t.Fatalf("seed other book's payment: %v", err)
	}

	var touched int64 = -1
	f.db.Callback().Update().After("gorm:update").Register("count_rows_10645", func(s *gorm.Scope) { touched = s.DB().RowsAffected })
	defer f.db.Callback().Update().Remove("count_rows_10645")

	if err := lockPaymentRow(f.db, invoiceFixtureUser, f.payments[0].ID); err != nil || touched != 1 {
		t.Fatalf("own payment: err %v, rows touched %d, want nil and 1", err, touched)
	}
	touched = -1
	if err := lockPaymentRow(f.db, invoiceFixtureUser, other.ID); err != nil || touched != 0 {
		t.Errorf("another book's payment: err %v, rows touched %d, want nil and 0", err, touched)
	}

	// The refusal the caller sees is unchanged: another book's id is a 404.
	if w := invoicingDo(f.r, "POST", fmt.Sprintf("/invoicing/payments/%d/remove", other.ID), ""); w.Code != http.StatusNotFound {
		t.Errorf("remove another book's payment: %d %s, want 404", w.Code, w.Body.String())
	}
}
