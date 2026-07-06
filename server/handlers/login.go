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

	// Set the JWT as an HttpOnly Secure SameSite=Strict cookie in addition to
	// returning it in the JSON body. Full-page browser navigations (e.g. to
	// /browser-flows/) do not carry Authorization headers or localStorage, so
	// the nginx auth_request gate needs the cookie to authenticate the request.
	// HttpOnly also protects the token from XSS exfiltration.
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie("auth_token", token, 86400, "/", "", true, true)

	c.Set("user_name", user.Username)
	c.Set("user_id", user.ID)
	c.Set("role", user.Role.RoleName)
	c.Set("token", token)

	RespondWithSuccess(c, gin.H{
		"token":    token,
		"username": user.Username,
		"role":     user.Role.RoleName,
	}, "Login successful")
}
