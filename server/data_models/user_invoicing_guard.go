package data_models

import (
	"errors"

	"github.com/jinzhu/gorm"
)

// ErrUserHasInvoicingData is what User.DeleteUser returns, having deleted
// nothing, when the account still owns invoicing records (system_3 #11590,
// Part A of #11471; the operator's answer c70521 on #11380). Issued invoices
// have a legal retention period, so a book is never removed as a side effect
// of removing its owner: it has to be exported and removed first, and that is
// a decision of its own. There is deliberately no path here that removes one.
var ErrUserHasInvoicingData = errors.New("the user still has invoicing records")

// invoicingOwnerTable is one invoicing table whose rows belong to a user.
type invoicingOwnerTable struct {
	Name  string
	Model interface{}
}

// userInvoicingTables are the tables that make up a book: a row in any of them
// refuses the delete. Line items, invoice payments and invoice PDFs hang off
// an invoice, so InvoicingInvoice covers them. All ten name their user in
// owner_user_id. A new per-owner invoicing table belongs in this list or the
// one below; the delete test names every table, so one left out fails there.
var userInvoicingTables = []invoicingOwnerTable{
	{"InvoicingProfile", &InvoicingProfile{}},
	{"InvoicingProfileLogo", &InvoicingProfileLogo{}},
	{"InvoicingClient", &InvoicingClient{}},
	{"InvoicingInvoice", &InvoicingInvoice{}},
	{"InvoicingPayment", &InvoicingPayment{}},
	{"InvoicingExpense", &InvoicingExpense{}},
	{"InvoicingExpenseReceipt", &InvoicingExpenseReceipt{}},
	{"InvoicingImportFailure", &InvoicingImportFailure{}},
	{"InvoicingBookSelection", &InvoicingBookSelection{}},
	{"InvoicingBookPreset", &InvoicingBookPreset{}},
}

// userInvoicingRemnantTables never refuse a delete, and go with the account
// (security's ruling c71589 on system_3 #11471). None of them is a book:
//   - expense categories are seeded by reads, so a member who only opened
//     "Change what counts" has them, and a name with no money behind it is
//     not a record;
//   - a tombstone is written by every hard delete of a payment or expense and
//     nothing removes it, so counting them would make anyone who ever deleted
//     one transaction undeletable. It only keeps a removed transaction out of
//     a later import into this user's book, and by now there is no book.
//
// invoicing_audit_events is in neither list, on purpose. Nothing deletes an
// audit row, so counting them would refuse everyone who ever wrote to a book,
// and an admin who downloaded someone's failed import has one under their own
// id. They are left exactly as they are: whether they go with the account is
// a retention decision that is with the operator (system_3 #11380).
var userInvoicingRemnantTables = []invoicingOwnerTable{
	{"InvoicingExpenseCategory", &InvoicingExpenseCategory{}},
	{"InvoicingPaymentTombstone", &InvoicingPaymentTombstone{}},
	{"InvoicingExpenseTombstone", &InvoicingExpenseTombstone{}},
}

// UserInvoicingTableNames lists the tables whose rows refuse a delete, and
// UserInvoicingRemnantTableNames those removed with the account, for the test
// that seeds one row in each.
func UserInvoicingTableNames() []string { return tableNames(userInvoicingTables) }

// UserInvoicingRemnantTableNames: see UserInvoicingTableNames.
func UserInvoicingRemnantTableNames() []string { return tableNames(userInvoicingRemnantTables) }

func tableNames(tables []invoicingOwnerTable) []string {
	names := make([]string, 0, len(tables))
	for _, t := range tables {
		names = append(names, t.Name)
	}
	return names
}

// userHasInvoicingData reports whether userID owns a row in any of
// userInvoicingTables. Counted Unscoped, so a row that only marks itself
// deleted still counts: the record is still there.
func userHasInvoicingData(tx *gorm.DB, userID uint) (bool, error) {
	for _, t := range userInvoicingTables {
		var n int
		if err := tx.Unscoped().Model(t.Model).Where("owner_user_id = ?", userID).Count(&n).Error; err != nil {
			return false, err
		}
		if n > 0 {
			return true, nil
		}
	}
	return false, nil
}

// deleteUserInvoicingRemnants removes the user's rows in
// userInvoicingRemnantTables. Called only after userHasInvoicingData said no,
// inside the same transaction as the rest of the delete.
func deleteUserInvoicingRemnants(tx *gorm.DB, userID uint) error {
	for _, t := range userInvoicingRemnantTables {
		if err := tx.Unscoped().Where("owner_user_id = ?", userID).Delete(t.Model).Error; err != nil {
			return err
		}
	}
	return nil
}
