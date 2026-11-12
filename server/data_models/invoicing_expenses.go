package data_models

import (
	"time"

	"github.com/jinzhu/gorm"
)

// ExpenseCategoryOther is the default when none is given, and the one
// default category every new book is seeded with that also serves as a
// catch-all.
const ExpenseCategoryOther = "other"

// DefaultExpenseCategories are seeded into a book the first time its owner
// touches expense categories (system_3 #10414-A/#10424, operator-confirmed
// list, c63803 on #9656: "That list is fine"). Unlike PaymentCategories,
// this is not a closed vocabulary: the owner can add her own on top of it
// (#10424), so the table, not a Go const list, is the source of truth after
// seeding.
var DefaultExpenseCategories = []string{
	"materials and supplies", "studio rent and energy", "insurance and fees",
	"advertising", "travel", "office and postage", "phone and internet",
	"advice", "training", ExpenseCategoryOther,
}

// InvoicingExpenseCategory is one expense category in the owner's book: one
// of the ten seeded starter types, or one she added herself on top (#10424).
// CountsAs is the report's Anlage EKS line (design_agent, report 886 §5, c63823
// on #10424): a starter category counts as itself; a user-added one must name
// one of the ten starter types, so every category the report sums resolves to
// exactly one line.
type InvoicingExpenseCategory struct {
	ID          uint      `json:"id" gorm:"primary_key"`
	OwnerUserID uint      `json:"-" gorm:"column:owner_user_id;not null;unique_index:idx_invoicing_expense_categories_owner_name"`
	Name        string    `json:"name" gorm:"type:text;not null;unique_index:idx_invoicing_expense_categories_owner_name"`
	CountsAs    string    `json:"counts_as" gorm:"type:text;not null"`
	CreatedAt   time.Time `json:"created_at"`
}

func (InvoicingExpenseCategory) TableName() string { return "invoicing_expense_categories" }

// IsStarterExpenseCategory reports whether name is one of the ten fixed
// starter types (c63823: "the starter types are fixed... because they carry
// the mapping").
func IsStarterExpenseCategory(name string) bool {
	for _, c := range DefaultExpenseCategories {
		if c == name {
			return true
		}
	}
	return false
}

// EnsureDefaultExpenseCategories seeds the owner's book with
// DefaultExpenseCategories, each counting as itself, if she has no
// categories yet. Idempotent: a book that already has any category rows
// (seeded or her own) is left alone.
func EnsureDefaultExpenseCategories(db *gorm.DB, owner uint) error {
	var n int
	if err := db.Model(&InvoicingExpenseCategory{}).Where("owner_user_id = ?", owner).Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	for _, name := range DefaultExpenseCategories {
		if err := db.Create(&InvoicingExpenseCategory{OwnerUserID: owner, Name: name, CountsAs: name}).Error; err != nil {
			return err
		}
	}
	return nil
}

// ListInvoicingExpenseCategories returns the owner's categories, seeding the
// defaults first if she has none, oldest (so seeded order) first.
func ListInvoicingExpenseCategories(db *gorm.DB, owner uint) ([]InvoicingExpenseCategory, error) {
	if err := EnsureDefaultExpenseCategories(db, owner); err != nil {
		return nil, err
	}
	var cats []InvoicingExpenseCategory
	err := db.Where("owner_user_id = ?", owner).Order("id asc").Find(&cats).Error
	return cats, err
}

// IsExpenseCategoryForOwner reports whether name is one of the owner's
// categories, seeding the defaults first if she has none.
func IsExpenseCategoryForOwner(db *gorm.DB, owner uint, name string) (bool, error) {
	if err := EnsureDefaultExpenseCategories(db, owner); err != nil {
		return false, err
	}
	var n int
	err := db.Model(&InvoicingExpenseCategory{}).Where("owner_user_id = ? AND name = ?", owner, name).Count(&n).Error
	return n > 0, err
}

// How an expense was recorded: entered by hand, or (system_3 #10414-B, not
// yet built) captured from an N26 statement's outgoing rows.
const (
	ExpenseEntryManual = "manual"
	ExpenseEntryImport = "import"

	ExpenseSourceManual = "manual"
)

// InvoicingExpense is one work expense, entered by hand or (once #10414-B
// lands) imported. Unlike InvoicingPayment, an expense is never invoiced and
// carries no fee/net split — it is one amount going out.
type InvoicingExpense struct {
	ID            uint      `json:"id" gorm:"primary_key"`
	OwnerUserID   uint      `json:"-" gorm:"column:owner_user_id;not null;default:0;unique_index:idx_invoicing_expenses_owner_source_txn"`
	Source        string    `json:"source" gorm:"type:text;not null;unique_index:idx_invoicing_expenses_owner_source_txn"`
	TransactionID string    `json:"transaction_id" gorm:"type:text;not null;unique_index:idx_invoicing_expenses_owner_source_txn"`
	PaidOn        time.Time `json:"paid_on" gorm:"type:date;not null"`
	Currency      string    `json:"currency" gorm:"type:text;not null"`
	AmountCents   int64     `json:"amount_cents" gorm:"not null"`
	Note          string    `json:"note" gorm:"type:text;not null;default:''"`
	Category      string    `json:"category" gorm:"type:text;not null;default:'other'"`
	ClientID      *uint     `json:"client_id"`
	Entry         string    `json:"entry" gorm:"type:text;not null;default:'manual'"`
	Reference     string    `json:"reference" gorm:"type:text;not null;default:''"`
	// Payee is who a hand-entered expense was paid to, if she says
	// (system_3 #10555-A1, design §3.143 Decision 3). An import keeps its
	// payee in Reference, as it always has.
	Payee     string    `json:"payee" gorm:"type:text;not null;default:''"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (InvoicingExpense) TableName() string { return "invoicing_expenses" }

// IsManual reports whether the owner entered this expense by hand.
func (e InvoicingExpense) IsManual() bool { return e.Entry == ExpenseEntryManual }

// newManualExpenseTransactionID is the database key of a hand-entered
// expense, mirroring newManualTransactionID for payments. Generated in the
// handler (crypto/rand lives there, beside the payment one it mirrors).

// ListInvoicingExpenses returns the owner's expenses oldest first, optionally
// filtered by category (empty means any).
func ListInvoicingExpenses(db *gorm.DB, owner uint, category string) ([]InvoicingExpense, error) {
	q := db.Where("owner_user_id = ?", owner).Order("paid_on asc, id asc")
	if category != "" {
		q = q.Where("category = ?", category)
	}
	var expenses []InvoicingExpense
	err := q.Find(&expenses).Error
	return expenses, err
}

// GetInvoicingExpense returns the owner's expense, or nil when the id is
// unknown or belongs to another book.
func GetInvoicingExpense(db *gorm.DB, owner, id uint) (*InvoicingExpense, error) {
	var e InvoicingExpense
	err := db.Where("id = ? AND owner_user_id = ?", id, owner).First(&e).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}
