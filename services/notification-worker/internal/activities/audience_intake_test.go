package activities

// SPEC-W28 WS-C (verification-gate fix): audience intake tests — members
// envelope decode (binding contract shape, incl. lead_id=null), lead→phone
// resolution via booking-service (success/partial/none/down), tenant
// scoping, idempotency, trajectory emission, and the end-to-end
// resolvable/leadless/unknown-lead split.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/opendesk/notification-worker/internal/workflows"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.uber.org/zap"
)

// fakeAudienceStarter records ExecuteWorkflow calls (notifyoutbox test idiom).
type fakeAudienceStarter struct {
	mu      sync.Mutex
	started []struct {
		id  string
		req workflows.PacedSendRequest
	}
	// errFor maps a recipient phone to the error returned for that start
	// (AlreadyStarted injection); err applies to all.
	err    error
	errFor map[string]error
}

func (f *fakeAudienceStarter) ExecuteWorkflow(_ context.Context, opts client.StartWorkflowOptions, _ interface{}, args ...interface{}) (client.WorkflowRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var req workflows.PacedSendRequest
	if len(args) > 0 {
		req, _ = args[0].(workflows.PacedSendRequest)
	}
	for phone, err := range f.errFor {
		if err != nil && req.GeoCampaign != nil && req.GeoCampaign.Phone == phone {
			return nil, err
		}
	}
	f.started = append(f.started, struct {
		id  string
		req workflows.PacedSendRequest
	}{opts.ID, req})
	return nil, nil
}

func (f *fakeAudienceStarter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started)
}

// fakePhoneResolver fakes booking-service's POST /v1/leads/resolve.
type fakePhoneResolver struct {
	mu       sync.Mutex
	phones   map[string]string // lead_id → e164
	err      error
	requests []struct {
		tenantSlug string
		leadIDs    []string
	}
}

func (r *fakePhoneResolver) ResolveLeadPhones(_ context.Context, tenantSlug string, leadIDs []string) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, struct {
		tenantSlug string
		leadIDs    []string
	}{tenantSlug, append([]string(nil), leadIDs...)})
	if r.err != nil {
		return nil, r.err
	}
	out := map[string]string{}
	for _, id := range leadIDs {
		if p, ok := r.phones[id]; ok {
			out[id] = p
		}
	}
	return out, nil
}

func (r *fakePhoneResolver) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

// fakeTrajectoryProducer records produced rows; fail makes Produce error.
type fakeTrajectoryProducer struct {
	mu   sync.Mutex
	rows []struct {
		topic   string
		key     string
		payload []byte
	}
	fail bool
}

func (p *fakeTrajectoryProducer) Produce(_ context.Context, topic string, key, payload []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail {
		return errors.New("kafka down")
	}
	p.rows = append(p.rows, struct {
		topic   string
		key     string
		payload []byte
	}{topic, string(key), append([]byte(nil), payload...)})
	return nil
}

func (p *fakeTrajectoryProducer) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.rows)
}

// graphStub serves a canned audience and records the tenant headers it saw.
func graphStub(t *testing.T, status int, body string, seen *map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			(*seen)["path"] = r.URL.Path
			(*seen)["tenant_id"] = r.Header.Get("X-Tenant-Id")
			(*seen)["tenant_slug"] = r.Header.Get("X-Tenant-Slug")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func strptr(s string) *string { return &s }

func newTestIntake(graphURL string, starter *fakeAudienceStarter, resolver *fakePhoneResolver, producer *fakeTrajectoryProducer) *AudienceIntake {
	var phones LeadPhoneResolver
	if resolver != nil {
		phones = resolver
	}
	return &AudienceIntake{
		GraphServiceURL: graphURL,
		TrajectoryTopic: DefaultTrajectoryTopic,
		Starter:         starter,
		TaskQueue:       "opendesk-main",
		Claims:          NewInMemoryAudienceClaims(),
		Phones:          phones,
		Trajectories:    producer,
		Log:             zap.NewNop(),
	}
}

func intakeReq(tenant, slug, campaign string) AudienceIntakeRequest {
	return AudienceIntakeRequest{
		TenantID:   tenant,
		TenantSlug: slug,
		SegmentID:  "seg-1",
		CampaignID: campaign,
		Message:    "Hi {name}, we miss you!",
	}
}

// decodeAudience: the binding contract envelope {"members":[{person_id,
// phone_hash, lead_id}]} decodes, lead_id=null survives as nil, and the
// legacy envelopes keep working.
func TestDecodeAudienceMembersEnvelope(t *testing.T) {
	members, err := decodeAudience([]byte(`{"members":[
		{"person_id":"p-1","phone_hash":"abc123","lead_id":"lead-1"},
		{"person_id":"p-2","phone_hash":"def456","lead_id":null}
	]}`))
	if err != nil {
		t.Fatalf("decode members: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("members = %d, want 2", len(members))
	}
	if members[0].PersonID != "p-1" || members[0].PhoneHash != "abc123" || members[0].LeadID == nil || *members[0].LeadID != "lead-1" {
		t.Fatalf("member[0] = %+v", members[0])
	}
	if members[1].LeadID != nil {
		t.Fatalf("member[1].lead_id = %v, want nil (JSON null)", *members[1].LeadID)
	}

	// Legacy fallbacks still decode.
	for _, body := range []string{
		`{"audience":[{"person_id":"p-1","phone":"+234801"}]}`,
		`{"persons":[{"person_id":"p-1","phone":"+234801"}]}`,
		`[{"person_id":"p-1","phone":"+234801"}]`,
	} {
		got, err := decodeAudience([]byte(body))
		if err != nil || len(got) != 1 || got[0].Phone != "+234801" {
			t.Fatalf("legacy decode %s = %+v, %v", body, got, err)
		}
	}
}

// Resolution success: a members-envelope audience has its lead_ids resolved
// via booking-service and the E.164 flows into the paced send unchanged.
func TestAudienceIntakeResolvesLeadPhones(t *testing.T) {
	gs := graphStub(t, http.StatusOK, `{"members":[
		{"person_id":"p-1","phone_hash":"h1","lead_id":"lead-1"},
		{"person_id":"p-2","phone_hash":"h2","lead_id":"lead-2"}
	]}`, nil)
	defer gs.Close()
	starter := &fakeAudienceStarter{}
	resolver := &fakePhoneResolver{phones: map[string]string{
		"lead-1": "+234801111111",
		"lead-2": "+234802222222",
	}}
	ai := newTestIntake(gs.URL, starter, resolver, &fakeTrajectoryProducer{})

	res, err := ai.Intake(context.Background(), intakeReq("tenant-a-id", "acme", "camp-1"))
	if err != nil {
		t.Fatalf("intake: %v", err)
	}
	if res.Enqueued != 2 || res.SkippedNoPhone != 0 {
		t.Fatalf("result = %+v, want enqueued 2", res)
	}
	// Exactly ONE bulk resolution call, tenant-scoped by slug.
	if resolver.callCount() != 1 {
		t.Fatalf("resolver calls = %d, want 1 (bulk)", resolver.callCount())
	}
	if resolver.requests[0].tenantSlug != "acme" {
		t.Fatalf("resolver tenant = %q, want acme", resolver.requests[0].tenantSlug)
	}
	if starter.started[0].req.GeoCampaign.Phone != "+234801111111" ||
		starter.started[1].req.GeoCampaign.Phone != "+234802222222" {
		t.Fatalf("phones = %q, %q", starter.started[0].req.GeoCampaign.Phone, starter.started[1].req.GeoCampaign.Phone)
	}
	// Resolved phones ride the EXISTING paced path (geo_campaign kind).
	for _, s := range starter.started {
		if s.req.Kind != workflows.PacedSendGeoCampaign {
			t.Fatalf("kind = %q, want geo_campaign", s.req.Kind)
		}
	}
}

// END-TO-END (orchestrator case): audience with 3 members — 1 resolvable,
// 1 leadless (lead_id=null), 1 unknown-lead (resolver omits it, e.g.
// cross-tenant or deleted) → exactly ONE workflow started.
func TestAudienceIntakeEndToEndResolvableLeadlessUnknown(t *testing.T) {
	gs := graphStub(t, http.StatusOK, `{"members":[
		{"person_id":"p-ok","phone_hash":"h1","lead_id":"lead-known"},
		{"person_id":"p-leadless","phone_hash":"h2","lead_id":null},
		{"person_id":"p-unknown","phone_hash":"h3","lead_id":"lead-gone"}
	]}`, nil)
	defer gs.Close()
	starter := &fakeAudienceStarter{}
	resolver := &fakePhoneResolver{phones: map[string]string{"lead-known": "+234801"}}
	ai := newTestIntake(gs.URL, starter, resolver, &fakeTrajectoryProducer{})

	res, err := ai.Intake(context.Background(), intakeReq("tenant-a-id", "acme", "camp-e2e"))
	if err != nil {
		t.Fatalf("intake: %v", err)
	}
	if res.AudienceSize != 3 || res.Enqueued != 1 || res.SkippedNoPhone != 2 {
		t.Fatalf("result = %+v, want size 3, enqueued 1, skipped_no_phone 2", res)
	}
	if starter.count() != 1 {
		t.Fatalf("workflows started = %d, want exactly 1", starter.count())
	}
	got := starter.started[0]
	if got.id != "audience-camp-e2e-p-ok" || got.req.GeoCampaign.Phone != "+234801" {
		t.Fatalf("started = %q %+v", got.id, got.req.GeoCampaign)
	}
	// The leadless member's id never reaches booking-service.
	for _, id := range resolver.requests[0].leadIDs {
		if id != "lead-known" && id != "lead-gone" {
			t.Fatalf("resolver asked about %q, want only lead-linked members", id)
		}
	}
}

// Resolution NONE: booking-service resolves zero leads (all unknown) →
// every member skips, no workflow starts, intake still succeeds (200).
func TestAudienceIntakeResolutionNone(t *testing.T) {
	gs := graphStub(t, http.StatusOK, `{"members":[
		{"person_id":"p-1","phone_hash":"h1","lead_id":"lead-x"},
		{"person_id":"p-2","phone_hash":"h2","lead_id":"lead-y"}
	]}`, nil)
	defer gs.Close()
	starter := &fakeAudienceStarter{}
	ai := newTestIntake(gs.URL, starter, &fakePhoneResolver{phones: map[string]string{}}, &fakeTrajectoryProducer{})
	res, err := ai.Intake(context.Background(), intakeReq("tenant-a-id", "acme", "camp-none"))
	if err != nil {
		t.Fatalf("intake: %v", err)
	}
	if res.Enqueued != 0 || res.SkippedNoPhone != 2 || starter.count() != 0 {
		t.Fatalf("result = %+v started %d, want 0 enqueued / 2 skipped", res, starter.count())
	}
}

// Booking-service DOWN: resolution failure degrades the whole intake as
// *AudienceGraphDownError, NOTHING is enqueued, and the claim is released
// so the retry (booking-service healthy again) succeeds.
func TestAudienceIntakeBookingServiceDownDegradation(t *testing.T) {
	gs := graphStub(t, http.StatusOK, `{"members":[
		{"person_id":"p-1","phone_hash":"h1","lead_id":"lead-1"}
	]}`, nil)
	defer gs.Close()
	starter := &fakeAudienceStarter{}
	producer := &fakeTrajectoryProducer{}
	resolver := &fakePhoneResolver{err: errors.New("booking-service unreachable")}
	ai := newTestIntake(gs.URL, starter, resolver, producer)

	req := intakeReq("tenant-a-id", "acme", "camp-retry")
	_, err := ai.Intake(context.Background(), req)
	var downErr *AudienceGraphDownError
	if !errors.As(err, &downErr) {
		t.Fatalf("err = %v (%T), want *AudienceGraphDownError", err, err)
	}
	if starter.count() != 0 || producer.count() != 0 {
		t.Fatal("booking-service down must enqueue/emit nothing")
	}

	// Claim released → retry succeeds once the resolver recovers.
	resolver.err = nil
	resolver.phones = map[string]string{"lead-1": "+234801"}
	res, err := ai.Intake(context.Background(), req)
	if err != nil {
		t.Fatalf("retry after degradation: %v", err)
	}
	if res.Duplicate || res.Enqueued != 1 {
		t.Fatalf("retry = %+v, want a fresh enqueued-1 intake", res)
	}
}

// Resolution without a configured resolver (nil Phones): lead-linked
// members skip gracefully with a warn; legacy direct-phone members send.
func TestAudienceIntakeResolverNotConfigured(t *testing.T) {
	gs := graphStub(t, http.StatusOK, `{"members":[
		{"person_id":"p-legacy","phone":"+234809"},
		{"person_id":"p-lead","phone_hash":"h1","lead_id":"lead-1"}
	]}`, nil)
	defer gs.Close()
	starter := &fakeAudienceStarter{}
	ai := newTestIntake(gs.URL, starter, nil, &fakeTrajectoryProducer{})
	res, err := ai.Intake(context.Background(), intakeReq("tenant-a-id", "acme", "camp-nores"))
	if err != nil {
		t.Fatalf("intake: %v", err)
	}
	if res.Enqueued != 1 || res.SkippedNoPhone != 1 {
		t.Fatalf("result = %+v, want enqueued 1 (legacy phone), skipped 1", res)
	}
}

// Tenant scoping (SPEC-W28 §5 gate 1): the tenant from the JWT seam rides
// the X-Tenant-Id header to graph-service and scopes every started send; two
// tenants may reuse the same campaign_id without dedupe collisions.
func TestAudienceIntakeTenantScoping(t *testing.T) {
	seen := map[string]string{}
	gs := graphStub(t, http.StatusOK, `{"members":[
		{"person_id":"p-1","phone_hash":"h1","lead_id":"lead-1"},
		{"person_id":"p-2","phone_hash":"h2","lead_id":"lead-2"}
	]}`, &seen)
	defer gs.Close()
	starter := &fakeAudienceStarter{}
	resolver := &fakePhoneResolver{phones: map[string]string{"lead-1": "+234801", "lead-2": "+234802"}}
	ai := newTestIntake(gs.URL, starter, resolver, &fakeTrajectoryProducer{})

	res, err := ai.Intake(context.Background(), intakeReq("tenant-a-id", "acme", "camp-1"))
	if err != nil {
		t.Fatalf("intake tenant A: %v", err)
	}
	if res.Enqueued != 2 {
		t.Fatalf("enqueued = %d, want 2", res.Enqueued)
	}
	if seen["tenant_id"] != "tenant-a-id" || seen["tenant_slug"] != "acme" {
		t.Fatalf("graph-service saw tenant headers %v, want tenant-a-id/acme", seen)
	}
	if seen["path"] != "/v1/graph/segments/seg-1/audience" {
		t.Fatalf("graph path = %q", seen["path"])
	}
	for _, s := range starter.started {
		if s.req.GeoCampaign == nil || s.req.GeoCampaign.TenantSlug != "acme" {
			t.Fatalf("send not scoped to tenant slug acme: %+v", s.req.GeoCampaign)
		}
	}
	if starter.started[0].id != "audience-camp-1-p-1" {
		t.Fatalf("workflow id = %q, want audience-camp-1-p-1", starter.started[0].id)
	}

	// Same campaign_id under ANOTHER tenant is a distinct claim.
	resB, err := ai.Intake(context.Background(), intakeReq("tenant-b-id", "beta", "camp-1"))
	if err != nil {
		t.Fatalf("intake tenant B: %v", err)
	}
	if resB.Duplicate {
		t.Fatal("tenant B must not dedupe against tenant A's campaign")
	}
	if starter.count() != 4 {
		t.Fatalf("started = %d, want 4 (2 per tenant)", starter.count())
	}
	if starter.started[2].req.GeoCampaign.TenantSlug != "beta" {
		t.Fatalf("tenant B send scoped to %q, want beta", starter.started[2].req.GeoCampaign.TenantSlug)
	}
}

// Idempotency (SPEC-W24): a replayed intake (same tenant+campaign_id)
// answers duplicate=true with zero side effects — no extra graph fetch
// effect, no resolver call, no workflow, no trajectory; per-recipient
// AlreadyStarted is tolerated and counted.
func TestAudienceIntakeIdempotentByCampaign(t *testing.T) {
	gs := graphStub(t, http.StatusOK, `{"members":[
		{"person_id":"p-1","phone_hash":"h1","lead_id":"lead-1"},
		{"person_id":"p-2","phone_hash":"h2","lead_id":"lead-2"}
	]}`, nil)
	defer gs.Close()
	starter := &fakeAudienceStarter{}
	resolver := &fakePhoneResolver{phones: map[string]string{"lead-1": "+234801", "lead-2": "+234802"}}
	producer := &fakeTrajectoryProducer{}
	ai := newTestIntake(gs.URL, starter, resolver, producer)

	req := intakeReq("tenant-a-id", "acme", "camp-dup")
	req.IdempotencyKey = "idem-123"
	res, err := ai.Intake(context.Background(), req)
	if err != nil {
		t.Fatalf("first intake: %v", err)
	}
	if res.Duplicate || res.Enqueued != 2 {
		t.Fatalf("first intake = %+v, want enqueued 2, not duplicate", res)
	}

	res2, err := ai.Intake(context.Background(), req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !res2.Duplicate {
		t.Fatal("replay must be flagged duplicate")
	}
	if starter.count() != 2 || producer.count() != 2 || resolver.callCount() != 1 {
		t.Fatalf("replay had side effects: started %d, trajectories %d, resolver calls %d",
			starter.count(), producer.count(), resolver.callCount())
	}

	// Per-recipient AlreadyStarted (e.g. Temporal restart mid-intake) is
	// tolerated: counted as already_running, never an error, never a resend.
	ai2 := newTestIntake(gs.URL, &fakeAudienceStarter{
		errFor: map[string]error{
			"+234802": serviceerror.NewWorkflowExecutionAlreadyStarted("already started", "req-1", "run-1"),
		},
	}, resolver, producer)
	res3, err := ai2.Intake(context.Background(), intakeReq("tenant-a-id", "acme", "camp-partial"))
	if err != nil {
		t.Fatalf("partial already-started intake: %v", err)
	}
	if res3.Enqueued != 1 || res3.AlreadyRunning != 1 {
		t.Fatalf("partial intake = %+v, want enqueued 1 already_running 1", res3)
	}
}

// Graph-service down: transport errors and 5xx surface as
// *AudienceGraphDownError, NOTHING is enqueued, and the claim is released
// so the caller's retry against a healthy graph-service succeeds.
func TestAudienceIntakeGraphDownDegradation(t *testing.T) {
	down := graphStub(t, http.StatusServiceUnavailable, `{"error":"falkordb unreachable"}`, nil)
	starter := &fakeAudienceStarter{}
	producer := &fakeTrajectoryProducer{}
	resolver := &fakePhoneResolver{phones: map[string]string{"lead-1": "+234801"}}
	ai := newTestIntake(down.URL, starter, resolver, producer)
	down.Close() // force transport error too

	req := intakeReq("tenant-a-id", "acme", "camp-retry")
	_, err := ai.Intake(context.Background(), req)
	var downErr *AudienceGraphDownError
	if !errors.As(err, &downErr) {
		t.Fatalf("err = %v (%T), want *AudienceGraphDownError", err, err)
	}
	if starter.count() != 0 || producer.count() != 0 || resolver.callCount() != 0 {
		t.Fatal("graph down must resolve/enqueue/emit nothing")
	}

	healthy := graphStub(t, http.StatusOK, `{"members":[
		{"person_id":"p-1","phone_hash":"h1","lead_id":"lead-1"}
	]}`, nil)
	defer healthy.Close()
	ai.GraphServiceURL = healthy.URL
	res, err := ai.Intake(context.Background(), req)
	if err != nil {
		t.Fatalf("retry after degradation: %v", err)
	}
	if res.Duplicate || res.Enqueued != 1 {
		t.Fatalf("retry = %+v, want a fresh enqueued-1 intake", res)
	}
}

// Trajectory emission (ART seam): one send×outcome row per enqueued
// recipient on opendesk.usage.events, keyed by campaign_id; skipped members
// emit nothing; a failing producer degrades to logged-only.
func TestAudienceIntakeTrajectoryEmission(t *testing.T) {
	gs := graphStub(t, http.StatusOK, `{"members":[
		{"person_id":"p-1","phone_hash":"h1","lead_id":"lead-1"},
		{"person_id":"p-2","phone_hash":"h2","lead_id":"lead-2"},
		{"person_id":"p-3","phone_hash":"h3","lead_id":null},
		{"person_id":"p-4","phone_hash":"h4","lead_id":"lead-4","quarantine":true}
	]}`, nil)
	defer gs.Close()
	starter := &fakeAudienceStarter{}
	resolver := &fakePhoneResolver{phones: map[string]string{"lead-1": "+234801", "lead-2": "+234802"}}
	producer := &fakeTrajectoryProducer{}
	ai := newTestIntake(gs.URL, starter, resolver, producer)

	res, err := ai.Intake(context.Background(), intakeReq("tenant-a-id", "acme", "camp-traj"))
	if err != nil {
		t.Fatalf("intake: %v", err)
	}
	if res.AudienceSize != 4 || res.Enqueued != 2 || res.SkippedNoPhone != 1 || res.SkippedQuarantined != 1 {
		t.Fatalf("result = %+v, want size 4 enqueued 2 skipped 1+1", res)
	}
	if res.TrajectoriesEmitted != 2 || producer.count() != 2 {
		t.Fatalf("trajectories = %d/%d, want 2", res.TrajectoriesEmitted, producer.count())
	}
	row := producer.rows[0]
	if row.topic != DefaultTrajectoryTopic {
		t.Fatalf("topic = %q, want %s", row.topic, DefaultTrajectoryTopic)
	}
	if row.key != "camp-traj" {
		t.Fatalf("kafka key = %q, want campaign_id camp-traj", row.key)
	}
	var evt struct {
		Type     string         `json:"type"`
		Source   string         `json:"source"`
		TenantID string         `json:"tenantid"`
		Data     map[string]any `json:"data"`
	}
	if err := json.Unmarshal(row.payload, &evt); err != nil {
		t.Fatalf("trajectory payload: %v", err)
	}
	if evt.Type != TrajectoryEventType || evt.Source != "notification-worker" || evt.TenantID != "tenant-a-id" {
		t.Fatalf("envelope = %+v", evt)
	}
	for k, want := range map[string]string{
		"tenant_id": "tenant-a-id", "campaign_id": "camp-traj", "segment_id": "seg-1",
		"person_id": "p-1", "channel": "sms", "send": "enqueued", "outcome": "pending",
	} {
		if fmt.Sprint(evt.Data[k]) != want {
			t.Fatalf("data[%s] = %v, want %s (row %s)", k, evt.Data[k], want, row.payload)
		}
	}
	// {name} substitution: the binding member shape carries no name → "there".
	if starter.started[0].req.GeoCampaign.Text != "Hi there, we miss you!" {
		t.Fatalf("text = %q, want empty name → 'there'", starter.started[0].req.GeoCampaign.Text)
	}

	// Producer failure: logged-only, the send side is unaffected.
	producer.fail = true
	res2, err := ai.Intake(context.Background(), intakeReq("tenant-a-id", "acme", "camp-traj-2"))
	if err != nil {
		t.Fatalf("intake with failing producer: %v", err)
	}
	if res2.Enqueued != 2 || res2.TrajectoriesEmitted != 0 {
		t.Fatalf("failing-producer result = %+v, want enqueued 2 emitted 0", res2)
	}
}

// Validation: tenant, segment, campaign and message are required; channels
// are allowlisted to the geo-campaign set.
func TestAudienceIntakeValidation(t *testing.T) {
	ai := newTestIntake("http://unused", &fakeAudienceStarter{}, &fakePhoneResolver{}, nil)
	cases := []struct {
		name string
		mut  func(*AudienceIntakeRequest)
	}{
		{"no tenant", func(r *AudienceIntakeRequest) { r.TenantID = "" }},
		{"no segment", func(r *AudienceIntakeRequest) { r.SegmentID = "" }},
		{"no campaign", func(r *AudienceIntakeRequest) { r.CampaignID = "" }},
		{"no message", func(r *AudienceIntakeRequest) { r.Message = "" }},
		{"bad channel", func(r *AudienceIntakeRequest) { r.Channel = "pager" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := intakeReq("tenant-a-id", "acme", "camp-v")
			tc.mut(&req)
			if _, err := ai.Intake(context.Background(), req); err == nil {
				t.Fatal("want validation error")
			}
		})
	}
}

// Resolution requires the tenant slug (booking-service's X-Tenant-Slug
// middleware); a request carrying only X-Tenant-Id fails as a caller error
// (400-class), NOT as degradation.
func TestAudienceIntakeResolutionRequiresSlug(t *testing.T) {
	gs := graphStub(t, http.StatusOK, `{"members":[
		{"person_id":"p-1","phone_hash":"h1","lead_id":"lead-1"}
	]}`, nil)
	defer gs.Close()
	ai := newTestIntake(gs.URL, &fakeAudienceStarter{}, &fakePhoneResolver{phones: map[string]string{}}, nil)
	req := intakeReq("tenant-a-id", "", "camp-noslug")
	_, err := ai.Intake(context.Background(), req)
	if err == nil {
		t.Fatal("want error")
	}
	var downErr *AudienceGraphDownError
	if errors.As(err, &downErr) {
		t.Fatalf("missing slug must not degrade as dependency-down: %v", err)
	}
}

// 404 from graph-service means the segment does not exist FOR THIS TENANT
// (cross-tenant reads are indistinguishable from missing — gate 1) and is a
// caller error, not degradation.
func TestAudienceIntakeSegmentNotFound(t *testing.T) {
	gs := graphStub(t, http.StatusNotFound, `{"error":"segment not found"}`, nil)
	defer gs.Close()
	ai := newTestIntake(gs.URL, &fakeAudienceStarter{}, &fakePhoneResolver{}, nil)
	_, err := ai.Intake(context.Background(), intakeReq("tenant-a-id", "acme", "camp-404"))
	if err == nil {
		t.Fatal("want error")
	}
	var downErr *AudienceGraphDownError
	if errors.As(err, &downErr) {
		t.Fatalf("404 must not degrade as graph-down: %v", err)
	}
}

// Unused helper guard: strptr is available for future fixture shaping.
var _ = strptr
