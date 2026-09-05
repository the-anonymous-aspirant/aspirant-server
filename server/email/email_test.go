package email

import (
	"errors"
	"fmt"
	"net/smtp"
	"strings"
	"testing"
)

// --- SenderFromEnv selection ------------------------------------------------

func setSMTPEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	// Clear every variable this package reads, then apply the case. t.Setenv
	// restores the previous value on cleanup, so a case cannot leak into the
	// next one.
	for _, k := range []string{EnvHost, EnvPort, EnvUsername, EnvPassword, EnvFrom} {
		t.Setenv(k, "")
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func fullEnv() map[string]string {
	return map[string]string{
		EnvHost:     "smtp.example.net",
		EnvUsername: "apikey",
		EnvPassword: "s3cret",
		EnvFrom:     "no-reply@the-aspirant.com",
	}
}

func TestSenderFromEnv_NoConfigYieldsDevSink(t *testing.T) {
	setSMTPEnv(t, nil)

	sender, sends, err := SenderFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sends {
		t.Error("reported that mail will be sent with no relay configured")
	}
	if _, ok := sender.(LogSender); !ok {
		t.Errorf("got %T, want LogSender", sender)
	}
}

func TestSenderFromEnv_PortAloneIsStillDevSink(t *testing.T) {
	// A lone SMTP_PORT is not evidence of intent to send — it must not trip
	// the incomplete-config error, or an unrelated compose default would stop
	// the container from starting.
	setSMTPEnv(t, map[string]string{EnvPort: "587"})

	sender, sends, err := SenderFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sends {
		t.Error("reported that mail will be sent with only SMTP_PORT set")
	}
	if _, ok := sender.(LogSender); !ok {
		t.Errorf("got %T, want LogSender", sender)
	}
}

func TestSenderFromEnv_FullConfigYieldsSMTPSender(t *testing.T) {
	setSMTPEnv(t, fullEnv())

	sender, sends, err := SenderFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !sends {
		t.Error("reported that mail will not be sent with a full relay config")
	}
	s, ok := sender.(SMTPSender)
	if !ok {
		t.Fatalf("got %T, want SMTPSender", sender)
	}
	if s.Host != "smtp.example.net" || s.From != "no-reply@the-aspirant.com" || s.Username != "apikey" || s.Password != "s3cret" {
		t.Errorf("config not carried through: %+v", s)
	}
	if s.Port != defaultPort {
		t.Errorf("Port = %q, want the submission default %q", s.Port, defaultPort)
	}
}

func TestSenderFromEnv_ExplicitPortWins(t *testing.T) {
	env := fullEnv()
	env[EnvPort] = "2525"
	setSMTPEnv(t, env)

	sender, _, err := SenderFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := sender.(SMTPSender).Port; got != "2525" {
		t.Errorf("Port = %q, want 2525", got)
	}
}

// The load-bearing case: a half-configured relay must stop the process, not
// silently discard every message into the log.
func TestSenderFromEnv_PartialConfigIsAnError(t *testing.T) {
	for _, missing := range []string{EnvHost, EnvUsername, EnvPassword, EnvFrom} {
		t.Run("missing_"+missing, func(t *testing.T) {
			env := fullEnv()
			delete(env, missing)
			setSMTPEnv(t, env)

			sender, sends, err := SenderFromEnv()
			if !errors.Is(err, ErrIncompleteConfig) {
				t.Fatalf("err = %v, want ErrIncompleteConfig", err)
			}
			// A usable sender MUST come back with the error. Returning nil is
			// what let main.go turn a mail misconfiguration into a site-wide
			// outage on 2026-09-05; there is no longer a code path that leaves
			// a caller without somewhere to put a message.
			if _, ok := sender.(LogSender); !ok {
				t.Errorf("got %T alongside the error, want LogSender — a caller must never be left without a sender", sender)
			}
			if sends {
				t.Error("reported sending on an incomplete config")
			}
			if !strings.Contains(err.Error(), missing) {
				t.Errorf("error %q does not name the missing variable %s", err, missing)
			}
		})
	}
}

func TestSenderFromEnv_RejectsUnparseableFrom(t *testing.T) {
	env := fullEnv()
	env[EnvFrom] = "not an address"
	setSMTPEnv(t, env)

	sender, sends, err := SenderFromEnv()
	if !errors.Is(err, ErrIncompleteConfig) {
		t.Fatalf("err = %v, want ErrIncompleteConfig", err)
	}
	if _, ok := sender.(LogSender); !ok {
		t.Errorf("got %T, want LogSender", sender)
	}
	if sends {
		t.Error("reported sending on an unparseable From")
	}
}

// The exact shape that took the site down: the deployment's compose file gives
// the server `env_file: .env`, so the monitor service's SMTP_HOST / SMTP_USER /
// SMTP_PASSWORD are visible to this process. Two of the four names this package
// reads matched, and the incomplete config was fatal.
//
// SMTP_USER is deliberately NOT read — adopting credentials that merely share a
// prefix would make the server send mail as whatever account they belong to.
func TestSenderFromEnv_AmbientMonitorCredentialsDoNotStopTheProcess(t *testing.T) {
	setSMTPEnv(t, map[string]string{
		EnvHost:     "smtp.gmail.com",
		EnvPort:     "587",
		EnvPassword: "app-password",
	})
	t.Setenv("SMTP_USER", "someone@gmail.com") // the monitor's variable, not ours

	sender, sends, err := SenderFromEnv()
	if !errors.Is(err, ErrIncompleteConfig) {
		t.Fatalf("err = %v, want ErrIncompleteConfig reported", err)
	}
	if _, ok := sender.(LogSender); !ok {
		t.Fatalf("got %T, want a usable LogSender — this is the outage", sender)
	}
	if sends {
		t.Error("adopted the monitor's credentials and reported that it would send")
	}
}

func TestSenderFromEnv_WhitespaceOnlyCountsAsUnset(t *testing.T) {
	env := fullEnv()
	env[EnvHost] = "   "
	setSMTPEnv(t, env)

	if _, _, err := SenderFromEnv(); !errors.Is(err, ErrIncompleteConfig) {
		t.Fatalf("err = %v, want ErrIncompleteConfig for a whitespace-only host", err)
	}
}

// --- LogSender --------------------------------------------------------------

func TestLogSender_CapturesTheMessage(t *testing.T) {
	var got string
	s := LogSender{Logf: func(format string, v ...any) { got = fmt.Sprintf(format, v...) }}

	if err := s.Send("user@example.com", "Verify your address", "Follow this link:\nhttps://the-aspirant.com/verify?t=abc"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	for _, want := range []string{"user@example.com", "Verify your address", "https://the-aspirant.com/verify?t=abc"} {
		if !strings.Contains(got, want) {
			t.Errorf("logged output missing %q\ngot:\n%s", want, got)
		}
	}
	// The sink has to say that nothing was sent, or a developer reads a
	// logged verification mail as a delivered one.
	if !strings.Contains(got, "not sent") {
		t.Errorf("logged output does not say the message was not sent\ngot:\n%s", got)
	}
}

func TestLogSender_RejectsBadInputLikeTheRealSender(t *testing.T) {
	// The dev sink must reject exactly what SMTPSender rejects. If it were
	// laxer, a header-injection bug would pass every test run in development
	// and only appear once a relay was configured in production.
	s := LogSender{Logf: func(string, ...any) {}}

	if err := s.Send("not-an-address", "hi", "body"); !errors.Is(err, ErrUnsendable) {
		t.Errorf("bad recipient: err = %v, want ErrUnsendable", err)
	}
	if err := s.Send("user@example.com", "hi\r\nBcc: victim@example.com", "body"); !errors.Is(err, ErrUnsendable) {
		t.Errorf("injected subject: err = %v, want ErrUnsendable", err)
	}
}

// --- SMTPSender -------------------------------------------------------------

type capturedSend struct {
	addr string
	from string
	to   []string
	msg  []byte
}

func senderWithCapture(cap *capturedSend) SMTPSender {
	return SMTPSender{
		Host:     "smtp.example.net",
		Port:     "587",
		From:     "no-reply@the-aspirant.com",
		Username: "apikey",
		Password: "s3cret",
		sendMail: func(addr string, _ smtp.Auth, from string, to []string, msg []byte) error {
			*cap = capturedSend{addr: addr, from: from, to: to, msg: msg}
			return nil
		},
	}
}

func TestSMTPSender_AssemblesMessage(t *testing.T) {
	var cap capturedSend
	s := senderWithCapture(&cap)

	if err := s.Send("user@example.com", "Verify your address", "line one\nline two"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if cap.addr != "smtp.example.net:587" {
		t.Errorf("addr = %q, want smtp.example.net:587", cap.addr)
	}
	if cap.from != "no-reply@the-aspirant.com" {
		t.Errorf("envelope from = %q", cap.from)
	}
	if len(cap.to) != 1 || cap.to[0] != "user@example.com" {
		t.Errorf("envelope to = %v, want exactly one recipient", cap.to)
	}

	msg := string(cap.msg)
	head, body, found := strings.Cut(msg, "\r\n\r\n")
	if !found {
		t.Fatalf("message has no header/body separator:\n%q", msg)
	}
	for _, want := range []string{
		"From: no-reply@the-aspirant.com\r\n",
		"To: user@example.com\r\n",
		"Subject: Verify your address\r\n",
		"MIME-Version: 1.0\r\n",
		"Content-Type: text/plain; charset=\"UTF-8\"\r\n",
		"Date: ",
	} {
		if !strings.Contains(head, want) {
			t.Errorf("headers missing %q\ngot:\n%q", want, head)
		}
	}
	if body != "line one\r\nline two\r\n" {
		t.Errorf("body = %q, want CRLF line endings and a trailing CRLF", body)
	}
	// A bare LF anywhere would mean a header or body line that some relays
	// reject and others silently mangle.
	if strings.Contains(strings.ReplaceAll(msg, "\r\n", ""), "\n") {
		t.Errorf("message contains a bare LF:\n%q", msg)
	}
}

func TestSMTPSender_DefaultsToSubmissionPort(t *testing.T) {
	var cap capturedSend
	s := senderWithCapture(&cap)
	s.Port = ""

	if err := s.Send("user@example.com", "s", "b"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if cap.addr != "smtp.example.net:"+defaultPort {
		t.Errorf("addr = %q, want the submission port %s", cap.addr, defaultPort)
	}
}

// The security case this package exists to get right. `subject` and `to` are
// composed from unauthenticated request bodies, so a CR/LF in either would let
// the poster append their own headers — a Bcc turning sign-up into an open
// relay (CWE-93).
func TestSMTPSender_RejectsHeaderInjection(t *testing.T) {
	cases := []struct {
		name    string
		to      string
		subject string
	}{
		{"crlf in subject", "user@example.com", "Hi\r\nBcc: victim@example.com"},
		{"bare lf in subject", "user@example.com", "Hi\nBcc: victim@example.com"},
		{"bare cr in subject", "user@example.com", "Hi\rBcc: victim@example.com"},
		{"nul in subject", "user@example.com", "Hi\x00Bcc: victim@example.com"},
		{"crlf in recipient", "user@example.com\r\nBcc: victim@example.com", "Hi"},
		{"bare lf in recipient", "user@example.com\nBcc: victim@example.com", "Hi"},
		{"two recipients", "user@example.com, victim@example.com", "Hi"},
		{"display name form", "Someone <user@example.com>", "Hi"},
		{"empty recipient", "", "Hi"},
		{"unparseable recipient", "user at example dot com", "Hi"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cap capturedSend
			s := senderWithCapture(&cap)

			err := s.Send(tc.to, tc.subject, "body")
			if !errors.Is(err, ErrUnsendable) {
				t.Fatalf("err = %v, want ErrUnsendable", err)
			}
			if cap.msg != nil {
				t.Errorf("message reached the relay despite the rejection:\n%q", cap.msg)
			}
		})
	}
}

// A body may legitimately contain anything, including a line that starts with
// a dot or looks like a header. It must not be rejected, and it must not be
// able to forge headers — the blank line separating head from body is what
// guarantees that.
func TestSMTPSender_BodyContentCannotForgeHeaders(t *testing.T) {
	var cap capturedSend
	s := senderWithCapture(&cap)

	body := "Bcc: victim@example.com\n.\nSubject: not a subject"
	if err := s.Send("user@example.com", "Real subject", body); err != nil {
		t.Fatalf("Send: %v", err)
	}

	head, gotBody, _ := strings.Cut(string(cap.msg), "\r\n\r\n")
	if strings.Contains(head, "Bcc") {
		t.Errorf("body content reached the header block:\n%q", head)
	}
	if !strings.Contains(gotBody, "Bcc: victim@example.com") {
		t.Errorf("body was altered: %q", gotBody)
	}
	// Dot-stuffing is net/textproto's DotWriter's job at the transport, not
	// this builder's; assert the builder leaves the line intact so the two
	// layers cannot both escape it.
	if !strings.Contains(gotBody, "\r\n.\r\n") {
		t.Errorf("builder altered a lone-dot line: %q", gotBody)
	}
}

func TestSMTPSender_PropagatesRelayFailure(t *testing.T) {
	relayErr := errors.New("535 authentication failed")
	s := SMTPSender{
		Host: "smtp.example.net", Port: "587", From: "no-reply@the-aspirant.com",
		sendMail: func(string, smtp.Auth, string, []string, []byte) error { return relayErr },
	}

	err := s.Send("user@example.com", "s", "b")
	if !errors.Is(err, relayErr) {
		t.Fatalf("err = %v, want it to wrap the relay error", err)
	}
}

// Compile-time proof that both senders satisfy the seam. If either drifts, the
// package stops building rather than failing at the first sign-up.
var (
	_ Sender = LogSender{}
	_ Sender = SMTPSender{}
)
