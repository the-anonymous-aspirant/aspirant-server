package handlers

import (
	"log"
	"net/http"
	"sync"
	"time"

	"aspirant-online/server/data_models"

	"github.com/jinzhu/gorm"
)

// Proxy-call latency instrumentation (system_3 #5953).
//
// Every proxied upstream call this service makes carries a fixed timeout
// ceiling (commander.valuation 300s, commander.decide 15s, advisor 300s, voice
// 120s, browser 60s, monitor 30s, translator 30s), and until now nothing
// measured the latency beneath those ceilings — so a workload that grew toward a
// ceiling (the #5919 OCR regression: commander.decide's upstream cost rising into
// its 15s ceiling) was
// invisible until it crossed and a member got a 502. This records route,
// upstream, status, latency and the ceiling for each call into a
// request_log-shaped table (#5931 consumes it to alert on margin, not failure).
//
// Design invariants (task acceptance):
//   - Recording must never add latency to, or fail, the proxied request. The
//     RoundTripper returns the base transport's (resp, err) unchanged, and a
//     sample is handed off by a non-blocking channel send that DROPS when the
//     buffer is full rather than blocking the request goroutine.
//   - Existing proxy behaviour is unchanged: because (resp, err) is returned
//     verbatim, respondCommanderError's net.Error.Timeout() detection and every
//     other caller path see exactly what http.DefaultTransport produced.

// proxySample is one recorded proxied call, handed from the request goroutine to
// the background writer.
type proxySample struct {
	route     string
	upstream  string
	status    int
	latencyMs int64
	timeoutMs int64
}

// proxySampleBufferSize bounds how many unwritten samples we hold. Sized
// generously relative to aspirant-server's proxied-call rate (uploads, voice,
// admin) so the buffer only fills if the writer/DB stalls — at which point
// dropping telemetry is the correct trade against blocking a user request.
const proxySampleBufferSize = 1024

var (
	proxySampleCh = make(chan proxySample, proxySampleBufferSize)

	// proxySink, when non-nil, receives samples instead of proxySampleCh. Tests
	// set it (see withProxySink) to observe emitted samples synchronously
	// without standing up the DB writer. Guarded because RoundTrip runs on many
	// request goroutines concurrently while a test swaps the sink.
	proxySinkMu sync.RWMutex
	proxySink   func(proxySample)
)

// recordProxySample hands a sample off without ever blocking the caller.
func recordProxySample(s proxySample) {
	proxySinkMu.RLock()
	sink := proxySink
	proxySinkMu.RUnlock()
	if sink != nil {
		sink(s)
		return
	}
	select {
	case proxySampleCh <- s:
	default:
		// Buffer full (writer or DB stalled): drop this sample. Telemetry is
		// best-effort and must never block or fail a proxied request.
	}
}

// proxyRoundTripper wraps a base RoundTripper (http.DefaultTransport), times the
// round trip, and records the outcome. It carries its client's upstream name and
// timeout ceiling so both are known at record time without inspecting the URL.
type proxyRoundTripper struct {
	base      http.RoundTripper
	upstream  string
	timeoutMs int64
}

func (rt *proxyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := rt.base.RoundTrip(req)
	latencyMs := time.Since(start).Milliseconds()

	status := 0 // no response (timeout / connection refusal) records as 0
	if resp != nil {
		status = resp.StatusCode
	}
	route := ""
	if req.URL != nil {
		route = req.URL.Path
	}
	recordProxySample(proxySample{
		route:     route,
		upstream:  rt.upstream,
		status:    status,
		latencyMs: latencyMs,
		timeoutMs: rt.timeoutMs,
	})

	// Return the base transport's result verbatim — never swallow or transform
	// the error, so timeout-vs-refusal detection and body/status handling are
	// exactly as they were before instrumentation.
	return resp, err
}

// newProxyClient is the single construction seam for every proxied client in
// this package: an *http.Client with the given timeout ceiling whose transport
// records per-call latency under the given upstream name.
func newProxyClient(upstream string, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &proxyRoundTripper{
			base:      http.DefaultTransport,
			upstream:  upstream,
			timeoutMs: timeout.Milliseconds(),
		},
	}
}

// StartProxyLatencyWriter launches the single background goroutine that drains
// recorded samples and persists them to aspirant_db. Called once from main after
// the DB connection is established. A nil db (tests, or a boot path without a DB)
// makes this a no-op so samples simply accumulate in the bounded buffer and drop.
func StartProxyLatencyWriter(db *gorm.DB) {
	if db == nil {
		return
	}
	go func() {
		for s := range proxySampleCh {
			row := data_models.ProxyRequestLog{
				Route:      s.route,
				Upstream:   s.upstream,
				StatusCode: s.status,
				LatencyMs:  s.latencyMs,
				TimeoutMs:  s.timeoutMs,
			}
			if err := db.Create(&row).Error; err != nil {
				// Best-effort: log and keep draining. One failed insert must
				// never crash the server or kill the drain loop.
				log.Printf("proxy latency writer: insert failed: %v", err)
			}
		}
	}()
}
