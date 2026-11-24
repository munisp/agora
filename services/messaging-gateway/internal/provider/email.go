package provider

// Email channel replies over SMTP (2026-11-24). The receptionist answers an inbound customer e-mail from the same
// business address the customer wrote to, as "Re: <subject>", threaded with In-Reply-To/References.
//
// Config: EMAIL_SMTP_HOST (empty = channel replies disabled), EMAIL_SMTP_PORT (default 25), and optional
// EMAIL_SMTP_USERNAME / EMAIL_SMTP_PASSWORD (PLAIN auth). The dev cluster points at Mailpit, which shows every
// sent mail; a real relay needs its host and credentials.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strings"
	"time"
)

type Email struct {
	Host     string
	Port     string
	Username string
	Password string
	// send is swappable in tests.
	send func(addr string, a smtp.Auth, from string, to []string, msg []byte) error
}

func (e *Email) Configured() bool { return e != nil && e.Host != "" }

// headerSafe drops CR/LF so a subject or address can never inject headers.
func headerSafe(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(strings.TrimSpace(s))
}

// ReplySubject is "Re: <subject>" (once), or "Re: your message" for an empty subject.
func ReplySubject(subject string) string {
	s := headerSafe(subject)
	if s == "" {
		return "Re: your message"
	}
	if strings.HasPrefix(strings.ToLower(s), "re:") {
		return s
	}
	return "Re: " + s
}

// BuildReply renders the RFC 5322 message (plain text, UTF-8, quoted-printable-free: 8bit body).
func BuildReply(from, fromName, to, subject, body, inReplyTo string, now time.Time) ([]byte, string) {
	id := make([]byte, 12)
	_, _ = rand.Read(id)
	domain := "agora.local"
	if at := strings.LastIndex(from, "@"); at >= 0 {
		domain = from[at+1:]
	}
	msgID := fmt.Sprintf("<%s@%s>", hex.EncodeToString(id), headerSafe(domain))
	var b strings.Builder
	fromHdr := headerSafe(from)
	if n := headerSafe(fromName); n != "" {
		fromHdr = fmt.Sprintf("%s <%s>", mime.QEncoding.Encode("utf-8", n), headerSafe(from))
	}
	fmt.Fprintf(&b, "From: %s\r\n", fromHdr)
	fmt.Fprintf(&b, "To: %s\r\n", headerSafe(to))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", ReplySubject(subject)))
	fmt.Fprintf(&b, "Date: %s\r\n", now.UTC().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: %s\r\n", msgID)
	if r := headerSafe(inReplyTo); r != "" {
		if !strings.HasPrefix(r, "<") {
			r = "<" + r + ">"
		}
		fmt.Fprintf(&b, "In-Reply-To: %s\r\nReferences: %s\r\n", r, r)
	}
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n"))
	b.WriteString("\r\n")
	return []byte(b.String()), msgID
}

// SendReply sends one reply. ctx bounds the dial only (net/smtp has no context support).
func (e *Email) SendReply(ctx context.Context, from, fromName, to, subject, body, inReplyTo string) (string, error) {
	if !e.Configured() {
		return "", fmt.Errorf("email provider not configured")
	}
	if !strings.Contains(to, "@") || !strings.Contains(from, "@") {
		return "", fmt.Errorf("email reply needs both addresses")
	}
	port := e.Port
	if port == "" {
		port = "25"
	}
	addr := net.JoinHostPort(e.Host, port)
	msg, msgID := BuildReply(from, fromName, to, subject, body, inReplyTo, time.Now())
	var auth smtp.Auth
	if e.Username != "" {
		auth = smtp.PlainAuth("", e.Username, e.Password, e.Host)
	}
	send := e.send
	if send == nil {
		send = smtp.SendMail
	}
	done := make(chan error, 1)
	go func() { done <- send(addr, auth, headerSafe(from), []string{headerSafe(to)}, msg) }()
	select {
	case err := <-done:
		if err != nil {
			return "", fmt.Errorf("smtp send: %w", err)
		}
		return msgID, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
