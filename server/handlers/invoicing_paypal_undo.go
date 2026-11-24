package handlers

import (
	"aspirant-online/server/data_models"

	"github.com/jinzhu/gorm"
)

// paypalUndonePayment is the stored payment a PayPal row undoes, as the
// import's answer names it: enough for the line "€120.00 from Mara Esempio,
// 2026-10-03" and for the row door that opens its Edit dialog.
type paypalUndonePayment struct {
	ID            uint   `json:"id"`
	TransactionID string `json:"transaction_id"`
	PaidOn        string `json:"paid_on"`
	SenderName    string `json:"sender_name"`
	GrossCents    int64  `json:"gross_cents"`
	Currency      string `json:"currency"`
}

// paypalUndoneInvoice is the issued invoice that covers such a payment.
type paypalUndoneInvoice struct {
	ID            uint   `json:"id"`
	InvoiceNumber string `json:"invoice_number"`
}

// paypalUndone is one row of the file that undoes money already in the book
// (system_3 #12259, ruling item 1): a reversal or a refund ze paid, and the
// payment it names. Full is true when it takes back the payment's whole
// gross. Invoice is set when an issued invoice covers the payment.
type paypalUndone struct {
	Kind          string               `json:"kind"`
	TransactionID string               `json:"transaction_id"`
	PaidOn        string               `json:"paid_on"`
	AmountCents   int64                `json:"amount_cents"`
	Full          bool                 `json:"full"`
	Payment       paypalUndonePayment  `json:"payment"`
	Invoice       *paypalUndoneInvoice `json:"invoice,omitempty"`
}

// matchPayPalUndos names the stored payment each undo row refers to. A row
// matches the owner's own live PayPal payment of money in whose transaction
// id is the row's reference id, in the same currency, for at least the
// row's amount. Anything else stays the counted skip it already is (ruling
// item 6): another book's payment, a removed one, a withdrawal, or nothing.
//
// It reads only. A partial undo is also returned as an expense candidate,
// unticked like every candidate, so the Book's Result can count it once ze
// confirms it (ruling item 5).
func matchPayPalUndos(db *gorm.DB, owner uint, undos []paypalUndoRow) ([]paypalUndone, []paypalExpenseCandidate, error) {
	undone := []paypalUndone{}
	var candidates []paypalExpenseCandidate
	if len(undos) == 0 {
		return undone, candidates, nil
	}
	var invoices map[uint]data_models.PaymentInvoiceRef
	for _, u := range undos {
		var p data_models.InvoicingPayment
		err := db.Where("owner_user_id = ? AND source = ? AND transaction_id = ? AND kind = ? AND removed_at IS NULL",
			owner, invoicingPaymentSourcePayPal, u.ReferenceTxnID, data_models.PaymentKindIncoming).First(&p).Error
		if gorm.IsRecordNotFoundError(err) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if p.Currency != u.Currency || u.AmountCents > p.GrossCents {
			continue
		}
		if invoices == nil {
			if invoices, err = data_models.LiveInvoicesForPayments(db, owner); err != nil {
				return nil, nil, err
			}
		}
		entry := paypalUndone{
			Kind:          u.Kind,
			TransactionID: u.TransactionID,
			PaidOn:        u.PaidOn.Format(invoicingDateLayout),
			AmountCents:   u.AmountCents,
			Full:          u.AmountCents == p.GrossCents,
			Payment: paypalUndonePayment{
				ID:            p.ID,
				TransactionID: p.TransactionID,
				PaidOn:        p.PaidOn.Format(invoicingDateLayout),
				SenderName:    p.SenderName,
				GrossCents:    p.GrossCents,
				Currency:      p.Currency,
			},
		}
		if ref, ok := invoices[p.ID]; ok && ref.Status == data_models.InvoiceStatusIssued && ref.InvoiceNumber != nil {
			entry.Invoice = &paypalUndoneInvoice{ID: ref.ID, InvoiceNumber: *ref.InvoiceNumber}
		}
		undone = append(undone, entry)
		if !entry.Full {
			c := paypalExpenseCandidates([]paypalExpenseRow{u.paypalExpenseRow})[0]
			c.RefundOf = p.ID
			c.UndoKind = u.Kind
			candidates = append(candidates, c)
		}
	}
	return undone, candidates, nil
}
