package data_models

import (
	"fmt"

	"github.com/jinzhu/gorm"
)

// EnsureInvoicingOwnerLockTable creates invoicing_owner_locks, one row per
// book, which LockInvoicingBook locks. Plain DDL rather than a model, so gorm
// never makes the user id an auto-increment key. Idempotent.
func EnsureInvoicingOwnerLockTable(db *gorm.DB) error {
	return db.Exec("CREATE TABLE IF NOT EXISTS invoicing_owner_locks (owner_user_id bigint PRIMARY KEY)").Error
}

// LockInvoicingBook serialises every write that must not interleave within one
// book: issuing and its number counter, a draft linking payments, a payment
// delete, an import's or hand entry's duplicate check (#8409 security F1, F2).
// It inserts the book's lock row when missing and then takes that row's lock,
// so the lock holds whether or not the user has saved a profile (system_3
// #10059, security review of PR 172 F1). Before the per-user book this was
// the shared profile row's lock; a user with no profile row would have locked
// nothing. Concurrent first calls for one book queue on the primary key.
func LockInvoicingBook(tx *gorm.DB, owner uint) error {
	if err := tx.Exec("INSERT INTO invoicing_owner_locks (owner_user_id) VALUES (?) ON CONFLICT (owner_user_id) DO NOTHING", owner).Error; err != nil {
		return err
	}
	res := tx.Exec("UPDATE invoicing_owner_locks SET owner_user_id = owner_user_id WHERE owner_user_id = ?", owner)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("invoicing lock row for user %d is missing", owner)
	}
	return nil
}

// invoicingSharedBookIndexes are the unique indexes from the one shared book,
// keyed without an owner (system_3 #10054). The per-owner indexes that replace
// them come from the struct tags under new names. AutoMigrate creates an index
// only when its name is missing and never drops one, so without this the old
// keys would stay and refuse a second book's invoice number or import.
var invoicingSharedBookIndexes = []string{
	"idx_invoicing_invoices_number",
	"idx_invoicing_payments_source_txn",
	"idx_invoicing_payment_tombstone",
}

// EnsureInvoicingOwnerIndexes drops the shared-book unique indexes. It is safe
// to run on every boot: once they are gone it matches nothing.
func EnsureInvoicingOwnerIndexes(db *gorm.DB) error {
	for _, name := range invoicingSharedBookIndexes {
		if err := db.Exec("DROP INDEX IF EXISTS " + name).Error; err != nil {
			return err
		}
	}
	return nil
}
