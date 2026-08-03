package pacer

// SPEC-W12 Agent B: DND 2442 suppression + quiet-hours guards.
//
// Every paced send kind is classified per SPEC-W12 contract §3:
//
//	marketing      — geo_campaign, promo, broadcast, drip
//	transactional  — booking confirmations, reminders, incident_alert, otp
//	                 (plus every other in-repo kind: waitlist claims, no-show
//	                 follow-ups, staff alerts, ... — never suppressed, never
//	                 deferred)
//
// Marketing kinds are DND-suppressed (activity-side, via Guards.PreSend,
// checked BEFORE the CPS token is acquired so suppressed sends never consume
// pacing budget) and quiet-hours deferred (workflow-side, via
// workflows.GuardedPacedSend, which workflow.Sleeps until the window opens).
// Transactional kinds are exempt from both. The incident_alert Priority
// fast-lane (SPEC-W11 Part B §5) is untouched: it never consults these
// guards.
//
// This file holds only PURE logic (classification table, quiet-hours time
// math) plus the activity-side guard orchestration — no I/O of its own — so
// the workflow package may import it without breaking determinism (DND
// persistence lives in internal/store and is injected behind DNDChecker).

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// ---------------------------------------------------------------------------
// Kind classification (SPEC-W12 contract §3)
// ---------------------------------------------------------------------------

// SendClass is the DND/quiet-hours compliance class of a paced send kind.
type SendClass string

const (
	// ClassMarketing sends are promotional: DND-suppressed and quiet-hours
	// deferred.
	ClassMarketing SendClass = "marketing"
	// ClassTransactional sends are service-triggered (booking lifecycle,
	// security): exempt from DND suppression and quiet hours.
	ClassTransactional SendClass = "transactional"
)

// kindClasses is the explicit classification table of SPEC-W12 contract §3.
// Kinds not listed here default to transactional (ClassifyKind) — a safe
// default: unlisted kinds are never suppressed or deferred.
var kindClasses = map[string]SendClass{
	// Marketing (contract §3 canonical list).
	"geo_campaign": ClassMarketing,
	"promo":        ClassMarketing,
	"broadcast":    ClassMarketing,
	"drip":         ClassMarketing,
	// Transactional (contract §3 canonical list).
	"confirmation":   ClassTransactional, // booking confirmations
	"reminder":       ClassTransactional, // T-24h / T-1h reminders
	"incident_alert": ClassTransactional, // + Priority fast-lane exemption
	"otp":            ClassTransactional,
	// Remaining in-repo kinds, transactional by nature (booking lifecycle /
	// staff traffic), listed explicitly so the table is auditable.
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
// Quiet hours (SPEC-W12 contract §8: QUIET_HOURS_DEFAULT "20:00-08:00",
// QUIET_HOURS_OVERRIDES per-channel JSON)
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

// QuietHoursConfig carries the resolved quiet-hours configuration
// (env QUIET_HOURS_DEFAULT / QUIET_HOURS_OVERRIDES, parsed by config/main).
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

// ---------------------------------------------------------------------------
// DND suppression guard (activity-side)
// ---------------------------------------------------------------------------

// Suppression reasons (the {reason} label of notifications_suppressed_total).
const (
	// ReasonTenantOptOut is a per-tenant opt-out row (the customer asked
	// this tenant to stop marketing messages).
	ReasonTenantOptOut = "tenant_optout"
	// ReasonGlobalDND is the global NCC 2442 do-not-disturb list.
	ReasonGlobalDND = "global_dnd"
)

// DNDChecker is the persistence slice the guard needs (internal/store's
// *Store satisfies it; tests use fakes). phone is normalized inside the
// implementation; tenantSlug "" means "no tenant scope" (global check only).
// Check order is per-tenant opt-out first, then the global NCC 2442 list.
type DNDChecker interface {
	IsSuppressed(ctx context.Context, tenantSlug, phone string) (suppressed bool, reason string, err error)
}

// GuardConfig configures the compliance guards.
type GuardConfig struct {
	// DNDEnforcement toggles DND suppression (env DND_ENFORCEMENT, default
	// true). When false, marketing sends pass unconditionally.
	DNDEnforcement bool
	// DND is the suppression lookup; nil disables suppression (no
	// DATABASE_URL — the worker logs that at boot).
	DND DNDChecker
	// QuietHours is the resolved quiet-hours configuration, kept here for
	// boot-time validation/logging; the workflow-side deferral receives it
	// via the scheduling workflow's input.
	QuietHours QuietHoursConfig
}

// GuardInput is one paced send as the guard sees it (extracted from the
// request payload by the caller — marketing kinds always carry a phone).
type GuardInput struct {
	Kind       string // PacedSend* kind
	TenantSlug string // "" = unknown tenant (global check only)
	Phone      string // recipient; raw (normalized by the checker)
	Channel    string // sms | whatsapp | telegram | ... (quiet-hours override key)
}

// GuardDecision is the outcome of a pre-send guard evaluation.
type GuardDecision struct {
	Class    SendClass
	Suppress bool
	Reason   string // ReasonTenantOptOut | ReasonGlobalDND when Suppress
}

// Guards evaluates the pre-send compliance guards for paced sends and
// meters every suppression (notifications_suppressed_total{reason} —
// process-local counters like the pacer's, exposed via SuppressedStats and
// logged on every decision; a metrics endpoint can scrape them later).
type Guards struct {
	cfg GuardConfig
	log *zap.Logger

	mu         sync.Mutex
	suppressed map[string]uint64
}

// NewGuards builds the guard set. A nil logger becomes a no-op logger.
func NewGuards(cfg GuardConfig, log *zap.Logger) *Guards {
	if log == nil {
		log = zap.NewNop()
	}
	return &Guards{cfg: cfg, log: log, suppressed: map[string]uint64{}}
}

// PreSend evaluates the DND guard for one paced send. Transactional kinds
// pass immediately (this is the entire guard surface for them — the
// Priority lane never reaches this code path). Marketing kinds are checked
// against the tenant opt-out list first, then the global NCC 2442 list; a
// hit suppresses the send and is counted + logged.
//
// Failure policy is FAIL-OPEN: a DND store error passes the send (logged),
// mirroring the pacer's redis fail-open — a notifications-DB outage must not
// stall time-sensitive traffic, and marketing sends also carry the
// per-send audit log for reconciliation.
func (g *Guards) PreSend(ctx context.Context, in GuardInput) GuardDecision {
	dec := GuardDecision{Class: ClassifyKind(in.Kind)}
	if dec.Class != ClassMarketing {
		return dec
	}
	if !g.cfg.DNDEnforcement {
		g.log.Debug("dnd guard: enforcement disabled (DND_ENFORCEMENT=false), passing marketing send",
			zap.String("kind", in.Kind), zap.String("phone", in.Phone))
		return dec
	}
	if g.cfg.DND == nil {
		g.log.Warn("dnd guard: no DND store configured, passing marketing send",
			zap.String("kind", in.Kind), zap.String("phone", in.Phone))
		return dec
	}
	if in.Phone == "" {
		g.log.Warn("dnd guard: marketing send without recipient phone, cannot check DND — passing",
			zap.String("kind", in.Kind), zap.String("tenant", in.TenantSlug))
		return dec
	}
	suppressed, reason, err := g.cfg.DND.IsSuppressed(ctx, in.TenantSlug, in.Phone)
	if err != nil {
		g.log.Error("dnd guard: lookup failed, failing open",
			zap.String("kind", in.Kind), zap.String("phone", in.Phone), zap.Error(err))
		return dec
	}
	if !suppressed {
		return dec
	}
	dec.Suppress = true
	dec.Reason = reason
	total := g.countSuppressed(reason)
	g.log.Info("notification suppressed by DND guard",
		zap.String("kind", in.Kind), zap.String("tenant", in.TenantSlug),
		zap.String("phone", in.Phone), zap.String("reason", reason),
		zap.String("counter", "notifications_suppressed_total"),
		zap.Uint64("suppressed_total", total))
	return dec
}

// countSuppressed increments notifications_suppressed_total{reason} and
// returns the new global total.
func (g *Guards) countSuppressed(reason string) uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.suppressed[reason]++
	var total uint64
	for _, n := range g.suppressed {
		total += n
	}
	return total
}

// SuppressedStats returns a copy of the notifications_suppressed_total
// counters keyed by reason.
func (g *Guards) SuppressedStats() map[string]uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]uint64, len(g.suppressed))
	for reason, n := range g.suppressed {
		out[reason] = n
	}
	return out
}

// Config returns the guard configuration (boot logging).
func (g *Guards) Config() GuardConfig { return g.cfg }
