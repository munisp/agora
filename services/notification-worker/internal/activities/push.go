package activities

// Push notification fan-out (SPEC-W16 contract §1).
//
// SendPushNotification delivers one notification to every device token of a
// contact (fetched from booking-service via Dapr service invocation GET
// /internal/devices?contact_id= — Agent B owns that endpoint; this file
// codes TO the contract: response is a JSON array of
// {tenant_id, contact_id, token, platform, app}) or to an explicit token
// list in the payload. Fan-out goes through the internal/provider
// implementations (fcm live, or the FCM_MOCK=1 deterministic mock when
// explicitly opted in — default OFF, SIM-010; apns stub — iOS tokens
// surface honest "not implemented" failures until the documented TODO
// lands).
//
// Per-token failures are RESULTS, not activity errors: a Temporal retry of
// the activity must not re-deliver to tokens that already succeeded. The
// activity returns an error only for contract-level failures (missing
// title/body, no token source, device-fetch failure).

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/opendesk/notification-worker/internal/provider"
	"github.com/opendesk/notification-worker/internal/workflows"
	"go.uber.org/zap"
)

// PushDeps bundles the push notification dependencies; set by main after
// New (nil Providers map disables push — every token resolves to an
// unroutable per-token failure).
type PushDeps struct {
	// Providers maps provider name → implementation ("fcm", "apns").
	Providers map[string]provider.PushProvider
}

// DeviceToken mirrors the booking-service GET /internal/devices response
// row (SPEC-W16 contract §1 — duplicated across the service boundary, not
// shared; keep the field tags in sync with booking-service's
// internal/devices package).
type DeviceToken struct {
	TenantID  string `json:"tenant_id"`
	ContactID string `json:"contact_id"`
	Token     string `json:"token"`
	Platform  string `json:"platform"` // android | ios | web
	App       string `json:"app"`      // admin | field
}

// pushProviderFor maps a device platform to its push provider: android and
// web tokens go through FCM (web push via FCM is the PWA path), ios through
// APNs. Empty platform defaults to fcm (Android is the primary fleet).
func pushProviderFor(platform string) string {
	if platform == "ios" {
		return "apns"
	}
	return "fcm"
}

// SendPushNotification is the SendPushNotification activity (SPEC-W16
// §1). It resolves the target tokens, fans out to the per-platform
// provider, and returns per-token results.
func (a *Activities) SendPushNotification(ctx context.Context, in workflows.PacedPushNotificationSend) (workflows.PushNotificationResult, error) {
	var res workflows.PushNotificationResult
	if in.Title == "" && in.Body == "" {
		return res, fmt.Errorf("push send: title or body is required (tenant %s)", in.TenantSlug)
	}
	if len(in.Tokens) == 0 && in.ContactID == "" {
		return res, fmt.Errorf("push send: tokens or contact_id is required (tenant %s)", in.TenantSlug)
	}

	targets, err := a.resolvePushTargets(ctx, in)
	if err != nil {
		return res, err
	}
	if len(targets) == 0 {
		a.Log.Info("push send: no device tokens resolved, nothing to send",
			zap.String("tenant", in.TenantSlug), zap.String("contact_id", in.ContactID))
		return res, nil
	}

	for _, t := range targets {
		r := a.sendOnePush(ctx, in, t)
		res.Results = append(res.Results, r)
		if r.Success {
			res.Sent++
		} else {
			res.Failed++
		}
	}
	a.Log.Info("push notification fan-out complete",
		zap.String("tenant", in.TenantSlug), zap.String("contact_id", in.ContactID),
		zap.Int("targets", len(targets)), zap.Int("sent", res.Sent), zap.Int("failed", res.Failed))
	return res, nil
}

// resolvePushTargets returns the explicit token list when present,
// otherwise fetches the contact's device tokens from booking-service
// (Dapr invoke GET /internal/devices?contact_id=, X-Tenant-Slug pattern)
// and applies the optional app filter.
func (a *Activities) resolvePushTargets(ctx context.Context, in workflows.PacedPushNotificationSend) ([]workflows.PushTarget, error) {
	if len(in.Tokens) > 0 {
		out := make([]workflows.PushTarget, 0, len(in.Tokens))
		for _, t := range in.Tokens {
			if t.Token != "" {
				out = append(out, t)
			}
		}
		return out, nil
	}
	var devices []DeviceToken
	method := "internal/devices?contact_id=" + url.QueryEscape(in.ContactID)
	err := a.Dapr.InvokeServiceMethod(ctx, http.MethodGet, a.BookingAppID, method, nil,
		map[string]string{"X-Tenant-Slug": in.TenantSlug}, &devices)
	if err != nil {
		return nil, fmt.Errorf("fetch device tokens (booking %s): %w", method, err)
	}
	out := make([]workflows.PushTarget, 0, len(devices))
	for _, d := range devices {
		if d.Token == "" {
			continue
		}
		if in.App != "" && d.App != in.App {
			continue
		}
		out = append(out, workflows.PushTarget{Token: d.Token, Platform: d.Platform})
	}
	return out, nil
}

// sendOnePush delivers to one token and builds its per-token result. It
// never returns an activity-level error.
func (a *Activities) sendOnePush(ctx context.Context, in workflows.PacedPushNotificationSend, t workflows.PushTarget) workflows.PushTokenResult {
	r := workflows.PushTokenResult{Token: t.Token, Platform: t.Platform}
	name := pushProviderFor(t.Platform)
	p, ok := a.Push.Providers[name]
	if !ok || p == nil {
		r.Error = fmt.Sprintf("no push provider configured for platform %q (want %s)", t.Platform, name)
		return r
	}
	r.Provider = name
	status, body, err := p.SendPush(ctx, provider.PushMessage{
		Token: t.Token, Title: in.Title, Body: in.Body, Data: in.Data,
	})
	if err != nil {
		r.StatusCode = status
		if pe, ok := err.(*provider.Error); ok {
			r.StatusCode = pe.StatusCode
		}
		r.Unregistered = provider.Unregistered(r.StatusCode, body)
		r.Error = err.Error()
		a.Log.Warn("push token send failed",
			zap.String("provider", name), zap.String("platform", t.Platform),
			zap.Int("status", r.StatusCode), zap.Bool("unregistered", r.Unregistered),
			zap.String("token_suffix", tokenSuffix(t.Token)), zap.Error(err))
		return r
	}
	r.Success = true
	r.StatusCode = status
	return r
}

// tokenSuffix returns the last 6 chars of a device token for logs (never
// the full token — PII, mirroring the provider package's log discipline).
func tokenSuffix(token string) string {
	if len(token) <= 6 {
		return "***"
	}
	return "***" + token[len(token)-6:]
}
