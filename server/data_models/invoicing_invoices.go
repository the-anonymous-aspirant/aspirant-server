package data_models

import (
	"time"

	"github.com/jinzhu/gorm"
)

// Invoice lifecycle (spec §4.6): a draft is freely editable; issuing assigns
// the number and the issue date server-side and freezes the invoice; a
// correction cancels it (Storno) and issues a new one. Nothing is deleted
// once issued.
const (
	InvoiceStatusDraft     = "draft"
	InvoiceStatusIssued    = "issued"
	InvoiceStatusCancelled = "cancelled"
)

// InvoicingInvoice is one invoice. Number, InvoiceNumber, IssueDate and the
// snapshots stay empty until issue and never change after it.
type InvoicingInvoice struct {
	ID              uint       `json:"id" gorm:"primary_key"`
	Status          string     `json:"status" gorm:"type:text;not null;default:'draft';index"`
	ClientID        *uint      `json:"client_id"`
	ServiceDateFrom *time.Time `json:"service_date_from" gorm:"type:date"`
	ServiceDateTo   *time.Time `json:"service_date_to" gorm:"type:date"`
	// VatRateBP is the VAT rate in basis points (1900 = 19 %), applied only
	// when the issuer is not a Kleinunternehmerin under § 19 UStG.
	VatRateBP       int        `json:"vat_rate_bp" gorm:"not null;default:1900"`
	Currency        string     `json:"currency" gorm:"type:text;not null;default:'EUR'"`
	Prefix          string     `json:"prefix" gorm:"type:text;not null;default:''"`
	Number          *int       `json:"number"`
	InvoiceNumber   *string    `json:"invoice_number" gorm:"type:text;unique_index:idx_invoicing_invoices_number"`
	IssueDate       *time.Time `json:"issue_date" gorm:"type:date"`
	NetCents        int64      `json:"net_cents" gorm:"not null;default:0"`
	VatCents        int64      `json:"vat_cents" gorm:"not null;default:0"`
	TotalCents      int64      `json:"total_cents" gorm:"not null;default:0"`
	ProfileSnapshot string     `json:"profile_snapshot" gorm:"type:text;not null;default:''"`
	ClientSnapshot  string     `json:"client_snapshot" gorm:"type:text;not null;default:''"`
	IssuedAt        *time.Time `json:"issued_at"`
	IssuedBy        uint       `json:"issued_by"`
	CancelledAt     *time.Time `json:"cancelled_at"`
	CancelledBy     uint       `json:"cancelled_by"`
	CancelReason    string     `json:"cancel_reason" gorm:"type:text;not null;default:''"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

func (InvoicingInvoice) TableName() string { return "invoicing_invoices" }

// InvoicingLineItem is one position on an invoice. Quantity is stored in
// thousandths so a fractional quantity (2.5 hours) stays an integer.
type InvoicingLineItem struct {
	ID             uint   `json:"id" gorm:"primary_key"`
	InvoiceID      uint   `json:"invoice_id" gorm:"not null;index"`
	Position       int    `json:"position" gorm:"not null"`
	Description    string `json:"description" gorm:"type:text;not null;default:''"`
	QuantityMilli  int64  `json:"quantity_milli" gorm:"not null"`
	UnitPriceCents int64  `json:"unit_price_cents" gorm:"not null"`
	LineTotalCents int64  `json:"line_total_cents" gorm:"not null"`
}

func (InvoicingLineItem) TableName() string { return "invoicing_line_items" }

// LineTotalCents is quantity × unit price, rounded half away from zero to the
// cent.
func LineTotalCents(quantityMilli, unitPriceCents int64) int64 {
	p := quantityMilli * unitPriceCents
	if p >= 0 {
		return (p + 500) / 1000
	}
	return -((-p + 500) / 1000)
}

// InvoicingInvoicePayment links an invoice to a payment it covers.
type InvoicingInvoicePayment struct {
	ID        uint `json:"-" gorm:"primary_key"`
	InvoiceID uint `json:"invoice_id" gorm:"not null;unique_index:idx_invoicing_invoice_payment"`
	PaymentID uint `json:"payment_id" gorm:"not null;unique_index:idx_invoicing_invoice_payment;index"`
}

func (InvoicingInvoicePayment) TableName() string { return "invoicing_invoice_payments" }

// GetInvoicingInvoice returns the invoice, or nil when the id is unknown.
func GetInvoicingInvoice(db *gorm.DB, id uint) (*InvoicingInvoice, error) {
	var inv InvoicingInvoice
	err := db.Where("id = ?", id).First(&inv).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &inv, nil
}

func ListInvoiceLineItems(db *gorm.DB, invoiceID uint) ([]InvoicingLineItem, error) {
	var items []InvoicingLineItem
	err := db.Where("invoice_id = ?", invoiceID).Order("position asc, id asc").Find(&items).Error
	return items, err
}

// ListInvoicePayments returns the payments linked to an invoice.
func ListInvoicePayments(db *gorm.DB, invoiceID uint) ([]InvoicingPayment, error) {
	var payments []InvoicingPayment
	err := db.Joins("JOIN invoicing_invoice_payments ip ON ip.payment_id = invoicing_payments.id").
		Where("ip.invoice_id = ?", invoiceID).
		Order("invoicing_payments.paid_on asc, invoicing_payments.id asc").
		Find(&payments).Error
	return payments, err
}

// IssuedInvoiceForPayment returns the id of the issued (not cancelled)
// invoice that covers the payment, excluding exceptInvoiceID, or 0.
func IssuedInvoiceForPayment(db *gorm.DB, paymentID, exceptInvoiceID uint) (uint, error) {
	var rows []struct{ ID uint }
	err := db.Table("invoicing_invoices").Select("invoicing_invoices.id").
		Joins("JOIN invoicing_invoice_payments ip ON ip.invoice_id = invoicing_invoices.id").
		Where("ip.payment_id = ? AND invoicing_invoices.status = ? AND invoicing_invoices.id <> ?", paymentID, InvoiceStatusIssued, exceptInvoiceID).
		Limit(1).Scan(&rows).Error
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	return rows[0].ID, nil
}

// LatestIssueDate returns the most recent issue date among issued and
// cancelled invoices (a cancelled invoice was issued too), or nil.
func LatestIssueDate(db *gorm.DB) (*time.Time, error) {
	var inv InvoicingInvoice
	err := db.Where("issue_date IS NOT NULL").Order("issue_date desc").First(&inv).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return inv.IssueDate, nil
}

// CountInvoicesWithPrefix counts invoices that ever received a number under
// the prefix (issued or cancelled).
func CountInvoicesWithPrefix(db *gorm.DB, prefix string) (int, error) {
	var n int
	err := db.Model(&InvoicingInvoice{}).Where("prefix = ? AND number IS NOT NULL", prefix).Count(&n).Error
	return n, err
}

// MaxInvoiceNumberWithPrefix returns the highest number issued under the
// prefix, or 0.
func MaxInvoiceNumberWithPrefix(db *gorm.DB, prefix string) (int, error) {
	var rows []struct{ N *int }
	err := db.Model(&InvoicingInvoice{}).Select("MAX(number) AS n").Where("prefix = ? AND number IS NOT NULL", prefix).Scan(&rows).Error
	if err != nil || len(rows) == 0 || rows[0].N == nil {
		return 0, err
	}
	return *rows[0].N, nil
}

// InvoicingInvoicePDF is the PDF rendered when the invoice was issued. It is
// stored once and served as-is, so a later profile or client edit can never
// change an issued document. Kept apart from invoicing_invoices so listing
// invoices never loads the bytes.
type InvoicingInvoicePDF struct {
	ID        uint      `json:"-" gorm:"primary_key"`
	InvoiceID uint      `json:"invoice_id" gorm:"not null;unique_index:idx_invoicing_invoice_pdfs_invoice"`
	Content   []byte    `json:"-" gorm:"not null"`
	SHA256    string    `json:"sha256" gorm:"column:sha256;type:text;not null"`
	CreatedAt time.Time `json:"created_at"`
}

func (InvoicingInvoicePDF) TableName() string { return "invoicing_invoice_pdfs" }

// GetInvoicePDF returns the stored PDF, or nil when there is none.
func GetInvoicePDF(db *gorm.DB, invoiceID uint) (*InvoicingInvoicePDF, error) {
	var p InvoicingInvoicePDF
	err := db.Where("invoice_id = ?", invoiceID).First(&p).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}
