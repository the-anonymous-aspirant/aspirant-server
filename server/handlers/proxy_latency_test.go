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

	client := newProxyClient("commander", 300*time.Second)
	resp, err := client.Get(upstream.URL + "/valuation-statement/decide")
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
	if s.upstream != "commander" {
		t.Errorf("upstream = %q, want commander", s.upstream)
	}
	if s.route != "/valuation-statement/decide" {
		t.Errorf("route = %q, want /valuation-statement/decide", s.route)
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
	if s := (*got)[0]; s.upstream != "commander" || s.route != "/valuation-statement/health" {
		t.Errorf("got upstream=%q route=%q, want commander//valuation-statement/health", s.upstream, s.route)
	}
}
