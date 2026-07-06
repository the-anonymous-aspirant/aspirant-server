package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// newLoginRouter builds a minimal router with the rate limiter in
// front of a stub /login handler that always returns 401. Tests
// count the 401 vs 429 responses.
func newLoginRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/login", LoginRateLimit(), func(c *gin.Context) {
		// Consume the body the way LoginHandler does so we surface any
		// body-restore regression.
		var payload map[string]string
		_ = c.ShouldBindJSON(&payload)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid login credentials"})
	})
	return r
}

func postLogin(r *gin.Engine, ip, username, password string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	req := httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader(body))
	req.RemoteAddr = ip + ":12345"
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestLoginRateLimit_PerIPBucketTripsAtThreshold(t *testing.T) {
	ResetLoginRateLimiter()
	r := newLoginRouter()

	// 20 failures from one IP should all return 401.
	for i := 0; i < PerIPLimit; i++ {
		// Vary username so the per-username bucket doesn't trip first.
		w := postLogin(r, "10.0.0.1", "user-"+string(rune('a'+i%26)), "bad")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: want 401, got %d", i+1, w.Code)
		}
	}
	// 21st from the same IP should return 429.
	w := postLogin(r, "10.0.0.1", "user-z", "bad")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("21st attempt: want 429, got %d", w.Code)
	}
	// Different IP still gets 401 — per-IP isolation.
	w2 := postLogin(r, "10.0.0.2", "user-a", "bad")
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("different IP: want 401, got %d", w2.Code)
	}
}

func TestLoginRateLimit_PerUsernameBucketTripsAtThreshold(t *testing.T) {
	ResetLoginRateLimiter()
	r := newLoginRouter()

	// 10 failures against one username from rotating IPs — all 401.
	for i := 0; i < PerUsernameLimit; i++ {
		w := postLogin(r, "10.0.1."+string(rune('0'+i)), "victim", "bad")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: want 401, got %d", i+1, w.Code)
		}
	}
	// 11th against the same username from yet another IP — 429.
	w := postLogin(r, "10.0.1.99", "victim", "bad")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("11th attempt: want 429, got %d", w.Code)
	}
	// Different username on that IP still gets 401.
	w2 := postLogin(r, "10.0.1.99", "innocent", "bad")
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("different username: want 401, got %d", w2.Code)
	}
}

func TestLoginRateLimit_ClearBucketReleasesUsernameHold(t *testing.T) {
	ResetLoginRateLimiter()
	r := newLoginRouter()

	// N-1 failures.
	for i := 0; i < PerUsernameLimit-1; i++ {
		if w := postLogin(r, "10.0.2.1", "recover-me", "bad"); w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: want 401, got %d", i+1, w.Code)
		}
	}
	// Simulate a successful login clearing the bucket.
	ClearLoginBucketForUsername("recover-me")
	// Fresh failures start counting again — the next N-1 should still
	// be 401 (bucket reset means we haven't crossed the threshold).
	for i := 0; i < PerUsernameLimit-1; i++ {
		if w := postLogin(r, "10.0.2.2", "recover-me", "bad"); w.Code != http.StatusUnauthorized {
			t.Fatalf("post-reset attempt %d: want 401, got %d", i+1, w.Code)
		}
	}
}

func TestLoginRateLimit_UsernameCaseIsFolded(t *testing.T) {
	ResetLoginRateLimiter()
	r := newLoginRouter()

	// Attacker rotates between "Victim" and "VICTIM" and "victim" —
	// the bucket should still trip at the shared threshold.
	for i := 0; i < PerUsernameLimit; i++ {
		names := []string{"Victim", "victim", "VICTIM"}
		w := postLogin(r, "10.0.3."+string(rune('0'+i)), names[i%len(names)], "bad")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: want 401, got %d", i+1, w.Code)
		}
	}
	w := postLogin(r, "10.0.3.99", "vIcTiM", "bad")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("case-folded 11th: want 429, got %d", w.Code)
	}
}

func TestLoginRateLimit_BodyIsRestoredForDownstreamHandler(t *testing.T) {
	ResetLoginRateLimiter()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// Echo handler reads the body and confirms it still contains the
	// username the middleware peeked at.
	r.POST("/login", LoginRateLimit(), func(c *gin.Context) {
		raw, _ := io.ReadAll(c.Request.Body)
		if !strings.Contains(string(raw), `"username":"alice"`) {
			t.Fatalf("body not restored: %q", raw)
		}
		c.String(http.StatusOK, "ok")
	})
	postLogin(r, "10.0.4.1", "alice", "pw")
}

func TestLoginRateLimit_SlidingWindowExpiresOldFailures(t *testing.T) {
	ResetLoginRateLimiter()
	fakeNow := time.Now()
	defaultLoginLimiter.nowFn = func() time.Time { return fakeNow }
	r := newLoginRouter()

	// Fill the per-IP bucket right up to the limit.
	for i := 0; i < PerIPLimit; i++ {
		if w := postLogin(r, "10.0.5.1", "user-"+string(rune('a'+i%26)), "bad"); w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: want 401, got %d", i+1, w.Code)
		}
	}
	// One tick past the window — old failures roll off, so a new
	// attempt is allowed again.
	fakeNow = fakeNow.Add(PerIPWindow + time.Second)
	if w := postLogin(r, "10.0.5.1", "fresh", "bad"); w.Code != http.StatusUnauthorized {
		t.Fatalf("post-window attempt: want 401, got %d", w.Code)
	}
}
