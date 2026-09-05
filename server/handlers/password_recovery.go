package handlers

import (
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"strings"

	"aspirant-online/server/data_models"
	"aspirant-online/server/middleware"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Password recovery by emailed single-use token (system_3 epic #5113,
// subtask #5221).
//
// Distinct from signup.go, and the distinction is worth stating because the two
// files look similar: sign-up mints a principal and grants it a tier, recovery
// rotates a secret on a principal that already exists and grants nothing. The
// shared machinery is the token model and the mail seam, not the semantics.
//
// The same non-disclosure requirement governs this file: /password/forgot must
// answer identically whether or not the address is registered. Here it is a
// sharper obligation than at sign-up, because the endpoint takes an address
// and nothing else — a distinguishable answer would turn it into a bulk
// address-membership oracle for any list someone cares to feed it.

// forgotResponseMessage is returned for every /password/forgot outcome. A
// constant so the two call sites cannot drift apart; one differing word would
// be the oracle.
const forgotResponseMessage = "If that address has an account, a reset link is on its way."

type forgotPasswordInput struct {
	Email string `json:"email" binding:"required"`
}

type resetPasswordInput struct {
	Token    string `json:"token" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// ForgotPasswordHandler issues a reset token and mails it, when the address is
// one we know.
//
// Public and unauthenticated. Rate limiting is middleware (#5222) — and this
// endpoint needs a per-address limit as well as a per-IP one, because it is an
// unauthenticated way to make this server send mail to an address the caller
// chooses.
func ForgotPasswordHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	var input forgotPasswordInput
	if err := c.ShouldBindJSON(&input); err != nil {
		RespondWithError(c, http.StatusBadRequest, "An email address is required")
		return
	}

	address := strings.TrimSpace(input.Email)
	if _, err := mail.ParseAddress(address); err != nil {
		// A shape check. It describes the request, not the database, so
		// answering distinctly leaks nothing.
		RespondWithError(c, http.StatusBadRequest, "That does not look like an email address")
		return
	}

	var user data_models.User
	err := db.Where("email = ?", address).First(&user).Error
	if err != nil {
		if !gorm.IsRecordNotFoundError(err) {
			log.Printf("ERROR: forgot-password lookup: %v", err)
		}
		// Unknown address: answer exactly as for a known one. No token is
		// issued and no mail is sent.
		forgotAccepted(c)
		return
	}

	token, issueErr := data_models.IssueUserToken(db, user.ID, data_models.PurposePasswordReset)
	if issueErr != nil {
		log.Printf("ERROR: issuing reset token for user %d: %v", user.ID, issueErr)
		forgotAccepted(c)
		return
	}

	// Issuing retired this user's earlier unconsumed reset tokens — see the
	// policy table in data_models/user_token.go. That matters most here: asking
	// for a reset is what someone does when they think they have lost control
	// of the account, so an older link, possibly the one an attacker holds,
	// must stop working the moment a new one is requested.
	sendOrLog(c, user.Email, "Reset your password", resetMailBody(user.Username, token), "password-reset")
	forgotAccepted(c)
}

func forgotAccepted(c *gin.Context) {
	RespondWithSuccess(c, gin.H{}, forgotResponseMessage)
}

// resetMailBody renders the reset message.
//
// Like the verification mail, the link points at a frontend page rather than a
// GET endpoint on this API: mail scanners and link-preview fetchers follow
// links, and a single-use token consumed by a prefetch leaves the user holding
// a link that reports itself invalid.
func resetMailBody(username, token string) string {
	return fmt.Sprintf(`Hello %s,

Someone asked to reset the password for your account. If it was you, use this
link:

%s/reset-password?token=%s

The link is good for one hour and can be used once. Any earlier reset link you
were sent has stopped working.

If it was not you, you can ignore this message — your password has not changed
and nobody can change it without this link.

— %s`, username, publicBaseURL(), token, publicBaseURL())
}

// ResetPasswordHandler consumes a reset token and installs a new password.
func ResetPasswordHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	var input resetPasswordInput
	if err := c.ShouldBindJSON(&input); err != nil {
		RespondWithError(c, http.StatusBadRequest, "A token and a new password are required")
		return
	}

	// Validate the new password BEFORE consuming the token. Consuming first
	// and then rejecting the password would burn a single-use link on a typo
	// and send the user back to their inbox for a new one.
	if len(input.Password) < minPasswordLength {
		RespondWithError(c, http.StatusBadRequest,
			fmt.Sprintf("Password must be at least %d characters", minPasswordLength))
		return
	}
	if len(input.Password) > maxPasswordLength {
		// bcrypt ignores everything past 72 bytes — see maxPasswordLength.
		RespondWithError(c, http.StatusBadRequest,
			fmt.Sprintf("Password must be at most %d characters", maxPasswordLength))
		return
	}

	token, err := data_models.ConsumeUserToken(db, data_models.PurposePasswordReset, input.Token)
	if err != nil {
		RespondWithError(c, http.StatusBadRequest, "That link is invalid or has expired. Request a new one.")
		return
	}

	var user data_models.User
	if err := db.Where("id = ?", token.UserID).First(&user).Error; err != nil {
		// The token was valid but its user is gone. The token is spent either
		// way, which is correct — it must not become reusable because the
		// lookup failed.
		log.Printf("ERROR: reset token %d references missing user %d: %v", token.ID, token.UserID, err)
		RespondWithError(c, http.StatusBadRequest, "That link is invalid or has expired. Request a new one.")
		return
	}

	if err := user.HashPassword(input.Password); err != nil {
		log.Printf("ERROR: hashing reset password for user %d: %v", user.ID, err)
		RespondWithError(c, http.StatusInternalServerError, "Could not reset the password")
		return
	}
	if err := db.Model(&user).Update("password", user.Password).Error; err != nil {
		log.Printf("ERROR: storing reset password for user %d: %v", user.ID, err)
		RespondWithError(c, http.StatusInternalServerError, "Could not reset the password")
		return
	}

	// Revoke every session issued before now (#5224). Without this a reset
	// performed BECAUSE someone else has the account does not evict them: the
	// JWT is stateless with a 24h expiry, so their session outlives the
	// recovery by up to a day — the whole window the flow exists to close.
	if err := data_models.RevokeSessions(db, user.ID); err != nil {
		// The password is already changed and the token already spent. Refusing
		// now would leave the caller unable to retry with a link that no longer
		// works, so this reports rather than fails — loudly, because a reset
		// that did not revoke is exactly the case worth investigating.
		log.Printf("ERROR: password was reset for user %d but revoking its sessions failed: %v", user.ID, err)
	}

	// Release the login rate-limit bucket. LoginHandler already does this on a
	// successful login for the same reason: someone who has just recovered
	// their account is exactly the person whose earlier failed attempts filled
	// the bucket, and leaving them locked out at that moment is when they give
	// up.
	middleware.ClearLoginBucketForUsername(user.Username)

	// A reset deliberately does NOT verify the address.
	//
	// Recovery proves control of the mailbox just as verification does, so it
	// is tempting to fold them together. Doing so would make recovery a way
	// around the sign-up gate: request a reset on an account whose address was
	// never confirmed and it becomes usable. The two flows answer different
	// questions and neither may stand in for the other.

	log.Printf("Password reset completed for user %d", user.ID)
	RespondWithSuccess(c, gin.H{}, "Your password has been changed. You can sign in now.")
}
