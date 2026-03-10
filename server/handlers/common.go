package handlers

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Standard response structures for consistent API responses
type ErrorResponse struct {
	Error string `json:"error"`
}

type SuccessResponse struct {
	Status  string      `json:"status"`
	Data    interface{} `json:"data,omitempty"`
	Message string      `json:"message,omitempty"`
}

// Helper functions for consistent error handling
func RespondWithError(c *gin.Context, code int, message string) {
	log.Printf("Error response: %d - %s", code, message)
	c.JSON(code, ErrorResponse{Error: message})
}

func RespondWithSuccess(c *gin.Context, data interface{}, message string) {
	c.JSON(http.StatusOK, SuccessResponse{
		Status:  "success",
		Data:    data,
		Message: message,
	})
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
