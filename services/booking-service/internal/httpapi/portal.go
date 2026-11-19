package httpapi

// Customer self-service portal (Wave 5 #7): magic-code login (SMS/email)
// → short-lived portal JWT → contact-scoped view/reschedule/cancel of own
// bookings. No account, no password: possessing the phone/e-mail of an
// existing contact IS the credential (same trust model as the waitlist
// claim token, SPEC-W3 §3).
//
// Flow:
//  1. POST /public/sites/{slug}/portal/request {phone|email}
//     → 6-digit code (hash stored in portal_tokens), code delivered via a
//     com.opendesk.notifications.SendPortalCode CloudEvent published to the
//     opendesk.notifications.outbox topic (notification-worker owns the
//     smtp/twilio bindings; the pubsub-kafka component scope already covers
//     both `booking` and `notification`, so no Dapr component change is
//     needed). Rate-limited to 5 requests/hour/contact (in-memory).
//  2. POST /public/sites/{slug}/portal/verify {phone|email, code}
//     → 15-minute HS256 portal JWT (PORTAL_SECRET), claims sub=contact_id,
//     tid=tenant_id, tsl=tenant_slug. 5 wrong codes lock the token.
//  3. /portal/* endpoints below carry `Authorization: Bearer <portal JWT>`
//     and are contact-scoped by construction.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/events"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

const (
	portalTokenTTL    = 10 * time.Minute // login code validity
	portalJWTTTL      = 15 * time.Minute // portal session validity
	portalMaxAttempts = 5                // wrong-code lockout per token
	portalRateLimit   = 5                // code requests per window
	portalRateWindow  = time.Hour        // rate-limit window
	portalEventType   = "com.opendesk.notifications.SendPortalCode"
	portalClaimScope  = "portal"
	errPortalBadCreds = "invalid or expired code"
)

// ---------------------------------------------------------------------------
// rate limiter (in-memory sliding window; per-service-instance)
// ---------------------------------------------------------------------------

type portalRateLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func newPortalRateLimiter() *portalRateLimiter {
	return &portalRateLimiter{hits: map[string][]time.Time{}}
}

// Allow records one hit for key and reports whether it is within the limit.
func (l *portalRateLimiter) Allow(key string, limit int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-window)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= limit {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	// bound memory: drop idle keys opportunistically
	if len(l.hits) > 10000 {
		for k, v := range l.hits {
			if len(v) == 0 {
				delete(l.hits, k)
			}
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// minimal HS256 JWT (no external dependency)
// ---------------------------------------------------------------------------

type portalClaims struct {
	Sub        string `json:"sub"` // contact id
	TenantID   string `json:"tid"`
	TenantSlug string `json:"tsl"`
	Scope      string `json:"scope"`
	IssuedAt   int64  `json:"iat"`
	ExpiresAt  int64  `json:"exp"`
}

func signPortalJWT(secret string, c portalClaims) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	body := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func verifyPortalJWT(secret, token string) (portalClaims, error) {
	var c portalClaims
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return c, errors.New("malformed token")
	}
	body := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(sig, mac.Sum(nil)) {
		return c, errors.New("bad signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(payload, &c) != nil {
		return c, errors.New("bad payload")
	}
	if c.Scope != portalClaimScope {
		return c, errors.New("not a portal token")
	}
	if time.Now().Unix() >= c.ExpiresAt {
		return c, errors.New("token expired")
	}
	if _, err := uuid.Parse(c.Sub); err != nil {
		return c, errors.New("bad subject")
	}
	if _, err := uuid.Parse(c.TenantID); err != nil {
		return c, errors.New("bad tenant")
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

type portalRequestBody struct {
	Phone   string `json:"phone"`
	Email   string `json:"email"`
	Channel string `json:"channel"` // optional explicit channel (SPEC-W81 B2)
}

// portalChannelFor resolves the portal delivery channel: it derives from
// the identifier (phone → sms, email → email) unless the caller explicitly
// selects one (SPEC-W81 B2, purely additive). whatsapp is phone-addressed,
// so it is only valid alongside a phone identifier; unknown values and
// channel/identifier mismatches are a 400 (msg != "").
func portalChannelFor(phone, email, want string) (channel, dest, msg string) {
	switch {
	case phone != "" && email == "":
		channel, dest = store.PortalChannelSMS, phone
	case email != "" && phone == "":
		channel, dest = store.PortalChannelEmail, email
	default:
		return "", "", "exactly one of phone or email is required"
	}
	switch want {
	case "", channel:
		return channel, dest, ""
	case store.PortalChannelWhatsApp:
		if channel == store.PortalChannelSMS {
			return store.PortalChannelWhatsApp, dest, ""
		}
	}
	return "", "", "channel must be one of sms, email, whatsapp (whatsapp requires a phone identifier)"
}

// portalRequestCode serves POST /public/sites/{slug}/portal/request.
// Always answers 202 (even for unknown contacts) so the endpoint cannot be
// used to enumerate customers.
func (s *server) portalRequestCode(w http.ResponseWriter, r *http.Request) {
	site, ok := s.resolveSite(w, r)
	if !ok {
		return
	}
	var req portalRequestBody
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Phone = strings.TrimSpace(req.Phone)
	req.Email = strings.TrimSpace(req.Email)
	req.Channel = strings.ToLower(strings.TrimSpace(req.Channel))
	channel, dest, cerr := portalChannelFor(req.Phone, req.Email, req.Channel)
	if cerr != "" {
		writeError(w, http.StatusBadRequest, cerr)
		return
	}
	if !s.portalLimiter.Allow(site.TenantID.String()+":"+channel+":"+dest, portalRateLimit, portalRateWindow) {
		writeError(w, http.StatusTooManyRequests, "too many code requests — try again later")
		return
	}
	contact, err := s.d.Store.GetContactByChannel(r.Context(), site.TenantID, channel, dest)
	if errors.Is(err, store.ErrNotFound) {
		// Unknown contact: indistinguishable 202 (anti-enumeration).
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "code_sent"})
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}

	code, err := sixDigitCode()
	if err != nil {
		s.internal(w, err)
		return
	}
	sum := sha256.Sum256([]byte(code))
	token := store.PortalToken{
		TenantID:  site.TenantID,
		ContactID: contact.ID,
		TokenHash: hex.EncodeToString(sum[:]),
		Channel:   channel,
		ExpiresAt: time.Now().Add(portalTokenTTL),
	}
	if err := s.d.Store.CreatePortalToken(r.Context(), &token); err != nil {
		s.internal(w, err)
		return
	}

	// Hand the plaintext code to notification-worker via the notifications
	// outbox topic — it owns the smtp/twilio bindings. A publish failure is
	// logged, not surfaced (the 202 contract must not leak contact state).
	evt := events.New("booking-service", portalEventType, site.TenantSlug, site.TenantID.String(), map[string]any{
		"channel":            channel,
		"destination":        dest,
		"code":               code,
		"contact_name":       contact.Name,
		"site_slug":          site.Slug,
		"expires_in_minutes": int(portalTokenTTL.Minutes()),
	})
	if err := s.portalPublisher().PublishEvent(r.Context(), s.d.PubSubName, s.d.NotificationsTopic, evt); err != nil {
		s.d.Logger.Error("SendPortalCode publish failed", zap.Error(err))
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "code_sent"})
}

type portalVerifyBody struct {
	Phone   string `json:"phone"`
	Email   string `json:"email"`
	Code    string `json:"code"`
	Channel string `json:"channel"` // optional explicit channel (SPEC-W81 B2)
}

// portalVerifyCode serves POST /public/sites/{slug}/portal/verify. Success
// consumes the token and returns a 15-minute portal JWT.
func (s *server) portalVerifyCode(w http.ResponseWriter, r *http.Request) {
	site, ok := s.resolveSite(w, r)
	if !ok {
		return
	}
	var req portalVerifyBody
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Phone = strings.TrimSpace(req.Phone)
	req.Email = strings.TrimSpace(req.Email)
	req.Code = strings.TrimSpace(req.Code)
	req.Channel = strings.ToLower(strings.TrimSpace(req.Channel))
	channel, dest, cerr := portalChannelFor(req.Phone, req.Email, req.Channel)
	if cerr != "" {
		writeError(w, http.StatusBadRequest, cerr)
		return
	}
	if req.Code == "" {
		writeError(w, http.StatusBadRequest, "code is required")
		return
	}
	contact, err := s.d.Store.GetContactByChannel(r.Context(), site.TenantID, channel, dest)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusUnauthorized, errPortalBadCreds)
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	token, err := s.d.Store.GetActivePortalToken(r.Context(), site.TenantID, contact.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusUnauthorized, errPortalBadCreds)
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	if token.Attempts >= portalMaxAttempts {
		writeError(w, http.StatusTooManyRequests, "too many failed attempts — request a new code")
		return
	}
	sum := sha256.Sum256([]byte(req.Code))
	if hex.EncodeToString(sum[:]) != token.TokenHash {
		if _, err := s.d.Store.IncrementPortalTokenAttempts(r.Context(), site.TenantID, token.ID); err != nil {
			s.internal(w, err)
			return
		}
		writeError(w, http.StatusUnauthorized, errPortalBadCreds)
		return
	}
	if err := s.d.Store.ConsumePortalToken(r.Context(), site.TenantID, token.ID); err != nil {
		s.internal(w, err)
		return
	}
	// SPEC-W87: persist the genuine OTP success as a time-boxed
	// verification grant so future PSTN voice sessions can honor it
	// (strict anchor rule, runtime-side). Fail-open in the safe
	// direction: a missing grant just re-OTPs the next call — the OTP
	// itself already succeeded.
	// SPEC-W94: pack policy is authoritative at WRITE time too —
	// enabled=false stops minting grants for the pack's tenants and a
	// pack ttlHours overrides the global env TTL.
	if grantEnabled, grantTTL := s.grantPolicyFor(r, site.TenantSlug); grantEnabled {
		if err := s.d.Store.UpsertVerificationGrant(r.Context(), site.TenantID, contact.ID, "otp", grantTTL); err != nil {
			s.d.Logger.Error("verification grant upsert failed (verification itself succeeded)", zap.Error(err))
		}
	}
	now := time.Now()
	jwt, err := signPortalJWT(s.d.PortalSecret, portalClaims{
		Sub:        contact.ID.String(),
		TenantID:   site.TenantID.String(),
		TenantSlug: site.TenantSlug,
		Scope:      portalClaimScope,
		IssuedAt:   now.Unix(),
		ExpiresAt:  now.Add(portalJWTTTL).Unix(),
	})
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"portal_token": jwt,
		"expires_in":   int(portalJWTTTL.Seconds()),
		"contact_id":   contact.ID,
		"contact_name": contact.Name,
	})
}

// ---------------------------------------------------------------------------
// portal-JWT middleware + contact-scoped booking endpoints
// ---------------------------------------------------------------------------

const ctxPortal ctxKey = "portal"

// portalMiddleware enforces the portal JWT on /portal/* routes. The claims
// (contact + tenant) scope every downstream query.
func (s *server) portalMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.d.PortalSecret == "" {
			writeError(w, http.StatusServiceUnavailable, "portal not configured (PORTAL_SECRET unset)")
			return
		}
		auth := r.Header.Get("Authorization")
		token := strings.TrimPrefix(auth, "Bearer ")
		if token == auth || token == "" {
			// The admin-web BFF strips inbound Authorization headers (it
			// attaches the Keycloak token itself), so browser portal sessions
			// send the token in a pass-through header instead.
			token = r.Header.Get("X-Portal-Token")
		}
		if token == "" {
			writeError(w, http.StatusUnauthorized, "portal bearer token required")
			return
		}
		claims, err := verifyPortalJWT(s.d.PortalSecret, token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid portal token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxPortal, claims)))
	})
}

func portalClaimsFrom(ctx context.Context) portalClaims {
	c, _ := ctx.Value(ctxPortal).(portalClaims)
	return c
}

// portalTenant resolves the tenant of the portal session (for timezone +
// slug needed by bookingops), using the injectable TenantBySlug hook.
func (s *server) portalTenant(ctx context.Context, claims portalClaims) (uuid.UUID, error) {
	tenantID, err := uuid.Parse(claims.TenantID)
	if err != nil {
		return uuid.Nil, err
	}
	return tenantID, nil
}

// portalListBookings serves GET /portal/bookings — the session contact's own
// bookings, newest first.
func (s *server) portalListBookings(w http.ResponseWriter, r *http.Request) {
	claims := portalClaimsFrom(r.Context())
	tenantID, err := s.portalTenant(r.Context(), claims)
	if err != nil {
		s.internal(w, err)
		return
	}
	contactID, _ := uuid.Parse(claims.Sub)
	items, err := s.d.Store.ListBookings(r.Context(), tenantID, store.BookingFilter{ContactID: &contactID})
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bookings": items})
}

// portalBookingFor loads a booking and enforces contact scoping: a booking
// that belongs to another contact is a 404 (existence is not leaked).
func (s *server) portalBookingFor(w http.ResponseWriter, r *http.Request, claims portalClaims) (store.Booking, uuid.UUID, bool) {
	var b store.Booking
	tenantID, err := s.portalTenant(r.Context(), claims)
	if err != nil {
		s.internal(w, err)
		return b, tenantID, false
	}
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return b, tenantID, false
	}
	b, err = s.d.Store.GetBooking(r.Context(), tenantID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "booking not found")
		return b, tenantID, false
	}
	if err != nil {
		s.internal(w, err)
		return b, tenantID, false
	}
	if b.ContactID.String() != claims.Sub {
		writeError(w, http.StatusNotFound, "booking not found")
		return store.Booking{}, tenantID, false
	}
	return b, tenantID, true
}

// portalRescheduleBooking serves POST /portal/bookings/{id}/reschedule —
// same availability rules as the staff endpoint (bookingops.Reschedule).
func (s *server) portalRescheduleBooking(w http.ResponseWriter, r *http.Request) {
	claims := portalClaimsFrom(r.Context())
	b, tenantID, ok := s.portalBookingFor(w, r, claims)
	if !ok {
		return
	}
	var req rescheduleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	tenant, err := s.d.TenantBySlug(r.Context(), claims.TenantSlug)
	if err != nil {
		s.internal(w, err)
		return
	}
	booking, err := s.d.Ops.Reschedule(r.Context(), tenantID, claims.TenantSlug, tenant.Timezone, b.ID, req.StartsAt)
	if err != nil {
		s.mapOpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, booking)
}

// portalCancelBooking serves POST /portal/bookings/{id}/cancel.
func (s *server) portalCancelBooking(w http.ResponseWriter, r *http.Request) {
	claims := portalClaimsFrom(r.Context())
	b, tenantID, ok := s.portalBookingFor(w, r, claims)
	if !ok {
		return
	}
	var req cancelRequest
	_ = decodeOptionalJSON(r, &req)
	booking, err := s.d.Ops.Cancel(r.Context(), tenantID, claims.TenantSlug, b.ID, defaultStr(req.Reason, "customer_portal"))
	if err != nil {
		s.mapOpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, booking)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// portalPublisher returns the configured event publisher, defaulting to the
// Dapr client (tests inject a fake via Deps.Publisher).
func (s *server) portalPublisher() EventPublisher {
	if s.d.Publisher != nil {
		return s.d.Publisher
	}
	return s.d.Dapr
}

// sixDigitCode generates a cryptographically random 6-digit login code.
func sixDigitCode() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	n := int(b[0])<<16 | int(b[1])<<8 | int(b[2])
	return fmt.Sprintf("%06d", n%1000000), nil
}
