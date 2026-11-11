package httpapi

// SPEC-W47-C (UIMOB-06): USSD pack-menu action dispatch, end-to-end.
//
// When a tenant pack defines ussd.menu items with an action (book / status /
// handoff / info), conversation-service menu mode resolves the selection and
// returns the action with the closing reply (channel.USSDTurnResponse).
// This dispatcher then performs the real effect AND delivers the promised
// confirmation SMS through notification-worker's paced send path
// (POST /v1/notify/ussd-action → PacedSendWorkflow kind ussd_action):
//
//   - book:    booking-service helpdesk ticket (category "ussd", normal
//     priority) so staff schedule the requested booking; SMS confirms the
//     request was received.
//   - status:  booking-service booking lookup by caller phone (GET
//     /v1/bookings?contact_phone=, internal-token-gated — the W47-A voice
//     lookup surface, reused); the SMS carries the live status (or an
//     honest "no booking found" / "could not retrieve" fallback).
//   - handoff: booking-service helpdesk ticket (high priority) — the
//     lightest real staff-ticket surface, mirroring conversation-service's
//     escalation handoff (services/conversation-service/app/helpdesk.py);
//     SMS confirms the callback request.
//   - info:    no booking-service effect; the SMS carries the
//     tenant-configured text (pack ussd.menu[].text, falling back to the
//     item label).
//
// Idempotency: the caller (ussdTurn) claims session+action in the session
// store BEFORE dispatching, and notification-worker derives the workflow id
// from session+action — an Africa's Talking at-least-once redelivery never
// double-books or double-sends.
//
// Downstream-call posture mirrors the existing invoke patterns
// (channel.ResolveInvokeBase for direct in-cluster vs Dapr sidecar bases,
// X-Internal-Token fail-closed machine auth). No effect failure ever changes
// the USSD reply the caller already received; every failure is logged
// loudly for ops replay.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/opendesk/messaging-gateway/internal/channel"
)

// USSDActionRequest carries one resolved menu-action dispatch.
type USSDActionRequest struct {
	TenantID   string
	TenantSlug string
	SessionID  string
	Phone      string // E.164 caller (callback.msishopdn)
	Action     string // book | status | handoff | info
	Label      string // selected menu item label (e.g. "Book an appointment")
	// Text is the tenant-configured info text (pack ussd.menu[].text) for
	// action=info; empty falls back to Label.
	Text           string
	ConversationID string // conversation-service conversation linkage (audit)
}

// USSDActionDispatcher performs the menu-action side effects + the
// confirmation SMS.
type USSDActionDispatcher interface {
	Dispatch(ctx context.Context, req USSDActionRequest) error
}

type httpUSSDActionDispatcher struct {
	bookingBase  string
	bookingToken string // MESSAGING_BOOKING_INTERNAL_TOKEN (K2 toward booking-service)
	notifyBase   string
	notifyToken  string // MESSAGING_NOTIFICATION_INTERNAL_TOKEN (K2 toward notification-worker)
	hc           *http.Client
	log          *zap.Logger
}

// NewUSSDActionDispatcher builds the production dispatcher. bookingBase /
// notifyBase come from channel.ResolveInvokeBase (direct in-cluster default
// or the Dapr sidecar when cfg.DaprSidecar=true). A nil logger becomes a
// no-op logger.
func NewUSSDActionDispatcher(bookingBase, bookingToken, notifyBase, notifyToken string, log *zap.Logger) USSDActionDispatcher {
	if log == nil {
		log = zap.NewNop()
	}
	return &httpUSSDActionDispatcher{
		bookingBase:  bookingBase,
		bookingToken: bookingToken,
		notifyBase:   notifyBase,
		notifyToken:  notifyToken,
		hc:           &http.Client{Timeout: 10 * time.Second},
		log:          log,
	}
}

// Dispatch performs the action effect and then sends the confirmation SMS.
// The SMS is attempted even when the booking-service effect fails (the USSD
// reply already promised it); effect failures are returned joined so the
// caller logs them.
func (d *httpUSSDActionDispatcher) Dispatch(ctx context.Context, req USSDActionRequest) error {
	req.Action = strings.ToLower(strings.TrimSpace(req.Action))
	var errs []error
	switch req.Action {
	case "book", "handoff":
		if err := d.createTicket(ctx, req); err != nil {
			d.log.Error("ussd action: helpdesk ticket creation failed (SMS still sent)",
				zap.String("action", req.Action), zap.String("tenant_slug", req.TenantSlug),
				zap.String("session_id", req.SessionID), zap.Error(err))
			errs = append(errs, err)
		}
	case "status":
		req.Text = d.statusText(ctx, req)
	case "info":
		if strings.TrimSpace(req.Text) == "" {
			// Tenant configured no dedicated info text: the label is the
			// only tenant-authored content available — carry it.
			req.Text = req.Label
		}
	default:
		// Forward-compat: unknown action kinds still get the confirmation
		// the reply promised (the request was received).
		d.log.Warn("ussd action: unknown action kind, sending generic confirmation",
			zap.String("action", req.Action), zap.String("tenant_slug", req.TenantSlug),
			zap.String("session_id", req.SessionID))
	}
	if err := d.sendConfirmation(ctx, req); err != nil {
		d.log.Error("ussd action: confirmation SMS NOT sent",
			zap.String("action", req.Action), zap.String("tenant_slug", req.TenantSlug),
			zap.String("session_id", req.SessionID), zap.String("phone", req.Phone), zap.Error(err))
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// createTicket opens a booking-service helpdesk ticket for book/handoff —
// the same surface conversation-service's escalation handoff uses
// (POST /v1/helpdesk/tickets with X-Internal-Token + tenant binding).
func (d *httpUSSDActionDispatcher) createTicket(ctx context.Context, req USSDActionRequest) error {
	if d.bookingToken == "" {
		return fmt.Errorf("MESSAGING_BOOKING_INTERNAL_TOKEN unset; booking-service would fail closed")
	}
	priority := "normal"
	kind := "booking request"
	if req.Action == "handoff" {
		priority = "high"
		kind = "human handoff"
	}
	subject := fmt.Sprintf("USSD %s: %s (%s)", kind, req.Label, req.Phone)
	body := map[string]any{
		"subject":  subject,
		"channel":  "ussd",
		"priority": priority,
	}
	// Best-effort conversation linkage for staff audit (optional column,
	// zero when the conversation row was never created).
	if req.ConversationID != "" {
		body["conversation_id"] = req.ConversationID
	}
	payload, _ := json.Marshal(body)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		d.bookingBase+"/v1/helpdesk/tickets", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Internal-Token", d.bookingToken)
	httpReq.Header.Set("X-Service-Name", "messaging-gateway")
	httpReq.Header.Set("X-Tenant-Slug", req.TenantSlug)
	var out struct {
		ID string `json:"id"`
	}
	if err := d.do(httpReq, http.StatusCreated, &out); err != nil {
		return fmt.Errorf("create helpdesk ticket: %w", err)
	}
	d.log.Info("ussd action: helpdesk ticket created",
		zap.String("action", req.Action), zap.String("tenant_slug", req.TenantSlug),
		zap.String("session_id", req.SessionID), zap.String("ticket_id", out.ID))
	return nil
}

// statusText looks up the caller's bookings by phone (W47-A voice lookup
// surface, internal-token-gated) and renders the confirmation text. Lookup
// failures degrade to an honest generic message — the SMS still goes out.
func (d *httpUSSDActionDispatcher) statusText(ctx context.Context, req USSDActionRequest) string {
	const unavailable = "We could not retrieve your booking status right now. Our team has your request and will follow up shortly."
	if d.bookingToken == "" {
		d.log.Error("ussd action status: MESSAGING_BOOKING_INTERNAL_TOKEN unset; booking-service would fail closed",
			zap.String("tenant_slug", req.TenantSlug), zap.String("session_id", req.SessionID))
		return unavailable
	}
	u := d.bookingBase + "/v1/bookings?contact_phone=" + url.QueryEscape(req.Phone)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return unavailable
	}
	httpReq.Header.Set("X-Internal-Token", d.bookingToken)
	httpReq.Header.Set("X-Service-Name", "messaging-gateway")
	httpReq.Header.Set("X-Tenant-Slug", req.TenantSlug)
	var out struct {
		Bookings []struct {
			ID        string `json:"id"`
			Status    string `json:"status"`
			StartsAt  string `json:"starts_at"`
			Service   string `json:"service"`
			ContactID string `json:"contact_id"`
		} `json:"bookings"`
	}
	if err := d.do(httpReq, http.StatusOK, &out); err != nil {
		d.log.Warn("ussd action status: booking lookup failed, sending fallback text",
			zap.String("tenant_slug", req.TenantSlug), zap.String("session_id", req.SessionID), zap.Error(err))
		return unavailable
	}
	if len(out.Bookings) == 0 {
		return "No booking was found for this number. Dial again and choose Book, or wait for our team to reach you."
	}
	latest := out.Bookings[0]
	status := latest.Status
	if status == "" {
		status = "received"
	}
	return fmt.Sprintf("Your booking status: %s (ref %s).", status, latest.ID)
}

// sendConfirmation delivers the promised confirmation SMS through
// notification-worker's paced send path (POST /v1/notify/ussd-action,
// K2-gated — fail-closed without the token, never sent raw).
func (d *httpUSSDActionDispatcher) sendConfirmation(ctx context.Context, req USSDActionRequest) error {
	if d.notifyToken == "" {
		return fmt.Errorf("MESSAGING_NOTIFICATION_INTERNAL_TOKEN unset; notification-worker would fail closed")
	}
	payload, _ := json.Marshal(map[string]any{
		"tenant_slug": req.TenantSlug,
		"phone":       req.Phone,
		"text":        req.Text,
		"action":      req.Action,
		"session_id":  req.SessionID,
	})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		d.notifyBase+"/v1/notify/ussd-action", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Internal-Token", d.notifyToken)
	if err := d.do(httpReq, 0, nil); err != nil {
		return fmt.Errorf("send confirmation: %w", err)
	}
	return nil
}

// do executes the request, decoding the body into out on wantStatus (0 = any
// 2xx). Non-2xx surfaces status + a trimmed body (no trust in the peer).
func (d *httpUSSDActionDispatcher) do(req *http.Request, wantStatus int, out any) error {
	resp, err := d.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	if wantStatus != 0 {
		ok = resp.StatusCode == wantStatus
	}
	if !ok {
		trimmed := strings.TrimSpace(string(body))
		if len(trimmed) > 300 {
			trimmed = trimmed[:300]
		}
		return fmt.Errorf("%s %s: status %d: %s", req.Method, req.URL.Path, resp.StatusCode, trimmed)
	}
	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// resolveUSSDActionContext finds the selected menu item (label + optional
// tenant-configured info text) for the dispatch request.
func resolveUSSDActionContext(menu []channel.USSDMenuItem, resp channel.USSDTurnResponse) (label, text string) {
	for _, item := range menu {
		if item.Key == resp.Selection {
			return item.Label, item.Text
		}
	}
	return resp.Action, ""
}
