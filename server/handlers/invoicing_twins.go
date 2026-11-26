package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// A look-alike of a hand entry is asked about once and never refused
// (system_3 #12295): two cash payments of one amount on one day can both be
// real. A create whose day, amount, currency and payer match a row that is
// not removed answers 409 possible_duplicate with that row, unless the
// request says confirm_duplicate: true. Only the creates ask: an edit would
// match its row against itself (review point 2). The check runs under the
// book lock the create already holds, so two tabs of one book agree.

const codePossibleDuplicate = "possible_duplicate"

// confirmDuplicateFlag reads the optional "confirm_duplicate". Only a JSON
// boolean is accepted; absent or null is false.
func confirmDuplicateFlag(body map[string]json.RawMessage) (bool, error) {
	raw, ok := body["confirm_duplicate"]
	if !ok || string(raw) == "null" {
		return false, nil
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return false, fmt.Errorf("must be true or false")
	}
	return v, nil
}

// samePayerNames reports whether two rows name the same payer under design
// §3.142 Decision 4. Each side is every name it holds (a client's name, a
// typed or imported name), so a payer held as a client and one held as a
// name are the same when the names are (review point 3). A row with no name
// at all matches only another with none.
func samePayerNames(a, b []string) bool {
	na, nb := payerKeys(a), payerKeys(b)
	if len(na) == 0 || len(nb) == 0 {
		return len(na) == 0 && len(nb) == 0
	}
	for k := range na {
		if nb[k] {
			return true
		}
	}
	return false
}

func payerKeys(names []string) map[string]bool {
	keys := map[string]bool{}
	for _, n := range names {
		if k := normalisePayerName(n); k != "" {
			keys[k] = true
		}
	}
	return keys
}

// paymentPayerNames is every name a payment holds for its payer: its
// client's name, when it has a client, and its sender name.
func paymentPayerNames(p data_models.InvoicingPayment, clientNames map[uint]string) []string {
	names := []string{p.SenderName}
	if p.ClientID != nil {
		names = append(names, clientNames[*p.ClientID])
	}
	return names
}

// findPaymentTwin returns the owner's incoming payment, imported or by hand,
// not removed, with this day, gross amount and currency and the same payer
// as names, or nil.
func findPaymentTwin(tx *gorm.DB, owner uint, paidOn time.Time, gross int64, currency string, names []string) (*data_models.InvoicingPayment, error) {
	var rows []data_models.InvoicingPayment
	err := tx.Where("owner_user_id = ? AND removed_at IS NULL AND kind = ? AND paid_on = ? AND gross_cents = ? AND currency = ?",
		owner, data_models.PaymentKindIncoming, paidOn, gross, currency).Order("id").Find(&rows).Error
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	clients, err := data_models.ListInvoicingClients(tx, owner)
	if err != nil {
		return nil, err
	}
	clientNames := make(map[uint]string, len(clients))
	for _, cl := range clients {
		clientNames[cl.ID] = cl.Name
	}
	for i := range rows {
		if samePayerNames(paymentPayerNames(rows[i], clientNames), names) {
			return &rows[i], nil
		}
	}
	return nil, nil
}

// expensePayee is who an expense was paid to: a hand entry's payee, or an
// import's, which it keeps in Reference.
func expensePayee(e data_models.InvoicingExpense) string {
	if e.Source == data_models.ExpenseSourceManual {
		return e.Payee
	}
	return e.Reference
}

// findExpenseTwin returns the owner's expense, imported or by hand, not
// removed, with this day, amount and currency and the same payee, or nil.
func findExpenseTwin(tx *gorm.DB, owner uint, paidOn time.Time, amount int64, currency, payee string) (*data_models.InvoicingExpense, error) {
	var rows []data_models.InvoicingExpense
	err := tx.Where("owner_user_id = ? AND removed_at IS NULL AND paid_on = ? AND amount_cents = ? AND currency = ?",
		owner, paidOn, amount, currency).Order("id").Find(&rows).Error
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if samePayerNames([]string{expensePayee(rows[i])}, []string{payee}) {
			return &rows[i], nil
		}
	}
	return nil, nil
}

// respondPossibleDuplicate writes the question: 409 possible_duplicate with
// the other row's id and the row, under key ("payment" or "expense"), so the
// form can name it and open it.
func respondPossibleDuplicate(c *gin.Context, key string, id uint, row interface{}) {
	what := map[string]string{"payment": "a payment with the same day, amount and payer", "expense": "an expense with the same day, amount and payee"}[key]
	c.JSON(http.StatusConflict, gin.H{
		"error": ErrorDetail{
			Code:    codePossibleDuplicate,
			Message: what + " is already in your book; send confirm_duplicate to save this one as well",
		},
		"duplicate_of": id,
		key:            row,
	})
}
