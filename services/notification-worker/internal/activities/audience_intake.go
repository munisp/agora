package activities

// SPEC-W28 WS-C: tenant knowledge-graph audience intake.
//
// POST /v1/audiences (handler: internal/httpapi/audience_routes.go) accepts
// {segment_id, campaign_id, message, channel} for the caller's tenant, fetches
// the materialized CONSENT-PASSING audience from graph-service
// (POST {GRAPH_SERVICE_URL}/v1/graph/segments/{segment_id}/audience — the
// graph is consent-gated by construction, SPEC-W28 §0: no Person leaves the
// graph in an audience without a purpose-matching CONSENTED edge, and
// quarantined Persons are audience-ineligible per §5 gate 4) and enqueues one
// PacedSendWorkflow per recipient on this worker's task queue.
//
// AUDIENCE MEMBER SHAPE (orchestrator contract ruling, binding):
//
//	{"person_id": str, "phone_hash": str, "lead_id": str|null}
//
// The graph stores phones HASHED only (raw PII stays in Postgres, SPEC-W28
// §3), so the intake resolves phones itself: the members' lead_ids are
// bulk-resolved to E.164 phones via booking-service's internal endpoint
// POST /v1/leads/resolve (Dapr service invocation, X-Tenant-Slug tenant
// scoping — the response contains only leads that exist AND belong to the
// tenant). Members with lead_id=null or an unresolved lead take the
// SkippedNoPhone path (counted, logged). The legacy member shape (direct
// "phone" field, "audience"/"persons"/"recipients"/"items" envelopes) is
// still decoded for backward compatibility.
//
// THE SEND PATH IS UNCHANGED: every recipient rides
// workflows.PacedSendWorkflow → GuardedPacedSend (quiet-hours deferral for
// marketing kinds) → the NotifyPaced activity (SPEC-W12 DND 2442 suppression
// BEFORE the CPS token is acquired, then CPS pacing + sender rotation) →
// SendGeoCampaignMessage (kind geo_campaign — MARKETING class, so all three
// gates apply). Nothing here calls a channel binding directly, and no gate
// is reimplemented; belt-and-braces, members the graph still flagged
// quarantine=true are skipped intake-side too.
//
// IDEMPOTENCY (SPEC-W24 Idempotency-Key pattern): the intake is deduped by
// (tenant_id, campaign_id) via AudienceClaimStore.Claim. The HTTP layer
// passes the Idempotency-Key header through for audit; the dedupe key itself
// is campaign_id, exactly like the W24 consumers that derive the idempotency
// key from the entity id so a redelivery/retry has exactly-once effect.
// Per-recipient, the Temporal workflow ID is "audience-{campaign_id}-
// {person_id}", so a redelivered intake that slips past the claim still hits
// WorkflowExecutionAlreadyStarted and never double-sends (same idiom as
// internal/notifyoutbox). A failed intake RELEASES the claim so the caller's
// retry can proceed; a successful one keeps it (replays answer duplicate).
//
// TRAJECTORY LOGGING (ART seam, SPEC-W28 §1 ART row + §4 WS-C): one
// CloudEvent per ENQUEUED recipient is produced to Kafka topic
// opendesk.usage.events (env AUDIENCE_TRAJECTORY_TOPIC overrides; ""/"off"
// disables). Send×outcome schema:
//
//	{specversion: "1.0", id, source: "notification-worker",
//	 type: "com.opendesk.graph.OutreachTrajectory",
//	 subject: <tenant_slug>, time, tenantid: <tenant_id>,
//	 data: {tenant_id, campaign_id, segment_id, person_id, channel,
//	        send: "enqueued",        // send side of the pair
//	        outcome: "pending",      // joined later by the ART sink
//	        ts}}
//
// The OUTCOME rows of the pair (delivered / suppressed_dnd / failed) ride
// the existing send-path events; the lakehouse trajectories sink joins on
// (tenant_id, campaign_id, person_id). Producer failures are logged and
// counted but NEVER fail the intake — metering must not block outreach
// (same posture as booking-service usage metering).
//
// Kafka key: campaign_id (all rows of one campaign land on one partition,
// preserving per-campaign ordering for the sink).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/notification-worker/internal/daprc"
	"github.com/opendesk/notification-worker/internal/workflows"
	"github.com/segmentio/kafka-go"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.uber.org/zap"
)

// DefaultGraphServiceURL is where graph-service listens (SPEC-W28 §2,
// compose service graph-service :7014). GRAPH_SERVICE_URL overrides.
const DefaultGraphServiceURL = "http://localhost:7014"

// DefaultTrajectoryTopic is the shared metering/trajectory topic
// (opendesk.usage.events — the ART seam of SPEC-W28 §1).
const DefaultTrajectoryTopic = "opendesk.usage.events"

// TrajectoryEventType is the CloudEvent type of one send×outcome row.
const TrajectoryEventType = "com.opendesk.graph.OutreachTrajectory"

// leadsResolveBatch mirrors booking-service's POST /v1/leads/resolve cap
// (500 lead_ids per request); larger audiences are resolved in batches.
const leadsResolveBatch = 500

// AudienceMember is one consent-passing Person of the materialized audience
// returned by graph-service. Contract shape (binding): {person_id,
// phone_hash, lead_id}; the phone is resolved from lead_id via
// booking-service. The legacy direct-phone shape still decodes.
type AudienceMember struct {
	PersonID       string   `json:"person_id"`
	PhoneHash      string   `json:"phone_hash,omitempty"`
	LeadID         *string  `json:"lead_id"` // null when the Person has no lead link
	Name           string   `json:"name,omitempty"`
	Phone          string   `json:"phone,omitempty"` // legacy shape only
	Channel        string   `json:"channel,omitempty"`
	Channels       []string `json:"channels,omitempty"`
	ConsentSummary string   `json:"consent_summary,omitempty"`
	Quarantined    bool     `json:"quarantine,omitempty"`
}

// AudienceIntakeRequest is the validated input of one intake. TenantID and
// TenantSlug come from the JWT seam (X-Tenant-Id / X-Tenant-Slug headers,
// injected by the APISIX jwt plugin) — never from the body.
type AudienceIntakeRequest struct {
	TenantID       string
	TenantSlug     string
	SegmentID      string
	CampaignID     string
	Message        string // "{name}" is substituted per recipient, geo-campaign style
	Channel        string // sms (default) | whatsapp | telegram
	IdempotencyKey string // audit copy of the Idempotency-Key header (dedupe is by campaign_id)
}

// AudienceIntakeResult is the intake outcome (also the HTTP 200/202 body).
type AudienceIntakeResult struct {
	CampaignID          string `json:"campaign_id"`
	SegmentID           string `json:"segment_id"`
	Duplicate           bool   `json:"duplicate"`
	AudienceSize        int    `json:"audience_size"`
	Enqueued            int    `json:"enqueued"`
	AlreadyRunning      int    `json:"already_running"`
	SkippedNoPhone      int    `json:"skipped_no_phone"`
	SkippedQuarantined  int    `json:"skipped_quarantined"`
	TrajectoriesEmitted int    `json:"trajectories_emitted"`
}

// AudienceWorkflowStarter abstracts Temporal workflow starts
// (client.Client satisfies it via ExecuteWorkflow) — same idiom as
// internal/notifyoutbox.WorkflowStarter.
type AudienceWorkflowStarter interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflowType interface{}, args ...interface{}) (client.WorkflowRun, error)
}

// AudienceClaimStore is the idempotency slice of the intake: Claim returns
// false when (tenantID, campaignID) was already claimed by a completed or
// in-flight intake. Release drops the claim after a FAILED intake so the
// caller's retry can proceed. InMemoryAudienceClaims is the default; a
// Postgres-backed implementation can be dropped in without touching this
// file (the claim/release semantics are the contract).
type AudienceClaimStore interface {
	Claim(ctx context.Context, tenantID, campaignID, idemKey string) (claimed bool, err error)
	Release(ctx context.Context, tenantID, campaignID string)
}

// InMemoryAudienceClaims is a process-local AudienceClaimStore. Like every
// in-memory dedupe it resets on restart — the per-recipient Temporal
// workflow IDs remain the restart-safe second line of defence.
type InMemoryAudienceClaims struct {
	mu     sync.Mutex
	claims map[string]string // "tenant/campaign" → idempotency key
}

// NewInMemoryAudienceClaims builds an empty claim store.
func NewInMemoryAudienceClaims() *InMemoryAudienceClaims {
	return &InMemoryAudienceClaims{claims: map[string]string{}}
}

func audienceClaimKey(tenantID, campaignID string) string { return tenantID + "/" + campaignID }

// Claim records (tenantID, campaignID); false when already claimed.
func (s *InMemoryAudienceClaims) Claim(_ context.Context, tenantID, campaignID, idemKey string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := audienceClaimKey(tenantID, campaignID)
	if _, ok := s.claims[k]; ok {
		return false, nil
	}
	s.claims[k] = idemKey
	return true, nil
}

// Release drops the claim (failed intake → caller may retry).
func (s *InMemoryAudienceClaims) Release(_ context.Context, tenantID, campaignID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.claims, audienceClaimKey(tenantID, campaignID))
}

// LeadPhoneResolver bulk-resolves lead_ids to E.164 phones for one tenant
// (DaprLeadPhoneResolver is the production implementation over booking-
// service's POST /v1/leads/resolve; tests use a fake).
type LeadPhoneResolver interface {
	ResolveLeadPhones(ctx context.Context, tenantSlug string, leadIDs []string) (map[string]string, error)
}

// DaprLeadPhoneResolver resolves lead phones via booking-service's internal
// endpoint (Dapr service invocation, X-Tenant-Slug tenant scoping — the
// ResolveTenant idiom of cmd/worker/main.go). Batches are chunked to the
// endpoint's 500-id cap.
type DaprLeadPhoneResolver struct {
	Dapr         *daprc.Client
	BookingAppID string
}

// ResolveLeadPhones implements LeadPhoneResolver.
func (r *DaprLeadPhoneResolver) ResolveLeadPhones(ctx context.Context, tenantSlug string, leadIDs []string) (map[string]string, error) {
	phones := make(map[string]string, len(leadIDs))
	for start := 0; start < len(leadIDs); start += leadsResolveBatch {
		end := start + leadsResolveBatch
		if end > len(leadIDs) {
			end = len(leadIDs)
		}
		var out struct {
			Phones map[string]string `json:"phones"`
		}
		err := r.Dapr.InvokeServiceWithHeaders(ctx, r.BookingAppID, "v1/leads/resolve",
			map[string]any{"lead_ids": leadIDs[start:end]},
			map[string]string{"X-Tenant-Slug": tenantSlug}, &out)
		if err != nil {
			return nil, fmt.Errorf("booking-service v1/leads/resolve: %w", err)
		}
		for id, phone := range out.Phones {
			phones[id] = phone
		}
	}
	return phones, nil
}

// TrajectoryProducer publishes one trajectory row to a Kafka topic
// (kafka.Writer satisfies it via an adapter; tests use a fake).
type TrajectoryProducer interface {
	Produce(ctx context.Context, topic string, key, payload []byte) error
}

// KafkaTrajectoryProducer adapts a kafka-go writer to TrajectoryProducer.
type KafkaTrajectoryProducer struct{ w *kafka.Writer }

// NewKafkaTrajectoryProducer builds the producer over the given broker list
// (KAFKA_BROKERS, same env as the worker's consumers). The topic is set
// per-message so one writer serves the configured topic.
func NewKafkaTrajectoryProducer(brokers []string) *KafkaTrajectoryProducer {
	return &KafkaTrajectoryProducer{w: &kafka.Writer{
		Addr:         kafka.TCP(brokers...),
		RequiredAcks: kafka.RequireOne,
		BatchTimeout: 50 * time.Millisecond,
	}}
}

// Produce writes one keyed message; the key (campaign_id) pins the campaign
// to one partition.
func (p *KafkaTrajectoryProducer) Produce(ctx context.Context, topic string, key, payload []byte) error {
	return p.w.WriteMessages(ctx, kafka.Message{Topic: topic, Key: key, Value: payload, Time: time.Now().UTC()})
}

// Close releases the writer.
func (p *KafkaTrajectoryProducer) Close() error { return p.w.Close() }

// AudienceGraphDownError marks an upstream dependency being unavailable
// (graph-service audience fetch OR booking-service phone resolution); the
// HTTP layer maps it to 502 (degradation: nothing enqueued, claim released,
// retry safe).
type AudienceGraphDownError struct{ Err error }

func (e *AudienceGraphDownError) Error() string {
	return "audience dependency unavailable: " + e.Err.Error()
}
func (e *AudienceGraphDownError) Unwrap() error { return e.Err }

// AudienceIntake holds the dependencies of the audience intake. Built by
// NewAudienceIntake (env-derived) or literally in tests.
type AudienceIntake struct {
	// GraphServiceURL is the graph-service base (GRAPH_SERVICE_URL, default
	// http://localhost:7014).
	GraphServiceURL string
	// TrajectoryTopic receives the send×outcome rows
	// (AUDIENCE_TRAJECTORY_TOPIC, default opendesk.usage.events; ""/"off"
	// disables trajectory emission).
	TrajectoryTopic string
	Starter         AudienceWorkflowStarter
	TaskQueue       string
	Claims          AudienceClaimStore
	// Phones resolves lead_ids → E.164 phones via booking-service (nil →
	// members that need resolution take the SkippedNoPhone path with a
	// warn; legacy direct-phone members still send).
	Phones LeadPhoneResolver
	// Trajectories is nil-safe: nil disables emission (metering posture).
	Trajectories TrajectoryProducer
	// HTTPClient is injectable for tests; nil → 15s default.
	HTTPClient *http.Client
	Log        *zap.Logger
}

// NewAudienceIntake builds the intake from the environment: GRAPH_SERVICE_URL
// (default http://localhost:7014), AUDIENCE_TRAJECTORY_TOPIC (default
// opendesk.usage.events, "off" disables) and the KAFKA_BROKERS list the
// caller passes through from config (same env the consumers use). brokers
// empty → trajectory producer disabled with a warn (sends still flow).
// dapr + bookingAppID wire the booking-service lead-phone resolution
// (cfg.BookingAppID in cmd/worker/main.go); a nil dapr client disables
// resolution (lead-linked members are skipped with a warn).
func NewAudienceIntake(dapr *daprc.Client, bookingAppID string, starter AudienceWorkflowStarter, taskQueue string, brokers []string, log *zap.Logger) *AudienceIntake {
	graphURL := strings.TrimRight(os.Getenv("GRAPH_SERVICE_URL"), "/")
	if graphURL == "" {
		graphURL = DefaultGraphServiceURL
	}
	topic := os.Getenv("AUDIENCE_TRAJECTORY_TOPIC")
	if topic == "" {
		topic = DefaultTrajectoryTopic
	}
	if strings.EqualFold(topic, "off") {
		topic = ""
	}
	var producer TrajectoryProducer
	if topic != "" && len(brokers) > 0 {
		producer = NewKafkaTrajectoryProducer(brokers)
	} else if topic != "" {
		log.Warn("AUDIENCE_TRAJECTORY_TOPIC set but no Kafka brokers; trajectory emission disabled")
	}
	var phones LeadPhoneResolver
	if dapr != nil {
		phones = &DaprLeadPhoneResolver{Dapr: dapr, BookingAppID: bookingAppID}
	} else {
		log.Warn("no Dapr client: lead phone resolution disabled (lead-linked audience members will be skipped)")
	}
	return &AudienceIntake{
		GraphServiceURL: graphURL,
		TrajectoryTopic: topic,
		Starter:         starter,
		TaskQueue:       taskQueue,
		Claims:          NewInMemoryAudienceClaims(),
		Phones:          phones,
		Trajectories:    producer,
		Log:             log,
	}
}

// Intake runs one audience intake end to end. Errors are returned for
// contract violations (400-class, plain errors) and upstream degradation
// (502-class, *AudienceGraphDownError); per-recipient Temporal
// AlreadyStarted is tolerated (counted), and trajectory failures are
// logged-only.
func (ai *AudienceIntake) Intake(ctx context.Context, req AudienceIntakeRequest) (AudienceIntakeResult, error) {
	res := AudienceIntakeResult{CampaignID: req.CampaignID, SegmentID: req.SegmentID}
	if req.TenantID == "" {
		return res, errors.New("tenant is required (X-Tenant-Id header)")
	}
	if strings.TrimSpace(req.SegmentID) == "" || strings.TrimSpace(req.CampaignID) == "" {
		return res, errors.New("segment_id and campaign_id are required")
	}
	if strings.TrimSpace(req.Message) == "" {
		return res, errors.New("message is required")
	}
	channel := strings.ToLower(strings.TrimSpace(req.Channel))
	if channel == "" {
		channel = ChannelSMS
	}
	switch channel {
	case ChannelSMS, "whatsapp", "telegram":
	default:
		return res, fmt.Errorf("unknown channel %q (want sms, whatsapp or telegram)", req.Channel)
	}
	if ai.Starter == nil {
		return res, errors.New("audience intake not configured (no Temporal starter)")
	}
	if ai.Claims == nil {
		ai.Claims = NewInMemoryAudienceClaims()
	}

	// Idempotency (SPEC-W24): dedupe by (tenant, campaign_id). A replayed
	// request answers duplicate=true with zero side effects.
	claimed, err := ai.Claims.Claim(ctx, req.TenantID, req.CampaignID, req.IdempotencyKey)
	if err != nil {
		return res, fmt.Errorf("claim campaign: %w", err)
	}
	if !claimed {
		res.Duplicate = true
		ai.log().Info("audience intake duplicate; replaying result without side effects",
			zap.String("tenant_id", req.TenantID), zap.String("campaign_id", req.CampaignID))
		return res, nil
	}

	members, err := ai.fetchAudience(ctx, req)
	if err != nil {
		ai.Claims.Release(ctx, req.TenantID, req.CampaignID)
		return res, err
	}
	res.AudienceSize = len(members)

	// Phone resolution (binding contract): the graph hands over
	// {person_id, phone_hash, lead_id}; lead_ids of the deliverable
	// (non-quarantined, phone-less) members are bulk-resolved to E.164 via
	// booking-service. A resolution failure degrades the WHOLE intake
	// (claim released, retry safe) — never send with silently partial data.
	phones, err := ai.resolvePhones(ctx, req, members)
	if err != nil {
		ai.Claims.Release(ctx, req.TenantID, req.CampaignID)
		return res, err
	}

	for _, m := range members {
		if m.Quarantined {
			// SPEC-W28 §5 gate 4 (belt-and-braces — graph-service already
			// excludes quarantined Persons from audiences).
			res.SkippedQuarantined++
			continue
		}
		phone := strings.TrimSpace(m.Phone) // legacy direct-phone shape
		if phone == "" && m.LeadID != nil {
			phone = strings.TrimSpace(phones[*m.LeadID])
		}
		if phone == "" {
			// lead_id=null or unresolved/unknown/cross-tenant lead.
			res.SkippedNoPhone++
			ai.log().Info("audience member skipped: no resolvable phone",
				zap.String("campaign_id", req.CampaignID), zap.String("person_id", m.PersonID))
			continue
		}
		m.Phone = phone
		started, err := ai.enqueueRecipient(ctx, req, m, channel)
		if err != nil {
			// Roll back nothing: started recipients keep their (idempotent)
			// workflows; the claim is released so the caller's retry
			// re-enqueues only the ones that never started (AlreadyStarted
			// is tolerated per recipient).
			ai.Claims.Release(ctx, req.TenantID, req.CampaignID)
			return res, fmt.Errorf("enqueue recipient %s: %w", m.PersonID, err)
		}
		if started {
			res.Enqueued++
			ai.emitTrajectory(ctx, req, m, channel, &res)
		} else {
			res.AlreadyRunning++
		}
	}

	ai.log().Info("audience intake complete",
		zap.String("tenant_id", req.TenantID), zap.String("segment_id", req.SegmentID),
		zap.String("campaign_id", req.CampaignID), zap.Int("audience", res.AudienceSize),
		zap.Int("enqueued", res.Enqueued), zap.Int("already_running", res.AlreadyRunning),
		zap.Int("skipped_no_phone", res.SkippedNoPhone),
		zap.Int("skipped_quarantined", res.SkippedQuarantined))
	return res, nil
}

// resolvePhones bulk-resolves the lead_ids of deliverable-but-phoneless
// members. Members with a legacy direct phone, no lead link, or the
// quarantine flag never reach the resolver. Requires the tenant slug
// (booking-service's tenant middleware resolves via X-Tenant-Slug).
func (ai *AudienceIntake) resolvePhones(ctx context.Context, req AudienceIntakeRequest, members []AudienceMember) (map[string]string, error) {
	need := make([]string, 0, len(members))
	seen := map[string]bool{}
	for _, m := range members {
		if m.Quarantined || strings.TrimSpace(m.Phone) != "" || m.LeadID == nil {
			continue
		}
		id := strings.TrimSpace(*m.LeadID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		need = append(need, id)
	}
	if len(need) == 0 {
		return nil, nil
	}
	if ai.Phones == nil {
		ai.log().Warn("lead phone resolution not configured; lead-linked members will be skipped",
			zap.String("campaign_id", req.CampaignID), zap.Int("lead_ids", len(need)))
		return nil, nil
	}
	if req.TenantSlug == "" {
		return nil, errors.New("X-Tenant-Slug header is required for lead phone resolution")
	}
	phones, err := ai.Phones.ResolveLeadPhones(ctx, req.TenantSlug, need)
	if err != nil {
		return nil, &AudienceGraphDownError{Err: err}
	}
	ai.log().Info("audience lead phones resolved",
		zap.String("campaign_id", req.CampaignID),
		zap.Int("requested", len(need)), zap.Int("resolved", len(phones)))
	return phones, nil
}

// fetchAudience materializes the consent-passing audience via graph-service.
// The tenant scope rides the X-Tenant-Id header (graph-service injects the
// tenant filter on ALL paths, SPEC-W28 §5 gate 1) — the tenant id is never
// taken from the graph response. Transport errors and 5xx answers degrade
// as *AudienceGraphDownError (502 intake-side); 404 means the segment does
// not exist for THIS tenant (400-class back to the caller, cross-tenant
// reads indistinguishable from missing — gate 1).
func (ai *AudienceIntake) fetchAudience(ctx context.Context, req AudienceIntakeRequest) ([]AudienceMember, error) {
	base := strings.TrimRight(ai.GraphServiceURL, "/")
	if base == "" {
		base = DefaultGraphServiceURL
	}
	url := fmt.Sprintf("%s/v1/graph/segments/%s/audience", base, req.SegmentID)
	body, err := json.Marshal(map[string]string{"campaign_id": req.CampaignID})
	if err != nil {
		return nil, fmt.Errorf("marshal audience request: %w", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("build audience request: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("X-Tenant-Id", req.TenantID)
	if req.TenantSlug != "" {
		hreq.Header.Set("X-Tenant-Slug", req.TenantSlug)
	}
	hc := ai.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(hreq)
	if err != nil {
		return nil, &AudienceGraphDownError{Err: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, &AudienceGraphDownError{Err: err}
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("segment %s not found for this tenant", req.SegmentID)
	case resp.StatusCode >= 500:
		return nil, &AudienceGraphDownError{Err: fmt.Errorf("status %d", resp.StatusCode)}
	case resp.StatusCode >= 300:
		return nil, fmt.Errorf("graph-service audience: status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return decodeAudience(raw)
}

// decodeAudience tolerates the envelope shapes graph-service may answer
// with: {"members":[...]} (binding contract shape), then the legacy
// {"audience":[...]}, {"persons":[...]}, {"recipients":[...]},
// {"items":[...]} or a bare top-level array.
func decodeAudience(raw []byte) ([]AudienceMember, error) {
	var arr []AudienceMember
	if err := json.Unmarshal(raw, &arr); err == nil && arr != nil {
		return arr, nil
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode audience: %w", err)
	}
	for _, key := range []string{"members", "audience", "persons", "recipients", "items"} {
		if v, ok := env[key]; ok && string(v) != "null" {
			if err := json.Unmarshal(v, &arr); err != nil {
				return nil, fmt.Errorf("decode audience.%s: %w", key, err)
			}
			return arr, nil
		}
	}
	return nil, nil
}

// enqueueRecipient starts one PacedSendWorkflow for the member (kind
// geo_campaign — the existing marketing campaign send: DND-suppressed,
// quiet-hours deferred, CPS-paced, sender-rotated, UNCHANGED). The workflow
// ID is deterministic per (campaign, person), so redeliveries hit
// WorkflowExecutionAlreadyStarted and are counted, never re-sent.
func (ai *AudienceIntake) enqueueRecipient(ctx context.Context, req AudienceIntakeRequest, m AudienceMember, channel string) (started bool, err error) {
	name := strings.TrimSpace(m.Name)
	if name == "" {
		name = "there"
	}
	send := workflows.PacedSendRequest{
		Kind: workflows.PacedSendGeoCampaign,
		GeoCampaign: &workflows.PacedGeoCampaignSend{
			TenantSlug: req.TenantSlug,
			CampaignID: req.CampaignID,
			Channel:    channel,
			Phone:      strings.TrimSpace(m.Phone),
			Name:       m.Name,
			Text:       strings.ReplaceAll(req.Message, "{name}", name),
		},
	}
	workflowID := fmt.Sprintf("audience-%s-%s", req.CampaignID, m.PersonID)
	_, err = ai.Starter.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: ai.TaskQueue,
	}, workflows.WorkflowTypePacedSend, send)
	if err != nil {
		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &alreadyStarted) || strings.Contains(err.Error(), "already started") {
			ai.log().Info("recipient send already running; counted",
				zap.String("workflow_id", workflowID))
			return false, nil
		}
		return false, fmt.Errorf("start %s: %w", workflows.WorkflowTypePacedSend, err)
	}
	return true, nil
}

// emitTrajectory publishes the send side of one send×outcome row (schema in
// the file header). Failures are logged and counted in the result but never
// fail the intake — the ART seam must not block outreach.
func (ai *AudienceIntake) emitTrajectory(ctx context.Context, req AudienceIntakeRequest, m AudienceMember, channel string, res *AudienceIntakeResult) {
	if ai.Trajectories == nil || ai.TrajectoryTopic == "" {
		return
	}
	now := time.Now().UTC()
	evt := map[string]any{
		"specversion": "1.0",
		"id":          uuid.NewString(),
		"source":      "notification-worker",
		"type":        TrajectoryEventType,
		"subject":     req.TenantSlug,
		"time":        now.Format(time.RFC3339),
		"tenantid":    req.TenantID,
		"data": map[string]any{
			"tenant_id":   req.TenantID,
			"campaign_id": req.CampaignID,
			"segment_id":  req.SegmentID,
			"person_id":   m.PersonID,
			"channel":     channel,
			"send":        "enqueued",
			"outcome":     "pending",
			"ts":          now.Format(time.RFC3339),
		},
	}
	payload, err := json.Marshal(evt)
	if err != nil {
		ai.log().Warn("trajectory marshal failed; skipping", zap.Error(err))
		return
	}
	if err := ai.Trajectories.Produce(ctx, ai.TrajectoryTopic, []byte(req.CampaignID), payload); err != nil {
		ai.log().Warn("trajectory produce failed; metering lost for one row",
			zap.String("campaign_id", req.CampaignID), zap.String("person_id", m.PersonID), zap.Error(err))
		return
	}
	res.TrajectoriesEmitted++
}

func (ai *AudienceIntake) log() *zap.Logger {
	if ai.Log != nil {
		return ai.Log
	}
	return zap.NewNop()
}
