package data_models

import "github.com/jinzhu/gorm"

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
