package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

// Refusals of the invoicing forms (system_3 #12300 ruling 1). Each cause a
// form can meet answers its own code, the request field it is about, and,
// where the client's sentence prints a number, that number as `limit`. The
// client holds one sentence per code and prints it at the field; the
// message stays in the answer for the logs and for clients that print it.
//
// The codes are a contract with the client: a renamed code reads as the
// client's fallback line. Add a code, never repurpose one.
const (
	codeNotAString         = "not_a_string"          // a field that must be a string is not one
	codeInvalidDate        = "invalid_date"          // not a YYYY-MM-DD date
	codeDateInFuture       = "date_in_future"        // after today in Berlin
	codeDateTooEarly       = "date_too_early"        // before earliestPaymentDate; limit = its year
	codeInvalidAmount      = "invalid_amount"        // missing, not whole cents, or not above zero
	codeAmountTooLarge     = "amount_too_large"      // above maxUnitPriceCents
	codeTooLong            = "too_long"              // over limit characters, or several lines in a one-line field
	codeHiddenCharacters   = "hidden_characters"     // a tab, control or invisible format character
	codeNoLetterOrDigit    = "no_letter_or_digit"    // a name made only of punctuation
	codeInvalidEmail       = "invalid_email"         // not one bare address
	codeInvalidChoice      = "invalid_choice"        // not one of the values the field takes
	codeInvalidID          = "invalid_id"            // an id that is not a positive whole number
	codeNoSuchClient       = "no_such_client"        // client_id names no client of the owner
	codeClientConflict     = "client_and_new_client" // client_id and new_client_name together
	codeNeedsNewClient     = "needs_new_client"      // a new client's details without its name
	codeNotCategorisable   = "not_categorisable"     // a category on a bank withdrawal
	codeRequired           = "required"              // an empty field that must be given
	codeInvalidCategory    = "invalid_category"      // not one of the owner's expense categories
	codeInvalidPrefix      = "invalid_prefix"        // an invoice number prefix of other characters or over limit
	codeNumberTooSmall     = "number_too_small"      // under limit
	codeBelowZero          = "below_zero"            // a draft line\'s quantity or unit price under 0
	codeQuantityTooLarge   = "quantity_too_large"    // a draft line\'s quantity over limit units
	codeInvalidLineItems   = "invalid_line_items"    // line_items is not a list of line items
	codeInvalidVATRate     = "invalid_vat_rate"      // not basis points 0-10000 in steps of 10
	codeMissingFile        = "missing_file"          // no file in the form field
	codeEmptyFile          = "empty_file"            // a file of no bytes
	codeFileTooLarge       = "too_large"             // over limit MB
	codeUnsupportedType    = "unsupported_type"      // not one of the types the field takes
	codeAmountDecimals     = "too_many_decimals"     // an amount in a file with more than two decimals
	codeInvalidCurrency    = "invalid_currency"      // not a three-letter ISO currency code
	codeDateOrdersDisagree = "date_orders_disagree"  // a file\'s dates written day/month and month/day
	codePaymentListedTwice = "payment_listed_twice"  // a payment id twice in one list; duplicate_payment is one already in the book
)

// refusal is why one field cannot be accepted. nil means it can.
type refusal struct {
	code    string
	message string // after the field's name: "must be ...", unless whole
	limit   int64
	lines   int64
	item    int // 1-based list position, 0 for none
	width   int // an image's size, where the refusal names it
	height  int
	whole   bool // message is a whole sentence, not prefixed with the field
}

func refuse(code, message string) *refusal { return &refusal{code: code, message: message} }

func refuseOver(code string, limit int64, message string) *refusal {
	return &refusal{code: code, message: message, limit: limit}
}

// refuseWhole is a refusal whose message is already a whole sentence, as
// the client book's and the editors' dedicated codes have always answered.
func refuseWhole(code, message string) *refusal {
	return &refusal{code: code, message: message, whole: true}
}

// respondFieldRefusal answers 400 with the refusal's code, field and limit.
// The message reads "<field> <message>", as these answers always have,
// unless the refusal's message is a whole sentence.
func respondFieldRefusal(c *gin.Context, field string, r *refusal) {
	msg := r.message
	if !r.whole {
		msg = field + " " + msg
	}
	log.Printf("Error response: %d - %s - %s", http.StatusBadRequest, r.code, msg)
	c.JSON(http.StatusBadRequest, ErrorResponse{Error: ErrorDetail{Code: r.code, Message: msg, Field: field, Limit: r.limit, Lines: r.lines, Item: r.item, Width: r.width, Height: r.height}})
}

var notAString = refuse(codeNotAString, "must be a string")

// oneLineTextRefusal is the rule for a one-line text typed by hand (a
// reference, a payer or payee, a name): at most max characters on one line,
// with no control or invisible format character (#8409 security F3). Length
// and a line break read as one sentence ("one line of at most ..."); the
// hidden characters have their own (#12300 review point 4).
func oneLineTextRefusal(s string, max int) *refusal {
	if utf8.RuneCountInString(s) > max || strings.ContainsAny(s, "\r\n") {
		return refuseOver(codeTooLong, int64(max), fmt.Sprintf("must be one line of at most %d characters", max))
	}
	if hasControlChars(s) || hasFormatChars(s) {
		return refuse(codeHiddenCharacters, "must not contain tabs, control or invisible format characters")
	}
	return nil
}

// textRefusal is the rule for a text of several lines: at most max
// characters, line feeds allowed, no other control character. Format
// characters are refused only where formatChars is set.
func textRefusal(s string, max int, formatChars bool) *refusal {
	if utf8.RuneCountInString(s) > max {
		return refuseOver(codeTooLong, int64(max), fmt.Sprintf("must be at most %d characters", max))
	}
	if hasControlChars(s) || (formatChars && hasFormatChars(s)) {
		return refuse(codeHiddenCharacters, "must not contain tabs, control or invisible format characters")
	}
	return nil
}

// paidOnRefusal checks a hand-typed date: a date, not after today in Berlin
// (futureHint says which day to enter), and not before earliestPaymentDate.
func paidOnRefusal(s, futureHint string) (time.Time, *refusal) {
	d, err := time.Parse(invoicingDateLayout, s)
	if err != nil {
		return time.Time{}, refuse(codeInvalidDate, "must be a date, YYYY-MM-DD")
	}
	if d.After(berlinToday()) {
		return time.Time{}, refuse(codeDateInFuture, "cannot be in the future: "+futureHint)
	}
	if d.Before(earliestPaymentDate) {
		return time.Time{}, refuseOver(codeDateTooEarly, int64(earliestPaymentDate.Year()), "must be 2000-01-01 or later")
	}
	return d, nil
}

// centsRefusal checks an amount typed by hand: whole cents, above zero, and
// at most maxUnitPriceCents.
func centsRefusal(raw json.RawMessage) (int64, *refusal) {
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil || v <= 0 {
		return 0, refuse(codeInvalidAmount, "must be a whole number of cents above zero")
	}
	if v > maxUnitPriceCents {
		return 0, refuseOver(codeAmountTooLarge, maxUnitPriceCents, fmt.Sprintf("must be at most %d cents", maxUnitPriceCents))
	}
	return v, nil
}

// emailRefusal accepts an empty string or one bare address.
func emailRefusal(s string) *refusal {
	if !validOptionalEmail(s) {
		return refuse(codeInvalidEmail, "must be a valid address when given")
	}
	return nil
}

// respondRowRefusal refuses row i (0-based) of a confirm. item is its
// 1-based position, so the client names the row from what it sent: its
// payer and date, never a number (c77188 point 9). The message is the
// one the logs have always read.
func respondRowRefusal(c *gin.Context, i int, msg string) {
	respondFieldRefusal(c, "rows", &refusal{code: "invalid_row", item: i + 1, whole: true, message: fmt.Sprintf("row %d: %s", i, msg)})
}

// respondNamedRowRefusal is respondRowRefusal for a row the hand entry's
// check refused: the message names the row by its day and payer or payee
// (importRowName, system_3 #12265), as a client that shows it as written
// reads it, and item still gives its position.
func respondNamedRowRefusal(c *gin.Context, i int, name, msg string) {
	respondFieldRefusal(c, "rows", &refusal{code: "invalid_row", item: i + 1, whole: true, message: name + ": " + msg})
}
