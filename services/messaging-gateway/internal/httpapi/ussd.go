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
// 5xx the aggregator would retry mid-session). The 4xx/5xx set is exactly:
// 503 (callback auth not configured), 401 (wrong path secret), 429 (per-phone
// rate limit), 400 (garbage form — missing sessionId/serviceCode/phoneNumber).
//
// Authentication (SPEC-W45 K14/OOS-06): the route is
// POST /ussd/callback/{secret} — the shared secret lives in the path (the
// Africa's Talking dashboard only lets you configure a callback URL, no
// custom headers). AT_CALLBACK_SECRET unset fails CLOSED (503); a wrong
// secret gets 401 via a constant-time compare. The phoneNumber form field
// is UNVERIFIED aggregator-asserted input: it keys only best-effort abuse
// control (the per-phone rate limit) and session continuity — never trust
// it for identity/authorization decisions.
package httpapi

import (
	"context"
	"crypto/subtle"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
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

	// Actions dispatches pack-menu action selections (book / status /
	// handoff / info) end-to-end — booking-service effect + the promised
	// confirmation SMS via notification-worker (SPEC-W47-C UIMOB-06). nil:
	// the END reply still promises the confirmation, so a nil dispatcher
	// logs an ERROR per action selection (loud misconfiguration, never a
	// silent no-op).
	Actions USSDActionDispatcher

	// CallbackSecret (AT_CALLBACK_SECRET) authenticates the callback path
	// (K14). Empty = fail-closed: every callback answers 503.
	CallbackSecret string
	// RatePerMinute bounds callbacks per phoneNumber (default 30, sliding
	// window). RateLimiter may be injected (tests); nil lazily builds the
	// default in-memory limiter (see ratelimit.go for the per-replica
	// residual note).
	RatePerMinute int
	RateLimiter   *PhoneRateLimiter

	rlMu sync.Mutex
}

// ussdFallbackLine is shown when conversation-service is unreachable —
// honest, short, low-literacy friendly. The session ends (END) so the
// subscriber is not stuck in a broken session.
const ussdFallbackLine = "Service unavailable. Please try again later."

// ussdDefaultRatePerMinute is the K14 per-phone sliding-window default.
const ussdDefaultRatePerMinute = 30

// limiter returns the per-phone rate limiter, lazily building the default.
func (c *USSDConfig) limiter() *PhoneRateLimiter {
	c.rlMu.Lock()
	defer c.rlMu.Unlock()
	if c.RateLimiter == nil {
		limit := c.RatePerMinute
		if limit <= 0 {
			limit = ussdDefaultRatePerMinute
		}
		c.RateLimiter = NewPhoneRateLimiter(limit, time.Minute)
	}
	return c.RateLimiter
}

// handleUSSDCallback implements SPEC-W12 §1 + the K14/OOS-06 auth contract:
// the shared secret rides in the path (/ussd/callback/{secret}) and is
// checked BEFORE any body parsing or logging; then the per-phone rate
// limit; then the session state machine.
func (s *Server) handleUSSDCallback(w http.ResponseWriter, r *http.Request) {
	if s.USSD == nil || s.USSD.CallbackSecret == "" {
		// Fail-closed (K14): an unauthenticated USSD callback endpoint is a
		// free conversation-service amplifier. 503, not a silent END line.
		s.Log.Warn("ussd callback rejected: AT_CALLBACK_SECRET not configured")
		writeError(w, http.StatusServiceUnavailable, "ussd callback not configured (AT_CALLBACK_SECRET)")
		return
	}
	provided := chi.URLParam(r, "secret")
	if subtle.ConstantTimeCompare([]byte(provided), []byte(s.USSD.CallbackSecret)) != 1 {
		// Never log the presented secret (it may be a near-miss of the real
		// one); the request id correlates abuse investigations.
		s.Log.Warn("ussd callback rejected: bad path secret")
		writeError(w, http.StatusUnauthorized, "invalid callback secret")
		return
	}

	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid form body")
		return
	}
	sessionID := r.PostForm.Get("sessionId")
	serviceCode := r.PostForm.Get("serviceCode")
	// phoneNumber is UNVERIFIED aggregator-asserted input — it keys the
	// rate limiter and session record only; never an auth/identity decision.
	phone := r.PostForm.Get("phoneNumber")
	text := r.PostForm.Get("text")
	if sessionID == "" || serviceCode == "" || phone == "" {
		writeError(w, http.StatusBadRequest, "sessionId, serviceCode and phoneNumber are required")
		return
	}
	if !s.USSD.limiter().Allow(phone) {
		s.Log.Warn("ussd callback rate limited",
			zap.String("service_code", serviceCode), zap.String("session_id", sessionID))
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}
	if s.USSD.Store == nil {
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
	resp, reply, end := s.ussdConversation(ctx, log, sess, text)

	// SPEC-W47-C (UIMOB-06): a closing menu selection carrying an action
	// dispatches it end-to-end (booking-service effect + confirmation SMS
	// via notification-worker). Idempotent per session+action: the session
	// row is deleted on END, so the claim rides a separate marker key —
	// an Africa's Talking at-least-once redelivery of the final callback
	// (which would otherwise rebuild a fresh session and re-dispatch) hits
	// the marker and skips. Claim BEFORE dispatch (at-most-once; failures
	// are logged loudly for ops replay).
	if end && resp.Action != "" {
		if cfg.Actions == nil {
			log.Error("ussd menu action selected but no dispatcher is wired (confirmation SMS NOT sent)",
				zap.String("action", resp.Action), zap.String("phone", phone))
		} else if s.claimUSSDAction(ctx, log, sess, resp.Action, ttl) {
			label, infoText := resolveUSSDActionContext(sess.Menu, resp)
			if err := cfg.Actions.Dispatch(ctx, USSDActionRequest{
				TenantID:       sess.TenantID,
				TenantSlug:     sess.SiteSlug, // single-site tenants: site slug is the tenant slug (channel.HTTPUSSDMenuFetcher contract)
				SessionID:      sess.ID,
				Phone:          sess.Phone,
				Action:         resp.Action,
				Label:          label,
				Text:           infoText,
				ConversationID: resp.ConversationID,
			}); err != nil {
				log.Error("ussd menu action dispatch failed",
					zap.String("action", resp.Action), zap.Error(err))
			}
		}
	}

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
// maps the response onto (reply, end): continue=true → CON, else END. The
// full response is returned so the caller can dispatch a closing menu
// action (SPEC-W47-C UIMOB-06).
func (s *Server) ussdConversation(ctx context.Context, log *zap.Logger, sess *channel.USSDSession, text string) (channel.USSDTurnResponse, string, bool) {
	if s.USSD.Conversation == nil {
		log.Warn("ussd conversation client not configured")
		return channel.USSDTurnResponse{}, ussdFallbackLine, true
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
		return channel.USSDTurnResponse{}, ussdFallbackLine, true
	}
	if resp.Reply == "" {
		return channel.USSDTurnResponse{}, ussdFallbackLine, true
	}
	return resp, resp.Reply, !resp.Continue
}

// ussdActionClaimTTL pins the session+action dispatch marker well beyond
// the session TTL: an AT redelivery of the final callback arrives within
// seconds, but the marker also covers operator replays of the callback log.
const ussdActionClaimTTL = 24 * time.Hour

// ussdActionClaimKey is the store key of the session+action dispatch marker
// (separate from the session row, which is deleted on END).
func ussdActionClaimKey(sessionID, action string) string {
	return "ussd-action-claim:" + sessionID + ":" + action
}

// claimUSSDAction records that the session+action dispatch is claimed and
// reports whether THIS callback holds the claim (first writer wins). Claim
// persistence failures fail OPEN to dispatch (logged): blocking the user's
// requested action because the dedupe marker could not be written is worse
// than the residual double-dispatch window (notification-worker's derived
// workflow id still suppresses a duplicate SMS).
func (s *Server) claimUSSDAction(ctx context.Context, log *zap.Logger, sess *channel.USSDSession, action string, sessionTTL time.Duration) bool {
	key := ussdActionClaimKey(sess.ID, action)
	existing, err := s.USSD.Store.Get(ctx, key)
	if err != nil {
		log.Warn("ussd action claim check failed, dispatching anyway", zap.String("action", action), zap.Error(err))
		return true
	}
	if existing != nil {
		log.Info("ussd action already dispatched for this session, skipping duplicate",
			zap.String("action", action))
		return false
	}
	ttl := sessionTTL
	if ttl < ussdActionClaimTTL {
		ttl = ussdActionClaimTTL
	}
	marker := &channel.USSDSession{ID: key, ServiceCode: sess.ServiceCode, Phone: sess.Phone,
		SiteSlug: sess.SiteSlug, TenantID: sess.TenantID, UpdatedAt: time.Now()}
	if err := s.USSD.Store.Save(ctx, marker, ttl); err != nil {
		log.Warn("ussd action claim save failed, dispatching anyway", zap.String("action", action), zap.Error(err))
	}
	return true
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
