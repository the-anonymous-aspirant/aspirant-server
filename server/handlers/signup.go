package handlers

import (
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"os"
	"strings"
	"time"

	"aspirant-online/server/data_models"
	"aspirant-online/server/email"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Public self-service sign-up and email verification (system_3 epic #5113,
// subtask #5220).
//
// Separate from login.go on purpose. That file is the authentication path and
// carries three security findings' worth of hardening; sign-up is a different
// concern and a reader of either should not have to walk the other.
//
// The shape of this file is dominated by one requirement: an unauthenticated
// caller must not be able to learn whether an account exists. Every branch below
// that could reveal it returns the same thing, and the tests assert that
// byte-for-byte rather than by inspection.

const (
	// signupRoleName is the tier a self-service account lands in. Public is the
	// absence of a role; Viewer is the lowest authenticated tier (#5113-A2).
	//
	// It is resolved by name from the roles table and is deliberately NOT read
	// from the request. BootstrapUserHandler does read a role from the body,
	// which is correct there — it is the first-admin path and refuses to run
	// once any user exists — but a public endpoint doing the same would be
	// privilege escalation at the front door.
	signupRoleName = "Viewer"

	// minPasswordLength follows NIST SP 800-63B: length is the control that
	// matters and composition rules mostly produce predictable substitutions.
	minPasswordLength = 10

	// maxPasswordLength is bcrypt's limit, not a policy choice.
	//
	// bcrypt silently ignores everything past 72 bytes. Accepting a longer
	// password would mean a user who chose a 100-character passphrase is
	// authenticated by its first 72 bytes while believing otherwise — and,
	// worse, would still be authenticated after changing only the tail. golang.org/x/crypto
	// returns an error above this length rather than truncating, so without
	// this check sign-up would fail with an opaque internal error instead.
	maxPasswordLength = 72

	// publicBaseURLEnv overrides the origin used to build verification and
	// recovery links.
	publicBaseURLEnv = "PUBLIC_BASE_URL"
	// defaultPublicBaseURL matches server.defaultCORSOrigin — the frontend is
	// served same-origin behind nginx.
	defaultPublicBaseURL = "https://the-aspirant.com"
)

// signupResponseMessage is returned for every outcome of POST /signup that is
// not a malformed request: a created account, a taken username, and a taken
// email all produce exactly this.
//
// It is a constant so that the three call sites cannot drift apart. A
// difference of one word between the "created" and "already exists" replies
// would be an account-existence oracle, and this codebase has had one before —
// LoginHandler's fixed dummy bcrypt hash exists because the login path leaked
// account existence through timing (CWE-204, security-finding #1380).
const signupResponseMessage = "If that username and address are available, a verification link has been sent. Check your inbox."

type signupInput struct {
	Username string `json:"username" binding:"required"`
	Email    string `json:"email" binding:"required"`
	Password string `json:"password" binding:"required"`
}

type verifyEmailInput struct {
	Token string `json:"token" binding:"required"`
}

// publicBaseURL is the origin links in outbound mail point at.
func publicBaseURL() string {
	if v := strings.TrimSpace(os.Getenv(publicBaseURLEnv)); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultPublicBaseURL
}

// mailerFrom returns the request's mail sender, or nil when none is wired.
//
// nil is treated as a send failure rather than a panic. A missing sender is a
// wiring bug in main.go, and taking down the process on a sign-up request is a
// worse response to it than logging loudly and returning the same reply every
// other path returns.
func mailerFrom(c *gin.Context) email.Sender {
	v, ok := c.Get("mailer")
	if !ok {
		return nil
	}
	sender, ok := v.(email.Sender)
	if !ok {
		return nil
	}
	return sender
}

// sendOrLog delivers a message, logging any failure.
//
// The error is deliberately swallowed at the call sites. Whether the mail left
// the building is not something the caller of a public sign-up endpoint gets to
// learn: "we could not send to that address" and "we did send to that address"
// are different answers to the question of whether the address is registered.
func sendOrLog(c *gin.Context, to, subject, body, context string) {
	sender := mailerFrom(c)
	if sender == nil {
		log.Printf("ERROR: no mail sender in context; %s mail to %s was not sent", context, to)
		return
	}
	if err := sender.Send(to, subject, body); err != nil {
		log.Printf("ERROR: sending %s mail: %v", context, err)
	}
}

// SignupHandler creates an unverified Viewer account and mails a verification
// link.
//
// Public and unauthenticated. Rate limiting belongs to the middleware (#5222),
// not here.
func SignupHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	var input signupInput
	if err := c.ShouldBindJSON(&input); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Username, email and password are required")
		return
	}

	username := strings.TrimSpace(input.Username)
	address := strings.TrimSpace(input.Email)

	// Shape checks come first and DO answer distinctly — they describe the
	// request, not the database, so they leak nothing about who has an account.
	if username == "" {
		RespondWithError(c, http.StatusBadRequest, "Username, email and password are required")
		return
	}
	if _, err := mail.ParseAddress(address); err != nil {
		RespondWithError(c, http.StatusBadRequest, "That does not look like an email address")
		return
	}
	if len(input.Password) < minPasswordLength {
		RespondWithError(c, http.StatusBadRequest,
			fmt.Sprintf("Password must be at least %d characters", minPasswordLength))
		return
	}
	if len(input.Password) > maxPasswordLength {
		RespondWithError(c, http.StatusBadRequest,
			fmt.Sprintf("Password must be at most %d characters", maxPasswordLength))
		return
	}

	// From here on every exit is signupAccepted: the answers below depend on
	// what is in the users table, and that is exactly what must not be visible.
	var existing data_models.User
	err := db.Where("username = ? OR email = ?", username, address).First(&existing).Error
	switch {
	case err == nil:
		// Taken. Tell the person who actually owns the address that someone
		// tried, and tell the requester nothing.
		//
		// The notice goes to the stored address, never to the one in the
		// request — they are equal in the email-collision case, but in the
		// username-collision case the requester supplied a different address
		// and mailing them would confirm that the username is taken.
		onExistingAccountSignupAttempt(c, existing, username)
		signupAccepted(c)
		return
	case !gorm.IsRecordNotFoundError(err):
		log.Printf("ERROR: signup existence check: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Could not process the request")
		return
	}

	role, err := data_models.GetRoleByName(db, signupRoleName)
	if err != nil {
		log.Printf("ERROR: signup could not resolve the %s role: %v", signupRoleName, err)
		RespondWithError(c, http.StatusInternalServerError, "Could not process the request")
		return
	}

	user := data_models.User{
		Username: username,
		Email:    address,
		RoleID:   role.ID,
		// EmailVerifiedAt stays nil: the account exists but cannot log in until
		// the link is followed. That is the whole point of the flow, and it is
		// also the bot filter — an address that cannot receive mail cannot
		// finish creating an account.
	}
	if err := user.HashPassword(input.Password); err != nil {
		log.Printf("ERROR: signup hashing password: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Could not process the request")
		return
	}
	if err := user.CreateUser(db); err != nil {
		// A unique-constraint violation here means someone took the name
		// between the check above and this insert. The database is the
		// authority; answer as though it were taken, which it is.
		log.Printf("signup: create failed for %q (likely a race on a unique index): %v", username, err)
		signupAccepted(c)
		return
	}

	token, err := data_models.IssueUserToken(db, user.ID, data_models.PurposeVerifyEmail)
	if err != nil {
		// The account exists but has no link. Requesting another verification
		// mail is the recovery, and the account cannot log in meanwhile, so
		// nothing is exposed by leaving it.
		log.Printf("ERROR: signup issuing verification token for user %d: %v", user.ID, err)
		signupAccepted(c)
		return
	}

	sendOrLog(c, address, "Confirm your address", verificationMailBody(username, token), "verification")
	signupAccepted(c)
}

// signupAccepted writes the single response every database-dependent path of
// SignupHandler returns.
func signupAccepted(c *gin.Context) {
	RespondWithSuccess(c, gin.H{}, signupResponseMessage)
}

// onExistingAccountSignupAttempt notifies the owner of an account that someone
// tried to sign up over it.
//
// This is not only courtesy. Without it the endpoint would be a silent oracle
// for the person probing it and invisible to the person being probed; with it,
// the only party who learns anything is the one who already has the account.
func onExistingAccountSignupAttempt(c *gin.Context, existing data_models.User, attemptedUsername string) {
	body := fmt.Sprintf(`Hello,

Someone just tried to create an account on %s using details that match your
existing account (the username %q or this email address).

If that was you, you already have an account — sign in as usual, or use the
password-recovery link on the sign-in page if you have forgotten your password.

If it was not you, there is nothing to do. No new account was created and
nothing about your account has changed.

— %s`, publicBaseURL(), attemptedUsername, publicBaseURL())

	sendOrLog(c, existing.Email, "Someone tried to sign up with your details", body, "duplicate-signup notice")
}

// verificationMailBody renders the verification message.
//
// The link points at a frontend page rather than at this API, and that page is
// expected to POST the token. A GET endpoint would be easier and is wrong here:
// mail scanners and link-preview fetchers follow links in messages, and since
// the token is single-use, one prefetch by a security appliance would consume
// it and leave the user with a link that reports itself invalid. The frontend
// page is not part of this subtask and is filed separately.
func verificationMailBody(username, token string) string {
	return fmt.Sprintf(`Hello %s,

Confirm this address to finish creating your account:

%s/verify-email?token=%s

The link is good for 24 hours and can be used once. If you did not sign up,
ignore this message — the account cannot be used until the link is followed.

— %s`, username, publicBaseURL(), token, publicBaseURL())
}

// VerifyEmailHandler consumes a verification token and marks the address
// confirmed.
//
// Idempotent from the user's side in the sense that matters: clicking twice
// produces one visible outcome and the second click changes nothing. It is not
// idempotent underneath — the second consume fails, which is the single-use
// guarantee doing its job.
func VerifyEmailHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	var input verifyEmailInput
	if err := c.ShouldBindJSON(&input); err != nil {
		RespondWithError(c, http.StatusBadRequest, "A verification token is required")
		return
	}

	token, err := data_models.ConsumeUserToken(db, data_models.PurposeVerifyEmail, input.Token)
	if err != nil {
		// One message for every failure — expired, already used, never existed,
		// or minted for password recovery. See ErrTokenInvalid.
		RespondWithError(c, http.StatusBadRequest, "That link is invalid or has expired. Request a new one.")
		return
	}

	now := time.Now()
	if err := db.Model(&data_models.User{}).
		Where("id = ?", token.UserID).
		Update("email_verified_at", now).Error; err != nil {
		log.Printf("ERROR: marking user %d verified: %v", token.UserID, err)
		RespondWithError(c, http.StatusInternalServerError, "Could not complete verification")
		return
	}

	log.Printf("Email verified for user %d", token.UserID)
	RespondWithSuccess(c, gin.H{}, "Your address is confirmed. You can sign in now.")
}
