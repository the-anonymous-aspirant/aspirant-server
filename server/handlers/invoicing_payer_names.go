package handlers

import (
	"strings"
	"unicode"

	"aspirant-online/server/data_models"

	"github.com/jinzhu/gorm"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// Payer names and the client book (system_3 #10539, design §3.142 Decision
// 4). One normaliser decides when a typed or imported payer name is "the
// same" as a client already in the book, so the hand-entry create and the
// payers dialog (#10539-A2) never disagree about it.

// payerNamePunctuation is dropped before names are compared: "Mara E." and
// "Mara E" are one name, as are "Esempio-Rossi" and "Esempio Rossi".
var payerNamePunctuation = strings.NewReplacer(".", " ", ",", " ", "-", " ", "'", " ", "’", " ")

// normalisePayerName folds a name for comparison: case folded, accents
// stripped ("Mára" → "mara"), the punctuation above dropped and runs of
// whitespace collapsed to one space. It is never shown or stored; the name
// as typed is what is saved.
func normalisePayerName(s string) string {
	stripped, _, err := transform.String(transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC), s)
	if err != nil {
		stripped = s
	}
	stripped = payerNamePunctuation.Replace(strings.ToLower(stripped))
	return strings.Join(strings.Fields(stripped), " ")
}

// sameClientName reports whether two names are the same payer under
// Decision 4: their normalised forms are equal and not empty.
func sameClientName(a, b string) bool {
	na := normalisePayerName(a)
	return na != "" && na == normalisePayerName(b)
}

// findSameClient returns the owner's client whose name is the same as name
// under Decision 4, or nil. A client book is small, so the comparison runs
// in Go over the owner's clients rather than in SQL.
func findSameClient(db *gorm.DB, owner uint, name string) (*data_models.InvoicingClient, error) {
	clients, err := data_models.ListInvoicingClients(db, owner)
	if err != nil {
		return nil, err
	}
	for i := range clients {
		if sameClientName(clients[i].Name, name) {
			return &clients[i], nil
		}
	}
	return nil, nil
}

// validClientName refuses a client name with nothing left once normalised —
// one made only of the folded punctuation, such as "..." or " - ". Such a
// name could never be "the same" as anything, so it would be added again and
// again (security review of PR 185, c65409 on #10547).
func validClientName(name string) bool {
	return normalisePayerName(name) != ""
}
