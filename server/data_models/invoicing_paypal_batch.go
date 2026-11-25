package data_models

import (
	"strings"
	"time"

	"github.com/jinzhu/gorm"
)

// The PayPal import reads what the book holds for a whole file at once and
// stores its new payments in a few statements (system_3 #12273 item 6). It
// read three things per row before, so a 3,000-row file held the issuing
// lock for 17 s. The file is still one transaction: a failure stores nothing.

// batchChunk bounds the ids in one IN list and the rows in one INSERT. An
// INSERT carries paymentInsertColumns values a row, and SQLite (32,766) and
// Postgres (65,535) bound a statement's parameters.
const batchChunk = 1000

func chunks(ids []string) [][]string {
	var out [][]string
	for len(ids) > batchChunk {
		out = append(out, ids[:batchChunk])
		ids = ids[batchChunk:]
	}
	if len(ids) > 0 {
		out = append(out, ids)
	}
	return out
}

// InvoicingPaymentPriors answers InvoicingPaymentPrior for every id at once.
// An id the book holds nothing for is absent from the map (PriorNone). A
// stored row wins over a removed one, and either over a tombstone, as
// importPrior has it.
func InvoicingPaymentPriors(db *gorm.DB, owner uint, source string, ids []string) (map[string]ImportPrior, error) {
	out := map[string]ImportPrior{}
	for _, part := range chunks(ids) {
		var rows []struct {
			TransactionID string
			RemovedAt     *time.Time
		}
		if err := db.Model(&InvoicingPayment{}).Select("transaction_id, removed_at").
			Where("owner_user_id = ? AND source = ? AND transaction_id IN (?)", owner, source, part).
			Scan(&rows).Error; err != nil {
			return nil, err
		}
		for _, r := range rows {
			if r.RemovedAt == nil {
				out[r.TransactionID] = PriorStored
			} else if out[r.TransactionID] != PriorStored {
				out[r.TransactionID] = PriorRemoved
			}
		}
		var deleted []string
		if err := db.Model(&InvoicingPaymentTombstone{}).
			Where("owner_user_id = ? AND source = ? AND transaction_id IN (?)", owner, source, part).
			Pluck("transaction_id", &deleted).Error; err != nil {
			return nil, err
		}
		for _, id := range deleted {
			if _, ok := out[id]; !ok {
				out[id] = PriorDeleted
			}
		}
	}
	return out, nil
}

// ManualPayPalReferences is ManualPayPalPaymentWithReference for every
// reference at once: the set of those a hand-entered PayPal payment carries.
func ManualPayPalReferences(db *gorm.DB, owner uint, refs []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, part := range chunks(nonEmpty(refs)) {
		var found []string
		if err := db.Model(&InvoicingPayment{}).
			Where("owner_user_id = ? AND entry = ? AND method = ? AND reference IN (?)", owner, PaymentEntryManual, "paypal", part).
			Pluck("reference", &found).Error; err != nil {
			return nil, err
		}
		for _, r := range found {
			out[r] = true
		}
	}
	return out, nil
}

// InvoicingClientIDsByPayPalEmail is FindInvoicingClientByPayPalEmail for
// every email at once: each email's client id, the lowest when two clients
// share one, as First by id has it. Another book's client never matches.
func InvoicingClientIDsByPayPalEmail(db *gorm.DB, owner uint, emails []string) (map[string]uint, error) {
	out := map[string]uint{}
	for _, part := range chunks(nonEmpty(emails)) {
		var rows []struct {
			ID          uint
			PayPalEmail string `gorm:"column:paypal_email"`
		}
		if err := db.Model(&InvoicingClient{}).Select("id, paypal_email").
			Where("owner_user_id = ? AND paypal_email IN (?)", owner, part).
			Order("id asc").Scan(&rows).Error; err != nil {
			return nil, err
		}
		for _, r := range rows {
			if _, ok := out[r.PayPalEmail]; !ok {
				out[r.PayPalEmail] = r.ID
			}
		}
	}
	return out, nil
}

func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// paymentInsertColumns are the stored columns of an imported payment, in the
// order InsertImportedPayments writes them.
var paymentInsertColumns = []string{
	"owner_user_id", "source", "transaction_id", "reference_txn_id", "kind", "paid_on",
	"currency", "gross_cents", "fee_cents", "net_cents", "sender_name", "sender_email",
	"note", "category", "client_id", "entry", "method", "reference", "sender_iban",
	"created_at", "updated_at",
}

// InsertImportedPayments stores imported payments in multi-row INSERTs of
// at most batchChunk rows, in the caller's transaction. It writes what
// tx.Create would: an empty Entry is stored as an import, and the times are
// gorm's now. The rows' ids are not read back; the import has no use for them.
func InsertImportedPayments(tx *gorm.DB, payments []InvoicingPayment) error {
	now := gorm.NowFunc()
	row := "(" + strings.TrimSuffix(strings.Repeat("?, ", len(paymentInsertColumns)), ", ") + ")"
	for start := 0; start < len(payments); start += batchChunk {
		end := start + batchChunk
		if end > len(payments) {
			end = len(payments)
		}
		part := payments[start:end]
		values := make([]string, 0, len(part))
		args := make([]interface{}, 0, len(part)*len(paymentInsertColumns))
		for _, p := range part {
			entry := p.Entry
			if entry == "" {
				entry = PaymentEntryImport
			}
			values = append(values, row)
			args = append(args, p.OwnerUserID, p.Source, p.TransactionID, p.ReferenceTxnID, p.Kind, p.PaidOn,
				p.Currency, p.GrossCents, p.FeeCents, p.NetCents, p.SenderName, p.SenderEmail,
				p.Note, p.Category, p.ClientID, entry, p.Method, p.Reference, p.SenderIBAN,
				now, now)
		}
		q := "INSERT INTO invoicing_payments (" + strings.Join(paymentInsertColumns, ", ") + ") VALUES " + strings.Join(values, ", ")
		if err := tx.Exec(q, args...).Error; err != nil {
			return err
		}
	}
	return nil
}
