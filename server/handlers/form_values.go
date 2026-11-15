package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"strings"
	"unicode/utf8"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Remembered form values (system_3 #11190, design_conventions §3.154 Decisions
// 1, 7 and 8, with security's additions in c68478 on #11034): one record per
// user per tool, scoped to the session's user, holding only a fixed set of
// keys per tool. The per-user scratchpad (scratchpad.go) is the pattern.
//
// No side paths. No value from a request or a record ever reaches a log line,
// an error body or any other output but the owner's own GET. respondError logs
// its message, so every message below is a fixed string that names at most a
// field, never a value and never a decoder error (which can echo input).

// formToolSpec is one allowlisted tool: the tier its user must hold, and the
// validator that turns a request body into the JSON stored for it.
type formToolSpec struct {
	minTier  AccessTier
	validate func(body []byte) (stored []byte, field string, ok bool)
}

// formTools is the allowlist. A later tool joins by one line here, reviewed by
// security (§3.154). An unknown tool answers 404 and stores nothing; there is
// no default tier, so no tool is open to every signed-in user by accident.
var formTools = map[string]formToolSpec{
	"kvitto": {minTier: TierAdmin, validate: validateKvittoValues},
}

// maxFormValuesBody bounds the request body before decoding. The largest
// legal kvitto body is well under 2 KiB (three capped texts and a number).
const maxFormValuesBody = 8 << 10

// The rent-receipt tool (/admin/tools/kvitto): exactly four keys. No year is
// stored. Caps are in characters, not bytes.
const (
	kvittoAddressMax = 300
	kvittoNameMax    = 200
	kvittoAmountMax  = 1_000_000
)

type kvittoValues struct {
	Address       string   `json:"address"`
	MonthlyAmount *float64 `json:"monthly_amount"`
	Recipient     string   `json:"recipient"`
	Payer         string   `json:"payer"`
}

const kvittoKeysMessage = "Only address, monthly_amount, recipient and payer are accepted"

// validateKvittoValues decodes body strictly (any other key is refused) and
// checks each field. On refusal it returns the field's name, or "" when the
// body as a whole is refused; never a value.
func validateKvittoValues(body []byte) ([]byte, string, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var v kvittoValues
	if err := dec.Decode(&v); err != nil {
		// A wrong type names its key, which is one of ours and safe to name;
		// the offending value never leaves this function.
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			switch typeErr.Field {
			case "address", "recipient", "payer":
				return nil, typeErr.Field + "_type", false
			case "monthly_amount":
				return nil, "monthly_amount", false
			}
		}
		return nil, "", false
	}
	// One JSON object only: trailing data is refused, not ignored.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, "", false
	}
	if utf8.RuneCountInString(v.Address) > kvittoAddressMax {
		return nil, "address", false
	}
	if utf8.RuneCountInString(v.Recipient) > kvittoNameMax {
		return nil, "recipient", false
	}
	if utf8.RuneCountInString(v.Payer) > kvittoNameMax {
		return nil, "payer", false
	}
	if a := v.MonthlyAmount; a != nil {
		cents := *a * 100
		if math.IsNaN(*a) || *a <= 0 || *a > kvittoAmountMax || math.Abs(cents-math.Round(cents)) > 1e-6 {
			return nil, "monthly_amount", false
		}
	}
	// Re-serialised from the struct, so only these four keys are ever stored.
	stored, err := json.Marshal(v)
	if err != nil {
		return nil, "", false
	}
	return stored, "", true
}

// fieldRefusal is the message for one refused field: it names the field and
// its rule, never what was sent.
func fieldRefusal(field string) string {
	switch field {
	case "address":
		return "address is longer than 300 characters"
	case "recipient":
		return "recipient is longer than 200 characters"
	case "payer":
		return "payer is longer than 200 characters"
	case "monthly_amount":
		return "monthly_amount must be a number above 0 and at most 1000000, with at most two decimals"
	case "address_type", "recipient_type", "payer_type":
		return strings.TrimSuffix(field, "_type") + " must be text"
	default:
		return kvittoKeysMessage
	}
}

// formToolFor runs the checks every form-values route shares, in order: no
// caching of any response, a known tool, a signed-in user at the tool's tier.
// The user always comes from the session; nothing in the path, query or body
// can name one.
func formToolFor(c *gin.Context) (formToolSpec, string, uint, bool) {
	c.Header("Cache-Control", "no-store")
	tool := c.Param("tool")
	spec, known := formTools[tool]
	if !known {
		respondError(c, http.StatusNotFound, "not_found", "No such tool")
		return formToolSpec{}, "", 0, false
	}
	role, _ := c.Get("role")
	roleStr, _ := role.(string)
	if tierOf(roleStr) < spec.minTier {
		respondError(c, http.StatusForbidden, "forbidden", "Insufficient permissions")
		return formToolSpec{}, "", 0, false
	}
	userID, ok := getAuthUserID(c)
	if !ok {
		return formToolSpec{}, "", 0, false
	}
	return spec, tool, userID, true
}

// GetFormValuesHandler returns the session user's remembered values for one
// tool, or {"values": null} when nothing is stored (200, not 404).
func GetFormValuesHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	_, tool, userID, ok := formToolFor(c)
	if !ok {
		return
	}
	fv, err := data_models.GetFormValues(db, userID, tool)
	if err != nil {
		log.Printf("form values: read failed for tool %q, user %d", tool, userID)
		respondError(c, http.StatusInternalServerError, "internal_error", "Failed to read form values")
		return
	}
	if fv == nil {
		c.JSON(http.StatusOK, gin.H{"values": nil})
		return
	}
	c.JSON(http.StatusOK, gin.H{"values": json.RawMessage(fv.Values), "updated_at": fv.UpdatedAt})
}

// PutFormValuesHandler stores the session user's values for one tool, on a
// deliberate save only (the form decides when; §3.154 forbids autosave).
func PutFormValuesHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	spec, tool, userID, ok := formToolFor(c)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxFormValuesBody))
	if err != nil {
		if strings.Contains(err.Error(), "request body too large") {
			respondError(c, http.StatusRequestEntityTooLarge, "payload_too_large", "Form values are too large")
			return
		}
		respondError(c, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}
	stored, field, valid := spec.validate(body)
	if !valid {
		respondError(c, http.StatusBadRequest, "validation_error", fieldRefusal(field))
		return
	}
	fv, err := data_models.UpsertFormValues(db, userID, tool, string(stored))
	if err != nil {
		log.Printf("form values: save failed for tool %q, user %d", tool, userID)
		respondError(c, http.StatusInternalServerError, "internal_error", "Failed to save form values")
		return
	}
	c.JSON(http.StatusOK, gin.H{"values": json.RawMessage(fv.Values), "updated_at": fv.UpdatedAt})
}

// DeleteFormValuesHandler removes the session user's record for one tool:
// the row goes, it is not emptied. 204 whether or not there was one.
func DeleteFormValuesHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	_, tool, userID, ok := formToolFor(c)
	if !ok {
		return
	}
	if err := data_models.DeleteFormValues(db, userID, tool); err != nil {
		log.Printf("form values: delete failed for tool %q, user %d", tool, userID)
		respondError(c, http.StatusInternalServerError, "internal_error", "Failed to delete form values")
		return
	}
	c.Status(http.StatusNoContent)
}
