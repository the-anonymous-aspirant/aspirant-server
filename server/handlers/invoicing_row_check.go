package handlers

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// The one row check (system_3 #12265): what a hand entry and the three import
// confirms (N26 payments, N26 expenses, PayPal expenses) each refuse about a
// row's date, amount and text. A stored imported row is the bank's record and
// cannot be corrected afterwards, only removed, so a confirm must not store
// what a hand entry would refuse. Per route, as before: whether a payer or
// payee is required, the length limits, and the currency.

// checkRowPaidOn parses a row's day and holds it from 2000-01-01 (a mistyped
// year, #8409 security F4) to latest. The answer is the refusal, or "".
func checkRowPaidOn(raw string, latest time.Time) (time.Time, string) {
	d, err := time.Parse(invoicingDateLayout, raw)
	if err != nil {
		return time.Time{}, "paid_on is not a yyyy-mm-dd date"
	}
	if d.After(latest) {
		return d, "paid_on is in the future"
	}
	if d.Before(earliestPaymentDate) {
		return d, "paid_on is before 2000-01-01"
	}
	return d, ""
}

// rowAmountInBounds holds an amount to the hand entry's range: above zero
// and at most 100 million euros (maxUnitPriceCents).
func rowAmountInBounds(cents int64) bool {
	return cents > 0 && cents <= maxUnitPriceCents
}

// rowAmountRefusal is the sentence for an amount out of that range.
func rowAmountRefusal(field string) string {
	return fmt.Sprintf("%s must be above zero and at most %d euros", field, maxUnitPriceCents/100)
}

// oneLineRowText is a payer's, a payee's or a reference's rule, as the hand
// entry has it (validOneLineName): one line, no control character and no
// invisible format character.
func oneLineRowText(s string) bool {
	return !strings.Contains(s, "\n") && !hasControlChars(s) && !hasFormatChars(s)
}

// rowNoteText is a note's rule: no control character but the line break, and
// no direction override or isolate. The rest of Unicode's format class stays
// allowed, so a note may hold the joiner inside a compound emoji (#12265
// review point 2).
func rowNoteText(s string) bool {
	return !hasControlChars(s) && !hasDirectionOverride(s)
}

// hasDirectionOverride reports U+202A-U+202E (the embeddings and overrides)
// and U+2066-U+2069 (the isolates), which make printed text read in an order
// other than the one stored.
func hasDirectionOverride(s string) bool {
	for _, r := range s {
		if (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) {
			return true
		}
	}
	return false
}

var rowDayPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// importRowName names a refused row by its day and its payer or payee, not by
// its place in the request (#12265): "the row of 2026-10-03 from Mara
// Esempio". The name is printed cleaned of control and format characters and
// cut to 40 characters, so a refused name cannot reorder the message.
func importRowName(paidOn, name, preposition string) string {
	day := ""
	if rowDayPattern.MatchString(strings.TrimSpace(paidOn)) {
		day = strings.TrimSpace(paidOn)
	}
	who := truncateRunes(cleanRowText(name), 40)
	switch {
	case day != "" && who != "":
		return fmt.Sprintf("the row of %s %s %s", day, preposition, who)
	case day != "":
		return fmt.Sprintf("the row of %s", day)
	case who != "":
		return fmt.Sprintf("the row %s %s", preposition, who)
	}
	return "a row with no day and no name"
}

// cleanRowText drops control and format characters and collapses every run
// of whitespace to one space: how every parse cleans the names and notes it
// offers, so no offered row fails the check above (#12265 review point 1).
func cleanRowText(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && !unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}
