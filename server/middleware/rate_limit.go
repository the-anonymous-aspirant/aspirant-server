package middleware

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// Rate-limit thresholds for the /login endpoint. Aspirant is
// single-node so an in-memory bucket store is sufficient; when the
// service scales horizontally these numbers move behind a Redis-backed
// limiter without changing the handler contract.
const (
	// PerIPLimit / PerIPWindow guard against a single attacker
	// credential-stuffing many usernames.
	PerIPLimit  = 20
	PerIPWindow = 5 * time.Minute

	// PerUsernameLimit / PerUsernameWindow guard against a distributed
	// attacker (many IPs, one target account).
	PerUsernameLimit  = 10
	PerUsernameWindow = 5 * time.Minute
)

// bucket tracks a rolling window of failed login timestamps. Trimmed
// lazily on each Hit / Count call to avoid a background goroutine.
type bucket struct {
	hits []time.Time
}

func (b *bucket) trim(now time.Time, window time.Duration) {
	cutoff := now.Add(-window)
	i := 0
	for ; i < len(b.hits); i++ {
		if b.hits[i].After(cutoff) {
			break
		}
	}
	if i > 0 {
		b.hits = b.hits[i:]
	}
}

// loginRateLimiter is the process-wide singleton state used by
// LoginRateLimit. Exposed only via constructor + accessor methods so
// tests can reset state between cases.
type loginRateLimiter struct {
	mu       sync.Mutex
	byIP     map[string]*bucket
	byUser   map[string]*bucket
	nowFn    func() time.Time
	ipLimit  int
	ipWindow time.Duration
	unLimit  int
	unWindow time.Duration
}

func newLoginRateLimiter() *loginRateLimiter {
	return &loginRateLimiter{
		byIP:     make(map[string]*bucket),
		byUser:   make(map[string]*bucket),
		nowFn:    time.Now,
		ipLimit:  PerIPLimit,
		ipWindow: PerIPWindow,
		unLimit:  PerUsernameLimit,
		unWindow: PerUsernameWindow,
	}
}

var defaultLoginLimiter = newLoginRateLimiter()

// ResetLoginRateLimiter clears the process-wide login rate-limit
// state. Used by tests; production paths never call it.
func ResetLoginRateLimiter() {
	defaultLoginLimiter = newLoginRateLimiter()
}

// ClearLoginBucketForUsername drops the failure counter for a
// username. Called by LoginHandler on a successful login so a user
// who mistyped a password N-1 times isn't locked out after
// self-recovery.
func ClearLoginBucketForUsername(username string) {
	if username == "" {
		return
	}
	defaultLoginLimiter.mu.Lock()
	delete(defaultLoginLimiter.byUser, strings.ToLower(username))
	defaultLoginLimiter.mu.Unlock()
}

// LoginRateLimit is a Gin middleware that rejects a POST /login
// request whose IP or target username has hit the failure threshold
// within the sliding window. The middleware peeks at (but does not
// consume) the JSON body to extract the username so the handler can
// still bind it. On a rate-limit hit it returns 429 with a
// generic "too many attempts" message (no distinction between IP-
// bucket and username-bucket exhaustion — the caller doesn't need to
// know which one tripped).
//
// The middleware counts an ATTEMPT on entry — an over-limit request
// short-circuits with 429 before the handler runs, so the login
// handler only sees requests that already passed the threshold. On
// a successful login the handler calls ClearLoginBucketForUsername
// to release the per-user hold; the per-IP bucket stays as-is so a
// single-attacker single-real-account harvest can't reset itself.
func LoginRateLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := c.ClientIP()
		username := peekJSONUsername(c)

		l := defaultLoginLimiter
		l.mu.Lock()
		now := l.nowFn()
		if l.hitAndCheck(&l.byIP, ip, now, l.ipLimit, l.ipWindow) {
			l.mu.Unlock()
			log.Printf("Rate limit: too many /login attempts from IP %s", ip)
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "Too many login attempts, please try again later"})
			c.Abort()
			return
		}
		if username != "" && l.hitAndCheck(&l.byUser, strings.ToLower(username), now, l.unLimit, l.unWindow) {
			l.mu.Unlock()
			log.Printf("Rate limit: too many /login attempts for username")
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "Too many login attempts, please try again later"})
			c.Abort()
			return
		}
		l.mu.Unlock()

		c.Next()
	}
}

// hitAndCheck records an attempt against the named bucket and returns
// true when the bucket is now over the threshold. Caller holds l.mu.
func (l *loginRateLimiter) hitAndCheck(store *map[string]*bucket, key string, now time.Time, limit int, window time.Duration) bool {
	b, ok := (*store)[key]
	if !ok {
		b = &bucket{}
		(*store)[key] = b
	}
	b.trim(now, window)
	b.hits = append(b.hits, now)
	return len(b.hits) > limit
}

// peekJSONUsername reads the JSON body, extracts the "username"
// field, and rewrites the body onto the request so the downstream
// handler's c.ShouldBindJSON still sees the payload. Returns "" on
// any parse failure — the handler will then return 400 and the
// per-username bucket is left untouched.
func peekJSONUsername(c *gin.Context) string {
	if c.Request == nil || c.Request.Body == nil {
		return ""
	}
	// Cap the read at 4 KiB — /login bodies are tiny, and a large
	// body from an attacker shouldn't burn memory here.
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 4096))
	if err != nil {
		return ""
	}
	// Restore the body so the handler's ShouldBindJSON sees it.
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	return extractUsername(body)
}

// extractUsername pulls a "username" string field from a small JSON
// object without allocating a full parse tree. Deliberately lenient
// — a malformed body returns "" and the handler's ShouldBindJSON
// will report the real 400 to the client.
func extractUsername(body []byte) string {
	// Naive but safe: search for the "username" key and take the
	// quoted string that follows. Handles the two casings the client
	// sends ("username" / "Username") and ignores extra whitespace.
	for _, key := range []string{`"username"`, `"Username"`} {
		i := strings.Index(string(body), key)
		if i < 0 {
			continue
		}
		rest := string(body)[i+len(key):]
		colon := strings.Index(rest, ":")
		if colon < 0 {
			continue
		}
		rest = strings.TrimLeft(rest[colon+1:], " \t\r\n")
		if len(rest) < 2 || rest[0] != '"' {
			continue
		}
		end := strings.IndexByte(rest[1:], '"')
		if end < 0 {
			continue
		}
		return rest[1 : 1+end]
	}
	return ""
}
