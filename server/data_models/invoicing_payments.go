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

// How a payment came in (system_3 #8409): imported from a PayPal file, or
// entered by hand by the owner. A hand-entered payment has source "manual"
// and a server-generated TransactionID ("M-…") that is only a database key;
// the printed reference is the owner's own (Reference), or nothing.
const (
	PaymentEntryImport = "import"
	PaymentEntryManual = "manual"

	PaymentSourceManual = "manual"
)

// Payment methods for a hand-entered payment, with the German wording the
// invoice prints ("Zahlung erhalten am … per <label>").
var PaymentMethodLabels = map[string]string{
	"bank_transfer": "Banküberweisung",
	"paypal":        "PayPal",
	"stripe":        "Stripe",
	"cash":          "Bar",
	"other":         "Sonstige",
}

// PaymentMethods is the closed method vocabulary, in display order.
var PaymentMethods = []string{"bank_transfer", "paypal", "stripe", "cash", "other"}

// InvoicingPayment is one received payment or bank withdrawal, imported or
// entered by hand. (Source, TransactionID) is unique, which is what makes
// re-uploading a file a no-op.
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
	Entry          string    `json:"entry" gorm:"type:text;not null;default:'import'"`
	Method         string    `json:"method" gorm:"type:text;not null;default:''"`
	Reference      string    `json:"reference" gorm:"type:text;not null;default:''"`
	// OnInvoice is computed for the list (#8409): whether any invoice, draft
	// or issued, covers this payment. Not stored.
	OnInvoice bool      `json:"on_invoice" gorm:"-"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (InvoicingPayment) TableName() string { return "invoicing_payments" }

// IsManual reports whether the owner entered this payment by hand.
func (p InvoicingPayment) IsManual() bool { return p.Entry == PaymentEntryManual }

// PrintedReference is the transaction reference shown on invoices and in
// exports: the owner's reference for a hand-entered payment (possibly empty),
// the provider's transaction id for an imported one. A manual payment's
// generated TransactionID is never printed.
func (p InvoicingPayment) PrintedReference() string {
	if p.IsManual() {
		return p.Reference
	}
	return p.TransactionID
}

// MethodLabel is how the payment was received, in the invoice's German.
func (p InvoicingPayment) MethodLabel() string {
	if p.IsManual() {
		if label, ok := PaymentMethodLabels[p.Method]; ok {
			return label
		}
		return p.Method
	}
	switch p.Source {
	case "paypal":
		return "PayPal"
	case "stripe":
		return "Stripe"
	default:
		return p.Source
	}
}

// PaymentOnAnyInvoice returns the id of an invoice (draft, issued or
// cancelled) the payment is linked to, or 0.
func PaymentOnAnyInvoice(db *gorm.DB, paymentID uint) (uint, error) {
	var link InvoicingInvoicePayment
	err := db.Where("payment_id = ?", paymentID).Order("invoice_id asc").First(&link).Error
	if err == gorm.ErrRecordNotFound {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return link.InvoiceID, nil
}

// PaymentIDsOnInvoices returns the ids of every payment some invoice (draft,
// issued or cancelled) covers.
func PaymentIDsOnInvoices(db *gorm.DB) (map[uint]bool, error) {
	var links []InvoicingInvoicePayment
	if err := db.Select("payment_id").Find(&links).Error; err != nil {
		return nil, err
	}
	out := make(map[uint]bool, len(links))
	for _, l := range links {
		out[l.PaymentID] = true
	}
	return out, nil
}

// ManualPayPalPaymentWithReference reports whether a hand-entered PayPal
// payment already carries this PayPal transaction id as its reference.
func ManualPayPalPaymentWithReference(db *gorm.DB, reference string) (bool, error) {
	if reference == "" {
		return false, nil
	}
	var n int
	err := db.Model(&InvoicingPayment{}).
		Where("entry = ? AND method = ? AND reference = ?", PaymentEntryManual, "paypal", reference).Count(&n).Error
	return n > 0, err
}

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
