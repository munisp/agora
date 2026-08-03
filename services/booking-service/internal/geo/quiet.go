package geo

// SPEC-W14 Agent D: quiet-hours/DND adoption for GeoCampaignWorkflow
// (docs/dnd-quiet-hours.md §Coordination).
//
// notification-worker's NotifyPaced activity already applies the DND 2442
// suppression guard activity-side (before acquiring a CPS token) and returns
// a PacedSendResult whose status is "sent" or "suppressed_dnd". The
// quiet-hours DEFERRAL, however, is workflow-side: scheduling workflows must
// sleep until the window opens instead of calling a bare NotifyPaced
// (notification-worker does this via workflows.GuardedPacedSend).
//
// Per the service-boundary rule (duplicated, not shared — the same rule the
// PacedSendRequest JSON contract already follows), this file duplicates the
// SMALL pieces of notification-worker/internal/pacer/guards.go (kind
// classification table + quiet-hours window math) and
// internal/workflows/paced.go (GuardedPacedSend) so the semantics mirror
// EXACTLY:
//
//   - marketing kinds (geo_campaign, promo, broadcast, drip) inside the
//     tenant's quiet-hours window (default 20:00-08:00 Africa/Lagos,
//     per-channel overrides) are deferred with a durable workflow.Sleep
//     until the window opens, then dispatched through NotifyPaced as usual;
//   - transactional kinds pass immediately — no sleep, ever;
//   - priority sends (the incident_alert fast-lane) never sleep either —
//     geo_campaign is never priority, so the geo PacedSendRequest carries
//     no Priority field and this guard only ever sees marketing traffic;
//   - an empty result status (older workers returning no payload) means
//     the send happened.
//
// Keep the classification table, window math, and status strings in sync
// with notification-worker when the contract changes.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.temporal.io/sdk/workflow"
)

// ---------------------------------------------------------------------------
// Kind classification (mirror of pacer.guards.go, SPEC-W12 contract §3)
// ---------------------------------------------------------------------------

// SendClass is the DND/quiet-hours compliance class of a paced send kind.
type SendClass string

const (
	// ClassMarketing sends are promotional: DND-suppressed (activity-side)
	// and quiet-hours deferred (workflow-side).
	ClassMarketing SendClass = "marketing"
	// ClassTransactional sends are service-triggered: exempt from both
	// guards.
	ClassTransactional SendClass = "transactional"
)

// kindClasses is the explicit classification table of SPEC-W12 contract §3,
// mirrored from notification-worker's pacer.kindClasses. Kinds not listed
// default to transactional (ClassifyKind) — a safe default: unlisted kinds
// are never suppressed or deferred.
var kindClasses = map[string]SendClass{
	// Marketing (contract §3 canonical list).
	"geo_campaign": ClassMarketing,
	"promo":        ClassMarketing,
	"broadcast":    ClassMarketing,
	"drip":         ClassMarketing,
	// Transactional (contract §3 canonical list).
	"confirmation":   ClassTransactional,
	"reminder":       ClassTransactional,
	"incident_alert": ClassTransactional, // + Priority fast-lane exemption
	"otp":            ClassTransactional,
	// Remaining in-repo kinds, transactional by nature, listed explicitly
	// so the table is auditable.
	"waitlist_claim":    ClassTransactional,
	"deposit_reminder":  ClassTransactional,
	"noshow_followup":   ClassTransactional,
	"intake_reminder":   ClassTransactional,
	"follow_up":         ClassTransactional,
	"proposal_reminder": ClassTransactional,
	"staff_alert":       ClassTransactional,
}

// ClassifyKind returns the compliance class of a paced send kind. Unknown
// kinds are transactional: the guards only ever apply to kinds explicitly
// classified as marketing.
func ClassifyKind(kind string) SendClass {
	if class, ok := kindClasses[kind]; ok {
		return class
	}
	return ClassTransactional
}

// ---------------------------------------------------------------------------
// Quiet hours (mirror of pacer.guards.go, SPEC-W12 contract §8:
// QUIET_HOURS_DEFAULT "20:00-08:00", QUIET_HOURS_OVERRIDES per-channel JSON)
// ---------------------------------------------------------------------------

// DefaultQuietHoursWindow is the contract default quiet-hours window.
const DefaultQuietHoursWindow = "20:00-08:00"

// DefaultQuietHoursTimezone is the tenant-timezone default (contract §8:
// "tenant tz (default Africa/Lagos)").
const DefaultQuietHoursTimezone = "Africa/Lagos"

// QuietWindow is a daily quiet-hours window in local wall-clock minutes
// from midnight. Overnight windows (Start > End, e.g. 20:00-08:00) are
// supported.
type QuietWindow struct {
	StartMin int // inclusive
	EndMin   int // exclusive
}

// ParseQuietWindow parses "HH:MM-HH:MM" (24h, local). Start and end must
// differ (a 24h window would be a hard block, not quiet hours).
func ParseQuietWindow(s string) (QuietWindow, error) {
	parts := strings.Split(strings.TrimSpace(s), "-")
	if len(parts) != 2 {
		return QuietWindow{}, fmt.Errorf("quiet hours window %q: want HH:MM-HH:MM", s)
	}
	start, err := parseHHMM(parts[0])
	if err != nil {
		return QuietWindow{}, fmt.Errorf("quiet hours window %q: %v", s, err)
	}
	end, err := parseHHMM(parts[1])
	if err != nil {
		return QuietWindow{}, fmt.Errorf("quiet hours window %q: %v", s, err)
	}
	if start == end {
		return QuietWindow{}, fmt.Errorf("quiet hours window %q: start and end must differ", s)
	}
	return QuietWindow{StartMin: start, EndMin: end}, nil
}

func parseHHMM(s string) (int, error) {
	hm := strings.Split(strings.TrimSpace(s), ":")
	if len(hm) != 2 {
		return 0, fmt.Errorf("bad time %q (want HH:MM)", s)
	}
	h, err := strconv.Atoi(hm[0])
	if err != nil || h < 0 || h > 23 {
		return 0, fmt.Errorf("bad hour in %q", s)
	}
	m, err := strconv.Atoi(hm[1])
	if err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("bad minute in %q", s)
	}
	return h*60 + m, nil
}

// Contains reports whether t (whose Location is used as the local clock)
// falls inside the window.
func (w QuietWindow) Contains(t time.Time) bool {
	clock := t.Hour()*60 + t.Minute()
	if w.StartMin < w.EndMin {
		return clock >= w.StartMin && clock < w.EndMin
	}
	// Overnight window: [start, midnight) ∪ [midnight, end).
	return clock >= w.StartMin || clock < w.EndMin
}

// OpenAfter returns the next instant the window opens (ends), assuming t is
// currently inside it. The result shares t's Location.
func (w QuietWindow) OpenAfter(t time.Time) time.Time {
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	open := day.Add(time.Duration(w.EndMin) * time.Minute)
	if !open.After(t) {
		// Overnight window and t is past the start: the open is tomorrow.
		open = open.Add(24 * time.Hour)
	}
	return open
}

// QuietHoursConfig carries the resolved quiet-hours configuration. It is
// passed INTO the workflow (GeoCampaignInput) at schedule time so replay
// stays deterministic when the QUIET_HOURS_* env changes between runs of
// the same workflow.
type QuietHoursConfig struct {
	// DefaultWindow applies to every channel without an override
	// (QUIET_HOURS_DEFAULT, default "20:00-08:00").
	DefaultWindow string
	// Overrides maps channel → "HH:MM-HH:MM" window
	// (QUIET_HOURS_OVERRIDES, JSON object).
	Overrides map[string]string
	// Timezone is the tenant's IANA timezone; empty defaults to
	// Africa/Lagos (contract §8).
	Timezone string
}

// windowFor resolves the window for a channel (override, else default).
func (c QuietHoursConfig) windowFor(channel string) (QuietWindow, error) {
	s := c.DefaultWindow
	if s == "" {
		s = DefaultQuietHoursWindow
	}
	if ov, ok := c.Overrides[channel]; ok && strings.TrimSpace(ov) != "" {
		s = ov
	}
	return ParseQuietWindow(s)
}

// QuietHoursOpenAt reports whether a send on channel at instant now falls
// inside the configured quiet-hours window (evaluated in the tenant
// timezone), and if so returns the instant the window opens. now itself is
// in any Location; the answer instant is in the tenant timezone (its
// absolute value is what matters).
func QuietHoursOpenAt(now time.Time, channel string, cfg QuietHoursConfig) (open time.Time, inWindow bool, err error) {
	tz := cfg.Timezone
	if tz == "" {
		tz = DefaultQuietHoursTimezone
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("quiet hours timezone %q: %w", tz, err)
	}
	window, err := cfg.windowFor(channel)
	if err != nil {
		return time.Time{}, false, err
	}
	local := now.In(loc)
	if !window.Contains(local) {
		return time.Time{}, false, nil
	}
	return window.OpenAfter(local), true, nil
}

// QuietHoursFromEnv mirrors notification-worker's
// workflows.QuietHoursFromEnv: it builds the config handed to
// guardedPacedSend from plain strings (GeoCampaignInput fields, populated
// from the QUIET_HOURS_* env at schedule time). tz defaults to
// Africa/Lagos, window to 20:00-08:00 (SPEC-W12 §8).
func QuietHoursFromEnv(defaultWindow, tz string, overrides map[string]string) QuietHoursConfig {
	if defaultWindow == "" {
		defaultWindow = DefaultQuietHoursWindow
	}
	if tz == "" {
		tz = DefaultQuietHoursTimezone
	}
	return QuietHoursConfig{DefaultWindow: defaultWindow, Overrides: overrides, Timezone: tz}
}

// ParseQuietHoursOverrides parses the QUIET_HOURS_OVERRIDES env value (a
// JSON object of per-channel windows, e.g. {"sms":"22:00-06:00"}) at
// schedule time. An empty value yields nil overrides.
func ParseQuietHoursOverrides(env string) (map[string]string, error) {
	env = strings.TrimSpace(env)
	if env == "" {
		return nil, nil
	}
	var overrides map[string]string
	if err := json.Unmarshal([]byte(env), &overrides); err != nil {
		return nil, fmt.Errorf("QUIET_HOURS_OVERRIDES: %w", err)
	}
	return overrides, nil
}

// ---------------------------------------------------------------------------
// Guarded paced send (mirror of workflows.GuardedPacedSend + the
// PacedSendResult contract, SPEC-W12)
// ---------------------------------------------------------------------------

// Paced send completion statuses (mirror of notification-worker's
// workflows.PacedSendStatus* — the JSON contract of the NotifyPaced result).
const (
	// PacedSendStatusSent means NotifyPaced dispatched the send to its
	// channel binding.
	PacedSendStatusSent = "sent"
	// PacedSendStatusSuppressedDND means the DND guard stopped a marketing
	// send: the recipient is on the NCC 2442 global list or the tenant's
	// opt-out list. The send consumed no CPS token; suppression is a
	// completion status, not an error.
	PacedSendStatusSuppressedDND = "suppressed_dnd"
)

// PacedSendResult mirrors notification-worker's workflows.PacedSendResult
// (service boundary: duplicated, not shared); the JSON contract must stay
// field-compatible.
type PacedSendResult struct {
	Status string `json:"status"` // sent | suppressed_dnd
	// Reason is the suppression reason (tenant_optout | global_dnd) when
	// Status is suppressed_dnd.
	Reason string `json:"reason,omitempty"`
}

// guardedPacedSend executes one paced send with the SPEC-W12 §3 quiet-hours
// guard applied workflow-side, mirroring notification-worker's
// workflows.GuardedPacedSend exactly:
//
//   - MARKETING kinds (geo_campaign, promo, broadcast, drip) arriving inside
//     the tenant's quiet-hours window are DEFERRED: the workflow durably
//     Sleeps until the window opens (default 20:00-08:00 Africa/Lagos,
//     per-channel overrides via quiet.Overrides), then sends.
//   - TRANSACTIONAL kinds pass immediately — no sleep, ever. (Priority
//     sends never sleep either; geo_campaign is never priority.)
//
// DND suppression itself is activity-side (NotifyPaced checks the registry
// before acquiring a CPS token); the returned PacedSendResult carries
// suppressed_dnd for the workflow to record. The caller must have
// configured ActivityOptions on ctx.
func guardedPacedSend(ctx workflow.Context, req PacedSendRequest, quiet QuietHoursConfig) (PacedSendResult, error) {
	var res PacedSendResult
	if ClassifyKind(req.Kind) == ClassMarketing {
		channel := ""
		if req.Geo != nil {
			channel = req.Geo.Channel
		}
		open, inWindow, err := QuietHoursOpenAt(workflow.Now(ctx), channel, quiet)
		if err != nil {
			return res, err
		}
		if inWindow {
			delay := open.Sub(workflow.Now(ctx))
			if delay > 0 {
				workflow.GetLogger(ctx).Info("quiet hours: deferring marketing send until window opens",
					"kind", req.Kind, "channel", channel,
					"window_open", open.String(), "delay", delay.String())
				if err := workflow.Sleep(ctx, delay); err != nil {
					return res, err
				}
			}
		}
	}
	if err := workflow.ExecuteActivity(ctx, ActivityNotifyPaced, req).Get(ctx, &res); err != nil {
		return res, err
	}
	if res.Status == "" {
		// Older workers returned no result payload; the send happened.
		res.Status = PacedSendStatusSent
	}
	return res, nil
}
