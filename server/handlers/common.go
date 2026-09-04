package handlers

import (
	"log"
	"net/http"
	"strconv"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// PaginatedResponse wraps a list of items with pagination metadata
type PaginatedResponse struct {
	Items    interface{} `json:"items"`
	Total    int64       `json:"total"`
	Page     int         `json:"page"`
	PageSize int         `json:"page_size"`
}

// parsePagination extracts page and page_size query parameters with defaults and bounds
func parsePagination(c *gin.Context) (page int, pageSize int) {
	page = 1
	pageSize = 20

	if p := c.Query("page"); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil && parsed > 0 {
			page = parsed
		}
	}

	if ps := c.Query("page_size"); ps != "" {
		if parsed, err := strconv.Atoi(ps); err == nil && parsed > 0 {
			pageSize = parsed
		}
	}

	if pageSize > 100 {
		pageSize = 100
	}

	return page, pageSize
}

// Standard response structures for consistent API responses
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

type SuccessResponse struct {
	Status  string      `json:"status"`
	Data    interface{} `json:"data,omitempty"`
	Message string      `json:"message,omitempty"`
}

// httpStatusToErrorCode maps HTTP status codes to standard error codes
func httpStatusToErrorCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusConflict:
		return "conflict"
	case http.StatusBadGateway:
		return "bad_gateway"
	default:
		return "internal_error"
	}
}

// respondError writes a structured error response with the standard envelope format
func respondError(c *gin.Context, status int, code string, message string) {
	log.Printf("Error response: %d - %s - %s", status, code, message)
	c.JSON(status, ErrorResponse{
		Error: ErrorDetail{Code: code, Message: message},
	})
}

// RespondWithError writes a structured error response, deriving the error code from the HTTP status
func RespondWithError(c *gin.Context, status int, message string) {
	respondError(c, status, httpStatusToErrorCode(status), message)
}

func RespondWithSuccess(c *gin.Context, data interface{}, message string) {
	c.JSON(http.StatusOK, SuccessResponse{
		Status:  "success",
		Data:    data,
		Message: message,
	})
}

// AccessTier ranks the four access tiers of system_3 epic #5113. "public" is
// the ABSENCE of a role (unauthenticated) and has no constant here — a handler
// in a tier-gated group has already passed AuthMiddleware. Higher = more access.
type AccessTier int

const (
	TierBlocked AccessTier = iota // authenticated, no access (legacy: Deleted)
	TierViewer                    // + applications (legacy: User/Guest/Gamer)
	TierMember                    // + member area, e.g. file storage (legacy: Trusted)
	TierAdmin                     // everything
)

// tierOf maps a role-claim string to its access tier. It accepts BOTH the tier
// names and the legacy six-role names, so a JWT minted before the #5113-A2
// migration (24h expiry) keeps resolving to the correct tier until it expires.
func tierOf(role string) AccessTier {
	switch role {
	case "Admin":
		return TierAdmin
	case "Member", "Trusted":
		return TierMember
	case "Viewer", "User", "Guest", "Gamer":
		return TierViewer
	default: // "Blocked", "Deleted", unknown, empty
		return TierBlocked
	}
}

// RequireTier gates a route to a minimum access tier. Layer it after
// AuthMiddleware (which sets the "role" claim in context). It replaces the
// legacy ValidateRole("Trusted","Admin") / ValidateRole("Admin") allow-lists:
// a tier floor is monotonic, so a higher tier always clears a lower gate
// without every superior role being enumerated at the call site.
func RequireTier(min AccessTier) gin.HandlerFunc {
	return func(c *gin.Context) {
		role, exists := c.Get("role")
		if !exists {
			log.Println("ERROR: Role information not available in context")
			RespondWithError(c, http.StatusForbidden, "Role information not available")
			c.Abort()
			return
		}
		roleStr, ok := role.(string)
		if !ok {
			log.Printf("ERROR: Role is not a string, got type: %T", role)
			RespondWithError(c, http.StatusInternalServerError, "Invalid role format")
			c.Abort()
			return
		}
		if tierOf(roleStr) < min {
			log.Printf("ERROR: role '%s' (tier %d) below required tier %d", roleStr, tierOf(roleStr), min)
			RespondWithError(c, http.StatusForbidden, "Insufficient permissions")
			c.Abort()
			return
		}
		c.Next()
	}
}

// Helper middleware to validate user roles with enhanced logging
func ValidateRole(allowedRoles ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		log.Println("Validating user role...")

		// Check for role in context
		role, exists := c.Get("role")

		// Log all context keys for debugging
		contextKeys := make([]string, 0)
		for k := range c.Keys {
			contextKeys = append(contextKeys, k)
		}
		log.Printf("Context keys: %v", contextKeys)

		if !exists {
			log.Println("ERROR: Role information not available in context")
			RespondWithError(c, http.StatusForbidden, "Role information not available")
			c.Abort()
			return
		}

		// Type assertion and validation
		roleStr, ok := role.(string)
		if !ok {
			log.Printf("ERROR: Role is not a string, got type: %T", role)
			RespondWithError(c, http.StatusInternalServerError, "Invalid role format")
			c.Abort()
			return
		}

		log.Printf("User role found: '%s' - checking against allowed roles: %v", roleStr, allowedRoles)

		allowed := false
		for _, r := range allowedRoles {
			if roleStr == r {
				allowed = true
				break
			}
		}

		if !allowed {
			log.Printf("ERROR: User with role '%s' does not have permission for this operation", roleStr)
			RespondWithError(c, http.StatusForbidden, "Insufficient permissions")
			c.Abort()
			return
		}

		log.Printf("Role validation successful for user with role: %s", roleStr)
		c.Next()
	}
}

// ValidateUserOrAdmin gates a route to a single named account plus any Admin.
// Used for single-user Member apps (e.g. the Jobs feed, owned by vinoly per
// #4196/#4194): Admin passes by role; every other authenticated user must BE
// the named account. The auth context carries user_id + role but not the
// username, so this resolves user_id to a row and compares Username. A
// non-matching authenticated user gets 403 — the app is not theirs. Layer it
// after AuthMiddleware (needs user_id/role in context) and the db middleware.
func ValidateUserOrAdmin(username string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if role, ok := c.Get("role"); ok {
			if roleStr, ok := role.(string); ok && roleStr == "Admin" {
				c.Next()
				return
			}
		}

		uidVal, exists := c.Get("user_id")
		if !exists {
			RespondWithError(c, http.StatusForbidden, "User information not available")
			c.Abort()
			return
		}
		uid, ok := uidVal.(uint)
		if !ok {
			log.Printf("ERROR: user_id is not a uint, got type: %T", uidVal)
			RespondWithError(c, http.StatusInternalServerError, "Invalid user id format")
			c.Abort()
			return
		}

		db := c.MustGet("db").(*gorm.DB)
		var user data_models.User
		if err := db.Where("id = ?", uid).First(&user).Error; err != nil {
			log.Printf("ValidateUserOrAdmin: user_id %d not found: %v", uid, err)
			RespondWithError(c, http.StatusForbidden, "Insufficient permissions")
			c.Abort()
			return
		}
		if user.Username == username {
			c.Next()
			return
		}

		log.Printf("ValidateUserOrAdmin: user '%s' is not the owner '%s'", user.Username, username)
		RespondWithError(c, http.StatusForbidden, "Insufficient permissions")
		c.Abort()
	}
}
