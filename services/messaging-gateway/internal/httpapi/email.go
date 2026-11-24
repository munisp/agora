package httpapi

// Inbound e-mail channel (2026-11-24). A mail forwarder (e.g. a Cloudflare Email Routing worker, or any provider's
// inbound-parse webhook) posts each customer e-mail here as JSON:
//
//	POST /webhooks/email/{secret}
//	{"from": "ada@example.com", "from_name": "Ada Obi", "to": "leadway-pensure@agora.newfire.app",
//	 "subject": "RSA balance", "text": "Hello ...", "message_id": "<abc@mail.example.com>"}
//
// The "to" address picks the business (CHANNEL_SITE_MAP key "email:<address>"); the message goes through the same
// bridge as WhatsApp (recorded in conversation-service, answered by the receptionist, reply sent from the business
// address as "Re: <subject>"). The path secret is EMAIL_INBOUND_SECRET: unset fails closed (503), wrong is 401.
// The webhook answers 202 at once and the reply runs in the background (an LLM answer can outlast a forwarder's
// timeout); the bridge dedupes on message_id, so a forwarder retry is harmless.

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/opendesk/messaging-gateway/internal/channel"
)

type inboundEmail struct {
	From      string `json:"from"`
	FromName  string `json:"from_name"`
	To        string `json:"to"`
	Subject   string `json:"subject"`
	Text      string `json:"text"`
	MessageID string `json:"message_id"`
}

// emailReplyTimeout bounds one background reply (receptionist answer + SMTP send).
const emailReplyTimeout = 150 * time.Second

var quotedHeader = regexp.MustCompile(`(?m)^\s*(On .{0,200}wrote:|-{2,}\s*Original Message\s*-{2,}|From: .+@.+)\s*$`)

// LatestEmailText keeps the customer's new words: it cuts the quoted thread ("On … wrote:", "-----Original
// Message-----", a forwarded "From:" header) and drops ">"-quoted lines.
func LatestEmailText(text string) string {
	t := strings.ReplaceAll(text, "\r\n", "\n")
	if loc := quotedHeader.FindStringIndex(t); loc != nil && loc[0] > 0 {
		t = t[:loc[0]]
	}
	var keep []string
	for _, line := range strings.Split(t, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), ">") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.TrimSpace(strings.Join(keep, "\n"))
}

func addressOnly(v string) string {
	v = strings.TrimSpace(v)
	if i := strings.LastIndex(v, "<"); i >= 0 {
		if j := strings.Index(v[i:], ">"); j > 0 {
			v = v[i+1 : i+j]
		}
	}
	return strings.ToLower(strings.TrimSpace(v))
}

func (s *Server) handleEmailWebhook(w http.ResponseWriter, r *http.Request) {
	if s.EmailInboundSecret == "" {
		writeError(w, http.StatusServiceUnavailable, "email inbound not configured")
		return
	}
	if subtle.ConstantTimeCompare([]byte(chi.URLParam(r, "secret")), []byte(s.EmailInboundSecret)) != 1 {
		writeError(w, http.StatusUnauthorized, "bad webhook secret")
		return
	}
	var in inboundEmail
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	from, to := addressOnly(in.From), addressOnly(in.To)
	if !strings.Contains(from, "@") || !strings.Contains(to, "@") {
		writeError(w, http.StatusBadRequest, "from and to must be e-mail addresses")
		return
	}
	text := LatestEmailText(in.Text)
	if text == "" {
		writeError(w, http.StatusBadRequest, "empty message")
		return
	}
	msgID := s.acceptInboundEmail(from, strings.TrimSpace(in.FromName), to, in.Subject, text, in.MessageID)
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted", "message_id": msgID})
}

// acceptInboundEmail hands one customer e-mail (already authenticated, addresses lower-cased, quoted thread cut) to
// the bridge in the background and returns its message id. Shared by the generic webhook and the Resend webhook.
func (s *Server) acceptInboundEmail(from, fromName, to, subject, text, messageID string) string {
	subject = strings.TrimSpace(subject)
	msgID := strings.TrimSpace(messageID)
	if msgID == "" {
		sum := sha256.Sum256([]byte(from + "\x00" + to + "\x00" + subject + "\x00" + text))
		msgID = "sha256-" + hex.EncodeToString(sum[:16])
	}
	body := text
	if subject != "" && !strings.HasPrefix(strings.ToLower(subject), "re:") {
		body = "Subject: " + subject + "\n\n" + text // the first mail's subject is often the question itself
	}
	msg := channel.InboundMessage{
		Channel: "email", From: from, To: to, Subject: subject, FromName: strings.TrimSpace(fromName),
		MessageID: msgID, Text: body, Timestamp: time.Now().Unix(),
	}
	s.Log.Info("inbound e-mail accepted", zap.String("to", to), zap.String("message_id", msgID))
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), emailReplyTimeout)
		defer cancel()
		if s.Bridge == nil {
			s.Log.Warn("inbound bridge not configured, dropping e-mail")
			return
		}
		if err := s.Bridge.Handle(ctx, msg, to); err != nil {
			s.Log.Warn("inbound e-mail failed", zap.String("to", to), zap.Error(err))
		}
	}()
	return msgID
}
