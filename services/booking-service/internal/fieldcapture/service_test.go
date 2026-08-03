package fieldcapture

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/leads"
	"github.com/opendesk/booking-service/internal/store"
)

// SPEC-W16 Agent B service tests run against embedded Postgres (same
// harness as the leads service tests; dedicated port 5562 avoids the
// postmaster.pid race under `go test ./...`; -short skips). The leads
// service runs with CACEventsTopic="" so funnel emission is disabled and
// no outbox table is needed.

type testRig struct {
	svc    *Service
	fstore *Store
	leads  *store.Store
	tenant bookingops.TenantInfo
}

func newTestRig(t *testing.T) testRig {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres fieldcapture test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_fieldcapture_test").
		Port(5562).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	dsn := "postgres://postgres:postgres@localhost:5562/booking_fieldcapture_test?sslmode=disable"
	leadStore, err := store.New(ctx, dsn, 0)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(leadStore.Close)
	fs, err := DialStore(ctx, dsn)
	if err != nil {
		t.Fatalf("DialStore: %v", err)
	}
	t.Cleanup(fs.Close)

	leadSvc := &leads.Service{Store: leadStore} // CACEventsTopic "" → emission off
	return testRig{
		svc:    &Service{Store: fs, Leads: leadSvc},
		fstore: fs,
		leads:  leadStore,
		tenant: bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"},
	}
}

func mkItem(clientID, kind string, payload any, gps *GPS) CaptureItem {
	raw, _ := json.Marshal(payload)
	now := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	return CaptureItem{ClientID: clientID, Kind: kind, Payload: raw, CapturedAt: &now, GPS: gps}
}

// kind=lead_capture: applies via the leads service with channel "field";
// a client_id replay dedupes onto the ORIGINAL lead (contract §4); the
// leads 24h dedupe stacks underneath (same phone re-captured with a NEW
// client_id same day → same first-touch lead, no duplicate row).
func TestLeadCaptureApplyAndDedupe(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	item := mkItem(uuid.NewString(), KindLeadCapture,
		LeadCapturePayload{PhoneE164: "+2348011111111", Name: "Ada", Notes: "met at stall"}, nil)
	res := rig.svc.Capture(ctx, rig.tenant, []CaptureItem{item})
	if len(res) != 1 || res[0].Status != StatusApplied || res[0].LeadID == nil {
		t.Fatalf("apply: %+v", res)
	}
	leadID := *res[0].LeadID

	lead, err := rig.leads.GetLead(ctx, rig.tenant.ID, leadID)
	if err != nil {
		t.Fatalf("lead persisted: %v", err)
	}
	if lead.ChannelOfFirstTouch != leads.ChannelField || lead.PhoneE164 != "+2348011111111" {
		t.Fatalf("lead fields: %+v", lead)
	}

	// Replay the SAME client_id (queue flush retry): deduped, same lead,
	// no new side effects.
	replay := rig.svc.Capture(ctx, rig.tenant, []CaptureItem{item})
	if len(replay) != 1 || replay[0].Status != StatusDeduped {
		t.Fatalf("replay: %+v", replay)
	}
	if replay[0].LeadID == nil || *replay[0].LeadID != leadID {
		t.Fatalf("replay must return the original lead_id: %+v", replay[0])
	}

	// Same phone under a NEW client_id on the same day: the anchor is fresh
	// (applied) but the leads 24h dedupe returns the SAME first-touch lead.
	samePhone := mkItem(uuid.NewString(), KindLeadCapture,
		LeadCapturePayload{PhoneE164: "+2348011111111"}, nil)
	res2 := rig.svc.Capture(ctx, rig.tenant, []CaptureItem{samePhone})
	if res2[0].Status != StatusApplied || res2[0].LeadID == nil || *res2[0].LeadID != leadID {
		t.Fatalf("leads 24h dedupe stacking: %+v", res2[0])
	}

	all, err := rig.leads.ListLeads(ctx, rig.tenant.ID, "", "", nil, nil, nil)
	if err != nil || len(all) != 1 {
		t.Fatalf("exactly one lead row expected: %+v, %v", all, err)
	}
}

// kind=checkin: appends to field_checkins (the W8 store has no history);
// GPS + note land on the row; a replay dedupes without a duplicate row.
func TestCheckinApplyAndDedupe(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	contactID := uuid.New()

	item := mkItem(uuid.NewString(), KindCheckin,
		CheckinPayload{ContactID: &contactID, Note: "site visit"},
		&GPS{Lat: 6.5244, Lng: 3.3792, Accuracy: 12.5})
	res := rig.svc.Capture(ctx, rig.tenant, []CaptureItem{item})
	if len(res) != 1 || res[0].Status != StatusApplied || res[0].CheckinID == nil {
		t.Fatalf("apply: %+v", res)
	}
	checkinID := *res[0].CheckinID

	rows, err := rig.fstore.ListCheckins(ctx, rig.tenant.ID, &contactID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("checkins: %+v, %v", rows, err)
	}
	c := rows[0]
	if c.ID != checkinID || c.Lat == nil || *c.Lat != 6.5244 || c.Lng == nil || *c.Lng != 3.3792 ||
		c.AccuracyM == nil || *c.AccuracyM != 12.5 || c.Note != "site visit" || c.CapturedAt == nil {
		t.Fatalf("checkin row: %+v", c)
	}

	replay := rig.svc.Capture(ctx, rig.tenant, []CaptureItem{item})
	if replay[0].Status != StatusDeduped || replay[0].CheckinID == nil || *replay[0].CheckinID != checkinID {
		t.Fatalf("replay: %+v", replay)
	}
	rows, _ = rig.fstore.ListCheckins(ctx, rig.tenant.ID, nil)
	if len(rows) != 1 {
		t.Fatalf("replay must not duplicate the check-in: %+v", rows)
	}

	// GPS is nullable (contract §4): a check-in without a fix still applies.
	noGPS := mkItem(uuid.NewString(), KindCheckin, CheckinPayload{Note: "manual"}, nil)
	res2 := rig.svc.Capture(ctx, rig.tenant, []CaptureItem{noGPS})
	if res2[0].Status != StatusApplied {
		t.Fatalf("no-gps checkin: %+v", res2[0])
	}
}

// Deterministic validation failures: invalid kind is rejected WITHOUT an
// anchor (a replay fails identically); a schema-valid but semantically
// invalid lead_capture (no phone) is anchored as error — its replay
// dedupes onto the recorded error.
func TestValidationOutcomes(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	badKind := mkItem(uuid.NewString(), "survey", map[string]any{}, nil)
	res := rig.svc.Capture(ctx, rig.tenant, []CaptureItem{badKind})
	if res[0].Status != StatusError {
		t.Fatalf("bad kind: %+v", res[0])
	}
	// No anchor recorded → replay is a fresh deterministic error, not a dedupe.
	if again := rig.svc.Capture(ctx, rig.tenant, []CaptureItem{badKind}); again[0].Status != StatusError {
		t.Fatalf("bad-kind replay: %+v", again[0])
	}

	noPhone := mkItem(uuid.NewString(), KindLeadCapture, LeadCapturePayload{Name: "no phone"}, nil)
	first := rig.svc.Capture(ctx, rig.tenant, []CaptureItem{noPhone})
	if first[0].Status != StatusError || first[0].Error == "" {
		t.Fatalf("no-phone: %+v", first[0])
	}
	replay := rig.svc.Capture(ctx, rig.tenant, []CaptureItem{noPhone})
	if replay[0].Status != StatusDeduped || replay[0].Error != first[0].Error {
		t.Fatalf("no-phone replay must dedupe onto the recorded error: %+v", replay[0])
	}

	badGPS := mkItem(uuid.NewString(), KindCheckin, CheckinPayload{}, &GPS{Lat: 91, Lng: 0})
	if out := rig.svc.Capture(ctx, rig.tenant, []CaptureItem{badGPS}); out[0].Status != StatusError {
		t.Fatalf("bad gps: %+v", out[0])
	}
	emptyClient := mkItem("", KindCheckin, CheckinPayload{}, nil)
	if out := rig.svc.Capture(ctx, rig.tenant, []CaptureItem{emptyClient}); out[0].Status != StatusError {
		t.Fatalf("empty client_id: %+v", out[0])
	}
}

// Batch semantics: one result per item, in request order; item failures do
// not fail sibling items.
func TestBatchOrdering(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	items := []CaptureItem{
		mkItem("11111111-1111-1111-1111-111111111111", KindLeadCapture, LeadCapturePayload{PhoneE164: "+234801"}, nil),
		mkItem("22222222-2222-2222-2222-222222222222", "bogus", map[string]any{}, nil),
		mkItem("33333333-3333-3333-3333-333333333333", KindCheckin, CheckinPayload{Note: "n"}, &GPS{Lat: 6.5, Lng: 3.4}),
	}
	res := rig.svc.Capture(ctx, rig.tenant, items)
	if len(res) != 3 {
		t.Fatalf("results: %+v", res)
	}
	if res[0].ClientID != items[0].ClientID || res[0].Status != StatusApplied ||
		res[1].ClientID != items[1].ClientID || res[1].Status != StatusError ||
		res[2].ClientID != items[2].ClientID || res[2].Status != StatusApplied {
		t.Fatalf("order/status: %+v", res)
	}
}

// Pure validation (no DB): item + GPS rules.
func TestItemValidate(t *testing.T) {
	ok := mkItem("c1", KindLeadCapture, map[string]any{"phone_e164": "+234"}, nil)
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid item rejected: %v", err)
	}
	if ok.Payload == nil {
		t.Fatal("nil payload must default to {}")
	}
	bad := mkItem("c1", KindCheckin, nil, &GPS{Lat: 0, Lng: 181})
	if err := bad.Validate(); err == nil {
		t.Fatal("lng 181 must fail")
	}
	raw := CaptureItem{ClientID: "c2", Kind: KindCheckin, Payload: json.RawMessage(`{not json`)}
	if err := raw.Validate(); err == nil {
		t.Fatal("invalid JSON payload must fail")
	}
}
