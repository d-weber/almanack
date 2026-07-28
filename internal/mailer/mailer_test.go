package mailer

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testFrom = "almanack@example.org"
	testTo   = "damien@example.org"
)

// Both implementations must satisfy the interface: /healthz and the notifier hold a
// mailer.Mailer, and a signature drift here is a compile error rather than a channel
// that quietly stops being wired up.
var (
	_ Mailer = (*SMTPMailer)(nil)
	_ Mailer = (*FileMailer)(nil)
)

// ---------------------------------------------------------------------------
// A fake MTA
// ---------------------------------------------------------------------------

// session is one SMTP transaction as the server saw it. The envelope is kept apart
// from the payload because it is the envelope, not the To: header, that decides where
// the mail actually goes.
type session struct {
	from  string
	rcpts []string
	data  []byte
}

// fakeConfig chooses how the fake MTA misbehaves. It is fixed before the listener
// starts serving, so the server goroutine never reads a field a test is writing.
type fakeConfig struct {
	// reply overrides one step of the conversation, keyed by "GREETING", "EHLO",
	// "MAIL", "RCPT", "DATA" or "EOD" (the response to the terminating dot).
	reply map[string]string
	// dropAfter closes the connection without answering once that step is reached:
	// an MTA restarted underneath the conversation.
	dropAfter string
	// stall accepts the connection and then says nothing at all, which is the only
	// way to hold a send open long enough to test cancellation without sleeping.
	stall bool
}

// fakeSMTP is enough of RFC 5321 for net/smtp to complete a transaction and no more.
// It advertises no extensions — in particular neither STARTTLS nor AUTH — because that
// is what the local msmtp or postfix this package is designed against looks like, and
// because an advertised extension would send the client down a path production never
// takes.
type fakeSMTP struct {
	cfg fakeConfig
	ln  net.Listener
	wg  sync.WaitGroup

	// refuse rejects every recipient while set, so a single endpoint can be made to
	// fail and recover within one test.
	refuse atomic.Bool

	mu    sync.Mutex
	dials int
	sess  []session
	conns []net.Conn
}

func newFakeSMTP(t *testing.T, cfg fakeConfig) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeSMTP{cfg: cfg, ln: ln}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		s.mu.Lock()
		for _, c := range s.conns {
			_ = c.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
	})
	return s
}

func (s *fakeSMTP) addr() string { return s.ln.Addr().String() }

func (s *fakeSMTP) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.dials++
		s.conns = append(s.conns, conn)
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer conn.Close()
			s.handle(conn)
		}()
	}
}

func (s *fakeSMTP) handle(conn net.Conn) {
	if s.cfg.stall {
		_, _ = io.Copy(io.Discard, conn)
		return
	}
	br := bufio.NewReader(conn)
	write := func(line string) bool {
		_, err := io.WriteString(conn, line)
		return err == nil
	}
	if s.cfg.dropAfter == "GREETING" {
		return
	}
	if !write(s.replyTo("GREETING", "220 fake.localhost ESMTP\r\n")) {
		return
	}

	var sess session
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		verb = strings.ToUpper(verb)
		if verb == s.cfg.dropAfter {
			return
		}
		switch verb {
		case "EHLO":
			if !write(s.replyTo("EHLO", "250-fake.localhost\r\n250 HELP\r\n")) {
				return
			}
		case "HELO":
			if !write("250 fake.localhost\r\n") {
				return
			}
		case "MAIL":
			r := s.replyTo("MAIL", "250 2.1.0 sender ok\r\n")
			if strings.HasPrefix(r, "2") {
				sess.from = envelopeAddr(arg)
			}
			if !write(r) {
				return
			}
		case "RCPT":
			r := s.replyTo("RCPT", "250 2.1.5 recipient ok\r\n")
			if s.refuse.Load() {
				r = "550 5.1.1 no such user here\r\n"
			}
			if strings.HasPrefix(r, "2") {
				sess.rcpts = append(sess.rcpts, envelopeAddr(arg))
			}
			if !write(r) {
				return
			}
		case "DATA":
			r := s.replyTo("DATA", "354 end with <CRLF>.<CRLF>\r\n")
			if !write(r) {
				return
			}
			if !strings.HasPrefix(r, "3") {
				continue
			}
			data, err := readDotStuffed(br)
			if err != nil {
				return
			}
			sess.data = data
			if s.cfg.dropAfter == "EOD" {
				return
			}
			// Recorded before the acknowledgement, so a test that has seen Send
			// return cannot race the server's own bookkeeping.
			s.record(sess)
			sess = session{}
			if !write(s.replyTo("EOD", "250 2.0.0 accepted\r\n")) {
				return
			}
		case "RSET":
			sess = session{}
			if !write("250 2.0.0 ok\r\n") {
				return
			}
		case "QUIT":
			_ = write("221 2.0.0 bye\r\n")
			return
		default:
			if !write("502 5.5.1 command not implemented\r\n") {
				return
			}
		}
	}
}

func (s *fakeSMTP) replyTo(step, def string) string {
	if r, ok := s.cfg.reply[step]; ok {
		return r
	}
	return def
}

func (s *fakeSMTP) record(sess session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sess = append(s.sess, sess)
}

func (s *fakeSMTP) sessions() []session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]session(nil), s.sess...)
}

func (s *fakeSMTP) dialCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dials
}

// envelopeAddr pulls the address out of "FROM:<a@b>" or "TO:<a@b> ORCPT=...".
func envelopeAddr(arg string) string {
	_, rest, ok := strings.Cut(arg, ":")
	if !ok {
		return ""
	}
	rest = strings.TrimSpace(rest)
	if i := strings.IndexByte(rest, ' '); i >= 0 {
		rest = rest[:i]
	}
	return strings.Trim(rest, "<>")
}

// wireBody is what the MTA receives for a given Text: the DATA phase ends at a line
// boundary, so net/smtp terminates a body that does not already end in CRLF. The extra
// CRLF belongs to the transport, not to the message.
func wireBody(text string) string {
	b := normalizeCRLF(text)
	if !strings.HasSuffix(b, "\r\n") {
		b += "\r\n"
	}
	return b
}

// readDotStuffed reads a DATA payload, undoing the dot-stuffing of RFC 5321 §4.5.2 but
// leaving the line endings exactly as they arrived: whether the wire form is CRLF is
// the thing several of these tests are about, so textproto's DotReader — which rewrites
// CRLF to LF — would destroy the evidence.
func readDotStuffed(br *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if line == ".\r\n" || line == ".\n" {
			return out, nil
		}
		if strings.HasPrefix(line, ".") {
			line = line[1:]
		}
		out = append(out, line...)
	}
}

// ---------------------------------------------------------------------------
// Reading the produced message back
// ---------------------------------------------------------------------------

func parseMessage(t *testing.T, raw []byte) *mail.Message {
	t.Helper()
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("message does not parse as RFC 5322: %v\n%s", err, raw)
	}
	return m
}

func decodeSubject(t *testing.T, m *mail.Message) string {
	t.Helper()
	raw := m.Header.Get("Subject")
	s, err := new(mime.WordDecoder).DecodeHeader(raw)
	if err != nil {
		t.Fatalf("decode subject %q: %v", raw, err)
	}
	return s
}

func readBody(t *testing.T, m *mail.Message) string {
	t.Helper()
	b, err := io.ReadAll(m.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// headerLines returns the raw header block and fails on any bare CR or bare LF in it.
// A bare LF is how a header block silently turns into a body, and MTAs disagree about
// what to do with one, which is the worst possible property for a delivery path.
func headerLines(t *testing.T, raw []byte) []string {
	t.Helper()
	i := bytes.Index(raw, []byte("\r\n\r\n"))
	if i < 0 {
		t.Fatalf("no CRLFCRLF separating headers from body in\n%q", raw)
	}
	head := string(raw[:i])
	for j := 0; j < len(head); j++ {
		switch head[j] {
		case '\r':
			if j+1 >= len(head) || head[j+1] != '\n' {
				t.Errorf("bare CR at byte %d of the header block: %q", j, head)
			}
		case '\n':
			if j == 0 || head[j-1] != '\r' {
				t.Errorf("bare LF at byte %d of the header block: %q", j, head)
			}
		}
	}
	return strings.Split(head, "\r\n")
}

// ---------------------------------------------------------------------------
// Header injection
// ---------------------------------------------------------------------------

// TestSendRefusesHostileMessage is the highest-value test in this package. 0.2.0 fixed
// a newline in an event title killing the reminder mail for every participant; the
// guard that stops the same newline from instead forging a header lives here, and it
// has to run before anything is dialled or written — a message that reaches the MTA
// half-refused is a message someone has to reason about.
func TestSendRefusesHostileMessage(t *testing.T) {
	tests := []struct {
		name string
		msg  Message
	}{
		{"no recipient", Message{Subject: "Rappel", Text: "corps"}},
		{"blank recipient", Message{To: "  \t ", Subject: "Rappel", Text: "corps"}},
		{"newline in recipient", Message{To: "damien@example.org\nBcc: attacker@example.net", Subject: "Rappel", Text: "corps"}},
		{"carriage return in recipient", Message{To: "damien@example.org\rBcc: attacker@example.net", Subject: "Rappel", Text: "corps"}},
		{"CRLF in recipient", Message{To: "damien@example.org\r\nBcc: attacker@example.net", Subject: "Rappel", Text: "corps"}},
		{"newline in subject", Message{To: testTo, Subject: "Rappel\nBcc: attacker@example.net", Text: "corps"}},
		{"carriage return in subject", Message{To: testTo, Subject: "Rappel\rBcc: attacker@example.net", Text: "corps"}},
		{"CRLF in subject", Message{To: testTo, Subject: "Rappel\r\nBcc: attacker@example.net", Text: "corps"}},
		{"subject ending in a newline", Message{To: testTo, Subject: "Rappel\n", Text: "corps"}},
		{"no text body", Message{To: testTo, Subject: "Rappel"}},
		{"whitespace-only text body", Message{To: testTo, Subject: "Rappel", Text: " \n\t "}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakeSMTP(t, fakeConfig{})
			m := NewSMTP(srv.addr(), testFrom)
			if err := m.Send(context.Background(), tc.msg); err == nil {
				t.Fatal("Send accepted a message it should have refused")
			}
			if n := srv.dialCount(); n != 0 {
				t.Errorf("the MTA saw %d connections; a refused message must never reach the wire", n)
			}
			// A refusal is not evidence about the MTA, and /healthz reads this
			// counter as MTA health: one malformed message must not report the
			// mail channel as down.
			if n := m.Failures(); n != 0 {
				t.Errorf("Failures() = %d after a rejected message, want 0", n)
			}

			// The dev sink has to refuse exactly the same messages, or what a
			// developer sees locally is not what the family would receive.
			dir := t.TempDir()
			fm, err := NewFile(dir)
			if err != nil {
				t.Fatalf("NewFile: %v", err)
			}
			if err := fm.Send(context.Background(), tc.msg); err == nil {
				t.Fatal("the file sink accepted a message the SMTP mailer refused")
			}
			if entries, err := os.ReadDir(dir); err != nil {
				t.Fatalf("read sink dir: %v", err)
			} else if len(entries) != 0 {
				t.Errorf("the file sink wrote %d files for a refused message, want 0", len(entries))
			}
		})
	}
}

// TestBuildNeutralisesControlCharactersInSubject covers the layer under validate. The
// subject is the one header carrying free text a family member typed, so even if a
// caller ever reaches build directly, RFC 2047 encoding must turn a newline into =0A
// rather than into a header of the attacker's choosing.
func TestBuildNeutralisesControlCharactersInSubject(t *testing.T) {
	tests := []struct {
		name    string
		subject string
	}{
		{"newline", "Rappel\nBcc: attacker@example.net"},
		{"CRLF", "Rappel\r\nBcc: attacker@example.net"},
		{"bare carriage return", "Rappel\rBcc: attacker@example.net"},
		{"trailing newline", "Rappel\n"},
		{"folded header lookalike", "Rappel\n Content-Type: text/html"},
		{"tab", "Rappel\tDentiste"},
		{"null byte", "Rappel\x00Dentiste"},
		// Neither of these ends in a space: trailing whitespace in an unencoded
		// header is not significant and every RFC 5322 parser drops it, so a
		// fixture ending in one would be testing the parser rather than build.
		{"very long", strings.Repeat("Rappel Dentiste ", 80) + "fin"},
		{"very long with accents", strings.Repeat("Rappel : Dentiste Léo ", 80) + "fin"},
		{"empty", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := build(testFrom, Message{To: testTo, Subject: tc.subject, Text: "corps"})

			for _, line := range headerLines(t, raw) {
				if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
					continue // a folded continuation, not a new field
				}
				name, _, ok := strings.Cut(line, ":")
				if !ok {
					t.Errorf("header line %q is not a field", line)
					continue
				}
				switch strings.ToLower(name) {
				case "from", "to", "subject", "date", "mime-version", "auto-submitted", "content-type":
				default:
					t.Errorf("hostile subject produced an extra header %q", line)
				}
			}
			m := parseMessage(t, raw)
			if len(m.Header) != 7 {
				t.Errorf("message has %d header fields, want 7: %v", len(m.Header), m.Header)
			}
			if got := m.Header.Get("Bcc"); got != "" {
				t.Errorf("Bcc header injected: %q", got)
			}
			// The value survives intact; it is escaped, not silently truncated,
			// so what the recipient reads is still the title that was set.
			if got := decodeSubject(t, m); got != tc.subject {
				t.Errorf("subject round trip = %q, want %q", got, tc.subject)
			}
			if got := readBody(t, m); got != "corps" {
				t.Errorf("body = %q, want %q", got, "corps")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The envelope
// ---------------------------------------------------------------------------

func TestBuildEnvelope(t *testing.T) {
	before := time.Now().Add(-2 * time.Second)
	raw := build(testFrom, Message{To: testTo, Subject: "Rappel : Dentiste", Text: "Bonjour"})
	m := parseMessage(t, raw)

	for _, tc := range []struct{ header, want string }{
		{"From", testFrom},
		{"To", testTo},
		{"MIME-Version", "1.0"},
		// Without Auto-Submitted a recipient's out-of-office answers the reminder,
		// the answer arrives at the sending address, and the loop is bounded only
		// by how many events the family has.
		{"Auto-Submitted", "auto-generated"},
		{"Content-Type", "text/plain; charset=utf-8"},
	} {
		if got := m.Header.Get(tc.header); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.header, got, tc.want)
		}
	}
	if got := decodeSubject(t, m); got != "Rappel : Dentiste" {
		t.Errorf("Subject = %q, want %q", got, "Rappel : Dentiste")
	}

	date, err := m.Header.Date()
	if err != nil {
		t.Fatalf("Date does not parse: %v (raw %q)", err, m.Header.Get("Date"))
	}
	if date.Before(before) || date.After(time.Now().Add(2*time.Second)) {
		t.Errorf("Date = %s, want a time around now", date)
	}
	if got := readBody(t, m); got != "Bonjour" {
		t.Errorf("body = %q, want %q", got, "Bonjour")
	}
}

// TestBuildMultipart checks the alternative part, and above all the closing delimiter:
// a multipart message missing its terminating boundary renders as an empty mail in
// several clients, which is indistinguishable from never having sent it.
func TestBuildMultipart(t *testing.T) {
	msg := Message{
		To:      testTo,
		Subject: "Rappel : Dentiste",
		Text:    "Bonjour Léo\nà 16 h 30",
		HTML:    "<p>Bonjour Léo</p>\n<p>à 16 h 30</p>",
	}
	m := parseMessage(t, build(testFrom, msg))

	mediatype, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("Content-Type does not parse: %v", err)
	}
	if mediatype != "multipart/alternative" {
		t.Fatalf("Content-Type = %q, want multipart/alternative", mediatype)
	}
	if params["boundary"] == "" {
		t.Fatal("multipart Content-Type has no boundary parameter")
	}

	want := []struct{ mediatype, body string }{
		// Plain text comes first: RFC 2046 orders alternatives worst to best, and a
		// client showing the last part it understands must land on the HTML.
		{"text/plain", "Bonjour Léo\r\nà 16 h 30"},
		{"text/html", "<p>Bonjour Léo</p>\r\n<p>à 16 h 30</p>"},
	}
	r := multipart.NewReader(m.Body, params["boundary"])
	for i, w := range want {
		part, err := r.NextPart()
		if err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		mt, ps, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if err != nil {
			t.Fatalf("part %d Content-Type does not parse: %v", i, err)
		}
		if mt != w.mediatype {
			t.Errorf("part %d media type = %q, want %q", i, mt, w.mediatype)
		}
		if ps["charset"] != "utf-8" {
			t.Errorf("part %d charset = %q, want utf-8", i, ps["charset"])
		}
		body, err := io.ReadAll(part)
		if err != nil {
			t.Fatalf("part %d body: %v", i, err)
		}
		if string(body) != w.body {
			t.Errorf("part %d body = %q, want %q", i, body, w.body)
		}
	}
	if _, err := r.NextPart(); !errors.Is(err, io.EOF) {
		t.Errorf("after the last part NextPart returned %v, want io.EOF (the closing delimiter is missing or there is a third part)", err)
	}
}

// TestBuildTextOnlyWhenNoHTML: the plain part is the one that always works on a phone,
// and a multipart wrapper around a single part only adds ways to render nothing.
func TestBuildTextOnlyWhenNoHTML(t *testing.T) {
	raw := build(testFrom, Message{To: testTo, Subject: "Rappel", Text: "Bonjour"})
	if bytes.Contains(raw, []byte("multipart")) {
		t.Errorf("a message with no HTML was built as multipart:\n%s", raw)
	}
}

// ---------------------------------------------------------------------------
// Encoding
// ---------------------------------------------------------------------------

// TestBuildEncodesNonASCIISubject: this is a French household, so accents are the
// normal case rather than an edge case. The header block has to stay 7-bit — a raw
// "é" in a Subject is what makes a reminder arrive as "RÃ©union" or not at all.
func TestBuildEncodesNonASCIISubject(t *testing.T) {
	tests := []struct {
		name    string
		subject string
	}{
		{"ascii is left alone", "Reminder: dentist"},
		{"french accents", "Rappel : Dentiste Léo à 16 h 30"},
		{"ligatures and dashes", "Rappel — cœur de la journée"},
		{"emoji", "Anniversaire 🎂 de Léo"},
		{"long enough to fold", strings.Repeat("Réunion de rentrée ", 12)},
		{"non-latin", "予約のリマインダー"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := build(testFrom, Message{To: testTo, Subject: tc.subject, Text: "corps"})

			i := bytes.Index(raw, []byte("\r\n\r\n"))
			for j, b := range raw[:i] {
				if b > 0x7f {
					t.Fatalf("byte %d of the header block is 0x%02x; headers must be 7-bit clean:\n%s", j, b, raw[:i])
				}
			}
			m := parseMessage(t, raw)
			if got := decodeSubject(t, m); got != tc.subject {
				t.Errorf("subject round trip = %q, want %q", got, tc.subject)
			}
			// Folding must not disturb the other fields.
			if got := m.Header.Get("To"); got != testTo {
				t.Errorf("To = %q, want %q", got, testTo)
			}
			if len(m.Header) != 7 {
				t.Errorf("message has %d header fields, want 7: %v", len(m.Header), m.Header)
			}
		})
	}
}

// TestBuildKeepsBodyUTF8 pins the other half of the encoding contract: the body is
// declared utf-8 and shipped as utf-8, byte for byte.
func TestBuildKeepsBodyUTF8(t *testing.T) {
	text := "Bonjour Léo,\n\nRappel : Dentiste — château d'Écouen à 16 h 30 🦷\n"
	m := parseMessage(t, build(testFrom, Message{To: testTo, Subject: "Rappel", Text: text}))

	_, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("Content-Type does not parse: %v", err)
	}
	if params["charset"] != "utf-8" {
		t.Errorf("charset = %q, want utf-8", params["charset"])
	}
	if got, want := readBody(t, m), normalizeCRLF(text); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// TestNormalizeCRLF: RFC 5322 line endings are CRLF, and a body carrying the bare LFs
// Go produces gets mangled differently by every MTA it meets.
func TestNormalizeCRLF(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"lf becomes crlf", "a\nb", "a\r\nb"},
		{"crlf is left alone", "a\r\nb", "a\r\nb"},
		{"mixed endings converge", "a\r\nb\nc", "a\r\nb\r\nc"},
		{"blank line", "a\n\nb", "a\r\n\r\nb"},
		{"trailing newline", "a\n", "a\r\n"},
		{"no newline at all", "a", "a"},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeCRLF(tc.in); got != tc.want {
				t.Errorf("normalizeCRLF(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The SMTP conversation
// ---------------------------------------------------------------------------

// TestSMTPSendDelivers walks the whole transaction. The envelope assertions matter as
// much as the payload: a To: header that disagrees with RCPT TO is a mail that looks
// right in the sender's logs and arrives at the wrong house.
func TestSMTPSendDelivers(t *testing.T) {
	tests := []struct {
		name string
		cfg  fakeConfig
	}{
		{"esmtp", fakeConfig{}},
		// An MTA that does not know EHLO must still work: net/smtp falls back to
		// HELO, and this package's whole premise is that the local MTA is whatever
		// the operating system provides.
		{"helo only", fakeConfig{reply: map[string]string{"EHLO": "502 5.5.1 unrecognised command\r\n"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakeSMTP(t, tc.cfg)
			m := NewSMTP(srv.addr(), testFrom)
			msg := Message{To: testTo, Subject: "Rappel : Dentiste Léo", Text: "Bonjour Léo\nà 16 h 30"}

			if err := m.Send(context.Background(), msg); err != nil {
				t.Fatalf("Send: %v", err)
			}
			sessions := srv.sessions()
			if len(sessions) != 1 {
				t.Fatalf("the MTA accepted %d messages, want 1", len(sessions))
			}
			got := sessions[0]
			if got.from != testFrom {
				t.Errorf("MAIL FROM = %q, want %q", got.from, testFrom)
			}
			if len(got.rcpts) != 1 || got.rcpts[0] != testTo {
				t.Errorf("RCPT TO = %q, want [%q]", got.rcpts, testTo)
			}

			delivered := parseMessage(t, got.data)
			if h := delivered.Header.Get("To"); h != got.rcpts[0] {
				t.Errorf("To header %q disagrees with the envelope recipient %q", h, got.rcpts[0])
			}
			if s := decodeSubject(t, delivered); s != msg.Subject {
				t.Errorf("delivered subject = %q, want %q", s, msg.Subject)
			}
			if b := readBody(t, delivered); b != wireBody(msg.Text) {
				t.Errorf("delivered body = %q, want %q", b, wireBody(msg.Text))
			}

			if n := m.Failures(); n != 0 {
				t.Errorf("Failures() = %d after a successful send, want 0", n)
			}
			if last := m.LastOK(); time.Since(last) > time.Minute {
				t.Errorf("LastOK() = %s, want a time around now", last)
			}
		})
	}
}

// TestSMTPSendPreservesBodyOnTheWire is about dot-stuffing. A line consisting of a
// single "." ends the DATA phase; unescaped, the rest of a digest listing everything
// after it is thrown away and the MTA still reports success. Notes and titles are free
// text, so a lone dot on a line is a matter of time.
func TestSMTPSendPreservesBodyOnTheWire(t *testing.T) {
	srv := newFakeSMTP(t, fakeConfig{})
	m := NewSMTP(srv.addr(), testFrom)
	text := "Ligne un\n.\nLigne deux\n.. et trois\n...\nfin"

	if err := m.Send(context.Background(), Message{To: testTo, Subject: "Rappel", Text: text}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	sessions := srv.sessions()
	if len(sessions) != 1 {
		t.Fatalf("the MTA accepted %d messages, want 1", len(sessions))
	}
	delivered := parseMessage(t, sessions[0].data)
	if got, want := readBody(t, delivered), wireBody(text); got != want {
		t.Errorf("delivered body = %q, want %q", got, want)
	}
}

// TestSMTPSendReportsFailures: a swallowed mail error is invisible, and email exists
// here precisely because the other channel fails silently. Every one of these has to
// come back as an error and count against /healthz.
func TestSMTPSendReportsFailures(t *testing.T) {
	tests := []struct {
		name string
		cfg  fakeConfig
		addr string // overrides the fake server's address
	}{
		// Port 1 is not a listener anyone runs an MTA on, so the dial is refused
		// without leaving the loopback interface.
		{"connection refused", fakeConfig{}, "127.0.0.1:1"},
		{"greeting refuses the connection", fakeConfig{reply: map[string]string{"GREETING": "421 4.3.2 service not available\r\n"}}, ""},
		{"sender rejected", fakeConfig{reply: map[string]string{"MAIL": "550 5.7.1 sender denied\r\n"}}, ""},
		{"recipient rejected", fakeConfig{reply: map[string]string{"RCPT": "550 5.1.1 no such user here\r\n"}}, ""},
		{"data refused", fakeConfig{reply: map[string]string{"DATA": "554 5.7.1 not accepting messages\r\n"}}, ""},
		{"message refused after the dot", fakeConfig{reply: map[string]string{"EOD": "554 5.7.1 message rejected by policy\r\n"}}, ""},
		{"connection dropped before the greeting", fakeConfig{dropAfter: "GREETING"}, ""},
		{"connection dropped at EHLO", fakeConfig{dropAfter: "EHLO"}, ""},
		{"connection dropped mid conversation", fakeConfig{dropAfter: "RCPT"}, ""},
		{"connection dropped after the message", fakeConfig{dropAfter: "EOD"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakeSMTP(t, tc.cfg)
			addr := srv.addr()
			if tc.addr != "" {
				addr = tc.addr
			}
			m := NewSMTP(addr, testFrom)

			err := m.Send(context.Background(), Message{To: testTo, Subject: "Rappel", Text: "corps"})
			if err == nil {
				t.Fatal("Send returned nil for a message the MTA never accepted")
			}
			if !strings.Contains(err.Error(), "send mail to") {
				t.Errorf("error %q does not say what failed", err)
			}
			// Addresses stay out of error strings, which end up in logs.
			if strings.Contains(err.Error(), "damien@") {
				t.Errorf("error leaks the recipient: %v", err)
			}
			if !strings.Contains(err.Error(), redact(testTo)) {
				t.Errorf("error %q does not identify the recipient even in redacted form", err)
			}
			if n := m.Failures(); n != 1 {
				t.Errorf("Failures() = %d after one failed send, want 1", n)
			}
			if last := m.LastOK(); !last.IsZero() {
				t.Errorf("LastOK() = %s after a failure, want the zero time", last)
			}
		})
	}
}

// TestSMTPFailuresAreConsecutive: /healthz reads this counter, so it has to rise while
// the MTA is unreachable and fall back to zero the moment it is not. A counter that
// only grows makes the health endpoint permanently red and therefore ignored.
func TestSMTPFailuresAreConsecutive(t *testing.T) {
	srv := newFakeSMTP(t, fakeConfig{})
	m := NewSMTP(srv.addr(), testFrom)
	msg := Message{To: testTo, Subject: "Rappel", Text: "corps"}

	srv.refuse.Store(true)
	for i := 1; i <= 3; i++ {
		if err := m.Send(context.Background(), msg); err == nil {
			t.Fatal("Send returned nil while the MTA was refusing recipients")
		}
		if n := m.Failures(); n != i {
			t.Errorf("Failures() = %d after %d failures, want %d", n, i, i)
		}
	}
	if last := m.LastOK(); !last.IsZero() {
		t.Errorf("LastOK() = %s before any message was accepted, want the zero time", last)
	}

	srv.refuse.Store(false)
	if err := m.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send after recovery: %v", err)
	}
	if n := m.Failures(); n != 0 {
		t.Errorf("Failures() = %d after a successful send, want 0", n)
	}
	if last := m.LastOK(); time.Since(last) > time.Minute {
		t.Errorf("LastOK() = %s, want a time around now", last)
	}

	srv.refuse.Store(true)
	if err := m.Send(context.Background(), msg); err == nil {
		t.Fatal("Send returned nil while the MTA was refusing recipients")
	}
	if n := m.Failures(); n != 1 {
		t.Errorf("Failures() = %d after one failure following a success, want 1", n)
	}
}

// TestSMTPSendHonoursContext: a wedged MTA must not hold a scheduler tick open. The
// fake accepts the connection and then says nothing, which is what a hung server looks
// like from the client side.
func TestSMTPSendHonoursContext(t *testing.T) {
	tests := []struct {
		name string
		ctx  func(t *testing.T) context.Context
		want error
	}{
		{"cancelled", func(t *testing.T) context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			t.Cleanup(cancel)
			return ctx
		}, context.Canceled},
		{"deadline passed", func(t *testing.T) context.Context {
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			t.Cleanup(cancel)
			return ctx
		}, context.DeadlineExceeded},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakeSMTP(t, fakeConfig{stall: true})
			m := NewSMTP(srv.addr(), testFrom)

			err := m.Send(tc.ctx(t), Message{To: testTo, Subject: "Rappel", Text: "corps"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("Send error = %v, want %v", err, tc.want)
			}
			// A cancelled send says nothing about the MTA — counting it would make
			// a shutdown look like an outage on the next /healthz.
			if n := m.Failures(); n != 0 {
				t.Errorf("Failures() = %d after a cancelled send, want 0", n)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The development file sink
// ---------------------------------------------------------------------------

var emlName = regexp.MustCompile(`^\d{8}-\d{6}-\d{3}-[\w.@-]*\.eml$`)

func TestFileMailerWritesWhatItClaims(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mail")
	m, err := NewFile(dir)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	if m.Dir() != dir {
		t.Errorf("Dir() = %q, want %q", m.Dir(), dir)
	}
	if m.Failures() != 0 {
		t.Errorf("Failures() = %d, want 0: the sink cannot fail to reach anything", m.Failures())
	}
	// NewFile creates the directory, since ALMANACK_MAIL_DIR defaults to a path
	// beside the database that nothing else makes.
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat sink dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm&^0o750 != 0 {
		t.Errorf("sink dir mode = %04o, want no bits outside 0750", perm)
	}

	msg := Message{To: testTo, Subject: "Rappel : Dentiste Léo", Text: "Bonjour Léo\nà 16 h 30"}
	for range 2 {
		if err := m.Send(context.Background(), msg); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	names := sinkFiles(t, dir)
	// Two messages written in the same second must not become one. A digest run
	// sends to the whole household at once, so a name without the sequence number
	// would show one mail and hide the rest.
	if len(names) != 2 {
		t.Fatalf("the sink holds %d files after two sends, want 2: %v", len(names), names)
	}
	if names[0] == names[1] {
		t.Fatalf("both sends wrote %q", names[0])
	}
	for _, name := range names {
		if !emlName.MatchString(name) {
			t.Errorf("file name %q does not match the documented timestamp-sequence-recipient form", name)
		}
	}

	path := filepath.Join(dir, names[0])
	info, err = os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm&^0o640 != 0 {
		t.Errorf("%s mode = %04o, want no bits outside 0640: the sink holds the family's diary", names[0], perm)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	written := parseMessage(t, raw)
	if got := written.Header.Get("From"); got != "almanack@localhost" {
		t.Errorf("From = %q, want almanack@localhost", got)
	}
	if got := written.Header.Get("To"); got != testTo {
		t.Errorf("To = %q, want %q", got, testTo)
	}
	if got := decodeSubject(t, written); got != msg.Subject {
		t.Errorf("Subject = %q, want %q", got, msg.Subject)
	}
	if got := readBody(t, written); got != normalizeCRLF(msg.Text) {
		t.Errorf("body = %q, want %q", got, normalizeCRLF(msg.Text))
	}
}

// TestFileMailerKeepsHostileRecipientsInTheDirectory: validate lets any non-empty
// recipient without CR or LF through, so the recipient reaches the filename. A "/" or
// a ".." there would put a dev-mode mail somewhere nothing expects to be written.
func TestFileMailerKeepsHostileRecipientsInTheDirectory(t *testing.T) {
	tests := []struct {
		name string
		to   string
	}{
		{"plus addressing", "damien+calendrier@example.org"},
		{"path separators", "../../etc/passwd"},
		{"traversal inside an address", "a/../../b@example.org"},
		{"accents", "léo@example.org"},
		{"spaces and quotes", `"Léo Weber" <leo@example.org>`},
		{"null byte", "leo\x00@example.org"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "mail")
			m, err := NewFile(dir)
			if err != nil {
				t.Fatalf("NewFile: %v", err)
			}
			if err := m.Send(context.Background(), Message{To: tc.to, Subject: "Rappel", Text: "corps"}); err != nil {
				t.Fatalf("Send: %v", err)
			}
			names := sinkFiles(t, dir)
			if len(names) != 1 {
				t.Fatalf("the sink holds %d files, want 1: %v", len(names), names)
			}
			if strings.ContainsAny(names[0], `/\`) || !emlName.MatchString(names[0]) {
				t.Errorf("file name %q is not a plain name in the sink directory", names[0])
			}
			// Nothing may appear beside the sink directory itself.
			outside, err := os.ReadDir(parent)
			if err != nil {
				t.Fatalf("read parent: %v", err)
			}
			if len(outside) != 1 || outside[0].Name() != "mail" {
				t.Errorf("the send touched %v outside the sink directory", outside)
			}
		})
	}
}

func TestNewFileRejectsAnUnusableDirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	path := filepath.Join(file, "mail")
	m, err := NewFile(path)
	if err == nil {
		t.Fatalf("NewFile(%q) = %v, want an error", path, m)
	}
	// Dev mode fatals on this, so the message has to name the path that is wrong.
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the directory", err)
	}
}

// TestFileMailerReportsWriteFailures: the sink is what a developer trusts when they
// check whether the notification pipeline fired, so a write that did not happen has to
// come back as an error rather than as an empty directory.
func TestFileMailerReportsWriteFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	m, err := NewFile(dir)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err = m.Send(context.Background(), Message{To: testTo, Subject: "Rappel", Text: "corps"})
	if err == nil {
		t.Fatal("Send returned nil although the sink directory is not writable")
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error %q does not name the file it could not write", err)
	}
}

func sinkFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read sink dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// TestRedact: log lines and error strings travel — to journald, to a bug report, to a
// pasted terminal — and an address in one of them is the family's email address in a
// place nobody meant to publish. Enough must survive to tell two recipients apart.
func TestRedact(t *testing.T) {
	tests := []struct {
		name, addr, want string
	}{
		{"ordinary address", "damien@example.org", "d***@example.org"},
		{"long local part", "damien.weber+cal@example.org", "d***@example.org"},
		{"single character local part", "d@example.org", "***"},
		{"no local part", "@example.org", "***"},
		{"not an address", "damien", "***"},
		{"empty", "", "***"},
		{"accented", "léo@example.org", "l***@example.org"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := redact(tc.addr)
			if got != tc.want {
				t.Errorf("redact(%q) = %q, want %q", tc.addr, got, tc.want)
			}
			if len(tc.addr) > 2 && strings.Contains(got, tc.addr) {
				t.Errorf("redact(%q) returned the address unchanged", tc.addr)
			}
		})
	}
}

func TestSafeName(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"ordinary address", "damien@example.org", "damien@example.org"},
		{"plus addressing", "damien+cal@example.org", "damien_cal@example.org"},
		{"path separators", "../../etc/passwd", ".._.._etc_passwd"},
		{"backslashes", `a\b@example.org`, "a_b@example.org"},
		{"spaces", "Léo Weber", "L_o_Weber"},
		{"control characters", "leo\x00\n@example.org", "leo_@example.org"},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := safeName(tc.in)
			if got != tc.want {
				t.Errorf("safeName(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if got != "" && (strings.ContainsAny(got, `/\`) || got != filepath.Base(got)) {
				t.Errorf("safeName(%q) = %q, which is not a single path element", tc.in, got)
			}
		})
	}
}

// A body carrying what used to be the fixed boundary must not be able to restructure
// the message around it. The delimiter was a compile-time constant, so a line reading
// "--almanack-boundary-x7f3a9" in an event title closed the part it was inside and let
// whatever followed describe parts of its own — the author of a title choosing the
// structure of a message somebody else receives.
func TestABodyCannotForgeTheMultipartBoundary(t *testing.T) {
	const forged = "--almanack-boundary-x7f3a9"
	msg := Message{
		To:      testTo,
		Subject: "Rappel",
		Text: forged + "\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" +
			"Texte que personne n'a écrit\r\n" + forged + "--",
		HTML: "<p>Bonjour</p>",
	}
	m := parseMessage(t, build(testFrom, msg))

	_, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("Content-Type does not parse: %v", err)
	}
	if params["boundary"] == forged[2:] {
		t.Fatal("the boundary is still the constant the body can be written to match")
	}

	// Exactly the two parts this message really has, with the forgery inert inside the
	// first of them.
	r := multipart.NewReader(m.Body, params["boundary"])
	var types []string
	for {
		part, err := r.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next part: %v", err)
		}
		mt, _, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if err != nil {
			t.Fatalf("part Content-Type does not parse: %v", err)
		}
		types = append(types, mt)
		if _, err := io.ReadAll(part); err != nil {
			t.Fatalf("read part: %v", err)
		}
	}
	if len(types) != 2 || types[0] != "text/plain" || types[1] != "text/html" {
		t.Errorf("parts = %v, want exactly [text/plain text/html]: the body added parts of its own", types)
	}
}

// Two messages never share a delimiter, which is what makes it unguessable to whatever
// is composing the body.
func TestEachMessageGetsItsOwnBoundary(t *testing.T) {
	msg := Message{To: testTo, Subject: "Rappel", Text: "Bonjour", HTML: "<p>Bonjour</p>"}
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		m := parseMessage(t, build(testFrom, msg))
		_, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
		if err != nil {
			t.Fatalf("Content-Type does not parse: %v", err)
		}
		b := params["boundary"]
		if b == "" {
			t.Fatal("no boundary parameter")
		}
		if len(b) > 70 {
			t.Errorf("boundary %q is %d characters, over the RFC 2046 limit of 70", b, len(b))
		}
		if seen[b] {
			t.Fatalf("boundary %q was reused between messages", b)
		}
		seen[b] = true
	}
}
