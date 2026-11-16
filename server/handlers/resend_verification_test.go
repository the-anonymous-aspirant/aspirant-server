package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aspirant-online/server/data_models"
	"aspirant-online/server/email"
	"aspirant-online/server/middleware"

	"github.com/gin-gonic/gin"
)

// POST /verify-email/resend (system_3 #11143; design c70048, cleared by
// security c70050). Its four conditions are the cases below: one answer in
// bytes AND time for every address, the per-recipient limit on the normalised
// address, no address in a log line, and older links that keep working.

func (h *signupHarness) resend(t *testing.T, address string) *httptest.ResponseRecorder {
	t.Helper()
	return h.post(t, "/verify-email/resend", gin.H{"email": address})
}

// verificationMailsTo counts confirmation mails to an address.
func (h *signupHarness) verificationMailsTo(addr string) int {
	n := 0
	for _, m := range h.mail.to(addr) {
		if m.subject == "Confirm your address" {
			n++
		}
	}
	return n
}

func TestResendAnswersTheSameForEveryAddress(t *testing.T) {
	h := newSignupHarness(t)

	// Waiting for confirmation.
	if w := h.signup(t, "waiting", "waiting@example.com", goodPassword); w.Code != http.StatusOK {
		t.Fatalf("seed signup failed: %d", w.Code)
	}
	// Confirmed.
	if w := h.signup(t, "done", "done@example.com", goodPassword); w.Code != http.StatusOK {
		t.Fatalf("seed signup failed: %d", w.Code)
	}
	if w := h.post(t, "/verify-email", gin.H{"token": h.tokenFromMail(t, "done@example.com")}); w.Code != http.StatusOK {
		t.Fatalf("seed verify failed: %d %s", w.Code, w.Body.String())
	}
	// Soft-deleted, never confirmed.
	if w := h.signup(t, "gone", "gone@example.com", goodPassword); w.Code != http.StatusOK {
		t.Fatalf("seed signup failed: %d", w.Code)
	}
	if err := h.db.Delete(h.user(t, "gone")).Error; err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	backgroundMail.Wait()
	before := map[string]int{}
	for _, a := range []string{"waiting@example.com", "done@example.com", "gone@example.com"} {
		before[a] = h.verificationMailsTo(a)
	}

	cases := []string{"nobody@example.com", "done@example.com", "gone@example.com", "waiting@example.com"}
	var first []byte
	for _, address := range cases {
		w := h.resend(t, address)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200 (%s)", address, w.Code, w.Body.String())
		}
		// The bytes, not only the status: one differing word is the oracle.
		if first == nil {
			first = w.Body.Bytes()
		} else if !bytes.Equal(w.Body.Bytes(), first) {
			t.Fatalf("%s answered differently:\n got %s\nwant %s", address, w.Body.Bytes(), first)
		}
	}
	if !bytes.Contains(first, []byte(resendResponseMessage)) {
		t.Fatalf("answer is not resendResponseMessage: %s", first)
	}

	backgroundMail.Wait()
	// A new link went only to the account waiting for one.
	for address, want := range map[string]int{
		"waiting@example.com": before["waiting@example.com"] + 1,
		"done@example.com":    before["done@example.com"],
		"gone@example.com":    before["gone@example.com"],
	} {
		if got := h.verificationMailsTo(address); got != want {
			t.Errorf("%s: %d confirmation mails, want %d", address, got, want)
		}
	}
	if got := len(h.mail.to("nobody@example.com")); got != 0 {
		t.Errorf("an unknown address was mailed %d times", got)
	}
}

func TestResendMailsTheStoredAddressAndTheOlderLinkStillWorks(t *testing.T) {
	h := newSignupHarness(t)

	// Stored as typed at sign-up, mixed case.
	stored := "Mara.Esempio@Example.se"
	if w := h.signup(t, "mara", stored, goodPassword); w.Code != http.StatusOK {
		t.Fatalf("seed signup failed: %d", w.Code)
	}
	older := h.tokenFromMail(t, stored)

	// Typed differently: the lookup normalises as the limit's key does.
	if w := h.resend(t, "  mara.esempio@EXAMPLE.SE "); w.Code != http.StatusOK {
		t.Fatalf("resend: %d %s", w.Code, w.Body.String())
	}
	backgroundMail.Wait()

	if got := h.verificationMailsTo(stored); got != 2 {
		t.Fatalf("%d confirmation mails to the stored address, want 2 (sign-up + resend)", got)
	}
	if got := len(h.mail.to("mara.esempio@example.se")); got != 0 {
		t.Fatalf("the typed spelling was mailed %d times; mail goes to the stored address only", got)
	}
	newer := h.tokenFromMail(t, stored)
	if newer == older {
		t.Fatal("the resend did not mint a new token")
	}
	// verify_email does not retire its siblings: the older link still works.
	if w := h.post(t, "/verify-email", gin.H{"token": older}); w.Code != http.StatusOK {
		t.Fatalf("older link refused after a resend: %d %s", w.Code, w.Body.String())
	}
	if h.user(t, "mara").EmailVerifiedAt == nil {
		t.Fatal("following the older link did not confirm the account")
	}
}

// blockingSender holds every Send until release is closed, the way a relay
// that hangs would.
type blockingSender struct {
	release chan struct{}
	inner   *captureSender
}

func (s *blockingSender) Send(to, subject, body string) error {
	<-s.release
	return s.inner.Send(to, subject, body)
}

var _ email.Sender = (*blockingSender)(nil)

// Security's first condition: constant in time as well as in bytes. If the
// handler waited on SMTP, a slow answer would say "this address has an
// account waiting". With a relay that never answers, the handler must still
// answer at once.
func TestResendDoesNotWaitForTheRelay(t *testing.T) {
	h := newSignupHarness(t)
	if w := h.signup(t, "waiting", "waiting@example.com", goodPassword); w.Code != http.StatusOK {
		t.Fatalf("seed signup failed: %d", w.Code)
	}
	backgroundMail.Wait()

	blocked := &blockingSender{release: make(chan struct{}), inner: h.mail}
	h.sink = blocked
	before := h.verificationMailsTo("waiting@example.com")

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- h.resend(t, "waiting@example.com") }()
	select {
	case w := <-done:
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), resendResponseMessage) {
			t.Fatalf("answer %d %s", w.Code, w.Body.String())
		}
	case <-time.After(5 * time.Second):
		close(blocked.release)
		t.Fatal("the handler waited for the relay")
	}
	// Answered, and the mail has not gone yet: it is held by the relay.
	if got := h.verificationMailsTo("waiting@example.com"); got != before {
		t.Fatalf("mail delivered before the relay was released (%d, want %d)", got, before)
	}

	close(blocked.release)
	backgroundMail.Wait()
	if got := h.verificationMailsTo("waiting@example.com"); got != before+1 {
		t.Fatalf("after the relay answered: %d confirmation mails, want %d", got, before+1)
	}
}

// Security's second condition: the per-recipient limit keys on the
// normalised address, so case and spaces do not buy more budget, and an
// unknown address is limited too, so the limit is not itself a probe.
func TestResendIsLimitedPerNormalisedAddress(t *testing.T) {
	h := newSignupHarness(t)
	middleware.ResetPublicAuthRateLimiter()
	t.Cleanup(middleware.ResetPublicAuthRateLimiter)

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("db", h.db); c.Next() })
	r.Use(func(c *gin.Context) { c.Set("mailer", h.sender()); c.Next() })
	r.POST("/verify-email/resend", middleware.PublicAuthRateLimit("email"), ResendVerificationHandler)

	post := func(address string) int {
		raw, _ := json.Marshal(gin.H{"email": address})
		req := httptest.NewRequest(http.MethodPost, "/verify-email/resend", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	// One inbox no account holds, spelled three ways: three of its three.
	for _, spelling := range []string{"Probe@Example.se", " probe@example.se", "PROBE@EXAMPLE.SE "} {
		if code := post(spelling); code != http.StatusOK {
			t.Fatalf("%q: %d, want 200", spelling, code)
		}
	}
	if code := post("probe@Example.SE"); code != http.StatusTooManyRequests {
		t.Fatalf("fourth request for one inbox within the hour: %d, want 429", code)
	}
	// A different inbox still has its own budget.
	if code := post("someone.else@example.se"); code != http.StatusOK {
		t.Fatalf("another address: %d, want 200", code)
	}
}

func TestResendValidatesAndRefusesWhenClosed(t *testing.T) {
	h := newSignupHarness(t)

	if w := h.resend(t, "not an address"); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), "That does not look like an email address") {
		t.Fatalf("malformed: %d %s", w.Code, w.Body.String())
	}
	if w := h.post(t, "/verify-email/resend", gin.H{}); w.Code != http.StatusBadRequest {
		t.Fatalf("missing email: %d %s", w.Code, w.Body.String())
	}

	// No relay: the same refusal as /signup, for everyone.
	h.sink = email.LogSender{}
	if w := h.resend(t, "anyone@example.com"); w.Code != http.StatusForbidden ||
		!strings.Contains(w.Body.String(), signupClosedMessage) {
		t.Fatalf("no relay: %d %s", w.Code, w.Body.String())
	}
	h.sink = nil

	// Sign-up closed.
	if err := data_models.SetSignupEnabled(h.db, false); err != nil {
		t.Fatalf("close sign-up: %v", err)
	}
	if w := h.resend(t, "anyone@example.com"); w.Code != http.StatusForbidden ||
		!strings.Contains(w.Body.String(), signupClosedMessage) {
		t.Fatalf("closed: %d %s", w.Code, w.Body.String())
	}
}

// Security's third condition: no address in a log line. A relay's error can
// quote the recipient, so deliverLater logs it through redactAddresses.
func TestRedactAddresses(t *testing.T) {
	cases := map[string]string{
		"550 5.1.1 <mara@example.se>: Recipient address rejected": "550 5.1.1 <address> Recipient address rejected",
		"dial tcp 127.0.0.1:25: connect: connection refused":      "dial tcp 127.0.0.1:25: connect: connection refused",
		"rejected: a@b.c and x@y.z":                               "rejected: <address> and <address>",
	}
	for in, want := range cases {
		if got := redactAddresses(in); got != want {
			t.Errorf("redactAddresses(%q) = %q, want %q", in, got, want)
		}
	}
}
