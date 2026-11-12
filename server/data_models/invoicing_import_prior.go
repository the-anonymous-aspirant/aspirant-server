package data_models

import "github.com/jinzhu/gorm"

// ImportPrior is what the owner's book already holds for an imported row's
// (source, transaction id), as every import path must tell it apart (system_3
// #10555-B1, §3.143 Decision 6): a row she still has, a row she removed (it
// can be restored on Transactions), or a row she deleted for good (only its
// tombstone is left, so a re-import never brings it back).
type ImportPrior int

const (
	PriorNone ImportPrior = iota
	PriorStored
	PriorRemoved
	PriorDeleted
)

// Seen reports whether an import must skip the row: anything but PriorNone.
func (p ImportPrior) Seen() bool { return p != PriorNone }

// InvoicingPaymentPrior answers ImportPrior for a payment. A stored row wins
// over a tombstone, so a row that exists is never reported as deleted.
func InvoicingPaymentPrior(db *gorm.DB, owner uint, source, transactionID string) (ImportPrior, error) {
	return importPrior(db, &InvoicingPayment{}, &InvoicingPaymentTombstone{}, owner, source, transactionID)
}

// InvoicingExpensePrior is InvoicingPaymentPrior for an expense, reading the
// expense tombstone #10555-A2 added.
func InvoicingExpensePrior(db *gorm.DB, owner uint, source, transactionID string) (ImportPrior, error) {
	return importPrior(db, &InvoicingExpense{}, &InvoicingExpenseTombstone{}, owner, source, transactionID)
}

func importPrior(db *gorm.DB, rowModel, tombstoneModel interface{}, owner uint, source, transactionID string) (ImportPrior, error) {
	key := "owner_user_id = ? AND source = ? AND transaction_id = ?"
	var active, removed int
	if err := db.Model(rowModel).Where(key+" AND removed_at IS NULL", owner, source, transactionID).Count(&active).Error; err != nil {
		return PriorNone, err
	}
	if active > 0 {
		return PriorStored, nil
	}
	if err := db.Model(rowModel).Where(key+" AND removed_at IS NOT NULL", owner, source, transactionID).Count(&removed).Error; err != nil {
		return PriorNone, err
	}
	if removed > 0 {
		return PriorRemoved, nil
	}
	var deleted int
	if err := db.Model(tombstoneModel).Where(key, owner, source, transactionID).Count(&deleted).Error; err != nil {
		return PriorNone, err
	}
	if deleted > 0 {
		return PriorDeleted, nil
	}
	return PriorNone, nil
}
