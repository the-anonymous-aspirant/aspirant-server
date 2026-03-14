package handlers

import (
	"log"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
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
