package data_models

import (
	"time"

	"github.com/jinzhu/gorm"
)

// Payment categories (spec §4.4). Every imported payment starts uncategorised;
// only paid_work can ever be invoiced, and that rule lives in the backend.
const (
	PaymentCategoryUncategorised = "uncategorised"
	PaymentCategoryPaidWork      = "paid_work"
	PaymentCategoryGift          = "gift"
	PaymentCategoryLoan          = "loan"
	PaymentCategoryRefund        = "refund"
	PaymentCategoryOther         = "other"
)

// PaymentCategories is the closed category vocabulary.
var PaymentCategories = []string{
	PaymentCategoryUncategorised, PaymentCategoryPaidWork, PaymentCategoryGift,
	PaymentCategoryLoan, PaymentCategoryRefund, PaymentCategoryOther,
}

func IsPaymentCategory(s string) bool {
	for _, c := range PaymentCategories {
		if s == c {
			return true
		}
	}
	return false
}

// Payment kinds: money in, or a withdrawal from PayPal to the bank kept as a
// bank-deposit reference so incoming payments can be grouped under the
// deposits the Jobcenter listed (spec §4.3).
const (
	PaymentKindIncoming   = "incoming"
	PaymentKindWithdrawal = "withdrawal"
)

// InvoicingPayment is one imported transaction. (Source, TransactionID) is
// unique, which is what makes re-uploading a file a no-op.
type InvoicingPayment struct {
	ID             uint      `json:"id" gorm:"primary_key"`
	Source         string    `json:"source" gorm:"type:text;not null;unique_index:idx_invoicing_payments_source_txn"`
	TransactionID  string    `json:"transaction_id" gorm:"type:text;not null;unique_index:idx_invoicing_payments_source_txn"`
	ReferenceTxnID string    `json:"reference_txn_id" gorm:"type:text;not null;default:''"`
	Kind           string    `json:"kind" gorm:"type:text;not null"`
	PaidOn         time.Time `json:"paid_on" gorm:"type:date;not null"`
	Currency       string    `json:"currency" gorm:"type:text;not null"`
	GrossCents     int64     `json:"gross_cents" gorm:"not null"`
	FeeCents       int64     `json:"fee_cents" gorm:"not null;default:0"`
	NetCents       int64     `json:"net_cents" gorm:"not null;default:0"`
	SenderName     string    `json:"sender_name" gorm:"type:text;not null;default:''"`
	SenderEmail    string    `json:"sender_email" gorm:"type:text;not null;default:''"`
	Note           string    `json:"note" gorm:"type:text;not null;default:''"`
	Category       string    `json:"category" gorm:"type:text;not null;default:'uncategorised'"`
	ClientID       *uint     `json:"client_id"`
	BankDepositID  *uint     `json:"bank_deposit_id"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func (InvoicingPayment) TableName() string { return "invoicing_payments" }

// ListInvoicingPayments returns payments oldest first, optionally filtered by
// kind and category (empty means any).
func ListInvoicingPayments(db *gorm.DB, kind, category string) ([]InvoicingPayment, error) {
	q := db.Order("paid_on asc, id asc")
	if kind != "" {
		q = q.Where("kind = ?", kind)
	}
	if category != "" {
		q = q.Where("category = ?", category)
	}
	var payments []InvoicingPayment
	err := q.Find(&payments).Error
	return payments, err
}

// GetInvoicingPayment returns the payment, or nil when the id is unknown.
func GetInvoicingPayment(db *gorm.DB, id uint) (*InvoicingPayment, error) {
	var p InvoicingPayment
	err := db.Where("id = ?", id).First(&p).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// InvoicingPaymentExists reports whether (source, transaction id) is stored.
func InvoicingPaymentExists(db *gorm.DB, source, transactionID string) (bool, error) {
	var n int
	err := db.Model(&InvoicingPayment{}).Where("source = ? AND transaction_id = ?", source, transactionID).Count(&n).Error
	return n > 0, err
}

// FindInvoicingClientByPayPalEmail returns the client whose PayPal email
// matches (already lower-cased on both sides), or nil.
func FindInvoicingClientByPayPalEmail(db *gorm.DB, email string) (*InvoicingClient, error) {
	if email == "" {
		return nil, nil
	}
	var cl InvoicingClient
	err := db.Where("paypal_email = ?", email).Order("id asc").First(&cl).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &cl, nil
}
