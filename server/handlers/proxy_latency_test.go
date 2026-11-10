package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// withProxySink swaps the sample sink for the duration of a test, collecting
// emitted samples synchronously, and restores the previous sink afterward. This
// lets tests observe what the RoundTripper records without standing up the DB
// writer (proxy-handler tests carry no database, matching commander_test.go).
func withProxySink(t *testing.T) *[]proxySample {
	t.Helper()
	got := &[]proxySample{}
	proxySinkMu.Lock()
	prev := proxySink
	proxySink = func(s proxySample) { *got = append(*got, s) }
	proxySinkMu.Unlock()
	t.Cleanup(func() {
		proxySinkMu.Lock()
		proxySink = prev
		proxySinkMu.Unlock()
	})
	return got
}

// TestNewProxyClientRecordsCall exercises the single construction seam: a client
// built by newProxyClient records route, upstream, status, latency and the
// timeout ceiling for a successful call, and returns the response unchanged.
func TestNewProxyClientRecordsCall(t *testing.T) {
	got := withProxySink(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	client := newProxyClient("commander.valuation", 300*time.Second)
	resp, err := client.Get(upstream.URL + "/valuation-statement/generate")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("response status = %d, want 201 (response must pass through unchanged)", resp.StatusCode)
	}

	if len(*got) != 1 {
		t.Fatalf("recorded %d samples, want 1", len(*got))
	}
	s := (*got)[0]
	if s.upstream != "commander.valuation" {
		t.Errorf("upstream = %q, want commander.valuation", s.upstream)
	}
	if s.route != "/valuation-statement/generate" {
		t.Errorf("route = %q, want /valuation-statement/generate", s.route)
	}
	if s.status != http.StatusCreated {
		t.Errorf("status = %d, want 201", s.status)
	}
	if s.timeoutMs != 300000 {
		t.Errorf("timeoutMs = %d, want 300000 (the client ceiling, per-row)", s.timeoutMs)
	}
	if s.latencyMs < 0 {
		t.Errorf("latencyMs = %d, want >= 0", s.latencyMs)
	}
	if s.source != sourceInternal {
		t.Errorf("source = %q, want %q (a bare client.Get with no doProxy/gin context attaches no classification)", s.source, sourceInternal)
	}
}

// TestNewProxyClientRecordsNonCommanderUpstream proves the instrumentation is
// not commander-special-cased: a different upstream/ceiling records its own
// values (task acceptance criterion 2).
func TestNewProxyClientRecordsNonCommanderUpstream(t *testing.T) {
	got := withProxySink(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	client := newProxyClient("translator", 30*time.Second)
	resp, err := client.Get(upstream.URL + "/translate")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if len(*got) != 1 {
		t.Fatalf("recorded %d samples, want 1", len(*got))
	}
	s := (*got)[0]
	if s.upstream != "translator" || s.timeoutMs != 30000 {
		t.Errorf("got upstream=%q timeoutMs=%d, want translator/30000", s.upstream, s.timeoutMs)
	}
}

// TestProxyRoundTripperRecordsTransportError proves a call that never gets a
// response (connection refused) still records a sample (status 0) AND returns
// the transport error unchanged, so callers like respondCommanderError keep
// their timeout-vs-refusal detection.
func TestProxyRoundTripperRecordsTransportError(t *testing.T) {
	got := withProxySink(t)

	// Start then immediately close a server so connecting is a deterministic
	// refusal (the commander_test.go pattern), not a slow hang.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	client := newProxyClient("commander", 300*time.Second)
	resp, err := client.Get(deadURL + "/x")
	if err == nil {
		if resp != nil {
			resp.Body.Close()
		}
		t.Fatalf("expected a transport error to be returned unchanged, got nil")
	}

	if len(*got) != 1 {
		t.Fatalf("recorded %d samples, want 1", len(*got))
	}
	if s := (*got)[0]; s.status != 0 {
		t.Errorf("status = %d on transport error, want 0", s.status)
	}
}

// TestRecordProxySampleNeverBlocks proves the non-blocking hand-off: with no
// writer draining and the sink cleared to the default channel path, recording
// far more samples than the buffer holds must return promptly (dropping the
// overflow) rather than deadlock the caller.
func TestRecordProxySampleNeverBlocks(t *testing.T) {
	// Force the default channel path (no sink) for this test, then restore.
	proxySinkMu.Lock()
	prev := proxySink
	proxySink = nil
	proxySinkMu.Unlock()
	t.Cleanup(func() {
		proxySinkMu.Lock()
		proxySink = prev
		proxySinkMu.Unlock()
		// Drain anything we left in the buffer so we don't perturb other tests.
		for {
			select {
			case <-proxySampleCh:
			default:
				return
			}
		}
	})

	done := make(chan struct{})
	go func() {
		for i := 0; i < proxySampleBufferSize*3; i++ {
			recordProxySample(proxySample{upstream: "commander", timeoutMs: 300000})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("recordProxySample blocked: the non-blocking send did not drop on a full buffer")
	}
}

// TestCommanderProxyGetRecordsViaWiredGlobal proves the package-level
// commanderClient is actually wired to the instrumented transport end-to-end
// through a handler (not just that the constructor works).
func TestCommanderProxyGetRecordsViaWiredGlobal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	got := withProxySink(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer upstream.Close()
	t.Setenv("COMMANDER_URL", upstream.URL)

	r := gin.New()
	r.GET("/probe", func(c *gin.Context) { commanderProxyGet(c, "/valuation-statement/health") })
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/probe", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("handler status = %d, want 200", w.Code)
	}
	if len(*got) != 1 {
		t.Fatalf("recorded %d samples via the wired global, want 1", len(*got))
	}
	if s := (*got)[0]; s.upstream != "commander.valuation" || s.route != "/valuation-statement/health" {
		t.Errorf("got upstream=%q route=%q, want commander.valuation//valuation-statement/health", s.upstream, s.route)
	}
}

// TestClassifySource is the table-driven unit test for the classification
// itself (system_3 #10490-A1), with SMOKE_USER_ID explicitly set — the
// unset-env case is its own test below, since it is the one security's
// ruling (c64784) specifically binds: no default, ever.
func TestClassifySource(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv(smokeUserIDEnv, "14")

	withUserID := func(uid any) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		if uid != nil {
			c.Set("user_id", uid)
		}
		return c
	}

	cases := []struct {
		name string
		c    *gin.Context
		want callerSource
	}{
		{"nil context", nil, sourceInternal},
		{"no user_id in context", withUserID(nil), sourceInternal},
		{"wrong-typed user_id is not trusted as member or smoke", withUserID("14"), sourceInternal},
		{"the configured smoke account", withUserID(uint(14)), sourceSmoke},
		{"an ordinary member", withUserID(uint(99)), sourceMember},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifySource(tc.c); got != tc.want {
				t.Errorf("classifySource() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClassifySourceHonoursSmokeUserIDEnv proves the smoke account id is
// configurable, not hardcoded — it must track aspirant-deploy's
// SMOKE_USER_ID rather than silently drift from it.
func TestClassifySourceHonoursSmokeUserIDEnv(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv(smokeUserIDEnv, "99")

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("user_id", uint(99))
	if got := classifySource(c); got != sourceSmoke {
		t.Errorf("classifySource() = %q, want %q (SMOKE_USER_ID=99)", got, sourceSmoke)
	}

	c2, _ := gin.CreateTestContext(httptest.NewRecorder())
	c2.Set("user_id", uint(14))
	if got := classifySource(c2); got != sourceMember {
		t.Errorf("classifySource() = %q, want %q (14 is an ordinary member once SMOKE_USER_ID=99)", got, sourceMember)
	}
}

// TestClassifySourceFailsTowardMemberNotSmoke is the security-ruled behaviour
// (c64784 on #10490-A1): an unset or malformed SMOKE_USER_ID must recognise
// NO smoke account at all, rather than falling back to a guessed id. On a
// stack where the env var is never set, this is what stops a real member 14
// from being silently reclassified as synthetic traffic forever.
func TestClassifySourceFailsTowardMemberNotSmoke(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("unset env", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Set("user_id", uint(14))
		if got := classifySource(c); got != sourceMember {
			t.Errorf("classifySource() = %q, want %q (no SMOKE_USER_ID configured — nobody is smoke)", got, sourceMember)
		}
	})

	t.Run("malformed env", func(t *testing.T) {
		t.Setenv(smokeUserIDEnv, "not-a-number")
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Set("user_id", uint(14))
		if got := classifySource(c); got != sourceMember {
			t.Errorf("classifySource() = %q, want %q (a malformed SMOKE_USER_ID recognises no smoke account, not a guessed one)", got, sourceMember)
		}
	})
}

// TestDoProxyClassifiesPerRequest proves doProxy attaches a per-call
// classification that survives through RoundTrip to the recorded sample —
// not a property fixed at client-construction time, since the same package-
// level *http.Client is shared across every caller of a given upstream.
func TestDoProxyClassifiesPerRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv(smokeUserIDEnv, "14")
	got := withProxySink(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	client := newProxyClient("advisor", 300*time.Second)

	memberCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	memberCtx.Set("user_id", uint(7))
	req1, _ := http.NewRequest("GET", upstream.URL+"/sources", nil)
	if _, err := doProxy(client, req1, memberCtx); err != nil {
		t.Fatalf("doProxy (member) failed: %v", err)
	}

	smokeCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	smokeCtx.Set("user_id", uint(14))
	req2, _ := http.NewRequest("GET", upstream.URL+"/sources", nil)
	if _, err := doProxy(client, req2, smokeCtx); err != nil {
		t.Fatalf("doProxy (smoke) failed: %v", err)
	}

	req3, _ := http.NewRequest("GET", upstream.URL+"/sources", nil)
	if _, err := doProxy(client, req3, nil); err != nil {
		t.Fatalf("doProxy (no context) failed: %v", err)
	}

	if len(*got) != 3 {
		t.Fatalf("recorded %d samples, want 3", len(*got))
	}
	if (*got)[0].source != sourceMember {
		t.Errorf("sample 0 source = %q, want %q", (*got)[0].source, sourceMember)
	}
	if (*got)[1].source != sourceSmoke {
		t.Errorf("sample 1 source = %q, want %q", (*got)[1].source, sourceSmoke)
	}
	if (*got)[2].source != sourceInternal {
		t.Errorf("sample 2 source = %q, want %q", (*got)[2].source, sourceInternal)
	}
}

// TestProxyHandlersClassifyCallerPerRoute is the handler-level proof, one
// route per classification, mirroring TestCommanderProxyGetRecordsViaWiredGlobal
// and commander_test.go's own c.Set("user_id", ...) middleware pattern — every
// proxy-forwarding handler this task touched is wired through doProxy, not
// just the one exercised above by direct call.
func TestProxyHandlersClassifyCallerPerRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv(smokeUserIDEnv, "14")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer upstream.Close()
	t.Setenv("ADVISOR_URL", upstream.URL)
	t.Setenv("TRANSLATOR_URL", upstream.URL)
	t.Setenv("MONITOR_URL", upstream.URL)

	cases := []struct {
		name   string
		userID any
		want   callerSource
	}{
		{"member", uint(7), sourceMember},
		{"smoke", uint(14), sourceSmoke},
		{"no auth context", nil, sourceInternal},
	}

	handlers := []struct {
		name  string
		mount func(r *gin.Engine)
	}{
		{"advisorProxyGet", func(r *gin.Engine) {
			r.GET("/a", func(c *gin.Context) { advisorProxyGet(c, "/sources") })
		}},
		{"translatorProxyGet", func(r *gin.Engine) {
			r.GET("/t", func(c *gin.Context) { translatorProxyGet(c, "/languages") })
		}},
		{"monitorProxyGet", func(r *gin.Engine) {
			r.GET("/m", func(c *gin.Context) { monitorProxyGet(c, "/health") })
		}},
	}

	for _, h := range handlers {
		for _, tc := range cases {
			t.Run(h.name+"/"+tc.name, func(t *testing.T) {
				got := withProxySink(t)
				r := gin.New()
				if tc.userID != nil {
					r.Use(func(c *gin.Context) { c.Set("user_id", tc.userID) })
				}
				h.mount(r)
				path := map[string]string{"advisorProxyGet": "/a", "translatorProxyGet": "/t", "monitorProxyGet": "/m"}[h.name]
				w := httptest.NewRecorder()
				req, _ := http.NewRequest("GET", path, nil)
				r.ServeHTTP(w, req)

				if w.Code != http.StatusOK {
					t.Fatalf("handler status = %d, want 200", w.Code)
				}
				if len(*got) != 1 {
					t.Fatalf("recorded %d samples, want 1", len(*got))
				}
				if (*got)[0].source != tc.want {
					t.Errorf("source = %q, want %q", (*got)[0].source, tc.want)
				}
			})
		}
	}
}
