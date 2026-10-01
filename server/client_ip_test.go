package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestMaskClientIPMasksGloballyRoutableAddresses(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		// The operator's own request to /signup/status, read out of the
		// production access log at 2026-10-01T01:39:13Z.
		{"public ipv4", "78.69.190.146", "78.69.190.0"},
		{"public ipv4 already zero host", "54.1.2.0", "54.1.2.0"},
		{"public ipv6", "2001:db8::1234:5678:9abc:def0", "2001:db8::"},
		// ::ffff:a.b.c.d is an IPv4 address in IPv6 form; it must take the
		// /24 rule, not the /64 one, or three octets survive unmasked.
		{"ipv4 mapped into ipv6", "::ffff:78.69.190.146", "78.69.190.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := maskClientIP(tc.raw); got != tc.want {
				t.Fatalf("maskClientIP(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestMaskClientIPKeepsAddressesThatCannotIdentifyAVisitor(t *testing.T) {
	// These are infrastructure talking to itself -- the health check is ~2800
	// lines a day on its own -- so they stay legible for debugging.
	for _, raw := range []string{"::1", "127.0.0.1", "10.0.1.7", "192.168.1.20", "172.17.0.3", "fd00::1", "fe80::1", "0.0.0.0"} {
		if got := maskClientIP(raw); got != raw {
			t.Errorf("maskClientIP(%q) = %q, want it kept verbatim", raw, got)
		}
	}
}

func TestMaskClientIPDropsUnparsableValues(t *testing.T) {
	// A value that is not an address is dropped rather than echoed, so a
	// crafted X-Forwarded-For cannot write arbitrary text into the log.
	for _, raw := range []string{"", "not-an-ip", "78.69.190.146, 10.0.0.1", "<script>", "78.69.190.999"} {
		if got := maskClientIP(raw); got != clientIPUnknown {
			t.Errorf("maskClientIP(%q) = %q, want %q", raw, got, clientIPUnknown)
		}
	}
}

// TestAccessLogLineCarriesNoFullClientIP is the behavioural half: it drives a
// request through the real SetupMiddleware formatter and asserts the emitted
// line carries the masked network and not the full address. Without it, a later
// change could drop maskClientIP from routes.go and the unit tests above would
// still pass.
func TestAccessLogLineCarriesNoFullClientIP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var logged strings.Builder
	prevWriter := gin.DefaultWriter
	gin.DefaultWriter = &logged
	t.Cleanup(func() { gin.DefaultWriter = prevWriter })

	r := gin.New()
	SetupMiddleware(r)
	r.GET("/health", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("X-Forwarded-For", "78.69.190.146")
	r.ServeHTTP(httptest.NewRecorder(), req)

	line := logged.String()
	// Guard against passing vacuously if the access log ever stops being
	// written at all: the assertion below would then hold for the wrong reason.
	if !strings.Contains(line, `"/health"`) {
		t.Fatalf("no access-log line was emitted for the request; got %q", line)
	}
	if strings.Contains(line, "78.69.190.146") {
		t.Errorf("access-log line carries the full client IP: %q", line)
	}
	if !strings.Contains(line, "78.69.190.0") {
		t.Errorf("access-log line does not carry the masked network: %q", line)
	}
}
