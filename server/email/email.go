// Package email is the outbound-mail seam for aspirant-server.
//
// It exists so that everything above it — sign-up, email verification and
// password recovery (system_3 epic #5113 part C1) — can be written, tested and
// deployed without naming a mail provider, and so that naming one later is a
// change to the environment rather than to any handler.
//
// # Why there is a seam at all
//
// Mail cannot be delivered from the box that serves the-aspirant.com. Measured
// 2026-09-05 and recorded in system_3
// corpus/decisions/2026-09-04-signup-email-provider.md:
//
//   - Outbound TCP port 25 is blocked by the ISP, while 443/465/587/2525 are
//     open. Port 25 is what a mail server needs to reach a recipient's MX host,
//     so a self-hosted MTA here could only ever forward to a relay.
//   - The egress IP carries generic consumer-broadband reverse DNS that cannot
//     be changed (81-235-172-142-no600.tbcn.telia.com). Major receivers reject
//     or spam-fold such hosts regardless of SPF and DKIM, and that is true even
//     if port 25 were opened.
//
// So the final hop is bought from a relay, and the rest stays here. Every
// candidate relay speaks SMTP submission, which is why this package depends on
// nothing outside the standard library: switching providers is a change to
// SMTP_HOST and the credentials, with no code change and no module added.
//
// # Why the dev sink is not a test double
//
// LogSender ships in the production binary and is the default. Sign-up,
// verification and recovery are being built before the operator has chosen a
// relay; without a sink that satisfies Sender, none of that work would compile
// or run end to end until an account existed. The sink is what lets the
// provider decision stay an open operator action item instead of a blocker.
package email

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"strings"
	"time"
)

// Environment variables read by SenderFromEnv. SMTP_PORT is optional and
// defaults to the submission port; the other four are all-or-nothing.
const (
	EnvHost     = "SMTP_HOST"
	EnvPort     = "SMTP_PORT"
	EnvUsername = "SMTP_USERNAME"
	EnvPassword = "SMTP_PASSWORD"
	EnvFrom     = "SMTP_FROM"
)

// defaultPort is the RFC 6409 submission port.
//
// 587 and not 465: SendMail speaks STARTTLS, which upgrades a plaintext
// connection, and cannot drive the implicit-TLS-from-byte-one dialogue that 465
// expects. 465 would hang. It is also not 25 — besides being blocked outbound
// here, 25 is for server-to-server relay and every candidate provider
// authenticates on the submission port.
const defaultPort = "587"

// Sender delivers one message. Implementations must treat every argument as
// untrusted: `to` and `subject` reach this package from unauthenticated
// sign-up and password-recovery request bodies.
//
// The interface is deliberately this small. It is the whole contract the
// provider decision is allowed to touch, so a provider swap can never reach
// past it into a handler.
type Sender interface {
	Send(to, subject, body string) error
}

// ErrIncompleteConfig reports a partially configured relay: some of the SMTP
// variables are set and some are missing.
//
// It is returned ALONGSIDE a usable LogSender, never instead of one, and the
// caller is expected to log it loudly and carry on. That is the opposite of
// what this package did first, and the reversal has a date attached.
//
// The original reasoning: a silent fallback lets the service answer 200 to
// sign-up while writing verification mail to a log nobody reads, so refusing to
// start turns a silent permanent bug into a container that will not start. Each
// clause of that is true and the conclusion was still wrong, because it weighed
// mail against nothing. At 18:01Z on 2026-09-05 it weighed mail against the
// whole site: the deployment's compose file gives the server `env_file: .env`,
// which injects the MONITOR service's SMTP credentials — SMTP_HOST, SMTP_USER
// and SMTP_PASSWORD, set for alert mail and nothing to do with this package —
// into the server's environment. Two of the four names matched, the config read
// as half-configured, and every page on the-aspirant.com returned 502 for as
// long as it took to notice.
//
// A subsystem that sends registration mail must not be able to stop the server
// from serving. Mail loss is loud in the log and recoverable by asking for
// another link; an outage is neither. And a process cannot assume the
// environment it is handed is addressed to it — ambient variables that merely
// share a prefix are not configuration for this package.
var ErrIncompleteConfig = errors.New("email: SMTP configuration is incomplete")

// ErrUnsendable reports a recipient, subject or sender the message builder
// refuses to encode. See sanitizeHeaderValue.
var ErrUnsendable = errors.New("email: message rejected before sending")

// LogSender writes messages to the process log instead of sending them.
//
// It is the default when no relay is configured, and it is what local
// development and the not-yet-provisioned production deployment both use.
//
// The body is logged in full. That is a deliberate trade and it is safe only
// because this sender never runs with a relay configured: verification and
// password-reset links are single-use credentials, and writing them to a log is
// exactly how a developer completes a sign-up with no mailbox. Once SMTP_* is
// set, SenderFromEnv returns an SMTPSender and no link is ever logged.
type LogSender struct {
	// Logf defaults to log.Printf. Tests substitute it to capture output.
	Logf func(format string, v ...any)
}

// Send records the message and reports success.
func (s LogSender) Send(to, subject, body string) error {
	if err := validateRecipient(to); err != nil {
		return err
	}
	if _, err := sanitizeHeaderValue(subject); err != nil {
		return fmt.Errorf("%w: subject: %v", ErrUnsendable, err)
	}

	logf := s.Logf
	if logf == nil {
		logf = log.Printf
	}
	logf("email(dev sink): no SMTP relay configured, message not sent\n"+
		"  to:      %s\n"+
		"  subject: %s\n"+
		"  body:\n%s", to, subject, indent(body))
	return nil
}

// SMTPSender delivers over authenticated SMTP submission with STARTTLS.
type SMTPSender struct {
	Host string
	Port string
	// From is the envelope and header sender. It must be an address the relay
	// is authorised to send as, and one the domain's SPF record authorises —
	// the-aspirant.com currently publishes "v=spf1 -all", which authorises
	// nothing, so that record has to be widened for the chosen relay before
	// any mail sent from here will be accepted (decision record, finding 3).
	From     string
	Username string
	Password string

	// sendMail is smtp.SendMail in production; tests substitute it. It is a
	// field rather than a package-level variable so parallel tests cannot
	// clobber each other's stub.
	sendMail func(addr string, a smtp.Auth, from string, to []string, msg []byte) error
}

// Send composes the message and hands it to the relay.
//
// smtp.SendMail is what enforces transport security here: it issues STARTTLS
// whenever the server advertises it, and smtp.PlainAuth independently refuses
// to hand over credentials on a connection that is not TLS-protected (except to
// localhost). So a relay that fails to offer STARTTLS produces an
// authentication error rather than a password sent in the clear.
func (s SMTPSender) Send(to, subject, body string) error {
	if err := validateRecipient(to); err != nil {
		return err
	}

	msg, err := buildMessage(s.From, to, subject, body)
	if err != nil {
		return err
	}

	send := s.sendMail
	if send == nil {
		send = smtp.SendMail
	}

	port := s.Port
	if port == "" {
		port = defaultPort
	}

	auth := smtp.PlainAuth("", s.Username, s.Password, s.Host)
	if err := send(net.JoinHostPort(s.Host, port), auth, s.From, []string{to}, msg); err != nil {
		// The relay's error text can quote the recipient; the recipient is the
		// caller's own address, so this adds no disclosure, and losing it would
		// make a bounce undiagnosable.
		return fmt.Errorf("email: send to %s failed: %w", to, err)
	}
	return nil
}

// SenderFromEnv builds the Sender the process should use.
//
// It ALWAYS returns a usable Sender. With none of the SMTP variables set, or
// with only some of them, that is a LogSender; only a complete configuration
// produces an SMTPSender, so a partial one can never half-send. An incomplete
// configuration also returns ErrIncompleteConfig for the caller to log — see
// that error's comment for why this reports rather than refuses.
//
// The returned bool reports whether mail will actually leave the process, so
// main can say so at startup. An operator reading "no SMTP relay configured"
// in the logs is the intended way to discover a deployment that is not sending.
//
// Note it reads SMTP_USERNAME, not the SMTP_USER that this deployment sets for
// its monitor service. Deliberately: adopting credentials that happen to be
// visible in the environment would make the server start sending mail as
// whatever account they belong to, which is not a decision a name collision
// gets to make.
func SenderFromEnv() (Sender, bool, error) {
	host := strings.TrimSpace(os.Getenv(EnvHost))
	username := strings.TrimSpace(os.Getenv(EnvUsername))
	password := os.Getenv(EnvPassword)
	from := strings.TrimSpace(os.Getenv(EnvFrom))
	port := strings.TrimSpace(os.Getenv(EnvPort))

	required := map[string]string{
		EnvHost:     host,
		EnvUsername: username,
		EnvPassword: password,
		EnvFrom:     from,
	}

	var missing []string
	set := 0
	for name, value := range required {
		if value == "" {
			missing = append(missing, name)
			continue
		}
		set++
	}

	if set == 0 {
		// Nothing configured, including SMTP_PORT on its own — a lone port is
		// not evidence of intent to send.
		return LogSender{}, false, nil
	}
	if len(missing) > 0 {
		// Sorted so the message is stable across runs; Go map iteration is not.
		return LogSender{}, false, fmt.Errorf("%w: set %s or unset all of them", ErrIncompleteConfig, strings.Join(sorted(missing), ", "))
	}

	if _, err := mail.ParseAddress(from); err != nil {
		return LogSender{}, false, fmt.Errorf("%w: %s is not a valid address: %v", ErrIncompleteConfig, EnvFrom, err)
	}

	if port == "" {
		port = defaultPort
	}

	return SMTPSender{
		Host:     host,
		Port:     port,
		From:     from,
		Username: username,
		Password: password,
	}, true, nil
}

// buildMessage renders an RFC 5322 message.
//
// Line endings are written as CRLF. net/textproto's DotWriter — which
// smtp.Client.Data returns — normalises bare LF to CRLF and escapes a leading
// "." on a body line, so the body needs no dot-stuffing here; the headers are
// written CRLF-correct regardless so that a caller substituting a different
// transport does not inherit a latent bug.
func buildMessage(from, to, subject, body string) ([]byte, error) {
	cleanSubject, err := sanitizeHeaderValue(subject)
	if err != nil {
		return nil, fmt.Errorf("%w: subject: %v", ErrUnsendable, err)
	}
	cleanFrom, err := sanitizeHeaderValue(from)
	if err != nil {
		return nil, fmt.Errorf("%w: from: %v", ErrUnsendable, err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", cleanFrom)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", cleanSubject)
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=\"UTF-8\"\r\n")
	b.WriteString("Auto-Submitted: auto-generated\r\n")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n"))
	if !strings.HasSuffix(body, "\n") {
		b.WriteString("\r\n")
	}
	return []byte(b.String()), nil
}

// sanitizeHeaderValue rejects a header value carrying a line break.
//
// This is the header-injection guard (CWE-93), and it is load-bearing rather
// than defensive: `subject` is composed from data that reaches this package
// from an unauthenticated request body. A CR or LF in a header value ends the
// header and starts a new one, so a subject containing "\r\nBcc: victim@..."
// turns the sign-up endpoint into an open relay for whoever posts it. NUL is
// rejected on the same principle — it is not encodable in a header and
// truncates the value in some downstream parsers.
//
// Rejecting is correct rather than stripping: a subject that arrived with a
// newline in it is not a subject this system composed, so there is nothing to
// salvage and a stripped version would hide the attempt.
func sanitizeHeaderValue(v string) (string, error) {
	if i := strings.IndexAny(v, "\r\n\x00"); i >= 0 {
		return "", fmt.Errorf("contains a line break or NUL at byte %d", i)
	}
	return v, nil
}

// validateRecipient rejects an address that is malformed or that carries a
// line break.
//
// mail.ParseAddress is the parse, and it is deliberately the same check the
// message builder would otherwise have to trust: an address is validated once,
// here, before it can reach either a header or an SMTP RCPT command. It also
// rejects the group and multiple-address syntaxes, so a single recipient stays
// a single recipient.
func validateRecipient(to string) error {
	if _, err := sanitizeHeaderValue(to); err != nil {
		return fmt.Errorf("%w: recipient: %v", ErrUnsendable, err)
	}
	addr, err := mail.ParseAddress(to)
	if err != nil {
		return fmt.Errorf("%w: recipient %q is not a valid address: %v", ErrUnsendable, to, err)
	}
	if addr.Address != to {
		// Reject display-name forms ("Name <a@b>"): callers pass a bare
		// address, and accepting the richer syntax here would let a recipient
		// field smuggle a name that renders as something else.
		return fmt.Errorf("%w: recipient %q must be a bare address", ErrUnsendable, to)
	}
	return nil
}

// indent prefixes each line of the dev sink's body so a multi-line message
// stays visually attached to its log entry.
func indent(s string) string {
	if s == "" {
		return "    (empty)"
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}

// sorted returns a sorted copy. Avoids pulling in sort just for a slice this
// small, and keeps the ErrIncompleteConfig message stable across runs.
func sorted(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
