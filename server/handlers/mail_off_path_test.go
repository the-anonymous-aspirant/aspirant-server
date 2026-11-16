package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Every mail /password/forgot and /signup send leaves the request path
// (system_3 #11393, security c70050 on #11143): a registered address must not
// answer more slowly than an unknown one, and no address reaches the log.
// Invented values only.

// answersWithin runs the request and fails if it has not answered in d.
func answersWithin(t *testing.T, d time.Duration, do func() *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- do() }()
	select {
	case w := <-done:
		return w
	case <-time.After(d):
		t.Fatalf("the handler did not answer within %s while the mailer was blocked: it is waiting on the send", d)
		return nil
	}
}

func TestPublicAuthMailIsSentOffTheRequestPath(t *testing.T) {
	h := newRecoveryHarness(t)
	loadSignupTestJWTSecret(t)
	seedVerifiedUser(t, h, "alice", "alice@example.com", goodPassword)

	// blockingSender (resend_verification_test.go) holds every send until
	// release is closed: a relay that has stopped answering.
	blocked := &blockingSender{release: make(chan struct{}), inner: &captureSender{}}
	h.sink = blocked
	released := false
	defer func() {
		if !released {
			close(blocked.release)
		}
	}()

	for _, tc := range []struct {
		name string
		do   func() *httptest.ResponseRecorder
	}{
		{"forgot-password, registered address", func() *httptest.ResponseRecorder { return h.forgot(t, "alice@example.com") }},
		{"forgot-password, unknown address", func() *httptest.ResponseRecorder { return h.forgot(t, "nobody@example.com") }},
		{"sign-up, fresh address", func() *httptest.ResponseRecorder {
			return h.signup(t, "bob", "bob@example.com", goodPassword)
		}},
		{"sign-up, taken address", func() *httptest.ResponseRecorder {
			return h.signup(t, "someone", "alice@example.com", goodPassword)
		}},
	} {
		w := answersWithin(t, 3*time.Second, tc.do)
		if w.Code != http.StatusOK {
			t.Errorf("%s: status %d %s", tc.name, w.Code, w.Body.String())
		}
	}

	// Released, every queued mail goes out: the reset, the verification and
	// the duplicate-account notice. The unknown address got nothing.
	close(blocked.release)
	released = true
	for addr, n := range map[string]int{"alice@example.com": 2, "bob@example.com": 1, "nobody@example.com": 0} {
		if got := len(blocked.inner.to(addr)); got != n { // to() waits for backgroundMail
			t.Errorf("mails to %s: %d, want %d", addr, got, n)
		}
	}
}

// failingSender fails every send with an error that quotes the recipient, as
// a real relay's 550 does.
type failingSender struct{}

func (failingSender) Send(to, _, _ string) error {
	return errors.New("550 5.1.1 <" + to + ">: mailbox unavailable")
}

func TestNoAddressReachesTheLogFromThePublicAuthMails(t *testing.T) {
	h := newRecoveryHarness(t)
	h.sink = failingSender{}
	buf := captureLog(t)
	const marker = "zz-marker-11393@example.test"

	h.signup(t, "marker", marker, goodPassword)         // verification mail
	h.signup(t, "someone", marker, goodPassword)        // duplicate-account notice
	h.forgot(t, marker)                                 // reset mail
	h.forgot(t, strings.ToUpper(marker[:3])+marker[3:]) // a case variant, too
	backgroundMail.Wait()

	out := buf.String()
	if strings.Contains(strings.ToLower(out), "zz-marker-11393") {
		t.Fatalf("an address reached the log:\n%s", out)
	}
	for _, kind := range []string{"verification", "duplicate-signup notice", "password-reset"} {
		if !strings.Contains(out, kind+" mail not sent") {
			t.Errorf("the log does not say the %s mail failed:\n%s", kind, out)
		}
	}
}

func TestATakenAddressSpendsTheSameHashAsAFreshSignup(t *testing.T) {
	h := newSignupHarness(t)
	calls := 0
	prev := equalizeSignupCost
	equalizeSignupCost = func(string) { calls++ }
	t.Cleanup(func() { equalizeSignupCost = prev })

	if w := h.signup(t, "first", "first@example.com", goodPassword); w.Code != http.StatusOK {
		t.Fatalf("seed: %d", w.Code)
	}
	if calls != 0 {
		t.Fatalf("a fresh sign-up spent the equalising hash (%d); it hashes for real", calls)
	}
	if w := h.signup(t, "second", "first@example.com", goodPassword); w.Code != http.StatusOK {
		t.Fatalf("taken: %d", w.Code)
	}
	if calls != 1 {
		t.Errorf("a taken address spent the equalising hash %d time(s), want 1", calls)
	}
}

func TestDeliverLaterRecoversAPanicAndLogsTheKindOnly(t *testing.T) {
	buf := captureLog(t)
	deliverLater(failingSender{}, "test-kind", func() (string, string, string, bool) {
		panic("compose failed for zz-panic@example.test")
	})
	backgroundMail.Wait() // a panic that escaped would have ended the test binary
	out := buf.String()
	if !strings.Contains(out, "test-kind mail") {
		t.Errorf("the recovery is not logged with the kind:\n%s", out)
	}
	if strings.Contains(out, "zz-panic") {
		t.Errorf("the panic value, which quotes an address, reached the log:\n%s", out)
	}
}
