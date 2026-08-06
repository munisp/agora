package civic

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/events"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// Store abstracts the civic persistence surface (store.Store satisfies it;
// tests use an in-memory fake).
type Store interface {
	ListCivicCategories(ctx context.Context, tenantID uuid.UUID, activeOnly bool) ([]store.CivicCategory, error)
	GetCivicCategoryBySlug(ctx context.Context, tenantID uuid.UUID, slug string) (store.CivicCategory, error)
	GetCivicCategory(ctx context.Context, tenantID, id uuid.UUID) (store.CivicCategory, error)
	CreateCivicCategory(ctx context.Context, c *store.CivicCategory) error
	UpdateCivicCategory(ctx context.Context, c *store.CivicCategory) error
	ListCivicRoutingRules(ctx context.Context, tenantID uuid.UUID) ([]store.CivicRoutingRule, error)
	CreateCivicRoutingRule(ctx context.Context, r *store.CivicRoutingRule) error
	UpdateCivicRoutingRule(ctx context.Context, r *store.CivicRoutingRule) error
	DeleteCivicRoutingRule(ctx context.Context, tenantID, id uuid.UUID) error
	NextCivicRefSeq(ctx context.Context, tenantID uuid.UUID, year int) (int64, error)
	InsertCivicCase(ctx context.Context, c *store.CivicCase) error
	GetCivicCase(ctx context.Context, tenantID, id uuid.UUID) (store.CivicCase, error)
	GetCivicCaseByRef(ctx context.Context, tenantID uuid.UUID, ref string) (store.CivicCase, error)
	ListCivicCases(ctx context.Context, tenantID uuid.UUID, f store.CivicCaseFilter) ([]store.CivicCase, error)
	SaveCivicCase(ctx context.Context, c *store.CivicCase) error
	NextCivicEventSeq(ctx context.Context, tenantID, caseID uuid.UUID) (int64, error)
	CivicCaseStats(ctx context.Context, tenantID uuid.UUID) (store.CivicStats, error)
	DuplicateCivicCaseCandidates(ctx context.Context, tenantID, categoryID, excludeID uuid.UUID, at time.Time) ([]store.CivicCase, error)
	EnqueueOutbox(ctx context.Context, aggregateID uuid.UUID, topic string, payload []byte) error
}

// Service bundles the civic reporting orchestration (SPEC-W32 WS-A).
type Service struct {
	Store Store
	// EventsTopic is the civic CloudEvents topic (CIVIC_EVENTS_TOPIC,
	// default opendesk.civic.events.v1; empty disables emission).
	EventsTopic string
	// Public intake throttling (per-IP AND per-phone, both windows must
	// pass). CIVIC_PUBLIC_RATE_PER_HOUR / _PER_DAY; <=0 disables.
	RatePerHour int
	RatePerDay  int
	Throttle    *Throttler // nil → one is created
	Log         *zap.Logger

	nowMu sync.Mutex
	now   func() time.Time
}

func (s *Service) log() *zap.Logger {
	if s.Log != nil {
		return s.Log
	}
	return zap.NewNop()
}

// clock returns the current time (overridable in tests).
func (s *Service) clock() time.Time {
	s.nowMu.Lock()
	defer s.nowMu.Unlock()
	if s.now != nil {
		return s.now()
	}
	return time.Now().UTC()
}

// SetClock overrides the service clock (tests).
func (s *Service) SetClock(now func() time.Time) {
	s.nowMu.Lock()
	defer s.nowMu.Unlock()
	s.now = now
}

func (s *Service) throttler() *Throttler {
	if s.Throttle != nil {
		return s.Throttle
	}
	s.Throttle = NewThrottler()
	return s.Throttle
}

// ---------------------------------------------------------------------------
// Public intake
// ---------------------------------------------------------------------------

// CheckThrottle applies the per-IP + per-phone sliding-window limits
// (10/hr, 50/day by default). A rejected call returns ErrThrottled (HTTP
// 429 at the API edge).
func (s *Service) CheckThrottle(ip, phone string) error {
	hour := s.RatePerHour
	if hour == 0 {
		hour = DefaultRatePerHour
	}
	day := s.RatePerDay
	if day == 0 {
		day = DefaultRatePerDay
	}
	t := s.throttler()
	if !t.Allow("ip:"+ip+":h", hour, time.Hour) || !t.Allow("ip:"+ip+":d", day, 24*time.Hour) {
		return fmt.Errorf("%w: too many reports from this network, try later", ErrThrottled)
	}
	if phone != "" {
		if !t.Allow("phone:"+phone+":h", hour, time.Hour) || !t.Allow("phone:"+phone+":d", day, 24*time.Hour) {
			return fmt.Errorf("%w: too many reports from this phone, try later", ErrThrottled)
		}
	}
	return nil
}

// Submit validates + persists one citizen report and emits
// com.opendesk.civic.ReportReceived. Used by the public web endpoint
// (channel web), the fieldcapture civic_report kind (channel pwa) and the
// WhatsApp intake (channel whatsapp).
func (s *Service) Submit(ctx context.Context, tenantID uuid.UUID, tenantSlug, channel string, in ReportInput) (store.CivicCase, error) {
	if err := in.Validate(); err != nil {
		return store.CivicCase{}, err
	}
	switch channel {
	case ChannelWeb, ChannelPWA, ChannelWhatsApp:
	default:
		return store.CivicCase{}, fmt.Errorf("%w: channel %q", ErrInvalidInput, channel)
	}
	cat, err := s.Store.GetCivicCategoryBySlug(ctx, tenantID, in.CategorySlug)
	if err != nil {
		if err == store.ErrNotFound {
			return store.CivicCase{}, fmt.Errorf("%w: unknown category_slug %q", ErrInvalidInput, in.CategorySlug)
		}
		return store.CivicCase{}, err
	}
	if !cat.Active {
		return store.CivicCase{}, fmt.Errorf("%w: category %q is not accepting reports", ErrInvalidInput, in.CategorySlug)
	}
	rules, err := s.Store.ListCivicRoutingRules(ctx, tenantID)
	if err != nil {
		return store.CivicCase{}, err
	}
	now := s.clock()
	seq, err := s.Store.NextCivicRefSeq(ctx, tenantID, now.Year())
	if err != nil {
		return store.CivicCase{}, err
	}
	ackDue, resolveDue := ComputeSLA(now, cat)
	c := store.CivicCase{
		ID:           uuid.New(),
		TenantID:     tenantID,
		Ref:          FormatRef(in.LGA, in.Ward, int64(now.Year()), seq),
		CategoryID:   cat.ID,
		Status:       store.CivicStatusNew,
		Description:  in.Description,
		Ward:         in.Ward,
		LGA:          in.LGA,
		Lat:          in.Lat,
		Lon:          in.Lon,
		LocationText: in.LocationText,
		Anonymous:    in.Anonymous,
		WantsUpdates: in.WantsStatusUpdates(),
		PhotoURL:     in.PhotoURL,
		Channel:      channel,
		MDAQueue:     ResolveMDAQueue(rules, cat, in.Ward),
		AckDueAt:     ackDue,
		ResolveDueAt: resolveDue,
	}
	if in.ReporterPhoneE164 != "" {
		p := in.ReporterPhoneE164
		c.ReporterPhoneE164 = &p
	}
	if in.ReporterName != "" {
		n := in.ReporterName
		c.ReporterName = &n
	}
	if err := s.Store.InsertCivicCase(ctx, &c); err != nil {
		return c, err
	}
	s.emit(ctx, tenantSlug, &c, EventTypeReportReceived, map[string]any{
		// category is the graph-sync CivicReportData key; category_slug
		// stays for existing consumers (additive, no renames).
		"category":       cat.Slug,
		"category_slug":  cat.Slug,
		"channel":        channel,
		"ack_due_at":     c.AckDueAt,
		"resolve_due_at": c.ResolveDueAt,
	})
	return c, nil
}

// Track resolves the citizen status lookup: ref + phone must match the
// case's reporter phone (possession, not identity — SPEC-W32 §0.2). A
// mismatch, an unknown ref, or a case without a phone all answer
// store.ErrNotFound (no oracle on which part failed). Merged cases are
// returned with their merged_into pointer set (SPEC-W32 §4.3).
func (s *Service) Track(ctx context.Context, tenantID uuid.UUID, ref, phone string) (store.CivicCase, error) {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return store.CivicCase{}, store.ErrNotFound
	}
	c, err := s.Store.GetCivicCaseByRef(ctx, tenantID, strings.TrimSpace(ref))
	if err != nil {
		return c, err
	}
	if c.ReporterPhoneE164 == nil || *c.ReporterPhoneE164 != phone {
		return store.CivicCase{}, store.ErrNotFound
	}
	return c, nil
}

// PublicStats returns the aggregate-only dashboard payload.
func (s *Service) PublicStats(ctx context.Context, tenantID uuid.UUID) (store.CivicStats, error) {
	return s.Store.CivicCaseStats(ctx, tenantID)
}

// ---------------------------------------------------------------------------
// Operator actions
// ---------------------------------------------------------------------------

// TriageInput is the POST /v1/civic/cases/{id}/triage body.
type TriageInput struct {
	CategoryID *uuid.UUID `json:"category_id,omitempty"`
	Ward       *string    `json:"ward,omitempty"`
	MDAQueue   *string    `json:"mda_queue,omitempty"`
}

// Triage re-categorizes/routes a case and moves it to triaged, stamping
// acked_at (first operator touch acknowledges receipt) and recomputing the
// SLA clocks from the (possibly new) category. Emits StatusChanged.
func (s *Service) Triage(ctx context.Context, tenantID uuid.UUID, tenantSlug string, id uuid.UUID, in TriageInput) (store.CivicCase, error) {
	c, err := s.mutableCase(ctx, tenantID, id)
	if err != nil {
		return c, err
	}
	cat, err := s.Store.GetCivicCategory(ctx, tenantID, c.CategoryID)
	if err != nil {
		return c, err
	}
	if in.CategoryID != nil && *in.CategoryID != c.CategoryID {
		nc, err := s.Store.GetCivicCategory(ctx, tenantID, *in.CategoryID)
		if err != nil {
			return c, err
		}
		c.CategoryID = nc.ID
		cat = nc
	}
	if in.Ward != nil {
		c.Ward = strings.TrimSpace(*in.Ward)
	}
	if in.MDAQueue != nil {
		c.MDAQueue = strings.TrimSpace(*in.MDAQueue)
	} else {
		rules, err := s.Store.ListCivicRoutingRules(ctx, tenantID)
		if err != nil {
			return c, err
		}
		c.MDAQueue = ResolveMDAQueue(rules, cat, c.Ward)
	}
	now := s.clock()
	c.Status = store.CivicStatusTriaged
	if c.AckedAt == nil {
		c.AckedAt = &now
	}
	c.AckDueAt, c.ResolveDueAt = ComputeSLA(now, cat)
	if err := s.Store.SaveCivicCase(ctx, &c); err != nil {
		return c, err
	}
	s.emit(ctx, tenantSlug, &c, EventTypeStatusChanged, nil)
	return c, nil
}

// Assign moves a case to assigned with an assignee handle. Emits
// StatusChanged.
func (s *Service) Assign(ctx context.Context, tenantID uuid.UUID, tenantSlug string, id uuid.UUID, assignee string) (store.CivicCase, error) {
	assignee = strings.TrimSpace(assignee)
	if assignee == "" {
		return store.CivicCase{}, fmt.Errorf("%w: assignee is required", ErrInvalidInput)
	}
	c, err := s.mutableCase(ctx, tenantID, id)
	if err != nil {
		return c, err
	}
	now := s.clock()
	c.Status = store.CivicStatusAssigned
	c.AssignedTo = &assignee
	if c.AckedAt == nil {
		c.AckedAt = &now
	}
	if err := s.Store.SaveCivicCase(ctx, &c); err != nil {
		return c, err
	}
	s.emit(ctx, tenantSlug, &c, EventTypeStatusChanged, nil)
	return c, nil
}

// SetStatus moves a case to in_progress | resolved | closed (operator
// workflow; resolved stamps resolved_at, closed stamps closed_at). The
// optional note rides the StatusChanged event data (no notes table in v1).
func (s *Service) SetStatus(ctx context.Context, tenantID uuid.UUID, tenantSlug string, id uuid.UUID, status, note string) (store.CivicCase, error) {
	switch status {
	case store.CivicStatusInProgress, store.CivicStatusResolved, store.CivicStatusClosed:
	default:
		return store.CivicCase{}, fmt.Errorf("%w: status %q (want in_progress|resolved|closed)", ErrInvalidInput, status)
	}
	c, err := s.mutableCase(ctx, tenantID, id)
	if err != nil {
		return c, err
	}
	now := s.clock()
	c.Status = status
	if c.AckedAt == nil {
		c.AckedAt = &now
	}
	if status == store.CivicStatusResolved && c.ResolvedAt == nil {
		c.ResolvedAt = &now
	}
	if status == store.CivicStatusClosed && c.ClosedAt == nil {
		c.ClosedAt = &now
	}
	if err := s.Store.SaveCivicCase(ctx, &c); err != nil {
		return c, err
	}
	var extra map[string]any
	if note != "" {
		extra = map[string]any{"note": note}
	}
	s.emit(ctx, tenantSlug, &c, EventTypeStatusChanged, extra)
	return c, nil
}

// Merge marks a case as a duplicate of the canonical case (SPEC-W32 §2:
// the case stays readable, points at the canonical; notifications follow
// the canonical — the Merged event carries both refs so WS-B re-targets).
func (s *Service) Merge(ctx context.Context, tenantID uuid.UUID, tenantSlug string, id, canonicalID uuid.UUID) (store.CivicCase, error) {
	if id == canonicalID {
		return store.CivicCase{}, fmt.Errorf("%w: cannot merge a case into itself", ErrInvalidInput)
	}
	c, err := s.mutableCase(ctx, tenantID, id)
	if err != nil {
		return c, err
	}
	canonical, err := s.Store.GetCivicCase(ctx, tenantID, canonicalID)
	if err != nil {
		return c, err
	}
	if canonical.MergedInto != nil {
		return c, fmt.Errorf("%w: canonical case %s is itself merged into %s", ErrInvalidInput, canonical.Ref, canonical.MergedInto)
	}
	c.MergedInto = &canonicalID
	if err := s.Store.SaveCivicCase(ctx, &c); err != nil {
		return c, err
	}
	s.emit(ctx, tenantSlug, &c, EventTypeMerged, map[string]any{
		"canonical_id":  canonicalID.String(),
		"canonical_ref": canonical.Ref,
	})
	return c, nil
}

// Duplicates returns the geo ≤500m + same category + ±72h candidate set for
// one case (coarse filter in the store, haversine in Go).
func (s *Service) Duplicates(ctx context.Context, tenantID, id uuid.UUID) ([]store.CivicCase, error) {
	c, err := s.Store.GetCivicCase(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	cands, err := s.Store.DuplicateCivicCaseCandidates(ctx, tenantID, c.CategoryID, c.ID, c.CreatedAt)
	if err != nil {
		return nil, err
	}
	out := []store.CivicCase{}
	for _, o := range cands {
		if IsDuplicateCandidate(c, o, DuplicateCandidateMaxM) {
			out = append(out, o)
		}
	}
	return out, nil
}

// MarkSLABreach sets one breach flag via the internal callback
// (notification-worker's SLA workflow; idempotent — an already-set flag is
// a no-op). kind is ack | resolve.
func (s *Service) MarkSLABreach(ctx context.Context, tenantID uuid.UUID, ref, kind string) (store.CivicCase, error) {
	c, err := s.Store.GetCivicCaseByRef(ctx, tenantID, ref)
	if err != nil {
		return c, err
	}
	switch kind {
	case BreachAck:
		c.SLABreachAck = true
	case BreachResolve:
		c.SLABreachResolve = true
	default:
		return c, fmt.Errorf("%w: kind %q (want ack|resolve)", ErrInvalidInput, kind)
	}
	if err := s.Store.SaveCivicCase(ctx, &c); err != nil {
		return c, err
	}
	return c, nil
}

// mutableCase loads a case for an operator mutation, rejecting terminal /
// merged rows.
func (s *Service) mutableCase(ctx context.Context, tenantID, id uuid.UUID) (store.CivicCase, error) {
	c, err := s.Store.GetCivicCase(ctx, tenantID, id)
	if err != nil {
		return c, err
	}
	if c.MergedInto != nil {
		return c, fmt.Errorf("%w: case %s is merged into another case", ErrInvalidInput, c.Ref)
	}
	if c.Status == store.CivicStatusClosed {
		return c, fmt.Errorf("%w: case %s is closed", ErrInvalidInput, c.Ref)
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// Events (CloudEvents on opendesk.civic.events.v1 via the outbox)
// ---------------------------------------------------------------------------

// emit appends one CloudEvent to the transactional outbox. The event id is
// deterministic per case: {tenantSlug}:civic:{ref}:{seq} with seq from the
// per-case event sequence (SPEC-W32 WS-A). Best-effort: the case mutation
// is already durable, so an outbox failure is logged, never fatal (same
// posture as the W11 incident metering).
func (s *Service) emit(ctx context.Context, tenantSlug string, c *store.CivicCase, eventType string, extra map[string]any) {
	if s.EventsTopic == "" {
		return
	}
	seq, err := s.Store.NextCivicEventSeq(ctx, c.TenantID, c.ID)
	if err != nil {
		s.log().Error("civic event seq failed; skipping emission",
			zap.String("ref", c.Ref), zap.String("type", eventType), zap.Error(err))
		return
	}
	data := map[string]any{
		"case_id":    c.ID.String(),
		"ref":        c.Ref,
		"status":     c.Status,
		"ward":       c.Ward,
		"lga":        c.LGA,
		"mda_queue":  c.MDAQueue,
		"channel":    c.Channel,
		"created_at": c.CreatedAt,
	}
	// Current SLA clocks ride every event (graph-sync's CivicReportData and
	// WS-B's due-time consumers decode these; null when unset).
	data["ack_due_at"] = c.AckDueAt
	data["resolve_due_at"] = c.ResolveDueAt
	// Geo fix + reporter name as VALUES when present (graph-sync decodes
	// scalar lat/lon/reporter_name, not pointers).
	if c.Lat != nil {
		data["lat"] = *c.Lat
	}
	if c.Lon != nil {
		data["lon"] = *c.Lon
	}
	if c.ReporterName != nil {
		data["reporter_name"] = *c.ReporterName
	}
	if c.AssignedTo != nil {
		data["assigned_to"] = *c.AssignedTo
	}
	if c.WantsUpdates && c.ReporterPhoneE164 != nil {
		data["reporter_phone"] = *c.ReporterPhoneE164
	}
	for k, v := range extra {
		data[k] = v
	}
	evt := events.New("booking-service", eventType, tenantSlug, c.TenantID.String(), data)
	evt.ID = fmt.Sprintf("%s:civic:%s:%06d", tenantSlug, c.Ref, seq)
	payload, err := json.Marshal(evt)
	if err != nil {
		s.log().Error("civic event marshal failed; skipping emission", zap.Error(err))
		return
	}
	if err := s.Store.EnqueueOutbox(ctx, c.ID, s.EventsTopic, payload); err != nil {
		s.log().Error("civic event enqueue failed; skipping emission",
			zap.String("ref", c.Ref), zap.String("type", eventType), zap.Error(err))
	}
}

// ---------------------------------------------------------------------------
// Category + routing CRUD (operator config)
// ---------------------------------------------------------------------------

var categorySlug = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// CreateCategory validates + inserts a category.
func (s *Service) CreateCategory(ctx context.Context, c *store.CivicCategory) error {
	c.Name = strings.TrimSpace(c.Name)
	c.Slug = strings.TrimSpace(c.Slug)
	c.MDAQueue = strings.TrimSpace(c.MDAQueue)
	if c.Name == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	if !categorySlug.MatchString(c.Slug) {
		return fmt.Errorf("%w: slug must be lowercase alnum-dash", ErrInvalidInput)
	}
	if c.AckSLAHours < 0 || c.ResolveSLAHours < 0 {
		return fmt.Errorf("%w: sla hours must be >= 0", ErrInvalidInput)
	}
	return s.Store.CreateCivicCategory(ctx, c)
}

// UpdateCategory validates + rewrites a category's mutable fields.
func (s *Service) UpdateCategory(ctx context.Context, c *store.CivicCategory) error {
	c.Name = strings.TrimSpace(c.Name)
	c.MDAQueue = strings.TrimSpace(c.MDAQueue)
	if c.Name == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	if c.AckSLAHours < 0 || c.ResolveSLAHours < 0 {
		return fmt.Errorf("%w: sla hours must be >= 0", ErrInvalidInput)
	}
	return s.Store.UpdateCivicCategory(ctx, c)
}

// CreateRoutingRule validates + inserts a routing rule.
func (s *Service) CreateRoutingRule(ctx context.Context, r *store.CivicRoutingRule) error {
	r.Ward = strings.TrimSpace(r.Ward)
	r.MDAQueue = strings.TrimSpace(r.MDAQueue)
	if r.MDAQueue == "" {
		return fmt.Errorf("%w: mda_queue is required", ErrInvalidInput)
	}
	if r.CategoryID == uuid.Nil {
		return fmt.Errorf("%w: category_id is required", ErrInvalidInput)
	}
	if _, err := s.Store.GetCivicCategory(ctx, r.TenantID, r.CategoryID); err != nil {
		return err
	}
	return s.Store.CreateCivicRoutingRule(ctx, r)
}
