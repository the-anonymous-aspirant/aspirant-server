package handlers

import (
	"log"
	"net/http"

	"aspirant-online/server/data_models"
	"aspirant-online/server/middleware"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	"golang.org/x/crypto/bcrypt"
)

// dummyBcryptHash is a fixed bcrypt hash used to keep the failed-
// login codepath's wall-clock time close to the successful path.
// Without this, an unknown-username failure short-circuits before
// bcrypt runs and leaks account existence via a timing oracle
// (CWE-204 / CWE-208).
//
// Value is bcrypt("aspirant-timing-oracle-dummy", DefaultCost). Any
// bytes work — nothing here is a secret; the hash's role is to make
// bcrypt.CompareHashAndPassword do a cost=10 pass. Regenerated any
// time bcrypt.DefaultCost changes.
var dummyBcryptHash = []byte("$2a$10$aE8CuPL9uNk7api5ULC2eeI46UDnCZb7tqFnXsrwQaG0gEDhSjkOe")

// LoginHandler handles user login
func LoginHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	var input struct {
		UserName string `json:"username" binding:"required"`
		Password string `json:"password" binding:"required"`
	}

	if err := c.ShouldBindJSON(&input); err != nil {
		log.Printf("Invalid login input: %v", err)
		RespondWithError(c, http.StatusBadRequest, "Invalid login credentials")
		return
	}

	var user data_models.User
	userFound := true
	if err := db.Preload("Role").Where("username= ?", input.UserName).First(&user).Error; err != nil {
		userFound = false
		log.Printf("Failed login attempt for username: %s", input.UserName)
	}

	// Constant-time hardening: on an unknown username, run bcrypt
	// against a dummy hash so the failure path takes as long as a
	// real password check. Closes the CWE-204 timing oracle called
	// out in security-finding #1380.
	if !userFound {
		_ = bcrypt.CompareHashAndPassword(dummyBcryptHash, []byte(input.Password))
		RespondWithError(c, http.StatusUnauthorized, "Invalid login credentials")
		return
	}

	if err := user.CheckPassword(input.Password); err != nil {
		log.Printf("Invalid password for username: %s", input.UserName)
		RespondWithError(c, http.StatusUnauthorized, "Invalid login credentials")
		return
	}

	// An unverified account exists but cannot authenticate (#5113-C1). This
	// check is deliberately placed AFTER CheckPassword rather than before it,
	// and returns the identical response: reaching it early, or answering
	// differently, would turn "this username exists and you got its password
	// right" into an observable outcome — reintroducing on the verification
	// axis exactly the oracle the dummy-bcrypt hash above closes on the
	// existence axis (CWE-204, security-finding #1380).
	if !user.IsEmailVerified() {
		log.Printf("Login refused for unverified account: %s", user.Username)
		RespondWithError(c, http.StatusUnauthorized, "Invalid login credentials")
		return
	}

	// Generate JWT token
	token, err := middleware.GenerateToken(user.ID, user.Role.RoleName)
	if err != nil {
		log.Printf("Failed to generate token: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Authentication error")
		return
	}

	// Successful login — release the per-username rate-limit bucket
	// so a user who mistyped their password N-1 times before finally
	// getting it right doesn't stay locked out.
	middleware.ClearLoginBucketForUsername(user.Username)

	log.Printf("Successful login for user: %s with role: %s", user.Username, user.Role.RoleName)

	// Deliver the JWT solely as an HttpOnly Secure SameSite=Strict cookie.
	// Full-page browser navigations (e.g. to /browser-flows/) do not carry
	// Authorization headers or localStorage, so the nginx auth_request gate
	// needs the cookie to authenticate the request, and HttpOnly keeps the
	// token out of reach of XSS. The token is deliberately NOT echoed in the
	// JSON body: the client is cookie-only (aspirant-client #2564) and a body
	// copy is a dead credential path no consumer reads (security-finding
	// #3095). auth.go still accepts an Authorization header for any non-browser
	// caller that already holds a token — that path is unchanged.
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie("auth_token", token, 86400, "/", "", true, true)

	c.Set("user_name", user.Username)
	c.Set("user_id", user.ID)
	c.Set("role", user.Role.RoleName)

	RespondWithSuccess(c, gin.H{
		"username": user.Username,
		"role":     user.Role.RoleName,
	}, "Login successful")
}

// LogoutHandler expires the auth_token cookie issued by LoginHandler.
//
// This has to happen server-side: auth_token is HttpOnly, and a
// document.cookie write from the SPA cannot overwrite or delete an
// HttpOnly cookie of the same name. The client's attempt to do so was a
// silent no-op — JavaScript cannot even read the cookie, so it had no way
// to notice the failure — which left a valid Admin credential in the
// browser for the full 24h token lifetime after the user clicked logout,
// while the UI reported a logged-out state (system_3 #2589).
//
// The cookie is the credential both enforcement points accept: the
// /api/ auth middleware falls back to it when no Authorization header is
// present (middleware/auth.go), and the nginx auth_request gate on
// /browser-flows, /admin/penpot/ and /admin/histoire/ authenticates on it
// alone. Not clearing it therefore leaves every admin surface reachable.
//
// The expiry MUST repeat the attributes the cookie was set with — same
// path, domain, Secure and SameSite. A Set-Cookie whose attributes do not
// match the original does not identify the same cookie, so the clear
// silently fails to remove it: the same class of bug one layer down.
//
// Deliberately unauthenticated. Logging out has to work when the token is
// already expired, malformed, or absent; requiring a valid token to log
// out would strand exactly the sessions most in need of clearing. The
// route reveals nothing and is idempotent.
//
// Scope: this ends the browser session. It does not revoke the JWT, which
// stays valid until its own exp (24h) — a token already exfiltrated is
// unaffected. Revocation needs a denylist or short-lived tokens plus
// refresh; see system_3 #2589.
func LogoutHandler(c *gin.Context) {
	// Ending the session also ends Constellations room presence: leave any
	// active game so the one-game-at-a-time lock does not strand the user out
	// of create/join on their next login (#4778). Read the identity from the
	// still-valid cookie BEFORE clearing it; ParseTokenIfPresent never aborts,
	// so an anonymous or already-expired caller is simply skipped and the
	// public logout contract is unchanged. Best-effort: a failure here is
	// logged, never surfaced — clearing the cookie must always succeed.
	if userID, _, ok := middleware.ParseTokenIfPresent(c); ok {
		db := c.MustGet("db").(*gorm.DB)
		if err := data_models.LeaveAllActiveRooms(db, userID); err != nil {
			log.Printf("Constellations: leave-all on logout for user %d: %v", userID, err)
		}
	}

	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie("auth_token", "", -1, "/", "", true, true)

	RespondWithSuccess(c, gin.H{}, "Logout successful")
}
