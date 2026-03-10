package middleware

import (
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/dgrijalva/jwt-go"
	"github.com/gin-gonic/gin"
)

var jwtSecret = []byte(getJWTSecret())

func getJWTSecret() string {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		log.Println("WARNING: JWT_SECRET not set, using insecure default")
		return "aspirant_secret_CHANGE_ME"
	}
	return secret
}

func GenerateToken(userID uint, role string) (string, error) {
	claims := jwt.MapClaims{
		"user_id": userID,
		"role":    role,
		"exp":     time.Now().Add(24 * time.Hour).Unix(), // Token expires in 24 hours
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString(jwtSecret)
	if err != nil {
		return "", err
	}

	log.Printf("Generated token: %s", tokenString)
	return tokenString, nil

}

// AuthMiddleware is a middleware function for the Gin framework that handles
// authentication by validating the JWT token provided in the "Authorization" header.
// If the token is missing, invalid, or the claims are not as expected, it responds
// with an HTTP 401 Unauthorized status and an appropriate error message.
// On successful validation, it sets the user ID and role in the context for further use.
//
// Claims are pieces of information asserted about a subject (typically, the user) and
// are encoded in the JWT token. Common claims include user ID, role, and other metadata.
func AuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Check Authorization header first, fall back to auth_token cookie
		// (cookies are needed for non-AJAX requests like iframes)
		tokenString := ""
		if authHeader := c.GetHeader("Authorization"); authHeader != "" {
			tokenString = strings.TrimPrefix(authHeader, "Bearer ")
		} else if cookie, err := c.Cookie("auth_token"); err == nil && cookie != "" {
			tokenString = cookie
		}

		if tokenString == "" {
			log.Println("Authorization required")
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authorization required"})
			c.Abort()
			return
		}

		log.Printf("Token string received: %s", tokenString)

		token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
			return jwtSecret, nil
		})

		if err != nil || !token.Valid {
			log.Printf("Invalid token: %v", err)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
			c.Abort()
			return
		}

		// Claims are key-value pairs that are encoded in the JWT token
		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			log.Println("Invalid token claims")
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token claims"})
			c.Abort()
			return
		}

		role, ok := claims["role"].(string)
		if !ok {
			log.Println("Invalid role in token claims")
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid role"})
			c.Abort()
			return
		}

		userID, ok := claims["user_id"].(float64)
		if !ok {
			log.Println("Invalid user_id in token claims")
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid user_id"})
			c.Abort()
			return
		}

		log.Printf("Token parsed successfully - role: %s, user_id: %d", role, uint(userID))

		c.Set("role", role)
		c.Set("user_id", uint(userID))
		c.Next()
	}
}
