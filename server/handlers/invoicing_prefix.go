package handlers

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"aspirant-online/server/data_models"
)

// The invoice number prefix's hyphen rules (system_3 #12272, the #11498
// review's C3-18): the app prints "<prefix>-<number>", so "2026-" issued
// 2026--001. A prefix does not start or end with a hyphen, and holds no two
// in a row. Each refusal has its own code and the sentence the field shows.
const (
	codePrefixEndsWithHyphen   = "prefix_ends_with_hyphen"
	codePrefixStartsWithHyphen = "prefix_starts_with_hyphen"
	codePrefixDoubleHyphen     = "prefix_double_hyphen"
)

// prefixHyphenRefusal returns the code and sentence that refuse prefix, or
// "" when its hyphens are allowed.
func prefixHyphenRefusal(prefix string) (code, message string) {
	switch {
	case strings.HasSuffix(prefix, "-"):
		return codePrefixEndsWithHyphen, "The prefix cannot end with a hyphen: the app adds one before the number."
	case strings.HasPrefix(prefix, "-"):
		return codePrefixStartsWithHyphen, "The prefix cannot start with a hyphen."
	case strings.Contains(prefix, "--"):
		return codePrefixDoubleHyphen, "The prefix cannot hold two hyphens in a row."
	}
	return "", ""
}

// checkPrefixHyphens applies the rules to a Save, writing the 400 itself.
// before is the stored profile, nil before the first Save. A prefix the Save
// does not change is not checked (the review's point 1): a profile whose
// stored prefix ends in a hyphen keeps saving its other fields and keeps its
// series, since changing "RE-" to "RE" would start a second one.
func checkPrefixHyphens(c *gin.Context, prefix string, before *data_models.InvoicingProfile) bool {
	if before != nil && before.InvoiceNumberPrefix == prefix {
		return true
	}
	if code, message := prefixHyphenRefusal(prefix); code != "" {
		respondError(c, http.StatusBadRequest, code, message)
		return false
	}
	return true
}
