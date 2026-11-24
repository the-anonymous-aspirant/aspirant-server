package data_models

import (
	"encoding/json"
	"strings"
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
// this is not a closed vocabulary: the owner can add zir own on top of it
// (#10424), so the table, not a Go const list, is the source of truth after
// seeding.
var DefaultExpenseCategories = []string{
	"materials and supplies", "studio rent and energy", "insurance and fees",
	"advertising", "travel", "office and postage", "phone and internet",
	"advice", "training", ExpenseCategoryOther,
}

// InvoicingExpenseCategory is one expense category in the owner's book: one
// of the ten seeded starter types, or one ze added zirself on top (#10424).
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
	// RemovedAt is when the owner removed one of zir own categories (system_3
	// #11579, design §3.161 Decision 5; until it merges, #11566). A removed
	// category is kept: every expense that has it keeps it, and the Book, its
	// selection, presets and CSV count it as before. It is only no longer
	// offered for an expense that does not already have it. Never named
	// deleted_at: gorm v1 reads that as a soft-delete marker and would hide
	// the row from the report that still counts it.
	RemovedAt *time.Time `json:"removed_at" gorm:"column:removed_at"`
	// Builtin is true for the ten starters and Private, which nobody can
	// remove. Computed when listed, not stored.
	Builtin bool `json:"builtin" gorm:"-"`
}

// Removed reports whether the owner has removed this category.
func (c InvoicingExpenseCategory) Removed() bool { return c.RemovedAt != nil }

func (InvoicingExpenseCategory) TableName() string { return "invoicing_expense_categories" }

// ExpenseCategoryPrivate is the built-in category for an expense ze records
// but never counts (system_3 #10555-A3, design §3.143 Decision 8). Every book
// has it; it is listed last, cannot be created, renamed or deleted, and no
// Book selection may include it. Its counts_as is itself, a value outside the
// ten Anlage EKS lines, so nothing maps it into the report.
const ExpenseCategoryPrivate = "private"

// IsBuiltinExpenseCategory reports whether name is a category every book has
// and nobody can create, rename or delete: the ten starters and Private.
func IsBuiltinExpenseCategory(name string) bool {
	return name == ExpenseCategoryPrivate || IsStarterExpenseCategory(name)
}

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
// DefaultExpenseCategories, each counting as itself, if ze has no
// categories yet. Idempotent: a book that already has any category rows
// (seeded or zir own) is left alone.
func EnsureDefaultExpenseCategories(db *gorm.DB, owner uint) error {
	var n int
	if err := db.Model(&InvoicingExpenseCategory{}).Where("owner_user_id = ?", owner).Count(&n).Error; err != nil {
		return err
	}
	if n == 0 {
		for _, name := range DefaultExpenseCategories {
			if err := db.Create(&InvoicingExpenseCategory{OwnerUserID: owner, Name: name, CountsAs: name}).Error; err != nil {
				return err
			}
		}
	}
	return ensurePrivateExpenseCategory(db, owner)
}

// ensurePrivateExpenseCategory gives the book its Private category if it does
// not have one yet, so books seeded before #10555-A3 gain it on first touch.
func ensurePrivateExpenseCategory(db *gorm.DB, owner uint) error {
	var n int
	if err := db.Model(&InvoicingExpenseCategory{}).Where("owner_user_id = ? AND name = ?", owner, ExpenseCategoryPrivate).Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	return db.Create(&InvoicingExpenseCategory{OwnerUserID: owner, Name: ExpenseCategoryPrivate, CountsAs: ExpenseCategoryPrivate}).Error
}

// ListInvoicingExpenseCategories returns the owner's categories, seeding the
// defaults first if ze has none, oldest (so seeded order) first, with
// Private always last whatever its id (§3.143 Decision 8).
func ListInvoicingExpenseCategories(db *gorm.DB, owner uint) ([]InvoicingExpenseCategory, error) {
	if err := EnsureDefaultExpenseCategories(db, owner); err != nil {
		return nil, err
	}
	var cats []InvoicingExpenseCategory
	if err := db.Where("owner_user_id = ?", owner).Order("id asc").Find(&cats).Error; err != nil {
		return nil, err
	}
	out := make([]InvoicingExpenseCategory, 0, len(cats))
	var private *InvoicingExpenseCategory
	for i := range cats {
		cats[i].Builtin = IsBuiltinExpenseCategory(cats[i].Name)
		if cats[i].Name == ExpenseCategoryPrivate {
			private = &cats[i]
			continue
		}
		out = append(out, cats[i])
	}
	if private != nil {
		out = append(out, *private)
	}
	return out, nil
}

// GetExpenseCategoryByName returns the owner's category with exactly this
// name, removed or not, seeding the defaults first; nil when ze has none.
func GetExpenseCategoryByName(db *gorm.DB, owner uint, name string) (*InvoicingExpenseCategory, error) {
	if err := EnsureDefaultExpenseCategories(db, owner); err != nil {
		return nil, err
	}
	return firstExpenseCategory(db.Where("owner_user_id = ? AND name = ?", owner, name))
}

// FindExpenseCategoryFold returns the owner's category whose name equals
// name ignoring case, removed or not, seeding the defaults first; nil when
// there is none. Names are unique per owner ignoring case: every create path
// refuses a case-twin.
func FindExpenseCategoryFold(db *gorm.DB, owner uint, name string) (*InvoicingExpenseCategory, error) {
	if err := EnsureDefaultExpenseCategories(db, owner); err != nil {
		return nil, err
	}
	var cats []InvoicingExpenseCategory
	if err := db.Where("owner_user_id = ?", owner).Order("id asc").Find(&cats).Error; err != nil {
		return nil, err
	}
	// Compared in Go, not with SQL LOWER(): SQLite's lower() folds ASCII
	// only, Postgres' folds by locale, and the create path's own duplicate
	// check has always been strings.EqualFold.
	for i := range cats {
		if strings.EqualFold(cats[i].Name, name) {
			cats[i].Builtin = IsBuiltinExpenseCategory(cats[i].Name)
			return &cats[i], nil
		}
	}
	return nil, nil
}

// GetExpenseCategory returns the owner's category by id, or nil when the id
// is unknown or belongs to another book.
func GetExpenseCategory(db *gorm.DB, owner, id uint) (*InvoicingExpenseCategory, error) {
	return firstExpenseCategory(db.Where("owner_user_id = ? AND id = ?", owner, id))
}

func firstExpenseCategory(q *gorm.DB) (*InvoicingExpenseCategory, error) {
	var c InvoicingExpenseCategory
	err := q.First(&c).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.Builtin = IsBuiltinExpenseCategory(c.Name)
	return &c, nil
}

// IsExpenseCategoryForOwner reports whether name is one of the owner's
// categories, removed ones included, seeding the defaults first if ze has
// none. It answers "does this book know the name", which is what the Book's
// selection, its presets and the list filter ask: a removed category still
// counts while any expense has it (§3.161). Paths that put a category on an
// expense ask the stricter question instead (handlers'
// checkExpenseCategoryChoice).
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
	// Payee is who a hand-entered expense was paid to, if ze says
	// (system_3 #10555-A1, design §3.143 Decision 3). An import keeps its
	// payee in Reference, as it always has.
	Payee string `json:"payee" gorm:"type:text;not null;default:''"`
	// RemovedAt is when the owner removed the expense (system_3 #10555-A2,
	// design §3.143 Decision 6): it is kept, but out of the list, the Book and
	// its CSV until ze restores it. Never named deleted_at: gorm v1 reads
	// that column as a soft-delete marker and hides the row from every query,
	// the removed view included.
	RemovedAt *time.Time `json:"removed_at" gorm:"column:removed_at"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	// HasReceipt says whether a receipt is attached (system_3 #11630-A1). It
	// is not a column: only the list handler reads it, from
	// invoicing_expense_receipts, and sets it. Every other response that
	// carries an expense leaves it nil, so it is left out of the JSON rather
	// than sent as false: a false there would claim "no receipt" without
	// having looked.
	HasReceipt *bool `json:"has_receipt,omitempty" gorm:"-"`
}

func (InvoicingExpense) TableName() string { return "invoicing_expenses" }

// InvoicingExpenseTombstone records that the owner deleted an expense for
// good (system_3 #10555-A2), the same shape as InvoicingPaymentTombstone, so
// an import can tell a re-import of it from a new row (#10555-B1 reads it).
// The time column is deleted_on, never deleted_at, for the reason given on
// InvoicingPaymentTombstone.
type InvoicingExpenseTombstone struct {
	ID            uint      `json:"id" gorm:"primary_key"`
	OwnerUserID   uint      `json:"-" gorm:"column:owner_user_id;not null;default:0;unique_index:idx_invoicing_expense_tombstone_owner"`
	Source        string    `json:"source" gorm:"type:text;not null;unique_index:idx_invoicing_expense_tombstone_owner"`
	TransactionID string    `json:"transaction_id" gorm:"type:text;not null;unique_index:idx_invoicing_expense_tombstone_owner"`
	DeletedOn     time.Time `json:"deleted_on" gorm:"not null"`
	DeletedBy     uint      `json:"deleted_by" gorm:"not null"`
}

func (InvoicingExpenseTombstone) TableName() string { return "invoicing_expense_tombstones" }

// TombstoneInvoicingExpense records the deletion of e in e's book. A second
// tombstone for the same (owner, source, transaction id) is a no-op.
func TombstoneInvoicingExpense(db *gorm.DB, e InvoicingExpense, deletedBy uint, at time.Time) error {
	t := InvoicingExpenseTombstone{OwnerUserID: e.OwnerUserID, Source: e.Source, TransactionID: e.TransactionID, DeletedOn: at, DeletedBy: deletedBy}
	return db.Where("owner_user_id = ? AND source = ? AND transaction_id = ?", e.OwnerUserID, e.Source, e.TransactionID).FirstOrCreate(&t).Error
}

// IsManual reports whether the owner entered this expense by hand.
func (e InvoicingExpense) IsManual() bool { return e.Entry == ExpenseEntryManual }

// ShownReference is the reference a view may show for the expense: the
// owner's own for a hand-entered expense (possibly empty), the provider's
// transaction id for an imported one, and nothing when the server made the
// id up (see IsDerivedTransactionID). An import's Reference is its payee, so
// it is not used here.
func (e InvoicingExpense) ShownReference() string {
	if e.IsManual() {
		return e.Reference
	}
	if IsDerivedTransactionID(e.Source, e.TransactionID) {
		return ""
	}
	return e.TransactionID
}

// MarshalJSON serves the stored fields plus `shown_reference` (system_3
// #12257).
func (e InvoicingExpense) MarshalJSON() ([]byte, error) {
	type stored InvoicingExpense
	return json.Marshal(struct {
		stored
		ShownReference string `json:"shown_reference"`
	}{stored(e), e.ShownReference()})
}

// newManualExpenseTransactionID is the database key of a hand-entered
// expense, mirroring newManualTransactionID for payments. Generated in the
// handler (crypto/rand lives there, beside the payment one it mirrors).

// ListInvoicingExpenses returns the owner's expenses oldest first, optionally
// filtered by category (empty means any). Removed expenses are left out: they
// count nowhere until restored (§3.143 Decision 6). The Book report reads
// this list too, so they leave the Book and its CSV with it.
func ListInvoicingExpenses(db *gorm.DB, owner uint, category string) ([]InvoicingExpense, error) {
	q := db.Where("owner_user_id = ? AND removed_at IS NULL", owner).Order("paid_on asc, id asc")
	if category != "" {
		q = q.Where("category = ?", category)
	}
	var expenses []InvoicingExpense
	err := q.Find(&expenses).Error
	return expenses, err
}

// ListRemovedInvoicingExpenses returns the owner's removed expenses, most
// recently removed first: the removed view.
func ListRemovedInvoicingExpenses(db *gorm.DB, owner uint) ([]InvoicingExpense, error) {
	var expenses []InvoicingExpense
	err := db.Where("owner_user_id = ? AND removed_at IS NOT NULL", owner).Order("removed_at desc, id desc").Find(&expenses).Error
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
