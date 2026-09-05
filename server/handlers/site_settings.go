package handlers

import (
	"log"
	"net/http"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Site-wide policy switches an admin flips at runtime (system_3 epic #5113,
// subtask #5289). Today that is one switch: whether public self-service
// sign-up is open.
//
// The enforcement point is SignupHandler, not this file and not the client.
// What lives here is the pair of endpoints that read and write the flag.

// signupSettingInput is the body of the admin write. A pointer so a body that
// omits the field is rejected rather than read as false — "close sign-up" is
// not something to infer from a typo in a JSON key.
type signupSettingInput struct {
	Enabled *bool `json:"enabled"`
}

// GetSignupStatusHandler reports whether sign-up is currently open.
//
// Public and unauthenticated, because its consumer is the sign-up page, whose
// visitor has no account by definition. It discloses one site-wide policy bit
// and nothing about any account — unlike the sign-up endpoint itself, it cannot
// be turned into an existence oracle because it takes no input.
//
// Deliberately NOT behind PublicAuthRateLimit. That limiter's per-IP buckets
// are shared across /signup, /verify-email, /password/forgot and /password/reset
// and are sized (10/min, 20/15min) against the ~5 requests one person's whole
// sign-up-and-recover journey costs — see the sizing note in
// middleware/public_auth_rate_limit.go, which was widened once already after a
// walk ended in a legitimate user being 429'd out of password recovery. Charging
// a page load to that budget would spend the allowance the flow needs, to
// protect a read that touches one indexed row and sends no mail.
func GetSignupStatusHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	enabled, err := data_models.SignupEnabled(db)
	if err != nil {
		log.Printf("ERROR: reading the signup_enabled setting: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Could not read the sign-up status")
		return
	}

	c.JSON(http.StatusOK, gin.H{"signup_enabled": enabled})
}

// PutSignupSettingHandler opens or closes public sign-up site-wide.
//
// Admin tier, enforced by the route group. The log line is the audit record and
// it names the admin's user id and the new value — and no client IP, which is
// the epic's binding constraint: moderation on this service retains no
// addresses, in a row or in a log.
func PutSignupSettingHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	var input signupSettingInput
	if err := c.ShouldBindJSON(&input); err != nil || input.Enabled == nil {
		RespondWithError(c, http.StatusBadRequest, "A boolean 'enabled' field is required")
		return
	}

	if err := data_models.SetSignupEnabled(db, *input.Enabled); err != nil {
		log.Printf("ERROR: writing the signup_enabled setting: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Could not update the sign-up status")
		return
	}

	actor, _ := c.Get("user_id")
	log.Printf("Sign-up %s by admin user %v", map[bool]string{true: "opened", false: "closed"}[*input.Enabled], actor)

	RespondWithSuccess(c, gin.H{"signup_enabled": *input.Enabled}, "Sign-up status updated")
}
