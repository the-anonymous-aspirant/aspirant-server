package middleware

import (
	"bytes"
	"encoding/json"
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
		// Read and parse the body BEFORE taking the lock. This has always been
		// the right order here and PR #106 copied the helper into a caller that
		// got it wrong; see PublicAuthRateLimit.
		usernames := extractJSONStringFields(peekJSONBody(c), "username")

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
		// Every case-variant spelling is charged, because encoding/json will
		// bind one of them and this must not depend on guessing which. Before
		// #5240 the scan matched only "username"/"Username", so `{"USERNAME":
		// "victim"}` bound in the handler while the per-username bucket was
		// skipped entirely — the distributed-credential-stuffing defence was
		// bypassable by changing the case of a JSON key.
		overUser := false
		for _, username := range usernames {
			if username == "" {
				continue
			}
			if l.hitAndCheck(&l.byUser, strings.ToLower(username), now, l.unLimit, l.unWindow) {
				overUser = true
			}
		}
		if overUser {
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

// peekJSONBody reads the request body and rewrites it onto the request so a
// downstream handler's ShouldBindJSON still sees the payload. Returns nil on
// any read failure — callers then key on nothing and the handler reports the
// real error to the client.
//
// Shared with PublicAuthRateLimit (#5222), which needs the same
// read-without-consuming for its per-recipient buckets. Two copies of a
// body-rewrite would be two chances to leave a handler with an empty body.
func peekJSONBody(c *gin.Context) []byte {
	if c.Request == nil || c.Request.Body == nil {
		return nil
	}
	// Cap the read at 4 KiB — these bodies are tiny, and a large
	// body from an attacker shouldn't burn memory here.
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 4096))
	if err != nil {
		return nil
	}
	// Restore the body so the handler's ShouldBindJSON sees it.
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	return body
}

// extractJSONStringFields returns every string value in a small JSON object
// whose key matches `field`, using the SAME matching rule encoding/json uses
// when it binds that object to a struct: an exact key match, and failing that
// any key that differs only by case.
//
// It parses rather than scans, and that is the fix for a real bypass. The
// previous version searched the raw bytes for two literals — `"email"` and
// `"Email"` — while the handlers bind with c.ShouldBindJSON, and encoding/json
// accepts `{"EMAIL": ...}` for a field tagged `json:"email"`. So a request with
// a case-varied key bound fine in the handler and extracted to "" here: the
// per-recipient bucket was skipped and the mail still went out. A limiter that
// keys on a different parse from the one the handler acts on is not a limiter
// (CWE-807; found by the security review of PR #106, system_3 #5240).
//
// ALL matching values are returned, not the first. `{"email":"a","EMAIL":"b"}`
// binds to whichever encoding/json processes last, and rather than replicate
// that ordering rule the caller charges every distinct value — being wrong in
// the safe direction, since the alternative is guessing which one the handler
// will act on. Two spellings of the SAME value are one recipient and one
// charge; the caller keys on the value, so case variation buys no extra budget
// and costs no legitimate caller anything either.
//
// Deliberately lenient about everything else: a body that is not a JSON object,
// or whose matching value is not a string, yields nothing and the handler's own
// ShouldBindJSON reports the real 400 to the client.
func extractJSONStringFields(body []byte, field string) []string {
	if len(body) == 0 {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil
	}
	var out []string
	for key, raw := range obj {
		if !strings.EqualFold(key, field) {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			continue
		}
		out = append(out, value)
	}
	return out
}
