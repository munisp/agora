package httpapi

// Resend inbound e-mail (2026-11-24). Agora shares the Resend account idre uses; Agora's business addresses live on
// their own receiving domain (EMAIL_SITE_MAP keys). Resend posts every account event to every webhook, so this
// endpoint verifies the Svix signature (RESEND_WEBHOOK_SECRET, "whsec_…" — the same scheme idre's mailhook.go checks),
// keeps only email.received events addressed to a known Agora address (idre's mail and delivery reports are acked and
// ignored), fetches the message body from Resend's receiving API (RESEND_API_KEY) and hands it to the same pipeline
// as the generic /webhooks/email/{secret}.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

const resendAPIBase = "https://api.resend.com"

// verifySvix: HMAC-SHA256 over "<id>.<timestamp>.<body>" with the base64 key after "whsec_"; timestamps more than
// five minutes off are refused (no replay).
func verifySvix(secret, id, ts, sigHeader string, body []byte, now time.Time) bool {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil || len(key) == 0 || id == "" || ts == "" {
		return false
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	if d := now.Sub(time.Unix(sec, 0)); d > 5*time.Minute || d < -5*time.Minute {
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(body)
	want := mac.Sum(nil)
	for _, part := range strings.Fields(sigHeader) {
		v, sig, ok := strings.Cut(part, ",")
		if !ok || v != "v1" {
			continue
		}
		if got, err := base64.StdEncoding.DecodeString(sig); err == nil && hmac.Equal(got, want) {
			return true
		}
	}
	return false
}

type resendEvent struct {
	Type string `json:"type"`
	Data struct {
		EmailID     string   `json:"email_id"`
		From        string   `json:"from"`
		To          []string `json:"to"`
		CC          []string `json:"cc"`
		ReceivedFor []string `json:"received_for"`
		Subject     string   `json:"subject"`
		MessageID   string   `json:"message_id"`
	} `json:"data"`
}

type resendReceived struct {
	From      string  `json:"from"`
	Subject   string  `json:"subject"`
	Text      *string `json:"text"`
	HTML      *string `json:"html"`
	MessageID string  `json:"message_id"`
}

func splitAddr(v string) (name, addr string) {
	if a, err := mail.ParseAddress(strings.TrimSpace(v)); err == nil {
		return a.Name, strings.ToLower(a.Address)
	}
	return "", strings.ToLower(strings.Trim(strings.TrimSpace(v), "<>"))
}

var htmlTag = regexp.MustCompile(`(?s)<(script|style)[^>]*>.*?</(script|style)>|<[^>]+>`)

// htmlToText is a last resort for HTML-only mail: tags dropped, entities for the common characters decoded.
func htmlToText(h string) string {
	h = regexp.MustCompile(`(?i)<br\s*/?>|</p>|</div>`).ReplaceAllString(h, "\n")
	t := htmlTag.ReplaceAllString(h, "")
	return strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&#39;", "'", "&quot;", `"`).Replace(t)
}

// agoraRecipient: the first recipient that is a known Agora business address ("" = not Agora's mail).
func (s *Server) agoraRecipient(lists ...[]string) string {
	for _, l := range lists {
		for _, v := range l {
			_, a := splitAddr(v)
			if _, ok := s.Sites["email:"+a]; ok {
				return a
			}
		}
	}
	return ""
}

func (s *Server) resendGet(ctx context.Context, path string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.resendBase()+path, nil)
	req.Header.Set("Authorization", "Bearer "+s.ResendAPIKey)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("resend %s: HTTP %d %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

func (s *Server) resendBase() string {
	if s.ResendBaseURL != "" {
		return s.ResendBaseURL
	}
	return resendAPIBase
}

func (s *Server) handleResendWebhook(w http.ResponseWriter, r *http.Request) {
	if s.ResendWebhookSecret == "" || s.ResendAPIKey == "" {
		writeError(w, http.StatusServiceUnavailable, "resend inbound not configured")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "unreadable body")
		return
	}
	if !verifySvix(s.ResendWebhookSecret, r.Header.Get("svix-id"), r.Header.Get("svix-timestamp"),
		r.Header.Get("svix-signature"), body, time.Now()) {
		writeError(w, http.StatusUnauthorized, "bad signature")
		return
	}
	var ev resendEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ignored"}) // never make the provider retry a bad body
		return
	}
	if ev.Type != "email.received" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ignored"}) // delivery reports, idre's events, ...
		return
	}
	to := s.agoraRecipient(ev.Data.ReceivedFor, ev.Data.To, ev.Data.CC)
	if to == "" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ignored"}) // not an Agora business address (e.g. idre's)
		return
	}
	var em resendReceived
	if err := s.resendGet(r.Context(), "/emails/receiving/"+ev.Data.EmailID, &em); err != nil {
		s.Log.Warn("resend: fetching the received e-mail failed", zap.String("email_id", ev.Data.EmailID), zap.Error(err))
		writeError(w, http.StatusBadGateway, "could not fetch the message") // Resend retries
		return
	}
	name, from := splitAddr(firstNonEmptyStr(em.From, ev.Data.From))
	text := ""
	if em.Text != nil {
		text = *em.Text
	} else if em.HTML != nil {
		text = htmlToText(*em.HTML)
	}
	text = LatestEmailText(text)
	if !strings.Contains(from, "@") || text == "" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ignored"})
		return
	}
	msgID := s.acceptInboundEmail(from, name, to, firstNonEmptyStr(em.Subject, ev.Data.Subject), text,
		firstNonEmptyStr(em.MessageID, ev.Data.MessageID, "resend-"+ev.Data.EmailID))
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted", "message_id": msgID})
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
