// Package mailer sends the email half of the notification system.
//
// Email is not a fallback that switches on when push fails — it is a parallel
// channel that is on by default. iOS push subscriptions die silently, with the push
// service still returning success to the server, so a design that waits for a
// delivery error before sending email would wait forever.
//
// The binary only ever speaks SMTP to a local MTA (msmtp or postfix on 127.0.0.1),
// which relays onward. That keeps provider authentication churn — and every major
// provider has broken SMTP basic auth at some point — an operating-system config
// edit rather than an application change.
package mailer

import (
	"context"
	"fmt"
	"log/slog"
	"mime"
	"net/smtp"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// Message is one email.
type Message struct {
	To      string
	Subject string
	Text    string // required: the family reads mail on phones, plain text always works
	HTML    string // optional
}

// Mailer sends messages.
type Mailer interface {
	Send(ctx context.Context, m Message) error
	// Failures reports consecutive send failures. /healthz surfaces this: an
	// unmonitored fallback channel is not a fallback.
	Failures() int
}

// SMTPMailer talks to a local MTA. No authentication and no TLS: the peer is
// 127.0.0.1, and adding credentials here would recreate the very coupling this
// design removes.
type SMTPMailer struct {
	addr     string
	from     string
	failures atomic.Int64
	lastOK   atomic.Int64 // unix seconds
}

func NewSMTP(addr, from string) *SMTPMailer {
	return &SMTPMailer{addr: addr, from: from}
}

func (m *SMTPMailer) Send(ctx context.Context, msg Message) error {
	if err := validate(msg); err != nil {
		return err
	}
	body := build(m.from, msg)

	done := make(chan error, 1)
	go func() {
		done <- smtp.SendMail(m.addr, nil, m.from, []string{msg.To}, body)
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		if err != nil {
			n := m.failures.Add(1)
			slog.Error("mail send failed", "to", redact(msg.To), "consecutive_failures", n, "error", err)
			return fmt.Errorf("send mail to %s: %w", redact(msg.To), err)
		}
		m.failures.Store(0)
		m.lastOK.Store(time.Now().Unix())
		slog.Info("mail sent", "to", redact(msg.To), "subject", msg.Subject)
		return nil
	}
}

func (m *SMTPMailer) Failures() int { return int(m.failures.Load()) }

// LastOK returns when a message was last accepted by the MTA, zero if never.
func (m *SMTPMailer) LastOK() time.Time {
	s := m.lastOK.Load()
	if s == 0 {
		return time.Time{}
	}
	return time.Unix(s, 0).UTC()
}

// FileMailer is the development sink: it writes .eml files to a directory instead of
// sending anything. It is what makes the whole notification pipeline testable on a
// laptop with no mail server — `make dev` then read them at /dev/mail.
type FileMailer struct {
	dir string
	n   atomic.Int64
}

func NewFile(dir string) (*FileMailer, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create mail dir %s: %w", dir, err)
	}
	return &FileMailer{dir: dir}, nil
}

func (m *FileMailer) Send(_ context.Context, msg Message) error {
	if err := validate(msg); err != nil {
		return err
	}
	seq := m.n.Add(1)
	name := fmt.Sprintf("%s-%03d-%s.eml", time.Now().UTC().Format("20060102-150405"), seq, safeName(msg.To))
	path := filepath.Join(m.dir, name)
	if err := os.WriteFile(path, build("agenda@localhost", msg), 0o640); err != nil {
		return fmt.Errorf("write dev mail %s: %w", path, err)
	}
	slog.Info("dev mail written", "path", path, "to", msg.To, "subject", msg.Subject)
	return nil
}

func (m *FileMailer) Failures() int { return 0 }

// Dir is where the sink writes, so the /dev/mail viewer can list it.
func (m *FileMailer) Dir() string { return m.dir }

func validate(m Message) error {
	if strings.TrimSpace(m.To) == "" {
		return fmt.Errorf("message has no recipient")
	}
	if strings.ContainsAny(m.To, "\r\n") || strings.ContainsAny(m.Subject, "\r\n") {
		return fmt.Errorf("header injection attempt in message to %q", redact(m.To))
	}
	if strings.TrimSpace(m.Text) == "" {
		return fmt.Errorf("message to %s has no text body", redact(m.To))
	}
	return nil
}

// build renders RFC 5322 bytes. Subjects are RFC 2047 encoded because French
// accents in "Rappel : Dentiste Léo" are not ASCII.
func build(from string, m Message) []byte {
	var b strings.Builder
	boundary := "agenda-boundary-x7f3a9"
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + m.To + "\r\n")
	b.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", m.Subject) + "\r\n")
	b.WriteString("Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Auto-Submitted: auto-generated\r\n")

	if m.HTML == "" {
		b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
		b.WriteString(normalizeCRLF(m.Text))
		return []byte(b.String())
	}
	b.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n\r\n")
	b.WriteString("--" + boundary + "\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString(normalizeCRLF(m.Text) + "\r\n")
	b.WriteString("--" + boundary + "\r\nContent-Type: text/html; charset=utf-8\r\n\r\n")
	b.WriteString(normalizeCRLF(m.HTML) + "\r\n")
	b.WriteString("--" + boundary + "--\r\n")
	return []byte(b.String())
}

func normalizeCRLF(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9._@-]+`)

func safeName(s string) string { return unsafeName.ReplaceAllString(s, "_") }

// redact keeps addresses out of logs in recognisable but non-identifying form.
func redact(addr string) string {
	at := strings.IndexByte(addr, '@')
	if at <= 1 {
		return "***"
	}
	return addr[:1] + "***" + addr[at:]
}
