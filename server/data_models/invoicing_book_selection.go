package data_models

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/jinzhu/gorm"
)

// What the Book counts (system_3 #10555-A3, design §3.143 Decision 8,
// operator c65429): one category per row stays the decision, and a selection
// of categories says which rows count. The selection is stored on the server
// for its owner, so the Book, its CSV, Transactions' "Not in the Book" flag
// and her other devices all agree. She can save selections as named presets.

// BookPresetWorkOnly is the built-in preset's id. It is not a row: it is
// BookWorkOnly(), so it can never be changed or deleted, and a category she
// adds later counts under it.
const BookPresetWorkOnly = "work_only"

// BookPresetWorkOnlyName is the built-in preset's name, reserved.
const BookPresetWorkOnlyName = "Work only"

// BookCounting is a selection: the payment categories that count as money
// in, and the expense categories that count as money out. ExpenseAll means
// every expense category except Private, now and later; Expense is then empty.
type BookCounting struct {
	Income     []string `json:"income"`
	ExpenseAll bool     `json:"expense_all"`
	Expense    []string `json:"expense"`
}

// BookWorkOnly is the built-in "Work only": paid work in, every expense
// category except Private out.
func BookWorkOnly() BookCounting {
	return BookCounting{Income: []string{PaymentCategoryPaidWork}, ExpenseAll: true, Expense: []string{}}
}

// Normalised returns the counting with its lists sorted, never nil, and
// Expense emptied when ExpenseAll is set, so two equal selections compare equal.
func (b BookCounting) Normalised() BookCounting {
	in := append([]string{}, b.Income...)
	sort.Strings(in)
	out := []string{}
	if !b.ExpenseAll {
		out = append(out, b.Expense...)
		sort.Strings(out)
	}
	return BookCounting{Income: in, ExpenseAll: b.ExpenseAll, Expense: out}
}

// Equal reports whether two selections count the same categories.
func (b BookCounting) Equal(o BookCounting) bool {
	x, y := b.Normalised(), o.Normalised()
	if x.ExpenseAll != y.ExpenseAll || len(x.Income) != len(y.Income) || len(x.Expense) != len(y.Expense) {
		return false
	}
	for i := range x.Income {
		if x.Income[i] != y.Income[i] {
			return false
		}
	}
	for i := range x.Expense {
		if x.Expense[i] != y.Expense[i] {
			return false
		}
	}
	return true
}

// BookCountingColumns holds a BookCounting as stored: two JSON arrays and the
// flag. Shared by the selection and the presets.
type BookCountingColumns struct {
	IncomeCategories string `json:"-" gorm:"column:income_categories;type:text;not null;default:'[]'"`
	// default:false, never true: gorm v1 omits a zero value (false) from an
	// INSERT when the column has a default, so default:true would silently
	// store true for every expense_all: false.
	ExpenseAll        bool   `json:"-" gorm:"column:expense_all;not null;default:false"`
	ExpenseCategories string `json:"-" gorm:"column:expense_categories;type:text;not null;default:'[]'"`
}

func (c BookCountingColumns) counting() BookCounting {
	var b BookCounting
	json.Unmarshal([]byte(c.IncomeCategories), &b.Income)
	json.Unmarshal([]byte(c.ExpenseCategories), &b.Expense)
	b.ExpenseAll = c.ExpenseAll
	return b.Normalised()
}

func columnsFor(b BookCounting) BookCountingColumns {
	n := b.Normalised()
	in, _ := json.Marshal(n.Income)
	out, _ := json.Marshal(n.Expense)
	return BookCountingColumns{IncomeCategories: string(in), ExpenseAll: n.ExpenseAll, ExpenseCategories: string(out)}
}

// InvoicingBookSelection is the owner's current selection. One row per
// owner; no row means Work only.
type InvoicingBookSelection struct {
	ID          uint `json:"-" gorm:"primary_key"`
	OwnerUserID uint `json:"-" gorm:"column:owner_user_id;not null;unique_index:idx_invoicing_book_selection_owner"`
	BookCountingColumns
	// Preset is what the selection was last set from: BookPresetWorkOnly, one
	// of her presets' ids in decimal, or empty for none.
	Preset    string    `json:"-" gorm:"type:text;not null;default:''"`
	UpdatedAt time.Time `json:"-"`
}

func (InvoicingBookSelection) TableName() string { return "invoicing_book_selections" }

// Counting returns the stored selection.
func (s InvoicingBookSelection) Counting() BookCounting { return s.counting() }

// InvoicingBookPreset is one of the owner's named selections.
type InvoicingBookPreset struct {
	ID          uint   `json:"-" gorm:"primary_key"`
	OwnerUserID uint   `json:"-" gorm:"column:owner_user_id;not null;unique_index:idx_invoicing_book_preset_owner_name"`
	Name        string `json:"-" gorm:"type:text;not null;unique_index:idx_invoicing_book_preset_owner_name"`
	BookCountingColumns
	CreatedAt time.Time `json:"-"`
	UpdatedAt time.Time `json:"-"`
}

func (InvoicingBookPreset) TableName() string { return "invoicing_book_presets" }

// Counting returns the preset's selection.
func (p InvoicingBookPreset) Counting() BookCounting { return p.counting() }

// SetCounting stores b on the preset.
func (p *InvoicingBookPreset) SetCounting(b BookCounting) { p.BookCountingColumns = columnsFor(b) }

// SetCounting stores b on the selection.
func (s *InvoicingBookSelection) SetCounting(b BookCounting) { s.BookCountingColumns = columnsFor(b) }

// GetInvoicingBookSelection returns the owner's stored selection, or nil
// when she has none (the Book then counts with Work only).
func GetInvoicingBookSelection(db *gorm.DB, owner uint) (*InvoicingBookSelection, error) {
	var s InvoicingBookSelection
	err := db.Where("owner_user_id = ?", owner).First(&s).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// ListInvoicingBookPresets returns the owner's presets by name.
func ListInvoicingBookPresets(db *gorm.DB, owner uint) ([]InvoicingBookPreset, error) {
	var ps []InvoicingBookPreset
	err := db.Where("owner_user_id = ?", owner).Order("name asc, id asc").Find(&ps).Error
	return ps, err
}

// GetInvoicingBookPreset returns the owner's preset, or nil when the id is
// unknown or belongs to another book.
func GetInvoicingBookPreset(db *gorm.DB, owner, id uint) (*InvoicingBookPreset, error) {
	var p InvoicingBookPreset
	err := db.Where("id = ? AND owner_user_id = ?", id, owner).First(&p).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}
