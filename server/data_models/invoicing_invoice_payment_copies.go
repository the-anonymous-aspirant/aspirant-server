package data_models

import (
	"time"

	"github.com/jinzhu/gorm"
)

// InvoicingInvoicePaymentCopy is a numbered invoice's own record of a payment
// it covers, as the payment stood when the invoice was issued (system_3
// #12266, from the #11498 review's C3-11).
//
// A payment is locked only while an issued invoice covers it. After a Storno
// it is free again: it can be edited, removed and deleted for good, which is
// the point of cancelling. So a numbered invoice must not read the payment
// live, or its CSV row and its detail change with the payment while its
// stored PDF does not. The copy holds what those readers use: the payment's
// id, date, how it was received, its reference or the provider's own id, its
// amount and currency, and its kind and category.
//
// Issuing writes the copy. A Storno writes it when the invoice has none (one
// issued before the copy existed): at that moment the payment is still locked
// by the issued invoice, so the live row is still what it was at issue. Both
// only add rows to this table; nothing here changes a stored row. Delete for
// good erases the payment and its links, and leaves the copy.
//
// The payer's IBAN, name, email and note are not copied: the readers do not
// use them (security C4/C5, c75108 on system_3 #11551).
type InvoicingInvoicePaymentCopy struct {
	ID            uint      `gorm:"primary_key"`
	InvoiceID     uint      `gorm:"not null;unique_index:idx_invoicing_invoice_payment_copy"`
	PaymentID     uint      `gorm:"not null;unique_index:idx_invoicing_invoice_payment_copy"`
	Source        string    `gorm:"type:text;not null;default:''"`
	Entry         string    `gorm:"type:text;not null;default:''"`
	TransactionID string    `gorm:"type:text;not null;default:''"`
	Reference     string    `gorm:"type:text;not null;default:''"`
	Method        string    `gorm:"type:text;not null;default:''"`
	Kind          string    `gorm:"type:text;not null;default:''"`
	Category      string    `gorm:"type:text;not null;default:''"`
	PaidOn        time.Time `gorm:"type:date;not null"`
	Currency      string    `gorm:"type:text;not null;default:''"`
	GrossCents    int64     `gorm:"not null;default:0"`
	CreatedAt     time.Time
}

func (InvoicingInvoicePaymentCopy) TableName() string { return "invoicing_invoice_payment_copies" }

// CopyInvoicePayments writes the invoice's copy of the payments it covers
// now, unless it already has one. Call it inside the issuing or cancelling
// transaction, before the payments are free.
func CopyInvoicePayments(tx *gorm.DB, invoiceID uint) error {
	var n int
	if err := tx.Model(&InvoicingInvoicePaymentCopy{}).Where("invoice_id = ?", invoiceID).Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	payments, err := ListInvoicePayments(tx, invoiceID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, p := range payments {
		row := InvoicingInvoicePaymentCopy{
			InvoiceID: invoiceID, PaymentID: p.ID, Source: p.Source, Entry: p.Entry, TransactionID: p.TransactionID,
			Reference: p.Reference, Method: p.Method, Kind: p.Kind, Category: p.Category, PaidOn: p.PaidOn,
			Currency: p.Currency, GrossCents: p.GrossCents, CreatedAt: now,
		}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
	}
	return nil
}

// ListInvoiceRecordPayments returns the payments as the invoice's record
// shows them: the invoice's copy for a numbered invoice that has one, else
// the live payments (a draft, or an invoice issued before the copy existed
// and not cancelled since). The rows carry the copied fields only; the rest
// are zero.
func ListInvoiceRecordPayments(db *gorm.DB, inv InvoicingInvoice) ([]InvoicingPayment, error) {
	if inv.InvoiceNumber == nil {
		return ListInvoicePayments(db, inv.ID)
	}
	var copies []InvoicingInvoicePaymentCopy
	if err := db.Where("invoice_id = ?", inv.ID).Order("paid_on asc, payment_id asc").Find(&copies).Error; err != nil {
		return nil, err
	}
	if len(copies) == 0 {
		return ListInvoicePayments(db, inv.ID)
	}
	out := make([]InvoicingPayment, 0, len(copies))
	for _, c := range copies {
		out = append(out, InvoicingPayment{
			ID: c.PaymentID, Source: c.Source, Entry: c.Entry, TransactionID: c.TransactionID, Reference: c.Reference,
			Method: c.Method, Kind: c.Kind, Category: c.Category, PaidOn: c.PaidOn, Currency: c.Currency,
			GrossCents: c.GrossCents,
		})
	}
	return out, nil
}
