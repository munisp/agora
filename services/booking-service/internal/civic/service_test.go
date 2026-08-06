package civic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/events"
	"github.com/opendesk/booking-service/internal/store"
)

// ---------------------------------------------------------------------------
// In-memory fake of the civic Store interface
// ---------------------------------------------------------------------------

type fakeOutboxRow struct {
	aggregate uuid.UUID
	topic     string
	event     events.CloudEvent
}

type fakeCivicStore struct {
	mu         sync.Mutex
	categories map[uuid.UUID]store.CivicCategory
	rules      []store.CivicRoutingRule
	cases      map[uuid.UUID]*store.CivicCase
	refSeqs    map[string]int64
	outbox     []fakeOutboxRow
}

func newFakeCivicStore() *fakeCivicStore {
	return &fakeCivicStore{
		categories: map[uuid.UUID]store.CivicCategory{},
		cases:      map[uuid.UUID]*store.CivicCase{},
		refSeqs:    map[string]int64{},
	}
}

// seedCategory adds a category fixture.
func (f *fakeCivicStore) seedCategory(tenantID uuid.UUID, slug, queue string, ack, resolve int, active bool) store.CivicCategory {
	c := store.CivicCategory{
		ID: uuid.New(), TenantID: tenantID, Name: slug, Slug: slug, MDAQueue: queue,
		AckSLAHours: ack, ResolveSLAHours: resolve, Active: active, CreatedAt: time.Now().UTC(),
	}
	f.categories[c.ID] = c
	return c
}

func (f *fakeCivicStore) ListCivicCategories(_ context.Context, tenantID uuid.UUID, activeOnly bool) ([]store.CivicCategory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []store.CivicCategory{}
	for _, c := range f.categories {
		if c.TenantID == tenantID && (!activeOnly || c.Active) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeCivicStore) GetCivicCategoryBySlug(_ context.Context, tenantID uuid.UUID, slug string) (store.CivicCategory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.categories {
		if c.TenantID == tenantID && c.Slug == slug {
			return c, nil
		}
	}
	return store.CivicCategory{}, store.ErrNotFound
}

func (f *fakeCivicStore) GetCivicCategory(_ context.Context, tenantID, id uuid.UUID) (store.CivicCategory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.categories[id]
	if !ok || c.TenantID != tenantID {
		return store.CivicCategory{}, store.ErrNotFound
	}
	return c, nil
}

func (f *fakeCivicStore) CreateCivicCategory(_ context.Context, c *store.CivicCategory) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.categories {
		if e.TenantID == c.TenantID && e.Slug == c.Slug {
			return store.ErrConflict
		}
	}
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	c.CreatedAt = time.Now().UTC()
	f.categories[c.ID] = *c
	return nil
}

func (f *fakeCivicStore) UpdateCivicCategory(_ context.Context, c *store.CivicCategory) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.categories[c.ID]; !ok {
		return store.ErrNotFound
	}
	f.categories[c.ID] = *c
	return nil
}

func (f *fakeCivicStore) ListCivicRoutingRules(_ context.Context, tenantID uuid.UUID) ([]store.CivicRoutingRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []store.CivicRoutingRule{}
	for _, r := range f.rules {
		if r.TenantID == tenantID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeCivicStore) CreateCivicRoutingRule(_ context.Context, r *store.CivicRoutingRule) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	r.CreatedAt = time.Now().UTC()
	f.rules = append(f.rules, *r)
	return nil
}

func (f *fakeCivicStore) UpdateCivicRoutingRule(_ context.Context, r *store.CivicRoutingRule) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.rules {
		if f.rules[i].ID == r.ID && f.rules[i].TenantID == r.TenantID {
			f.rules[i] = *r
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *fakeCivicStore) DeleteCivicRoutingRule(_ context.Context, tenantID, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.rules {
		if f.rules[i].ID == id && f.rules[i].TenantID == tenantID {
			f.rules = append(f.rules[:i], f.rules[i+1:]...)
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *fakeCivicStore) NextCivicRefSeq(_ context.Context, tenantID uuid.UUID, year int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := tenantID.String() + "|" + time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC).Format("2006")
	f.refSeqs[key]++
	return f.refSeqs[key], nil
}

func (f *fakeCivicStore) InsertCivicCase(_ context.Context, c *store.CivicCase) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.cases {
		if e.TenantID == c.TenantID && e.Ref == c.Ref {
			return store.ErrConflict
		}
	}
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	now := time.Now().UTC()
	c.CreatedAt, c.UpdatedAt = now, now
	cp := *c
	f.cases[c.ID] = &cp
	return nil
}

func (f *fakeCivicStore) GetCivicCase(_ context.Context, tenantID, id uuid.UUID) (store.CivicCase, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.cases[id]
	if !ok || c.TenantID != tenantID {
		return store.CivicCase{}, store.ErrNotFound
	}
	return *c, nil
}

func (f *fakeCivicStore) GetCivicCaseByRef(_ context.Context, tenantID uuid.UUID, ref string) (store.CivicCase, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.cases {
		if c.TenantID == tenantID && c.Ref == ref {
			return *c, nil
		}
	}
	return store.CivicCase{}, store.ErrNotFound
}

func (f *fakeCivicStore) ListCivicCases(_ context.Context, tenantID uuid.UUID, filter store.CivicCaseFilter) ([]store.CivicCase, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []store.CivicCase{}
	for _, c := range f.cases {
		if c.TenantID != tenantID {
			continue
		}
		if filter.Status != "" && c.Status != filter.Status {
			continue
		}
		if filter.CategoryID != nil && c.CategoryID != *filter.CategoryID {
			continue
		}
		if filter.Ward != "" && c.Ward != filter.Ward {
			continue
		}
		switch filter.SLABreach {
		case "any", "true":
			if !c.SLABreachAck && !c.SLABreachResolve {
				continue
			}
		case "ack":
			if !c.SLABreachAck {
				continue
			}
		case "resolve":
			if !c.SLABreachResolve {
				continue
			}
		}
		if filter.Query != "" {
			q := strings.ToLower(filter.Query)
			if !strings.Contains(strings.ToLower(c.Ref), q) &&
				!strings.Contains(strings.ToLower(c.Description), q) &&
				!strings.Contains(strings.ToLower(c.LocationText), q) {
				continue
			}
		}
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (f *fakeCivicStore) SaveCivicCase(_ context.Context, c *store.CivicCase) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.cases[c.ID]; !ok {
		return store.ErrNotFound
	}
	c.UpdatedAt = time.Now().UTC()
	cp := *c
	f.cases[c.ID] = &cp
	return nil
}

func (f *fakeCivicStore) NextCivicEventSeq(_ context.Context, tenantID, caseID uuid.UUID) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.cases[caseID]
	if !ok || c.TenantID != tenantID {
		return 0, store.ErrNotFound
	}
	c.EventSeq++
	return c.EventSeq, nil
}

func (f *fakeCivicStore) CivicCaseStats(_ context.Context, tenantID uuid.UUID) (store.CivicStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	stats := store.CivicStats{ByCategory: []store.CivicStatRow{}, ByWard: []store.CivicStatRow{}}
	byCat := map[string]*store.CivicStatRow{}
	byWard := map[string]*store.CivicStatRow{}
	bump := func(row *store.CivicStatRow, c *store.CivicCase) {
		if c.Status == store.CivicStatusResolved || c.Status == store.CivicStatusClosed {
			row.Resolved++
		} else {
			row.Open++
		}
	}
	for _, c := range f.cases {
		if c.TenantID != tenantID || c.MergedInto != nil {
			continue
		}
		tmp := store.CivicStatRow{}
		bump(&tmp, c)
		stats.Open += tmp.Open
		stats.Resolved += tmp.Resolved
		slug := "other"
		if cat, ok := f.categories[c.CategoryID]; ok {
			slug = cat.Slug
		}
		if byCat[slug] == nil {
			byCat[slug] = &store.CivicStatRow{Key: slug}
		}
		bump(byCat[slug], c)
		ward := c.Ward
		if ward == "" {
			ward = "unspecified"
		}
		if byWard[ward] == nil {
			byWard[ward] = &store.CivicStatRow{Key: ward}
		}
		bump(byWard[ward], c)
	}
	for _, r := range byCat {
		stats.ByCategory = append(stats.ByCategory, *r)
	}
	for _, r := range byWard {
		stats.ByWard = append(stats.ByWard, *r)
	}
	return stats, nil
}

func (f *fakeCivicStore) DuplicateCivicCaseCandidates(_ context.Context, tenantID, categoryID, excludeID uuid.UUID, at time.Time) ([]store.CivicCase, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []store.CivicCase{}
	for _, c := range f.cases {
		if c.TenantID != tenantID || c.CategoryID != categoryID || c.ID == excludeID || c.MergedInto != nil {
			continue
		}
		if c.CreatedAt.Before(at.Add(-72*time.Hour)) || c.CreatedAt.After(at.Add(72*time.Hour)) {
			continue
		}
		out = append(out, *c)
	}
	return out, nil
}

func (f *fakeCivicStore) EnqueueOutbox(_ context.Context, aggregateID uuid.UUID, topic string, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var evt events.CloudEvent
	if err := json.Unmarshal(payload, &evt); err != nil {
		return err
	}
	f.outbox = append(f.outbox, fakeOutboxRow{aggregate: aggregateID, topic: topic, event: evt})
	return nil
}

// ---------------------------------------------------------------------------
// Service fixtures
// ---------------------------------------------------------------------------

type civicFixture struct {
	svc      *Service
	fake     *fakeCivicStore
	tenantID uuid.UUID
	slug     string
	cat      store.CivicCategory
	now      time.Time
}

func newCivicFixture(t *testing.T) *civicFixture {
	t.Helper()
	fake := newFakeCivicStore()
	tenantID := uuid.New()
	now := time.Date(2026, 8, 7, 23, 30, 0, 0, time.UTC) // Friday night (SLA boundary checks)
	fx := &civicFixture{fake: fake, tenantID: tenantID, slug: "ikeja-lga", now: now}
	fx.cat = fake.seedCategory(tenantID, "roads", "mda-works", 2, 49, true)
	fake.seedCategory(tenantID, "archived", "mda-none", 24, 72, false)
	fx.svc = &Service{Store: fake, EventsTopic: TopicCivicEvents, Log: nil}
	fx.svc.SetClock(func() time.Time { return fx.now })
	return fx
}

func (fx *civicFixture) validInput() ReportInput {
	return ReportInput{
		CategorySlug:      "roads",
		Description:       "Deep pothole at the junction blocking one full lane",
		Ward:              "Ward 3",
		LGA:               "Ikeja",
		ReporterPhoneE164: "+2348012345678",
		ReporterName:      "Adaeze Obi",
	}
}

func (fx *civicFixture) submit(t *testing.T, in ReportInput) store.CivicCase {
	t.Helper()
	c, err := fx.svc.Submit(context.Background(), fx.tenantID, fx.slug, ChannelWeb, in)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return c
}

func (fx *civicFixture) lastEvent(t *testing.T) events.CloudEvent {
	t.Helper()
	if len(fx.fake.outbox) == 0 {
		t.Fatal("no outbox events")
	}
	return fx.fake.outbox[len(fx.fake.outbox)-1].event
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// Submit: case persisted with ref GOV-..., SLA dues from the category,
// routing applied, ReportReceived emitted (tenantid ext, deterministic id).
func TestSubmitCreatesCaseWithRefSLAAndEvent(t *testing.T) {
	fx := newCivicFixture(t)
	lat, lon := 6.5244, 3.3792
	in := fx.validInput()
	in.Lat, in.Lon = &lat, &lon
	c := fx.submit(t, in)

	if c.Ref != "GOV-IKEJA-WARD3-2026-000001" {
		t.Fatalf("ref = %q", c.Ref)
	}
	if c.Status != store.CivicStatusNew || c.Channel != ChannelWeb {
		t.Fatalf("status/channel = %q/%q", c.Status, c.Channel)
	}
	if c.MDAQueue != "mda-works" {
		t.Fatalf("mda_queue = %q", c.MDAQueue)
	}
	// ack SLA 2h from Friday 23:30 → Saturday 01:30.
	if c.AckDueAt == nil || c.AckDueAt.Weekday() != time.Saturday {
		t.Fatalf("ack_due_at = %v", c.AckDueAt)
	}
	if c.ResolveDueAt == nil || !c.ResolveDueAt.Equal(fx.now.Add(49*time.Hour)) {
		t.Fatalf("resolve_due_at = %v", c.ResolveDueAt)
	}
	if !c.WantsUpdates {
		t.Fatal("phone supplied → wants_updates defaults true")
	}

	evt := fx.lastEvent(t)
	if evt.Type != EventTypeReportReceived {
		t.Fatalf("event type = %q", evt.Type)
	}
	if evt.TenantID != fx.tenantID.String() {
		t.Fatalf("tenantid ext = %q", evt.TenantID)
	}
	if evt.ID != "ikeja-lga:civic:GOV-IKEJA-WARD3-2026-000001:000001" {
		t.Fatalf("event id = %q", evt.ID)
	}
	if fx.fake.outbox[0].topic != TopicCivicEvents {
		t.Fatalf("topic = %q", fx.fake.outbox[0].topic)
	}
	if evt.Data["ref"] != c.Ref || evt.Data["ack_due_at"] == nil || evt.Data["resolve_due_at"] == nil {
		t.Fatalf("event data incomplete: %v", evt.Data)
	}
	if evt.Data["reporter_phone"] != "+2348012345678" {
		t.Fatalf("wants_updates → reporter_phone in event data: %v", evt.Data)
	}
	// graph-sync CivicReportData keys (FIX B1): category + lat/lon +
	// reporter_name decoded as scalar values; category_slug stays.
	if evt.Data["category"] != "roads" || evt.Data["category_slug"] != "roads" {
		t.Fatalf("category keys = %v", evt.Data)
	}
	if evt.Data["lat"] != lat || evt.Data["lon"] != lon {
		t.Fatalf("lat/lon = %v/%v", evt.Data["lat"], evt.Data["lon"])
	}
	if evt.Data["reporter_name"] != "Adaeze Obi" {
		t.Fatalf("reporter_name = %v", evt.Data["reporter_name"])
	}
	// Due times survive the JSON round trip with correct values.
	if got := eventTime(t, evt.Data["ack_due_at"]); got == nil || !got.Equal(*c.AckDueAt) {
		t.Fatalf("ack_due_at = %v, want %v", evt.Data["ack_due_at"], c.AckDueAt)
	}
	if got := eventTime(t, evt.Data["resolve_due_at"]); got == nil || !got.Equal(*c.ResolveDueAt) {
		t.Fatalf("resolve_due_at = %v, want %v", evt.Data["resolve_due_at"], c.ResolveDueAt)
	}
}

// eventTime decodes a due-time key from a JSON-round-tripped event data map.
func eventTime(t *testing.T, v any) *time.Time {
	t.Helper()
	s, ok := v.(string)
	if !ok || s == "" {
		return nil
	}
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("due time not RFC3339: %q", s)
	}
	return &ts
}

// Ref uniqueness: the per-tenant sequence makes concurrent reports unique.
func TestSubmitRefSequenceUnique(t *testing.T) {
	fx := newCivicFixture(t)
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		c := fx.submit(t, fx.validInput())
		if seen[c.Ref] {
			t.Fatalf("duplicate ref %q", c.Ref)
		}
		seen[c.Ref] = true
	}
	if !seen["GOV-IKEJA-WARD3-2026-000005"] {
		t.Fatalf("seq did not reach 5: %v", seen)
	}
	// Cross-tenant sequences are independent.
	other := uuid.New()
	fx.fake.seedCategory(other, "roads", "mda-works", 2, 49, true)
	c, err := fx.svc.Submit(context.Background(), other, "other-lga", ChannelWeb, fx.validInput())
	if err != nil {
		t.Fatalf("cross-tenant submit: %v", err)
	}
	if !strings.HasSuffix(c.Ref, "-000001") {
		t.Fatalf("other tenant seq = %q", c.Ref)
	}
}

// Submit validation: bad input, unknown category, inactive category.
func TestSubmitValidationFailures(t *testing.T) {
	fx := newCivicFixture(t)
	ctx := context.Background()

	in := fx.validInput()
	in.Description = "short"
	if _, err := fx.svc.Submit(ctx, fx.tenantID, fx.slug, ChannelWeb, in); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("short description err = %v", err)
	}
	in = fx.validInput()
	in.CategorySlug = "nonexistent"
	if _, err := fx.svc.Submit(ctx, fx.tenantID, fx.slug, ChannelWeb, in); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown category err = %v", err)
	}
	in = fx.validInput()
	in.CategorySlug = "archived" // seeded inactive
	if _, err := fx.svc.Submit(ctx, fx.tenantID, fx.slug, ChannelWeb, in); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("inactive category err = %v", err)
	}
	in = fx.validInput()
	if _, err := fx.svc.Submit(ctx, fx.tenantID, fx.slug, "carrier-pigeon", in); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad channel err = %v", err)
	}
	// Nothing persisted, no events.
	if len(fx.fake.cases) != 0 || len(fx.fake.outbox) != 0 {
		t.Fatal("failed submits must not persist or emit")
	}
}

// Routing precedence end-to-end: a ward override wins over the category
// default at intake.
func TestSubmitAppliesWardRoutingOverride(t *testing.T) {
	fx := newCivicFixture(t)
	ctx := context.Background()
	if err := fx.svc.Store.CreateCivicRoutingRule(ctx, &store.CivicRoutingRule{
		TenantID: fx.tenantID, CategoryID: fx.cat.ID, Ward: "Ward 3", MDAQueue: "mda-rapid-response",
	}); err != nil {
		t.Fatalf("create rule: %v", err)
	}
	c := fx.submit(t, fx.validInput()) // Ward 3
	if c.MDAQueue != "mda-rapid-response" {
		t.Fatalf("ward override = %q", c.MDAQueue)
	}
	in := fx.validInput()
	in.Ward = "Ward 9"
	c = fx.submit(t, in)
	if c.MDAQueue != "mda-works" {
		t.Fatalf("category default = %q", c.MDAQueue)
	}
}

// Tracking auth: ref+phone must match (SPEC-W32 §4.1 / WS-A).
func TestTrackMatchAndMismatch(t *testing.T) {
	fx := newCivicFixture(t)
	ctx := context.Background()
	c := fx.submit(t, fx.validInput())

	got, err := fx.svc.Track(ctx, fx.tenantID, c.Ref, "+2348012345678")
	if err != nil {
		t.Fatalf("track with correct phone: %v", err)
	}
	if got.Ref != c.Ref {
		t.Fatalf("tracked ref = %q", got.Ref)
	}
	if _, err := fx.svc.Track(ctx, fx.tenantID, c.Ref, "+2348099999999"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("wrong phone err = %v, want ErrNotFound", err)
	}
	if _, err := fx.svc.Track(ctx, fx.tenantID, c.Ref, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing phone err = %v, want ErrNotFound", err)
	}
	if _, err := fx.svc.Track(ctx, fx.tenantID, "GOV-NOPE-00-2026-000001", "+2348012345678"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown ref err = %v, want ErrNotFound", err)
	}
	// Anonymous report without a phone cannot be tracked at all.
	in := fx.validInput()
	in.ReporterPhoneE164 = ""
	in.Anonymous = true
	c2 := fx.submit(t, in)
	if _, err := fx.svc.Track(ctx, fx.tenantID, c2.Ref, "+2348012345678"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("phoneless case err = %v, want ErrNotFound", err)
	}
}

// Triage: status → triaged, acked_at stamped, SLA recomputed from the
// triage time, category/queue changes honored, StatusChanged emitted.
func TestTriageComputesSLAAndEmits(t *testing.T) {
	fx := newCivicFixture(t)
	ctx := context.Background()
	c := fx.submit(t, fx.validInput())
	other := fx.fake.seedCategory(fx.tenantID, "water", "mda-water", 12, 72, true)

	fx.now = fx.now.Add(3 * time.Hour)
	got, err := fx.svc.Triage(ctx, fx.tenantID, fx.slug, c.ID, TriageInput{CategoryID: &other.ID})
	if err != nil {
		t.Fatalf("triage: %v", err)
	}
	if got.Status != store.CivicStatusTriaged {
		t.Fatalf("status = %q", got.Status)
	}
	if got.AckedAt == nil || !got.AckedAt.Equal(fx.now) {
		t.Fatalf("acked_at = %v", got.AckedAt)
	}
	if got.AckDueAt == nil || !got.AckDueAt.Equal(fx.now.Add(12*time.Hour)) {
		t.Fatalf("ack due recomputed from triage time: %v", got.AckDueAt)
	}
	if got.ResolveDueAt == nil || !got.ResolveDueAt.Equal(fx.now.Add(72*time.Hour)) {
		t.Fatalf("resolve due recomputed: %v", got.ResolveDueAt)
	}
	if got.MDAQueue != "mda-water" {
		t.Fatalf("queue re-resolved: %q", got.MDAQueue)
	}
	evt := fx.lastEvent(t)
	if evt.Type != EventTypeStatusChanged || evt.Data["status"] != store.CivicStatusTriaged {
		t.Fatalf("event = %q %v", evt.Type, evt.Data)
	}
	// FIX W3: StatusChanged carries the recomputed due times.
	if v := eventTime(t, evt.Data["ack_due_at"]); v == nil || !v.Equal(*got.AckDueAt) {
		t.Fatalf("StatusChanged ack_due_at = %v, want %v", evt.Data["ack_due_at"], got.AckDueAt)
	}
	if v := eventTime(t, evt.Data["resolve_due_at"]); v == nil || !v.Equal(*got.ResolveDueAt) {
		t.Fatalf("StatusChanged resolve_due_at = %v, want %v", evt.Data["resolve_due_at"], got.ResolveDueAt)
	}
	if evt.ID != fx.slug+":civic:"+c.Ref+":000002" {
		t.Fatalf("event seq id = %q", evt.ID)
	}
	// Explicit queue override wins.
	got, err = fx.svc.Triage(ctx, fx.tenantID, fx.slug, c.ID, TriageInput{MDAQueue: strPtr("mda-override")})
	if err != nil {
		t.Fatalf("triage override: %v", err)
	}
	if got.MDAQueue != "mda-override" {
		t.Fatalf("explicit queue = %q", got.MDAQueue)
	}
}

func strPtr(s string) *string { return &s }

// Assign: status → assigned, assignee recorded, StatusChanged carries the
// reporter phone when wants_updates (WS-B notification payload).
func TestAssignEmitsStatusChanged(t *testing.T) {
	fx := newCivicFixture(t)
	ctx := context.Background()
	c := fx.submit(t, fx.validInput())

	got, err := fx.svc.Assign(ctx, fx.tenantID, fx.slug, c.ID, "works-team-lead")
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if got.Status != store.CivicStatusAssigned || got.AssignedTo == nil || *got.AssignedTo != "works-team-lead" {
		t.Fatalf("assign result = %q %v", got.Status, got.AssignedTo)
	}
	evt := fx.lastEvent(t)
	if evt.Type != EventTypeStatusChanged || evt.Data["status"] != store.CivicStatusAssigned {
		t.Fatalf("event = %q %v", evt.Type, evt.Data)
	}
	if evt.Data["reporter_phone"] != "+2348012345678" {
		t.Fatalf("reporter_phone missing: %v", evt.Data)
	}
	if evt.Data["assigned_to"] != "works-team-lead" {
		t.Fatalf("assigned_to missing: %v", evt.Data)
	}
	if _, err := fx.svc.Assign(ctx, fx.tenantID, fx.slug, c.ID, "  "); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty assignee err = %v", err)
	}
}

// Status machine: in_progress/resolved/closed stamp their clocks; bad
// targets rejected; closed cases are immutable.
func TestSetStatusTransitions(t *testing.T) {
	fx := newCivicFixture(t)
	ctx := context.Background()
	c := fx.submit(t, fx.validInput())

	got, err := fx.svc.SetStatus(ctx, fx.tenantID, fx.slug, c.ID, store.CivicStatusInProgress, "crew dispatched")
	if err != nil {
		t.Fatalf("in_progress: %v", err)
	}
	if got.Status != store.CivicStatusInProgress {
		t.Fatalf("status = %q", got.Status)
	}
	if evt := fx.lastEvent(t); evt.Data["note"] != "crew dispatched" {
		t.Fatalf("note not in event: %v", evt.Data)
	}
	got, err = fx.svc.SetStatus(ctx, fx.tenantID, fx.slug, c.ID, store.CivicStatusResolved, "")
	if err != nil {
		t.Fatalf("resolved: %v", err)
	}
	if got.ResolvedAt == nil {
		t.Fatal("resolved_at not stamped")
	}
	got, err = fx.svc.SetStatus(ctx, fx.tenantID, fx.slug, c.ID, store.CivicStatusClosed, "")
	if err != nil {
		t.Fatalf("closed: %v", err)
	}
	if got.ClosedAt == nil {
		t.Fatal("closed_at not stamped")
	}
	if _, err := fx.svc.SetStatus(ctx, fx.tenantID, fx.slug, c.ID, "reopened", ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad status err = %v", err)
	}
	if _, err := fx.svc.SetStatus(ctx, fx.tenantID, fx.slug, c.ID, store.CivicStatusResolved, ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("closed case mutation err = %v", err)
	}
}

// Merge semantics (SPEC-W32 §2/§4.3): the merged case points at the
// canonical, the Merged event carries both refs (notifications follow the
// canonical), merged rows are immutable, canonical-of-merged rejected.
func TestMergeSemantics(t *testing.T) {
	fx := newCivicFixture(t)
	ctx := context.Background()
	canonical := fx.submit(t, fx.validInput())
	dup := fx.submit(t, fx.validInput())

	got, err := fx.svc.Merge(ctx, fx.tenantID, fx.slug, dup.ID, canonical.ID)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got.MergedInto == nil || *got.MergedInto != canonical.ID {
		t.Fatalf("merged_into = %v", got.MergedInto)
	}
	// The merged case keeps its own status (it stays readable; the
	// canonical carries the workflow).
	if got.Status != store.CivicStatusNew {
		t.Fatalf("merged case status = %q", got.Status)
	}
	evt := fx.lastEvent(t)
	if evt.Type != EventTypeMerged {
		t.Fatalf("event = %q", evt.Type)
	}
	if evt.Data["canonical_ref"] != canonical.Ref || evt.Data["ref"] != dup.Ref {
		t.Fatalf("merged event data = %v", evt.Data)
	}
	if evt.Data["reporter_phone"] != "+2348012345678" {
		t.Fatalf("notification hand-off phone missing: %v", evt.Data)
	}
	// Merged case is immutable; tracking still resolves it (§4.3).
	if _, err := fx.svc.Triage(ctx, fx.tenantID, fx.slug, dup.ID, TriageInput{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("merged case triage err = %v", err)
	}
	if _, err := fx.svc.Track(ctx, fx.tenantID, dup.Ref, "+2348012345678"); err != nil {
		t.Fatalf("merged case must stay trackable: %v", err)
	}
	// Self-merge and merge-into-merged rejected.
	if _, err := fx.svc.Merge(ctx, fx.tenantID, fx.slug, dup.ID, dup.ID); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("self-merge err = %v", err)
	}
	third := fx.submit(t, fx.validInput())
	if _, err := fx.svc.Merge(ctx, fx.tenantID, fx.slug, third.ID, dup.ID); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("merge into merged err = %v", err)
	}
}

// Duplicates endpoint: geo ≤500m + same category + ±72h via the service.
func TestDuplicatesEndToEnd(t *testing.T) {
	fx := newCivicFixture(t)
	ctx := context.Background()
	lat, lon := 6.5244, 3.3792
	near := 6.5271
	far := 6.6

	mk := func(l float64) store.CivicCase {
		in := fx.validInput()
		in.Lat, in.Lon = &l, &lon
		return fx.submit(t, in)
	}
	base := mk(lat)
	nearCase := mk(near)
	mk(far)

	cands, err := fx.svc.Duplicates(ctx, fx.tenantID, base.ID)
	if err != nil {
		t.Fatalf("duplicates: %v", err)
	}
	if len(cands) != 1 || cands[0].ID != nearCase.ID {
		t.Fatalf("candidates = %+v, want only the near case", cands)
	}
	// No geo → no candidates (the far/phones-only cases can't be matched).
	in := fx.validInput()
	in.Lat, in.Lon = nil, nil
	noGeo := fx.submit(t, in)
	cands, err = fx.svc.Duplicates(ctx, fx.tenantID, noGeo.ID)
	if err != nil {
		t.Fatalf("duplicates no-geo: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("no-geo candidates = %+v", cands)
	}
}

// Internal sla-breach callback: flags set per kind, idempotent.
func TestMarkSLABreach(t *testing.T) {
	fx := newCivicFixture(t)
	ctx := context.Background()
	c := fx.submit(t, fx.validInput())

	got, err := fx.svc.MarkSLABreach(ctx, fx.tenantID, c.Ref, BreachAck)
	if err != nil {
		t.Fatalf("ack breach: %v", err)
	}
	if !got.SLABreachAck || got.SLABreachResolve {
		t.Fatalf("flags = %v/%v", got.SLABreachAck, got.SLABreachResolve)
	}
	got, err = fx.svc.MarkSLABreach(ctx, fx.tenantID, c.Ref, BreachResolve)
	if err != nil {
		t.Fatalf("resolve breach: %v", err)
	}
	if !got.SLABreachAck || !got.SLABreachResolve {
		t.Fatalf("flags = %v/%v", got.SLABreachAck, got.SLABreachResolve)
	}
	if _, err := fx.svc.MarkSLABreach(ctx, fx.tenantID, c.Ref, "bogus"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad kind err = %v", err)
	}
	if _, err := fx.svc.MarkSLABreach(ctx, fx.tenantID, "GOV-NOPE-00-2026-000001", BreachAck); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown ref err = %v", err)
	}
}

// Throttling: per-IP and per-phone limits both apply (10/hr, 50/day).
func TestCheckThrottle(t *testing.T) {
	fake := newFakeCivicStore()
	now := time.Now()
	svc := &Service{Store: fake, RatePerHour: 3, RatePerDay: 5}
	svc.Throttle = NewThrottler()
	svc.Throttle.SetClock(func() time.Time { return now })

	for i := 0; i < 3; i++ {
		if err := svc.CheckThrottle("1.2.3.4", "+2348012345678"); err != nil {
			t.Fatalf("hit %d: %v", i+1, err)
		}
	}
	if err := svc.CheckThrottle("1.2.3.4", ""); !errors.Is(err, ErrThrottled) {
		t.Fatalf("hourly ip limit err = %v", err)
	}
	// Per-phone limit independent of IP.
	if err := svc.CheckThrottle("9.9.9.9", "+2348012345678"); !errors.Is(err, ErrThrottled) {
		t.Fatalf("hourly phone limit err = %v", err)
	}
	// Daily limit: 2 more next hour (total 5), then blocked even hourly-ok.
	now = now.Add(61 * time.Minute)
	if err := svc.CheckThrottle("1.2.3.4", ""); err != nil {
		t.Fatalf("day hit 4: %v", err)
	}
	if err := svc.CheckThrottle("1.2.3.4", ""); err != nil {
		t.Fatalf("day hit 5: %v", err)
	}
	if err := svc.CheckThrottle("1.2.3.4", ""); !errors.Is(err, ErrThrottled) {
		t.Fatalf("daily ip limit err = %v", err)
	}
	// Disabled limits never throttle.
	open := &Service{Store: fake, RatePerHour: -1, RatePerDay: -1}
	for i := 0; i < 100; i++ {
		if err := open.CheckThrottle("1.2.3.4", "+2348012345678"); err != nil {
			t.Fatalf("disabled throttle: %v", err)
		}
	}
}

// Public stats: aggregate-only counts, merged duplicates excluded from
// their own bucket (SPEC-W32 §4.1 — no person fields exist in the payload).
func TestPublicStatsAggregateOnly(t *testing.T) {
	fx := newCivicFixture(t)
	ctx := context.Background()
	fx.submit(t, fx.validInput()) // open, Ward 3
	c2 := fx.submit(t, fx.validInput())
	if _, err := fx.svc.SetStatus(ctx, fx.tenantID, fx.slug, c2.ID, store.CivicStatusResolved, ""); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	c3 := fx.submit(t, fx.validInput())
	if _, err := fx.svc.Merge(ctx, fx.tenantID, fx.slug, c3.ID, c2.ID); err != nil {
		t.Fatalf("merge: %v", err)
	}

	stats, err := fx.svc.PublicStats(ctx, fx.tenantID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Open != 1 || stats.Resolved != 1 {
		t.Fatalf("totals = %d/%d (merged must be excluded)", stats.Open, stats.Resolved)
	}
	if len(stats.ByCategory) != 1 || stats.ByCategory[0].Key != "roads" ||
		stats.ByCategory[0].Open != 1 || stats.ByCategory[0].Resolved != 1 {
		t.Fatalf("by_category = %+v", stats.ByCategory)
	}
	if len(stats.ByWard) != 1 || stats.ByWard[0].Key != "Ward 3" {
		t.Fatalf("by_ward = %+v", stats.ByWard)
	}
	// The stats payload is JSON-serialized the same way the HTTP layer does
	// — assert no person data can leak through it.
	raw, err := json.Marshal(stats)
	if err != nil {
		t.Fatalf("marshal stats: %v", err)
	}
	for _, leak := range []string{"+2348012345678", "Adaeze", "phone", "reporter"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("stats leak %q: %s", leak, raw)
		}
	}
}

// Event sequence: every emission bumps the per-case seq → deterministic,
// strictly increasing CloudEvent ids (tenant:civic:ref:{seq}).
func TestEventIDSequence(t *testing.T) {
	fx := newCivicFixture(t)
	ctx := context.Background()
	c := fx.submit(t, fx.validInput())
	if _, err := fx.svc.Triage(ctx, fx.tenantID, fx.slug, c.ID, TriageInput{}); err != nil {
		t.Fatalf("triage: %v", err)
	}
	if _, err := fx.svc.Assign(ctx, fx.tenantID, fx.slug, c.ID, "crew"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if len(fx.fake.outbox) != 3 {
		t.Fatalf("events = %d", len(fx.fake.outbox))
	}
	for i, row := range fx.fake.outbox {
		want := fx.slug + ":civic:" + c.Ref + ":" + seq6(int64(i+1))
		if row.event.ID != want {
			t.Fatalf("event %d id = %q, want %q", i, row.event.ID, want)
		}
		if row.event.TenantID != fx.tenantID.String() {
			t.Fatalf("event %d tenantid = %q", i, row.event.TenantID)
		}
	}
}

func seq6(n int64) string { return fmt.Sprintf("%06d", n) }
