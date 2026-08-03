// USSD callback handler (SPEC-W12 Agent A, contract §1).
//
// Africa's Talking posts application/x-www-form-urlencoded callbacks with
// sessionId, serviceCode, phoneNumber, text (text = cumulative "1*2*3"
// input, empty on the first request of a session). The answer is
// text/plain prefixed "CON " (continue) or "END " (terminate).
//
// Reliability contract: unlike the fire-and-forget provider webhooks, USSD
// is synchronous request/reply — the aggregator shows our response body to
// the subscriber. Processing is bounded by the shared 25s webhook context;
// internal failures are logged and surfaced as a generic END line (never a
// 5xx the aggregator would retry mid-session). The only 4xx is 400 for a
// garbage form (missing sessionId/serviceCode/phoneNumber).
package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/opendesk/messaging-gateway/internal/channel"
	"go.uber.org/zap"
)

// USSDConfig bundles the USSD callback dependencies (wired in main).
type USSDConfig struct {
	Sites        map[string]channel.Site // shared CHANNEL_SITE_MAP; route key "ussd:<serviceCode>"
	Store        channel.USSDSessionStore
	Menus        channel.USSDMenuFetcher  // nil: pass-through text mode for every tenant
	Conversation channel.USSDConversation // nil: every session ends with the fallback line
	SessionTTL   time.Duration            // default channel.USSDSessionTTL (180s)
}

// ussdFallbackLine is shown when conversation-service is unreachable —
// honest, short, low-literacy friendly. The session ends (END) so the
// subscriber is not stuck in a broken session.
const ussdFallbackLine = "Service unavailable. Please try again later."

// handleUSSDCallback implements SPEC-W12 §1.
func (s *Server) handleUSSDCallback(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid form body")
		return
	}
	sessionID := r.PostForm.Get("sessionId")
	serviceCode := r.PostForm.Get("serviceCode")
	phone := r.PostForm.Get("phoneNumber")
	text := r.PostForm.Get("text")
	if sessionID == "" || serviceCode == "" || phone == "" {
		writeError(w, http.StatusBadRequest, "sessionId, serviceCode and phoneNumber are required")
		return
	}
	if s.USSD == nil || s.USSD.Store == nil {
		s.Log.Warn("ussd callback: not configured, ending session",
			zap.String("service_code", serviceCode))
		writeUSSD(w, "END", ussdFallbackLine)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), webhookTimeout)
	defer cancel()
	reply, end := s.ussdTurn(ctx, sessionID, serviceCode, phone, text)
	writeUSSD(w, replyPrefix(end), reply)
}

// ussdTurn runs one callback against the session state machine and returns
// the reply body + whether the session terminates.
func (s *Server) ussdTurn(ctx context.Context, sessionID, serviceCode, phone, text string) (string, bool) {
	cfg := s.USSD
	ttl := cfg.SessionTTL
	if ttl <= 0 {
		ttl = channel.USSDSessionTTL
	}
	log := s.Log.With(zap.String("session_id", sessionID), zap.String("service_code", serviceCode))

	sess, err := cfg.Store.Get(ctx, sessionID)
	if err != nil {
		log.Warn("ussd session load failed", zap.Error(err))
		return ussdFallbackLine, true
	}
	if sess == nil {
		site, ok := cfg.Sites["ussd:"+serviceCode]
		if !ok {
			log.Info("ussd callback: no CHANNEL_SITE_MAP entry, ending session")
			return "Unknown service code.", true
		}
		sess = &channel.USSDSession{
			ID:          sessionID,
			ServiceCode: serviceCode,
			Phone:       phone,
			SiteSlug:    site.SiteSlug,
			TenantID:    site.TenantID,
		}
		// Menu fetch is best-effort (same enrichment posture as the voice
		// runtime's tenant context): failure → pass-through text mode.
		if cfg.Menus != nil {
			menu, merr := cfg.Menus.USSDMenu(ctx, site.SiteSlug)
			if merr != nil {
				log.Warn("ussd menu fetch failed, pass-through mode", zap.Error(merr))
			} else if len(menu) > 0 {
				sess.Menu = menu
			}
		}
	}

	// Every callback is forwarded to conversation-service (menu navigation
	// lives conversation-side — the resolved pack menu rides along).
	reply, end := s.ussdConversation(ctx, log, sess, text)

	if end {
		if err := cfg.Store.Delete(ctx, sessionID); err != nil {
			log.Warn("ussd session delete failed", zap.Error(err))
		}
	} else {
		sess.UpdatedAt = time.Now()
		if err := cfg.Store.Save(ctx, sess, ttl); err != nil {
			log.Warn("ussd session save failed", zap.Error(err))
			return ussdFallbackLine, true
		}
	}
	return reply, end
}

// ussdConversation forwards one callback to conversation-service via the
// synchronous request/reply contract (Agent D's POST /v1/ussd/turns) and
// maps the response onto (reply, end): continue=true → CON, else END.
func (s *Server) ussdConversation(ctx context.Context, log *zap.Logger, sess *channel.USSDSession, text string) (string, bool) {
	if s.USSD.Conversation == nil {
		log.Warn("ussd conversation client not configured")
		return ussdFallbackLine, true
	}
	resp, err := s.USSD.Conversation.Turn(ctx, channel.USSDTurnRequest{
		TenantID:    sess.TenantID,
		SiteSlug:    sess.SiteSlug,
		SessionID:   sess.ID,
		ServiceCode: sess.ServiceCode,
		PhoneNumber: sess.Phone,
		Text:        text,
		Menu:        sess.Menu,
	})
	if err != nil {
		log.Warn("ussd conversation turn failed", zap.Error(err))
		return ussdFallbackLine, true
	}
	if resp.Reply == "" {
		return ussdFallbackLine, true
	}
	return resp.Reply, !resp.Continue
}

// replyPrefix maps the end flag onto the contract prefix.
func replyPrefix(end bool) string {
	if end {
		return "END"
	}
	return "CON"
}

// writeUSSD answers text/plain "<CON|END> <reply>" (SPEC-W12 §1).
func writeUSSD(w http.ResponseWriter, prefix, reply string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(prefix + " " + reply)) //nolint:errcheck
}
