package middleware

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// --- harness ----------------------------------------------------------------

// limiterHarness drives PublicAuthRateLimit through a real gin router with a
// controllable clock, and records whether the wrapped handler ran.
type limiterHarness struct {
	router *gin.Engine
	now    time.Time
	served int
}

func newLimiterHarness(t *testing.T, path string, targetFields ...string) *limiterHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)

	ResetPublicAuthRateLimiter()
	t.Cleanup(ResetPublicAuthRateLimiter)

	h := &limiterHarness{now: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)}
	defaultPublicAuthLimiter.nowFn = func() time.Time { return h.now }

	r := gin.New()
	r.POST(path, PublicAuthRateLimit(targetFields...), func(c *gin.Context) {
		h.served++
		// Read the body so a failure to restore it after the middleware's peek
		// shows up here rather than in some later handler.
		raw, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unreadable body"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"saw": string(raw)})
	})
	h.router = r
	return h
}

func (h *limiterHarness) post(path, ip, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = ip + ":54321"
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w
}

func (h *limiterHarness) advance(d time.Duration) { h.now = h.now.Add(d) }

// --- per-IP limits ----------------------------------------------------------

// A burst from one address is stopped inside the short window.
func TestPublicAuthBurstLimit(t *testing.T) {
	h := newLimiterHarness(t, "/signup")

	for i := 0; i < PublicAuthBurstLimit; i++ {
		if w := h.post("/signup", "203.0.113.7", `{}`); w.Code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200 (under the burst limit)", i+1, w.Code)
		}
	}
	if w := h.post("/signup", "203.0.113.7", `{}`); w.Code != http.StatusTooManyRequests {
		t.Fatalf("request %d = %d, want 429", PublicAuthBurstLimit+1, w.Code)
	}
	if h.served != PublicAuthBurstLimit {
		t.Errorf("the handler ran %d times, want %d — a throttled request reached it", h.served, PublicAuthBurstLimit)
	}
}

func TestPublicAuthBurstWindowRecovers(t *testing.T) {
	h := newLimiterHarness(t, "/signup")

	for i := 0; i < PublicAuthBurstLimit+1; i++ {
		h.post("/signup", "203.0.113.7", `{}`)
	}
	h.advance(PublicAuthBurstWindow + time.Second)

	if w := h.post("/signup", "203.0.113.7", `{}`); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 once the burst window has passed", w.Code)
	}
}

// A grind that stays under the burst limit is stopped by the long window. This
// is the case a single threshold cannot catch.
func TestPublicAuthSustainedLimitCatchesASlowGrind(t *testing.T) {
	h := newLimiterHarness(t, "/signup")

	// One request every 30 seconds. That is at most 3 inside the one-minute
	// burst window, well under PublicAuthBurstLimit, so the burst limit never
	// fires — but 21 of them land inside the 15-minute sustained window.
	var lastCode int
	for i := 0; i < PublicAuthSustainedLimit+1; i++ {
		lastCode = h.post("/signup", "203.0.113.7", `{}`).Code
		if i < PublicAuthSustainedLimit && lastCode != http.StatusOK {
			t.Fatalf("request %d = %d, want 200 — the burst limit fired and this test would prove nothing about the sustained one", i+1, lastCode)
		}
		h.advance(30 * time.Second)
	}
	if lastCode != http.StatusTooManyRequests {
		t.Fatalf("last status = %d, want 429 — a paced grind was never throttled", lastCode)
	}
	if h.served != PublicAuthSustainedLimit {
		t.Errorf("the handler ran %d times, want %d", h.served, PublicAuthSustainedLimit)
	}
}

// Buckets are per address: one abuser must not lock everyone else out.
func TestPublicAuthLimitsArePerIP(t *testing.T) {
	h := newLimiterHarness(t, "/signup")

	for i := 0; i < PublicAuthBurstLimit+2; i++ {
		h.post("/signup", "203.0.113.7", `{}`)
	}
	if w := h.post("/signup", "198.51.100.4", `{}`); w.Code != http.StatusOK {
		t.Fatalf("a second address got %d, want 200 — one abuser locked out everyone", w.Code)
	}
}

// --- the per-recipient limit ------------------------------------------------

// The limit that is not about protecting us: /password/forgot and /signup mail
// an address the caller names, so without a per-recipient cap they are a
// mail-bombing tool aimed at a third party — and the per-IP limits do not help,
// because the attacker is not the victim.
func TestPerRecipientLimitSurvivesRotatingIPs(t *testing.T) {
	h := newLimiterHarness(t, "/password/forgot", "email")

	body := `{"email":"victim@example.com"}`
	for i := 0; i < PublicAuthTargetLimit; i++ {
		ip := fmt.Sprintf("203.0.113.%d", i+1)
		if w := h.post("/password/forgot", ip, body); w.Code != http.StatusOK {
			t.Fatalf("request %d from a fresh IP = %d, want 200", i+1, w.Code)
		}
	}
	// A brand-new IP, so every per-IP bucket is empty. Only the per-recipient
	// bucket can stop this.
	if w := h.post("/password/forgot", "198.51.100.99", body); w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 — rotating IPs defeated the per-recipient limit", w.Code)
	}
}

func TestPerRecipientLimitIsPerAddress(t *testing.T) {
	h := newLimiterHarness(t, "/password/forgot", "email")

	for i := 0; i < PublicAuthTargetLimit+1; i++ {
		h.post("/password/forgot", "203.0.113.7", `{"email":"victim@example.com"}`)
	}
	// A different recipient from a different IP is unaffected.
	if w := h.post("/password/forgot", "198.51.100.4", `{"email":"someone@example.com"}`); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — one targeted address blocked another", w.Code)
	}
}

// Addresses are normalised, or the limit is trivially bypassed by changing case
// or adding a space.
func TestPerRecipientLimitNormalisesTheKey(t *testing.T) {
	h := newLimiterHarness(t, "/password/forgot", "email")

	variants := []string{
		`{"email":"victim@example.com"}`,
		`{"email":"VICTIM@example.com"}`,
		`{"email":"  Victim@Example.com  "}`,
	}
	for i := 0; i < PublicAuthTargetLimit; i++ {
		h.post("/password/forgot", "203.0.113.7", variants[i%len(variants)])
	}
	if w := h.post("/password/forgot", "198.51.100.4", `{"email":"ViCtIm@ExAmPlE.cOm"}`); w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 — changing the case of the address bypassed the limit", w.Code)
	}
}

// /signup keys on the username as well, because its duplicate-signup notice
// goes to the address on file: someone who knows only a victim's username can
// aim mail at them without ever typing their address.
func TestSignupLimitsOnUsernameNotJustEmail(t *testing.T) {
	h := newLimiterHarness(t, "/signup", "email", "username")

	for i := 0; i < PublicAuthTargetLimit; i++ {
		body := fmt.Sprintf(`{"username":"victim","email":"throwaway%d@example.com"}`, i)
		if w := h.post("/signup", fmt.Sprintf("203.0.113.%d", i+1), body); w.Code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i+1, w.Code)
		}
	}
	// Fresh IP, fresh email, same username.
	body := `{"username":"victim","email":"another@example.com"}`
	if w := h.post("/signup", "198.51.100.99", body); w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 — a username-targeted mail bomb was not limited", w.Code)
	}
}

// Endpoints that send nothing take no recipient key, so a shared token value
// cannot throttle unrelated callers.
func TestNoTargetFieldsMeansNoRecipientBucket(t *testing.T) {
	h := newLimiterHarness(t, "/verify-email")

	for i := 0; i < PublicAuthTargetLimit+2; i++ {
		ip := fmt.Sprintf("203.0.113.%d", i+1)
		if w := h.post("/verify-email", ip, `{"token":"same-token-value"}`); w.Code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i+1, w.Code)
		}
	}
}

// --- the body must survive the peek ----------------------------------------

// The middleware reads the body to find the recipient. If it does not put the
// body back, every handler behind it sees an empty request — a failure that
// would look like a client bug.
func TestBodySurvivesTheRecipientPeek(t *testing.T) {
	h := newLimiterHarness(t, "/password/forgot", "email")

	body := `{"email":"someone@example.com","extra":"kept"}`
	w := h.post("/password/forgot", "203.0.113.7", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "kept") {
		t.Errorf("the handler did not receive the body it was sent:\n%s", w.Body.String())
	}
}

func TestMalformedBodyIsPassedThroughUnthrottled(t *testing.T) {
	h := newLimiterHarness(t, "/password/forgot", "email")

	// A body with no extractable recipient keys on nothing; the handler gets to
	// report the real 400 rather than the middleware masking it with a 429.
	if w := h.post("/password/forgot", "203.0.113.7", `not json at all`); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want the request to reach the handler", w.Code)
	}
}

// --- no IP retention --------------------------------------------------------

// The epic's binding constraint. The buckets are transient by design, but a log
// file is not: writing the key on the rejection path would turn a throttle that
// retains nothing into a retention surface.
func TestOverLimitLogLineCarriesNoIPOrRecipient(t *testing.T) {
	h := newLimiterHarness(t, "/password/forgot", "email")

	var captured bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&captured)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	const ip = "203.0.113.7"
	const recipient = "victim@example.com"
	for i := 0; i < PublicAuthBurstLimit+2; i++ {
		h.post("/password/forgot", ip, fmt.Sprintf(`{"email":%q}`, recipient))
	}

	logged := captured.String()
	if !strings.Contains(logged, "Rate limit") {
		t.Fatalf("no rate-limit line was logged; this test would pass vacuously:\n%s", logged)
	}
	if strings.Contains(logged, ip) {
		t.Errorf("the client IP was written to the log:\n%s", logged)
	}
	if strings.Contains(strings.ToLower(logged), recipient) {
		t.Errorf("the recipient address was written to the log:\n%s", logged)
	}
}

// --- bucket growth ----------------------------------------------------------

// Trimming empties a bucket's slice but leaves the map entry, so without a
// sweep the maps grow one entry per distinct IP forever — a slow memory leak
// reachable from an unauthenticated endpoint.
func TestExpiredBucketsAreSweptOnce(t *testing.T) {
	h := newLimiterHarness(t, "/signup")

	// Fill the map past the sweep threshold with one-hit buckets from distinct
	// addresses, then let them all age out.
	for i := 0; i < sweepThreshold; i++ {
		h.post("/signup", fmt.Sprintf("10.%d.%d.%d", i/65536%256, i/256%256, i%256), `{}`)
	}
	before := len(defaultPublicAuthLimiter.byIP)
	if before < sweepThreshold {
		t.Fatalf("only %d buckets accumulated; the sweep would not fire and this test proves nothing", before)
	}

	h.advance(PublicAuthSustainedWindow + time.Minute)

	// One address bursts, which is what triggers the sweep.
	for i := 0; i < PublicAuthBurstLimit+2; i++ {
		h.post("/signup", "203.0.113.7", `{}`)
	}

	after := len(defaultPublicAuthLimiter.byIP)
	if after >= before {
		t.Errorf("bucket count went %d -> %d; expired buckets were not swept", before, after)
	}
	// The bursting address's own buckets must survive, or the sweep would
	// hand an attacker a fresh allowance.
	if w := h.post("/signup", "203.0.113.7", `{}`); w.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 — the sweep cleared the live bucket that was throttling this caller", w.Code)
	}
}

// --- the whole flow, from one address ---------------------------------------

// The regression test for the defect the dogfood walk found and the unit tests
// above could not: every test here builds a fresh limiter, so none of them
// measured what one person doing the entire flow costs against a single
// bucket. At the original burst limit of five, this sequence ended with the
// password reset itself returning 429 — the limit locking a legitimate user
// out of recovery.
//
// The sequence is deliberately unkind: it includes the fumbles a real person
// makes, because the limit has to survive those too.
func TestOneUserCanCompleteTheWholeFlowFromOneAddress(t *testing.T) {
	h := newLimiterHarness(t, "/x", "email", "username")

	const ip = "203.0.113.7"
	steps := []struct {
		what string
		body string
	}{
		{"sign up", `{"username":"newcomer","email":"newcomer@example.com"}`},
		{"sign up again, having mistyped the address the first time", `{"username":"newcomer","email":"newcomer@exampl.com"}`},
		{"follow the verification link", `{"token":"t1"}`},
		{"double-click it", `{"token":"t1"}`},
		{"ask for a password reset", `{"email":"newcomer@example.com"}`},
		{"mistype the address and ask again", `{"email":"newcomer@exampl.com"}`},
		{"follow the reset link", `{"token":"t2"}`},
		{"submit a password that is too short, and be told so", `{"token":"t2"}`},
		{"submit an acceptable one", `{"token":"t2"}`},
	}

	for i, step := range steps {
		if w := h.post("/x", ip, step.body); w.Code != http.StatusOK {
			t.Fatalf("step %d (%s) = %d, want 200 — a legitimate user was throttled mid-flow", i+1, step.what, w.Code)
		}
	}
}

// The burst limit still has to stop what it is for.
func TestMachineSpeedBurstIsStillStopped(t *testing.T) {
	h := newLimiterHarness(t, "/signup")

	var blocked int
	for i := 0; i < 50; i++ {
		if h.post("/signup", "203.0.113.7", `{}`).Code == http.StatusTooManyRequests {
			blocked++
		}
	}
	if blocked != 50-PublicAuthBurstLimit {
		t.Errorf("blocked %d of 50 same-instant requests, want %d", blocked, 50-PublicAuthBurstLimit)
	}
	if h.served != PublicAuthBurstLimit {
		t.Errorf("the handler ran %d times, want %d", h.served, PublicAuthBurstLimit)
	}
}
