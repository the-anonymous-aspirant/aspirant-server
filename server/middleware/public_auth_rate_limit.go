package middleware

import (
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// Abuse limits for the public account endpoints (system_3 epic #5113,
// subtask #5222): /signup, /verify-email, /password/forgot, /password/reset.
//
// These are unauthenticated, they write to the database, and two of them make
// this server send mail to an address the caller supplies. That last property
// is the one that makes a limit mandatory rather than prudent — without it,
// /password/forgot and /signup are a free relay for delivering mail to a
// stranger's inbox as often as somebody likes.
//
// # No IP retention
//
// This is the epic's binding constraint and it is a property of the design, not
// a rule this file remembers to follow. Buckets live in a map in process
// memory, are keyed by the client IP, are trimmed on every access, and are
// dropped when the process restarts. Nothing here writes an IP to the database
// and nothing here writes an IP to the log — the over-limit log line names the
// route and the reason and no more, which is why it reads differently from the
// one in LoginRateLimit. A test asserts that.
//
// # Two windows, not one
//
// A single threshold cannot separate a burst from sustained abuse: set it low
// enough to stop a hundred requests in ten seconds and it also stops a family
// signing up from one home connection over an afternoon; set it high enough for
// the family and the burst goes through. So there are two limits on the same
// key — a short window that catches a burst, and a long one that catches a
// slow grind — and a request must clear both.

const (
	// PublicAuthBurstLimit / PublicAuthBurstWindow catch a rapid burst from one
	// address.
	//
	// Ten and not five. Five was the first number here and it was wrong, in a
	// way no unit test could show: each test builds a fresh limiter, so nothing
	// measured what ONE person doing the whole flow actually costs. Walking it
	// end to end did — sign up, follow the verification link, ask for a reset,
	// follow that link is five requests before anything goes wrong, and a
	// mistyped address or a double-clicked link puts it over. The walk ended
	// with the reset itself returning 429, i.e. the limit locking a legitimate
	// user out of recovery.
	//
	// Ten leaves room for that sequence plus fumbles, and for two or three
	// people signing up from one household connection at the same time, while
	// still stopping the machine-speed case this window exists for at request
	// eleven instead of at the sustained limit's twenty-first.
	PublicAuthBurstLimit  = 10
	PublicAuthBurstWindow = time.Minute

	// PublicAuthSustainedLimit / PublicAuthSustainedWindow catch a slow grind
	// that stays under the burst threshold. Sized so that several people behind
	// one NAT — a household, which is the expected shape of this site's traffic
	// — can each sign up, verify and even fumble a password reset without
	// tripping it.
	PublicAuthSustainedLimit  = 20
	PublicAuthSustainedWindow = 15 * time.Minute

	// PublicAuthTargetLimit / PublicAuthTargetWindow bound how often mail can be
	// aimed at one recipient, independently of who is asking.
	//
	// This is the limit that is not about protecting us. Both /signup and
	// /password/forgot send mail to an address the caller names — the reset
	// link, and the "someone tried to sign up with your details" notice — so
	// without a per-recipient cap the endpoints are a mail-bombing tool aimed
	// at a third party, and the per-IP limits do not help because the attacker
	// is not the victim. Three an hour is generous for a person who genuinely
	// cannot find the mail and useless as a weapon.
	PublicAuthTargetLimit  = 3
	PublicAuthTargetWindow = time.Hour
)

// publicAuthLimiter is the process-wide state behind PublicAuthRateLimit.
//
// It is separate from defaultLoginLimiter rather than an extra store on it:
// sharing would mean a burst of sign-ups consuming a legitimate user's login
// allowance, and the two have different thresholds because they defend
// different things.
type publicAuthLimiter struct {
	mu       sync.Mutex
	byIP     map[string]*bucket
	byTarget map[string]*bucket
	nowFn    func() time.Time
}

func newPublicAuthLimiter() *publicAuthLimiter {
	return &publicAuthLimiter{
		byIP:     make(map[string]*bucket),
		byTarget: make(map[string]*bucket),
		nowFn:    time.Now,
	}
}

var defaultPublicAuthLimiter = newPublicAuthLimiter()

// ResetPublicAuthRateLimiter clears the process-wide state. Tests only.
func ResetPublicAuthRateLimiter() {
	defaultPublicAuthLimiter = newPublicAuthLimiter()
}

// PublicAuthRateLimit throttles an unauthenticated account endpoint.
//
// targetFields names the JSON body fields whose values identify who a message
// would be sent to, and each gets its own per-recipient bucket. Pass them for
// the endpoints that send mail:
//
//	/signup           -> "email", "username"
//	/password/forgot  -> "email"
//	/verify-email     -> none (the body is a token; nothing is sent)
//	/password/reset   -> none
//
// /signup needs both. Its duplicate-signup notice goes to the address on file,
// so an attacker who knows only a victim's *username* can aim mail at them
// without ever typing their address — keying on "email" alone would miss
// exactly that case.
//
// A rejected request is counted before it is rejected, so hammering the
// endpoint keeps the bucket full rather than letting it drain.
func PublicAuthRateLimit(targetFields ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		l := defaultPublicAuthLimiter

		ip := c.ClientIP()

		// Read and parse the body BEFORE taking the lock. peekJSONBody blocks on
		// io.ReadAll of the client stream, and all four public account endpoints
		// share one limiter — so doing this under the mutex let a single slow
		// client stall sign-up, verification and recovery together, for everyone
		// (CWE-667/CWE-400; security review of PR #106, system_3 #5240). The
		// sibling LoginRateLimit always had this order and this file copied its
		// helper without its ordering.
		//
		// Every case-variant spelling of a target field is collected, because
		// encoding/json binds `{"EMAIL": ...}` to a field tagged `json:"email"`
		// and the limiter must key on the same value the handler will act on.
		targets := make(map[string]struct{})
		if len(targetFields) > 0 {
			body := peekJSONBody(c)
			for _, field := range targetFields {
				for _, raw := range extractJSONStringFields(body, field) {
					value := strings.ToLower(strings.TrimSpace(raw))
					if value == "" {
						continue
					}
					targets[field+"\x00"+value] = struct{}{}
				}
			}
		}

		l.mu.Lock()
		now := l.nowFn()

		overBurst := hitAndCheckBucket(l.byIP, "burst\x00"+ip, now, PublicAuthBurstLimit, PublicAuthBurstWindow)
		overSustained := hitAndCheckBucket(l.byIP, "sustained\x00"+ip, now, PublicAuthSustainedLimit, PublicAuthSustainedWindow)

		overTarget := false
		for key := range targets {
			if hitAndCheckBucket(l.byTarget, key, now, PublicAuthTargetLimit, PublicAuthTargetWindow) {
				overTarget = true
			}
		}
		if overBurst || overSustained || overTarget {
			// Only on the rejection path: the cost of the sweep falls on the
			// traffic that grew the maps.
			sweepExpired(l.byIP, now, PublicAuthSustainedWindow)
			sweepExpired(l.byTarget, now, PublicAuthTargetWindow)
		}
		l.mu.Unlock()

		if overBurst || overSustained || overTarget {
			// Deliberately no IP and no target in this line. The buckets are
			// transient by design and a log file is not; writing the key here
			// would turn a throttle that retains nothing into a retention
			// surface, which is the epic's binding constraint.
			log.Printf("Rate limit: too many requests to %s", c.FullPath())
			// One message for all three causes. Which limit tripped tells a
			// prober whether the address they guessed is one we mail, and the
			// caller cannot act on the difference anyway.
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "Too many attempts, please try again later"})
			c.Abort()
			return
		}

		c.Next()
	}
}

// hitAndCheckBucket records an attempt against a keyed bucket and reports
// whether it is now over the threshold. Caller holds the limiter's mutex.
//
// It is the free-function twin of (*loginRateLimiter).hitAndCheck, which is a
// method on that type and reaches its fields. Lifting the shared behaviour into
// one place keeps the trimming policy — lazily, on access, with no background
// goroutine — from drifting between the two limiters.
func hitAndCheckBucket(store map[string]*bucket, key string, now time.Time, limit int, window time.Duration) bool {
	b, ok := store[key]
	if !ok {
		b = &bucket{}
		store[key] = b
	}
	b.trim(now, window)
	b.hits = append(b.hits, now)
	return len(b.hits) > limit
}

// sweepThreshold is the map size past which an over-limit request also drops
// expired buckets.
//
// Trimming a bucket empties its slice but leaves the map entry, so under a
// distributed probe the maps would grow with one entry per distinct IP or
// address and never shrink — a slow memory leak reachable from an
// unauthenticated endpoint. The sweep runs only when a request is already
// being rejected, so the cost lands on the traffic that caused it and never on
// a legitimate caller. Dropping an expired bucket is safe by construction: an
// empty bucket and an absent one produce the same answer.
const sweepThreshold = 1024

// sweepExpired drops buckets with no hits left inside the window. Caller holds
// the limiter's mutex.
func sweepExpired(store map[string]*bucket, now time.Time, window time.Duration) {
	if len(store) < sweepThreshold {
		return
	}
	for key, b := range store {
		b.trim(now, window)
		if len(b.hits) == 0 {
			delete(store, key)
		}
	}
}
