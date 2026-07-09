package middleware

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

const (
	jwtSecretEnv    = "JWT_SECRET"
	jwtMinSecretLen = 32
)

// jwtPlaceholders enumerates values that must never reach production. If
// any of these ends up in the environment LoadJWTSecret refuses to install
// it — see system_3 #1374.
var jwtPlaceholders = map[string]struct{}{
	"change-me":                 {},
	"aspirant_secret":           {},
	"aspirant_secret_CHANGE_ME": {},
}

var (
	jwtSecret []byte

	// jwtParser pins HS256 at the parser layer so alg:none and
	// alg-confusion attacks are rejected before the keyfunc runs. The
	// keyfunc adds a second defence-in-depth SigningMethodHMAC assertion.
	jwtParser = jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuedAt(),
	)
)

// LoadJWTSecret reads JWT_SECRET from the environment and installs it
// into the package-level jwtSecret. It refuses empty values, any known
// placeholder, and values shorter than jwtMinSecretLen bytes. Callers
// (main.go, TestMain) must invoke this before AuthMiddleware or
// GenerateToken are used.
func LoadJWTSecret() error {
	raw := os.Getenv(jwtSecretEnv)
	if raw == "" {
		return errors.New("JWT_SECRET must be set (generate with: openssl rand -base64 32)")
	}
	if _, isPlaceholder := jwtPlaceholders[raw]; isPlaceholder {
		return fmt.Errorf("JWT_SECRET is a known placeholder (%q); rotate before boot (openssl rand -base64 32)", raw)
	}
	if len(raw) < jwtMinSecretLen {
		return fmt.Errorf("JWT_SECRET is too short (%d bytes; minimum %d)", len(raw), jwtMinSecretLen)
	}
	jwtSecret = []byte(raw)
	return nil
}

func GenerateToken(userID uint, role string) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"user_id": userID,
		"role":    role,
		"iat":     now.Unix(),
		"exp":     now.Add(24 * time.Hour).Unix(),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString(jwtSecret)
	if err != nil {
		return "", err
	}
	return tokenString, nil
}

// parseAndValidate runs a token string through jwtParser and returns
// the claims map on success. Rejects any non-HMAC method and any token
// whose Valid flag is false (jwt/v5 covers exp/nbf/iat validation via
// the parser options).
func parseAndValidate(tokenString string) (jwt.MapClaims, error) {
	token, err := jwtParser.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return jwtSecret, nil
	})
	if err != nil {
		return nil, err
	}
	if !token.Valid {
		return nil, errors.New("token invalid")
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, errors.New("invalid claims shape")
	}
	return claims, nil
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

		claims, err := parseAndValidate(tokenString)
		if err != nil {
			log.Printf("Invalid token: %v", err)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
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

// ParseTokenIfPresent decodes the same Authorization/auth_token cookie
// AuthMiddleware uses but never aborts the request — it returns
// ok=false on any missing/invalid/expired token so a public-shaped
// handler can grant an authenticated caller extra capability without
// changing the endpoint's status-code contract for unauthenticated
// callers. Used by /fetch-object so admins previewing uploaded
// assets bypass the published-ETag whitelist (security-finding
// system_3 #1381) without breaking the pre-JWT SPA shell.
func ParseTokenIfPresent(c *gin.Context) (userID uint, role string, ok bool) {
	tokenString := ""
	if authHeader := c.GetHeader("Authorization"); authHeader != "" {
		tokenString = strings.TrimPrefix(authHeader, "Bearer ")
	} else if cookie, err := c.Cookie("auth_token"); err == nil && cookie != "" {
		tokenString = cookie
	}
	if tokenString == "" {
		return 0, "", false
	}

	claims, err := parseAndValidate(tokenString)
	if err != nil {
		return 0, "", false
	}
	r, hasRole := claims["role"].(string)
	uidFloat, hasUID := claims["user_id"].(float64)
	if !hasRole || !hasUID {
		return 0, "", false
	}
	return uint(uidFloat), r, true
}
