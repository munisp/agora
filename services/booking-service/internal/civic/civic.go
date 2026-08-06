// Package civic implements SPEC-W32 WS-A: the civic reporting module of
// booking-service — citizen intake (public, unauthenticated, abuse-protected),
// operator triage/assign/merge, MDA routing, SLA clocks and CloudEvents on
// opendesk.civic.events.v1 via the transactional outbox.
//
// Citizens are NOT users (SPEC-W32 §0.2): the public intake resolves the
// tenant from the URL slug, tracking binds case reference + reporter phone
// (possession, not identity), and stats are aggregate-only. Operator
// endpoints ride the tenant middleware + manage_bookings perm like the W11
// incidents routes; anonymous reporter masking is role-based (owner/admin
// see the reporter on detail views).
package civic

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/opendesk/booking-service/internal/store"
)

// TopicCivicEvents is the Kafka topic every civic CloudEvent lands on via
// the transactional outbox (SPEC-W32 §0.3).
const TopicCivicEvents = "opendesk.civic.events.v1"

// CloudEvent types emitted by this module.
const (
	EventTypeReportReceived = "com.opendesk.civic.ReportReceived"
	EventTypeStatusChanged  = "com.opendesk.civic.StatusChanged"
	EventTypeMerged         = "com.opendesk.civic.Merged"
)

// Case channels (CHECK constraint of civic_cases; SPEC-W32 §0.6).
const (
	ChannelWeb      = "web"
	ChannelPWA      = "pwa"
	ChannelWhatsApp = "whatsapp"
)

// SLA breach kinds (POST /v1/civic/internal/cases/{ref}/sla-breach).
const (
	BreachAck     = "ack"
	BreachResolve = "resolve"
)

// ErrInvalidInput marks deterministic validation failures (no retry).
var ErrInvalidInput = errors.New("invalid civic input")

// ErrThrottled marks public-intake rate-limit rejections (HTTP 429).
var ErrThrottled = errors.New("throttled")

// Public-intake abuse limits (defaults; configurable via env, SPEC-W32
// WS-A: per-IP + per-phone 10/hr, 50/day).
const (
	DefaultRatePerHour = 10
	DefaultRatePerDay  = 50
)

// Description bounds (SPEC-W32 WS-A: 10..2000 chars).
const (
	DescriptionMinLen = 10
	DescriptionMaxLen = 2000
)

// DuplicateCandidateMaxM is the geo radius of the duplicates endpoint
// (SPEC-W32: ≤500m + same category + ±72h).
const DuplicateCandidateMaxM = 500.0

// DuplicateCandidateWindow is the ±72h time window for duplicates.
const DuplicateCandidateWindow = 72 * time.Hour

// e164 is a light E.164 check (leading +, 7..15 digits, first non-zero).
var e164 = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)

// ReportInput is one citizen report (public web portal body and the
// fieldcapture civic_report payload share this shape, SPEC-W32 WS-A).
type ReportInput struct {
	CategorySlug      string   `json:"category_slug"`
	Description       string   `json:"description"`
	Ward              string   `json:"ward,omitempty"`
	LGA               string   `json:"lga,omitempty"`
	Lat               *float64 `json:"lat,omitempty"`
	Lon               *float64 `json:"lon,omitempty"`
	LocationText      string   `json:"location_text,omitempty"`
	ReporterPhoneE164 string   `json:"reporter_phone_e164,omitempty"`
	ReporterName      string   `json:"reporter_name,omitempty"`
	Anonymous         bool     `json:"anonymous,omitempty"`
	WantsUpdates      *bool    `json:"wants_updates,omitempty"`
	PhotoURL          string   `json:"photo_url,omitempty"`
}

// Validate checks the report body (category existence/activity is resolved
// separately against the store — this is the pure field-level matrix).
func (in *ReportInput) Validate() error {
	in.CategorySlug = strings.TrimSpace(in.CategorySlug)
	in.Description = strings.TrimSpace(in.Description)
	in.Ward = strings.TrimSpace(in.Ward)
	in.LGA = strings.TrimSpace(in.LGA)
	in.LocationText = strings.TrimSpace(in.LocationText)
	in.ReporterPhoneE164 = strings.TrimSpace(in.ReporterPhoneE164)
	in.ReporterName = strings.TrimSpace(in.ReporterName)
	in.PhotoURL = strings.TrimSpace(in.PhotoURL)
	if in.CategorySlug == "" {
		return fmt.Errorf("%w: category_slug is required", ErrInvalidInput)
	}
	if n := len(in.Description); n < DescriptionMinLen || n > DescriptionMaxLen {
		return fmt.Errorf("%w: description must be %d..%d chars (got %d)", ErrInvalidInput, DescriptionMinLen, DescriptionMaxLen, n)
	}
	if (in.Lat == nil) != (in.Lon == nil) {
		return fmt.Errorf("%w: lat and lon must be given together", ErrInvalidInput)
	}
	if in.Lat != nil && (*in.Lat < -90 || *in.Lat > 90) {
		return fmt.Errorf("%w: lat out of range", ErrInvalidInput)
	}
	if in.Lon != nil && (*in.Lon < -180 || *in.Lon > 180) {
		return fmt.Errorf("%w: lon out of range", ErrInvalidInput)
	}
	if in.ReporterPhoneE164 != "" && !e164.MatchString(in.ReporterPhoneE164) {
		return fmt.Errorf("%w: reporter_phone_e164 is not E.164", ErrInvalidInput)
	}
	if in.WantsUpdates != nil && *in.WantsUpdates && in.ReporterPhoneE164 == "" {
		return fmt.Errorf("%w: wants_updates requires reporter_phone_e164", ErrInvalidInput)
	}
	if in.PhotoURL != "" && !(strings.HasPrefix(in.PhotoURL, "https://") || strings.HasPrefix(in.PhotoURL, "http://")) {
		return fmt.Errorf("%w: photo_url must be an absolute http(s) URL", ErrInvalidInput)
	}
	return nil
}

// WantsStatusUpdates resolves the effective wants_updates flag: explicit
// wins; default is true when a phone was supplied (the citizen can always
// opt out explicitly), false without a phone.
func (in ReportInput) WantsStatusUpdates() bool {
	if in.WantsUpdates != nil {
		return *in.WantsUpdates
	}
	return in.ReporterPhoneE164 != ""
}

// ---------------------------------------------------------------------------
// Reference: GOV-{LGA}-{WARD}-YYYY-{seq6}
// ---------------------------------------------------------------------------

// refComponent uppercases and strips a ref path component to [A-Z0-9]
// (max 8 chars); empty input falls back to the placeholder.
func refComponent(s, fallback string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
		if b.Len() >= 8 {
			break
		}
	}
	if b.Len() == 0 {
		return fallback
	}
	return b.String()
}

// FormatRef renders GOV-{LGA}-{WARD}-YYYY-{seq6} (SPEC-W32 §2). LGA/WARD
// are sanitized to uppercase alphanumerics; absent values become GEN / 00.
func FormatRef(lga, ward string, year, seq int64) string {
	return fmt.Sprintf("GOV-%s-%s-%04d-%06d",
		refComponent(lga, "GEN"), refComponent(ward, "00"), year, seq%1000000)
}

// ---------------------------------------------------------------------------
// SLA clocks (plain wall-clock hours, no business calendar — SPEC-W32 §4.2)
// ---------------------------------------------------------------------------

// ComputeSLA derives the ack/resolve due timestamps from a category's SLA
// hours relative to the given start time. Zero/negative SLA hours leave the
// corresponding due unset.
func ComputeSLA(start time.Time, cat store.CivicCategory) (ackDue, resolveDue *time.Time) {
	if cat.AckSLAHours > 0 {
		t := start.Add(time.Duration(cat.AckSLAHours) * time.Hour)
		ackDue = &t
	}
	if cat.ResolveSLAHours > 0 {
		t := start.Add(time.Duration(cat.ResolveSLAHours) * time.Hour)
		resolveDue = &t
	}
	return ackDue, resolveDue
}

// ---------------------------------------------------------------------------
// Routing: ward-specific override wins over the category default (§2/§3)
// ---------------------------------------------------------------------------

// ResolveMDAQueue picks the dispatch queue for a report: the routing rule
// whose ward matches exactly wins; a ward-less rule for the category is
// next; the category default is the fallback.
func ResolveMDAQueue(rules []store.CivicRoutingRule, cat store.CivicCategory, ward string) string {
	categoryDefault := ""
	for _, r := range rules {
		if r.CategoryID != cat.ID {
			continue
		}
		if r.Ward != "" && strings.EqualFold(r.Ward, ward) && ward != "" {
			return r.MDAQueue
		}
		if r.Ward == "" {
			categoryDefault = r.MDAQueue
		}
	}
	if categoryDefault != "" {
		return categoryDefault
	}
	return cat.MDAQueue
}

// ---------------------------------------------------------------------------
// Duplicates: haversine ≤500m (Go-side, no PostGIS dependency)
// ---------------------------------------------------------------------------

// HaversineM returns the great-circle distance in meters between two fixes.
func HaversineM(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadiusM = 6371000.0
	toRad := func(d float64) float64 { return d * math.Pi / 180 }
	dLat := toRad(lat2 - lat1)
	dLon := toRad(lon2 - lon1)
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(toRad(lat1))*math.Cos(toRad(lat2))*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadiusM * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

// IsDuplicateCandidate reports whether other is a duplicate candidate of c:
// same category, geo ≤500m (both fixes present), created within ±72h,
// unmerged, different case (SPEC-W32 WS-A).
func IsDuplicateCandidate(c, other store.CivicCase, maxM float64) bool {
	if other.ID == c.ID || other.CategoryID != c.CategoryID || other.MergedInto != nil {
		return false
	}
	if c.Lat == nil || c.Lon == nil || other.Lat == nil || other.Lon == nil {
		return false
	}
	if d := other.CreatedAt.Sub(c.CreatedAt); d > DuplicateCandidateWindow || d < -DuplicateCandidateWindow {
		return false
	}
	return HaversineM(*c.Lat, *c.Lon, *other.Lat, *other.Lon) <= maxM
}

// ---------------------------------------------------------------------------
// Reporter masking (SPEC-W32 §2/§4.4)
// ---------------------------------------------------------------------------

// MaskPhone masks all but the leading country code (up to 4 chars) and the
// trailing 2 digits of an E.164 number: "+2348012345678" → "+234*****78".
func MaskPhone(phone string) string {
	if phone == "" {
		return ""
	}
	prefix := 0
	for i, r := range phone {
		if i >= 4 {
			break
		}
		if r == '+' || unicode.IsDigit(r) {
			prefix = i + 1
		}
	}
	if prefix == 0 {
		prefix = 1
	}
	suffix := 2
	if len(phone)-prefix < suffix+1 {
		// Too short to keep both ends distinct — mask everything after the prefix.
		suffix = 0
	}
	masked := len(phone) - prefix - suffix
	if masked < 1 {
		masked = 1
	}
	return phone[:prefix] + strings.Repeat("*", masked) + phone[len(phone)-suffix:]
}

// MaskReporter hides the reporter identity of an anonymous case for
// non-owner/admin operator views (list, detail, search — SPEC-W32 §4.4).
// Non-anonymous cases pass through unchanged; masking applies only when the
// citizen explicitly asked for anonymity.
func MaskReporter(c *store.CivicCase, canViewReporter bool) {
	if !c.Anonymous || canViewReporter {
		return
	}
	if c.ReporterPhoneE164 != nil {
		m := MaskPhone(*c.ReporterPhoneE164)
		c.ReporterPhoneE164 = &m
	}
	if c.ReporterName != nil {
		m := "Anonymous"
		c.ReporterName = &m
	}
}

// CanViewReporterRole reports whether the realm roles entitle the operator
// to unmasked reporter PII (owner/admin only — docs/security/roles.md).
func CanViewReporterRole(roles []string) bool {
	for _, r := range roles {
		if r == "owner" || r == "admin" {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// In-memory public-intake throttler (per-IP + per-phone; APISIX limit-req
// is the outer wall, this is the app-level abuse guard — SPEC-W32 §4.6)
// ---------------------------------------------------------------------------

// Throttler is a sliding-window in-memory limiter keyed by arbitrary
// strings ("ip:1.2.3.4", "phone:+234..."). Mirrors the portal rate limiter
// idiom; per-process state is acceptable because APISIX already enforces a
// distributed limit-req in front.
type Throttler struct {
	mu   chan struct{}
	hits map[string][]time.Time
	now  func() time.Time
}

// NewThrottler builds an empty throttler.
func NewThrottler() *Throttler {
	t := &Throttler{hits: map[string][]time.Time{}, now: time.Now, mu: make(chan struct{}, 1)}
	return t
}

// SetClock overrides the clock (tests).
func (t *Throttler) SetClock(now func() time.Time) { t.now = now }

// Allow records one hit for key and reports whether it is within limit for
// the window. limit <= 0 disables the check (always allowed).
func (t *Throttler) Allow(key string, limit int, window time.Duration) bool {
	if limit <= 0 || key == "" {
		return true
	}
	t.mu <- struct{}{}
	defer func() { <-t.mu }()
	now := t.now()
	cutoff := now.Add(-window)
	kept := t.hits[key][:0]
	for _, ts := range t.hits[key] {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	if len(kept) >= limit {
		t.hits[key] = kept
		return false
	}
	t.hits[key] = append(kept, now)
	// bound memory: drop idle keys opportunistically
	if len(t.hits) > 10000 {
		for k, v := range t.hits {
			if len(v) == 0 {
				delete(t.hits, k)
			}
		}
	}
	return true
}
