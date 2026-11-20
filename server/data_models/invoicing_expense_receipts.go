package data_models

import (
	"time"

	"github.com/jinzhu/gorm"
)

// InvoicingExpenseReceipt is one receipt file attached to one work expense
// (system_3 #10537-A1, operator c65188: "attach to the expense in the
// book if its smal[l]"). One row per expense, mirroring
// InvoicingProfileLogo's one-row-per-owner shape: the uploaded bytes are
// kept as-is (unlike the logo, never normalized — a receipt may be a PDF or
// any image), with their sha256 recorded for the audit trail.
type InvoicingExpenseReceipt struct {
	ID          uint      `json:"-" gorm:"primary_key"`
	ExpenseID   uint      `json:"-" gorm:"column:expense_id;not null;default:0;unique_index:idx_invoicing_expense_receipts_expense"`
	OwnerUserID uint      `json:"-" gorm:"column:owner_user_id;not null;default:0"`
	Content     []byte    `json:"-" gorm:"not null"`
	ContentType string    `json:"content_type" gorm:"type:text;not null"`
	Filename    string    `json:"filename" gorm:"type:text;not null"`
	SHA256      string    `json:"sha256" gorm:"column:sha256;type:text;not null"`
	SizeBytes   int64     `json:"size_bytes" gorm:"not null"`
	CreatedAt   time.Time `json:"created_at"`
	CreatedBy   uint      `json:"created_by"`
}

func (InvoicingExpenseReceipt) TableName() string { return "invoicing_expense_receipts" }

// GetInvoicingExpenseReceipt returns the expense's receipt, or nil when none
// is attached. Scoped by owner so a receipt never answers for an expense
// that belongs to someone else's book.
func GetInvoicingExpenseReceipt(db *gorm.DB, owner, expenseID uint) (*InvoicingExpenseReceipt, error) {
	var r InvoicingExpenseReceipt
	err := db.Where("expense_id = ? AND owner_user_id = ?", expenseID, owner).First(&r).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// InvoicingExpenseIDsWithReceipt returns the set of the owner's expense ids
// that have a receipt attached (system_3 #11630-A1). One query for the whole
// list, and it reads only expense_id, never a receipt's bytes. Scoped by
// owner, so another book's receipt never marks an expense as having one.
func InvoicingExpenseIDsWithReceipt(db *gorm.DB, owner uint) (map[uint]bool, error) {
	var ids []uint
	if err := db.Model(&InvoicingExpenseReceipt{}).Where("owner_user_id = ?", owner).Pluck("expense_id", &ids).Error; err != nil {
		return nil, err
	}
	set := make(map[uint]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set, nil
}

// SaveInvoicingExpenseReceipt upserts the expense's receipt row (one per
// expense: a second upload replaces the first).
func SaveInvoicingExpenseReceipt(db *gorm.DB, r *InvoicingExpenseReceipt, owner, expenseID uint) error {
	existing, err := GetInvoicingExpenseReceipt(db, owner, expenseID)
	if err != nil {
		return err
	}
	r.ID = 0
	if existing != nil {
		r.ID = existing.ID
	}
	r.ExpenseID = expenseID
	r.OwnerUserID = owner
	r.CreatedAt = time.Now().UTC()
	r.CreatedBy = owner
	return db.Save(r).Error
}

// DeleteInvoicingExpenseReceipt removes the expense's receipt. It reports
// whether there was one to remove.
func DeleteInvoicingExpenseReceipt(db *gorm.DB, owner, expenseID uint) (bool, error) {
	res := db.Where("expense_id = ? AND owner_user_id = ?", expenseID, owner).Delete(&InvoicingExpenseReceipt{})
	return res.RowsAffected > 0, res.Error
}
