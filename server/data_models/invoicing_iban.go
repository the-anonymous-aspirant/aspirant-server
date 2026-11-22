package data_models

import (
	"math/big"
	"regexp"
	"strings"
	"unicode"
)

// ibanShape is an IBAN in NormaliseIBAN's form: a country code, two check
// digits and an 11-30 character account part (ISO 13616; 15-34 in all).
var ibanShape = regexp.MustCompile(`^[A-Z]{2}[0-9]{2}[A-Z0-9]{11,30}$`)

// NormaliseIBAN is the one form an IBAN is stored and compared in (security
// C3, system_3 #11551): every space removed and upper case, so "DE89 3704 …"
// and "de893704…" are the same account. It does not validate; see ValidIBAN.
func NormaliseIBAN(s string) string {
	return strings.ToUpper(strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s))
}

// ValidIBAN reports whether a normalised IBAN has the right shape and passes
// the ISO 13616 mod-97 check, which catches a mistyped or swapped digit.
func ValidIBAN(iban string) bool {
	if !ibanShape.MatchString(iban) {
		return false
	}
	rearranged := iban[4:] + iban[:4]
	var digits strings.Builder
	for _, r := range rearranged {
		if r >= 'A' && r <= 'Z' {
			digits.WriteString(big.NewInt(int64(r - 'A' + 10)).String())
		} else {
			digits.WriteRune(r)
		}
	}
	n, ok := new(big.Int).SetString(digits.String(), 10)
	if !ok {
		return false
	}
	return new(big.Int).Mod(n, big.NewInt(97)).Int64() == 1
}
