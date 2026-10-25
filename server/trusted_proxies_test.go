package server

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestTrustedProxyCIDRsReturnsOwnNonLoopbackIPv4Network(t *testing.T) {
	cidrs, err := TrustedProxyCIDRs()
	if err != nil {
		t.Fatalf("TrustedProxyCIDRs: %v", err)
	}
	if len(cidrs) == 0 {
		t.Fatal("no CIDRs returned")
	}
	for _, c := range cidrs {
		_, ipNet, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatalf("%q is not a valid CIDR: %v", c, err)
		}
		if ipNet.IP.IsLoopback() {
			t.Fatalf("%q is a loopback network; TrustedProxyCIDRs must exclude it", c)
		}
	}
}

// clientIPRouter mirrors the production wiring in main.go: a bare engine
// with SetTrustedProxies and RemoteIPHeaders applied, exposing c.ClientIP()
// for inspection.
func clientIPRouter(t *testing.T, trusted []string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	if err := r.SetTrustedProxies(trusted); err != nil {
		t.Fatalf("SetTrustedProxies: %v", err)
	}
	r.RemoteIPHeaders = []string{"X-Forwarded-For"}
	r.GET("/whoami", func(c *gin.Context) {
		c.String(http.StatusOK, c.ClientIP())
	})
	return r
}

func TestClientIPHonoursForwardedForOnlyFromATrustedProxy(t *testing.T) {
	// $http_cf_connecting_ip, as nginx's /api/ location now sets it
	// (system_3 #8821): a single address, with no client-controlled entry
	// ahead of it.
	const nginxSetXFF = "198.51.100.42"
	r := clientIPRouter(t, []string{"172.18.0.0/16"})

	req := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	req.RemoteAddr = "172.18.0.5:54321" // inside the compose network -> this is nginx
	req.Header.Set("X-Forwarded-For", nginxSetXFF)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := w.Body.String(); got != nginxSetXFF {
		t.Fatalf("trusted proxy: ClientIP() = %q, want %q", got, nginxSetXFF)
	}
}

func TestClientIPIgnoresForwardedForFromAnUntrustedPeer(t *testing.T) {
	// A caller that reached the server directly (e.g. the published :8081
	// port, bypassing nginx) and spoofs a left-most X-Forwarded-For entry
	// to pick its own rate-limit bucket -- the defect this fix closes.
	const spoofedXFF = "9.9.9.9, 203.0.113.7"
	const directPeer = "203.0.113.99"
	r := clientIPRouter(t, []string{"172.18.0.0/16"})

	req := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	req.RemoteAddr = directPeer + ":54321" // outside the compose network
	req.Header.Set("X-Forwarded-For", spoofedXFF)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := w.Body.String(); got != directPeer {
		t.Fatalf("untrusted peer: ClientIP() = %q, want the real peer %q (X-Forwarded-For must be ignored)", got, directPeer)
	}
}

// TestClientIPIgnoresXRealIPFromTheTrustedProxy covers security's finding
// on c54490: gin's default RemoteIPHeaders also reads X-Real-IP, and
// nginx's /api/ location passes a client-supplied X-Real-IP through
// unchanged. A request from the trusted nginx peer with no
// X-Forwarded-For (one that never reached Cloudflare) must not fall back
// to a caller-controlled X-Real-IP.
func TestClientIPIgnoresXRealIPFromTheTrustedProxy(t *testing.T) {
	const spoofedXRealIP = "6.6.6.6"
	const nginxAddr = "172.18.0.5"
	r := clientIPRouter(t, []string{"172.18.0.0/16"})

	req := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	req.RemoteAddr = nginxAddr + ":54321" // the trusted nginx peer
	req.Header.Set("X-Real-IP", spoofedXRealIP)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := w.Body.String(); got != nginxAddr {
		t.Fatalf("no X-Forwarded-For, spoofed X-Real-IP: ClientIP() = %q, want the peer %q (X-Real-IP must be ignored)", got, nginxAddr)
	}
}

// TestClientIPPrefersForwardedForOverXRealIPFromTheTrustedProxy: when both
// headers are present from the trusted peer, the legitimate
// X-Forwarded-For (nginx's own $http_cf_connecting_ip value) wins over a
// spoofed X-Real-IP riding alongside it.
func TestClientIPPrefersForwardedForOverXRealIPFromTheTrustedProxy(t *testing.T) {
	const cfConnectingIP = "198.51.100.42"
	const spoofedXRealIP = "6.6.6.6"
	r := clientIPRouter(t, []string{"172.18.0.0/16"})

	req := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	req.RemoteAddr = "172.18.0.5:54321" // the trusted nginx peer
	req.Header.Set("X-Forwarded-For", cfConnectingIP)
	req.Header.Set("X-Real-IP", spoofedXRealIP)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := w.Body.String(); got != cfConnectingIP {
		t.Fatalf("X-Forwarded-For + spoofed X-Real-IP: ClientIP() = %q, want %q", got, cfConnectingIP)
	}
}
