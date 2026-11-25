package handlers

import (
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"aspirant-online/server/data_models"
)

// The issuer's name, Steuernummer and address print in every invoice's
// header, and every Save copies the profile into an audit row (system_3
// #12270, the #11498 review's C3-16: a 1,000,000-character name saved). The
// name and the Steuernummer are one line of at most 200 characters; the
// address is at most 10 lines of at most 200, the client address's bounds
// (#12264). The other fields keep their own rules.
const (
	maxProfileLineRunes    = 200
	maxProfileAddressLines = 10
)

// The refusals' codes, one per sentence the editor prints (#12300's table).
const (
	codeFullNameTooLong     = "full_name_too_long"
	codeSteuernummerTooLong = "steuernummer_too_long"
	codeAddressTooLong      = "address_too_long"
)

// oneLineWithin reports whether s is one line of at most max characters.
func oneLineWithin(s string, max int) bool {
	return !strings.ContainsAny(s, "\r\n") && utf8.RuneCountInString(s) <= max
}

// addressWithin reports whether s is at most lines lines, each of at most
// max characters. A line ends at "\n"; a "\r" before it is not counted.
func addressWithin(s string, lines, max int) bool {
	parts := strings.Split(s, "\n")
	if len(parts) > lines {
		return false
	}
	for _, p := range parts {
		if utf8.RuneCountInString(strings.TrimSuffix(p, "\r")) > max {
			return false
		}
	}
	return true
}

// checkProfileBounds applies the bounds to a Save, writing the 400 itself.
// before is the stored profile, nil before the first Save. A field the Save
// does not change is not checked (the #11498 review's point 1, the rule of
// #12264): the editor sends the whole profile with every Save, so a profile
// stored over a bound stays editable in its other fields.
func checkProfileBounds(c *gin.Context, req invoicingProfileRequest, before *data_models.InvoicingProfile) bool {
	changed := func(stored, sent string) bool { return before == nil || stored != sent }
	var stored data_models.InvoicingProfile
	if before != nil {
		stored = *before
	}
	switch {
	case changed(stored.FullName, req.FullName) && !oneLineWithin(req.FullName, maxProfileLineRunes):
		respondError(c, http.StatusBadRequest, codeFullNameTooLong,
			fmt.Sprintf("full_name must be one line of at most %d characters", maxProfileLineRunes))
		return false
	case changed(stored.Address, req.Address) && !addressWithin(req.Address, maxProfileAddressLines, maxProfileLineRunes):
		respondError(c, http.StatusBadRequest, codeAddressTooLong,
			fmt.Sprintf("address must be at most %d lines of at most %d characters each", maxProfileAddressLines, maxProfileLineRunes))
		return false
	case changed(stored.Steuernummer, req.Steuernummer) && !oneLineWithin(req.Steuernummer, maxProfileLineRunes):
		respondError(c, http.StatusBadRequest, codeSteuernummerTooLong,
			fmt.Sprintf("steuernummer must be one line of at most %d characters", maxProfileLineRunes))
		return false
	}
	return true
}
