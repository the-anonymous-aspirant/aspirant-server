package handlers

import (
	"log"
	"net/http"
	"time"

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

// adminRoleName is the role whose claim maps to TierAdmin (common.go tierOf).
const adminRoleName = "Admin"

// cappedAdminRoleName is what /login puts in an Admin account's claim while
// the Cloudflare Access gate is on (system_3 #8798): the Admin tier is issued
// only by AdminLoginHandler, so a stolen password alone reaches no admin route.
const cappedAdminRoleName = "Member"

// LoginHandler handles user login. While the Access gate is on (enforced or
// misconfigured), an Admin account signs in here as Member; AdminLoginHandler
// is the only way to the Admin tier.
func LoginHandler(c *gin.Context) {
	user, ok := authenticatePassword(c)
	if !ok {
		return
	}
	role := user.Role.RoleName
	if _, mode := middleware.LoadAccessConfig(); role == adminRoleName && mode != middleware.AccessOff {
		role = cappedAdminRoleName
	}
	issueSession(c, user, role)
}

// AdminLoginHandler is the admin sign-in, POST /login/admin, served to the
// browser as /admin/sign-in inside the /admin Access application (system_3
// #8798). With the gate off it is LoginHandler for Admin accounts. With it on,
// the request must first prove it passed Cloudflare Access (a valid
// Cf-Access-Jwt-Assertion) or came through the loopback break-glass listener,
// and that is checked before the password, so guessing the admin password
// needs the PIN first. Misconfigured fails closed: break-glass only.
func AdminLoginHandler(c *gin.Context) {
	cfg, mode := middleware.LoadAccessConfig()
	breakGlass := middleware.BreakGlassValid(c.GetHeader(middleware.BreakGlassHeader))
	if breakGlass {
		// Every use leaves a trace, whether or not the password then
		// matches; the header's value is never logged (security c57514).
		log.Printf("Admin sign-in: break-glass header accepted on %s (mode %s)", c.FullPath(), mode)
	}
	switch mode {
	case middleware.AccessEnforced:
		if !breakGlass {
			if err := middleware.VerifyAccessAssertion(c.GetHeader(middleware.AccessAssertionHeader), cfg); err != nil {
				log.Printf("Admin sign-in refused: Access assertion invalid: %v", err)
				RespondWithError(c, http.StatusForbidden, "Admin sign-in requires Cloudflare Access")
				return
			}
		}
	case middleware.AccessMisconfigured:
		log.Printf("Admin sign-in: %s, %s and %s must be set together or not at all; only break-glass is accepted",
			middleware.AccessTeamDomainEnv, middleware.AccessAUDEnv, middleware.AccessAdminEmailEnv)
		if !breakGlass {
			RespondWithError(c, http.StatusForbidden, "Admin sign-in requires Cloudflare Access")
			return
		}
	}
	user, ok := authenticatePassword(c)
	if !ok {
		return
	}
	if user.Role.RoleName != adminRoleName {
		log.Printf("Admin sign-in refused for non-admin account: %s", user.Username)
		RespondWithError(c, http.StatusForbidden, "Admin sign-in is for admin accounts")
		return
	}
	if breakGlass {
		log.Printf("Admin sign-in through break-glass for user: %s", user.Username)
	}
	issueSession(c, user, adminRoleName)
}

// AdminGateModeSetting remembers the Admin-tier gate mode the server last ran
// under. It is state, not configuration (the mode comes from the environment),
// kept in site_settings because that is the server's one keyed store.
const AdminGateModeSetting = "admin_second_factor_mode"

// ReconcileAdminGateMode runs at startup. When the gate's mode differs from the
// one the server last ran under, it bumps the session epoch of every Admin
// account, so no Admin cookie issued under the other mode survives the change,
// in either direction (security c57514 on system_3 #8798). The first start
// with this code bumps too, unless the mode is off. It logs the mode either
// way, and returns it for the caller's log line.
func ReconcileAdminGateMode(db *gorm.DB) (middleware.AccessMode, error) {
	_, mode := middleware.LoadAccessConfig()
	var last data_models.SiteSetting
	err := db.Where("key = ?", AdminGateModeSetting).First(&last).Error
	switch {
	case gorm.IsRecordNotFoundError(err):
		if mode == middleware.AccessOff {
			return mode, db.Create(&data_models.SiteSetting{Key: AdminGateModeSetting, Value: mode.String(), UpdatedAt: time.Now()}).Error
		}
	case err != nil:
		return mode, err
	case last.Value == mode.String():
		return mode, nil
	}
	tx := db.Begin()
	bumped := tx.Exec(
		"UPDATE users SET session_epoch = session_epoch + 1 WHERE role_id IN (SELECT id FROM roles WHERE role_name = ?)",
		adminRoleName,
	)
	if bumped.Error != nil {
		tx.Rollback()
		return mode, bumped.Error
	}
	// Create on the first write: Save on a missing string-keyed row is an
	// UPDATE that matches nothing and reports success (site_setting.go).
	row := data_models.SiteSetting{Key: AdminGateModeSetting, Value: mode.String(), UpdatedAt: time.Now()}
	write := tx.Save
	if gorm.IsRecordNotFoundError(err) {
		write = tx.Create
	}
	if err := write(&row).Error; err != nil {
		tx.Rollback()
		return mode, err
	}
	if err := tx.Commit().Error; err != nil {
		return mode, err
	}
	log.Printf("Admin gate mode changed from %q to %q: revoked the sessions of %d admin account(s)",
		last.Value, mode, bumped.RowsAffected)
	return mode, nil
}

// authenticatePassword binds the credentials and checks them. On failure it has
// already written the response, which is identical for every failure so that
// neither the username's existence nor its verification state is observable.
func authenticatePassword(c *gin.Context) (data_models.User, bool) {
	db := c.MustGet("db").(*gorm.DB)

	var input struct {
		UserName string `json:"username" binding:"required"`
		Password string `json:"password" binding:"required"`
	}

	if err := c.ShouldBindJSON(&input); err != nil {
		log.Printf("Invalid login input: %v", err)
		RespondWithError(c, http.StatusBadRequest, "Invalid login credentials")
		return data_models.User{}, false
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
		return data_models.User{}, false
	}

	if err := user.CheckPassword(input.Password); err != nil {
		log.Printf("Invalid password for username: %s", input.UserName)
		RespondWithError(c, http.StatusUnauthorized, "Invalid login credentials")
		return data_models.User{}, false
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
		return data_models.User{}, false
	}

	return user, true
}

// issueSession mints the session token with the given role claim and sets it
// as the auth cookie. role is the account's role, or the capped one LoginHandler
// chose; the token carries the role, not the tier (common.go tierOf).
func issueSession(c *gin.Context, user data_models.User, role string) {
	// Generate JWT token
	// Stamped with the user's current session epoch, which is already loaded on
	// the row above — no extra read. A later RevokeSessions bumps that epoch and
	// this token stops being current everywhere at once (#5224/#5275).
	token, err := middleware.GenerateTokenAtEpoch(user.ID, role, user.SessionEpoch)
	if err != nil {
		log.Printf("Failed to generate token: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Authentication error")
		return
	}

	// Successful login — release the per-username rate-limit bucket
	// so a user who mistyped their password N-1 times before finally
	// getting it right doesn't stay locked out.
	middleware.ClearLoginBucketForUsername(user.Username)

	log.Printf("Successful login for user: %s with role: %s", user.Username, role)

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
	c.Set("role", role)

	RespondWithSuccess(c, gin.H{
		"username": user.Username,
		"role":     role,
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
	// ParseTokenIgnoringRevocation, not ParseTokenIfPresent: logging out has to
	// work when the session has already been revoked (#5224), which is exactly
	// when someone most wants their cookie cleared. Refusing to identify them
	// here would strand the room lock held by a session nobody can use.
	if userID, _, ok := middleware.ParseTokenIgnoringRevocation(c); ok {
		db := c.MustGet("db").(*gorm.DB)
		if err := data_models.LeaveAllActiveRooms(db, userID); err != nil {
			log.Printf("Constellations: leave-all on logout for user %d: %v", userID, err)
		}
	}

	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie("auth_token", "", -1, "/", "", true, true)

	RespondWithSuccess(c, gin.H{}, "Logout successful")
}
