package activities

// SPEC-W32 WS-B: citizen status send (paced machinery, DND bypass, delivery
// ledger) + SLA-breach internal callback payload + escalation event.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/opendesk/notification-worker/internal/daprc"
	"github.com/opendesk/notification-worker/internal/pacer"
	"github.com/opendesk/notification-worker/internal/workflows"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// fakeCivicDapr emulates a daprd sidecar; records path, body AND headers
// (the incident_test fakeDapr does not capture headers, and the
// X-Tenant-Slug header is part of the WS-B contract).
type fakeCivicDapr struct {
	srv *httptest.Server
	mu  sync.Mutex
	// calls records each request: path → {body, headers}.
	paths   []string
	bodies  map[string][]byte
	headers map[string]http.Header
	status  int
}

func newFakeCivicDapr(t *testing.T, status int) *fakeCivicDapr {
	t.Helper()
	f := &fakeCivicDapr{bodies: map[string][]byte{}, headers: map[string]http.Header{}, status: status}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.bodies[r.URL.Path] = body
		f.headers[r.URL.Path] = r.Header.Clone()
		f.mu.Unlock()
		w.WriteHeader(f.status)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCivicDapr) client(t *testing.T) *daprc.Client {
	t.Helper()
	u, err := url.Parse(f.srv.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	return daprc.New(u.Hostname(), port)
}

func (f *fakeCivicDapr) called(substr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.paths {
		if strings.Contains(p, substr) {
			return true
		}
	}
	return false
}

// fakeCivicLedger records ledger rows.
type fakeCivicLedger struct {
	mu   sync.Mutex
	rows []CivicNotification
}

func (l *fakeCivicLedger) RecordCivicNotification(_ context.Context, tenantID, tenantSlug, ref, status, channel, phone, outcome string, attempt int, errText string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows = append(l.rows, CivicNotification{
		TenantID: tenantID, TenantSlug: tenantSlug, Ref: ref, Status: status,
		Channel: channel, Phone: phone, Outcome: outcome, Attempt: attempt, Error: errText,
	})
	return nil
}

// fakeEscalations records produced escalation rows.
type fakeEscalations struct {
	mu   sync.Mutex
	rows []struct {
		topic, key string
		payload    []byte
	}
}

func (p *fakeEscalations) Produce(_ context.Context, topic string, key, payload []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rows = append(p.rows, struct {
		topic, key string
		payload    []byte
	}{topic, string(key), payload})
	return nil
}

// suppressAllDND suppresses every marketing lookup (to prove civic_status
// never even consults it).
type suppressAllDND struct{ lookups int }

func (d *suppressAllDND) IsSuppressed(_ context.Context, _, _ string) (bool, string, error) {
	d.lookups++
	return true, pacer.ReasonGlobalDND, nil
}

func civicTestActivities(t *testing.T, dapr *daprc.Client) *Activities {
	t.Helper()
	a := New(dapr, "booking", "payments", "identity",
		"bindings-smtp", "bindings-twilio", "no-reply@test", "+10000000000", "", IndustryDeps{}, zap.NewNop())
	return a
}

func civicSend() workflows.PacedCivicStatusSend {
	return workflows.PacedCivicStatusSend{
		TenantID: "t-1", TenantSlug: "ikeja-lga", Ref: "GOV-IKEJA-03-2026-000042",
		Status: "assigned", Channel: ChannelSMS, Phone: "+2348012345678",
		Text: "Case GOV-IKEJA-03-2026-000042: now assigned",
	}
}

// Happy path: sms binding call + a sent ledger row with the attempt number.
func TestSendCivicStatusUpdateSendsAndLedgers(t *testing.T) {
	dapr := newFakeCivicDapr(t, http.StatusOK)
	ledger := &fakeCivicLedger{}
	a := civicTestActivities(t, dapr.client(t))
	a.Civic = CivicDeps{Ledger: ledger}

	require.NoError(t, a.SendCivicStatusUpdate(context.Background(), civicSend()))
	require.True(t, dapr.called("/v1.0/bindings/bindings-twilio"))
	require.Len(t, ledger.rows, 1)
	row := ledger.rows[0]
	require.Equal(t, CivicOutcomeSent, row.Outcome)
	require.Equal(t, "GOV-IKEJA-03-2026-000042", row.Ref)
	require.Equal(t, "assigned", row.Status)
	require.Equal(t, "+2348012345678", row.Phone)
	require.Equal(t, "sms", row.Channel)
	require.Equal(t, 1, row.Attempt)
	require.Empty(t, row.Error)
}

// A failed binding call returns the error AND lands a failed ledger row —
// every attempt lands in the delivery ledger (SPEC-W32 §3 WS-B).
func TestSendCivicStatusUpdateFailureLedgered(t *testing.T) {
	dapr := newFakeCivicDapr(t, http.StatusInternalServerError)
	ledger := &fakeCivicLedger{}
	a := civicTestActivities(t, dapr.client(t))
	a.Civic = CivicDeps{Ledger: ledger}

	err := a.SendCivicStatusUpdate(context.Background(), civicSend())
	require.Error(t, err)
	require.Len(t, ledger.rows, 1)
	require.Equal(t, CivicOutcomeFailed, ledger.rows[0].Outcome)
	require.NotEmpty(t, ledger.rows[0].Error)
}

// DND bypass: civic_status is TRANSACTIONAL-class — even with a
// suppress-everything DND registry, NotifyPaced dispatches the send and
// never consults the registry (SPEC-W32 §0.4).
func TestNotifyPacedCivicStatusBypassesDND(t *testing.T) {
	dapr := newFakeCivicDapr(t, http.StatusOK)
	dnd := &suppressAllDND{}
	a := civicTestActivities(t, dapr.client(t))
	a.Pacer = pacer.New(pacer.Config{CPS: 100, Burst: 10, Backend: "local"}, zap.NewNop())
	a.Guards = pacer.NewGuards(pacer.GuardConfig{DNDEnforcement: true, DND: dnd}, zap.NewNop())
	a.Civic = CivicDeps{Ledger: &fakeCivicLedger{}}

	res, err := a.NotifyPaced(context.Background(), workflows.PacedSendRequest{
		Kind:  workflows.PacedSendCivicStatus,
		Civic: ptr(civicSend()),
	})
	require.NoError(t, err)
	require.Equal(t, workflows.PacedSendStatusSent, res.Status,
		"transactional civic updates must never be suppressed_dnd")
	require.Zero(t, dnd.lookups, "transactional kinds must not consult the DND registry")
	require.True(t, dapr.called("/v1.0/bindings/bindings-twilio"))
	// Sanity: the kind is explicitly transactional-classified.
	require.Equal(t, pacer.ClassTransactional, pacer.ClassifyKind(workflows.PacedSendCivicStatus))
}

func ptr[T any](v T) *T { return &v }

// Breach callback: exact Dapr invocation path, body {kind, mda_queue,
// notify_mda: true} and the X-Tenant-Slug tenant-scoping header.
func TestReportCivicSLABreachCallbackPayload(t *testing.T) {
	dapr := newFakeCivicDapr(t, http.StatusOK)
	esc := &fakeEscalations{}
	ledger := &fakeCivicLedger{}
	a := civicTestActivities(t, dapr.client(t))
	a.Civic = CivicDeps{Ledger: ledger, Escalations: esc, EscalationTopic: DefaultCivicEscalationTopic}

	rep := workflows.CivicSLABreachReport{
		TenantID: "t-1", TenantSlug: "ikeja-lga", Ref: "GOV-IKEJA-03-2026-000042",
		Kind: workflows.CivicBreachKindAck, MDAQueue: "roads-dept",
	}
	require.NoError(t, a.ReportCivicSLABreach(context.Background(), rep))

	const path = "/v1.0/invoke/booking/method/v1/civic/internal/cases/GOV-IKEJA-03-2026-000042/sla-breach"
	require.True(t, dapr.called(path), "callback must hit the booking-service internal route (paths: %v)", dapr.paths)
	var body map[string]any
	require.NoError(t, json.Unmarshal(dapr.bodies[path], &body))
	require.Equal(t, "ack", body["kind"])
	require.Equal(t, true, body["notify_mda"])
	require.Equal(t, "roads-dept", body["mda_queue"])
	require.Equal(t, "ikeja-lga", dapr.headers[path].Get("X-Tenant-Slug"))
}

// The escalation event rides the civic events topic keyed by ref, and the
// escalation lands in the delivery ledger (SPEC-W32 §3 WS-B: "emit
// escalation event", escalation delivery recorded).
func TestReportCivicSLABreachEscalationAndLedger(t *testing.T) {
	dapr := newFakeCivicDapr(t, http.StatusOK)
	esc := &fakeEscalations{}
	ledger := &fakeCivicLedger{}
	a := civicTestActivities(t, dapr.client(t))
	a.Civic = CivicDeps{Ledger: ledger, Escalations: esc, EscalationTopic: DefaultCivicEscalationTopic}

	rep := workflows.CivicSLABreachReport{
		TenantID: "t-1", TenantSlug: "ikeja-lga", Ref: "GOV-IKEJA-03-2026-000042",
		Kind: workflows.CivicBreachKindResolve, MDAQueue: "roads-dept",
	}
	require.NoError(t, a.ReportCivicSLABreach(context.Background(), rep))

	require.Len(t, esc.rows, 1)
	require.Equal(t, DefaultCivicEscalationTopic, esc.rows[0].topic)
	require.Equal(t, "GOV-IKEJA-03-2026-000042", esc.rows[0].key)
	var evt map[string]any
	require.NoError(t, json.Unmarshal(esc.rows[0].payload, &evt))
	require.Equal(t, EventTypeCivicSLABreachEscalated, evt["type"])
	require.Equal(t, "t-1", evt["tenantid"])
	data, _ := evt["data"].(map[string]any)
	require.Equal(t, "resolve", data["kind"])

	require.Len(t, ledger.rows, 1)
	require.Equal(t, CivicOutcomeEscalated, ledger.rows[0].Outcome)
	require.Equal(t, "sla_breach_resolve", ledger.rows[0].Status)
	require.Equal(t, "webhook", ledger.rows[0].Channel)
}

// Callback failure → activity error (Temporal retries), no escalation
// event (the breach never landed).
func TestReportCivicSLABreachCallbackFailure(t *testing.T) {
	dapr := newFakeCivicDapr(t, http.StatusInternalServerError)
	esc := &fakeEscalations{}
	a := civicTestActivities(t, dapr.client(t))
	a.Civic = CivicDeps{Escalations: esc, EscalationTopic: DefaultCivicEscalationTopic}

	rep := workflows.CivicSLABreachReport{
		TenantSlug: "ikeja-lga", Ref: "GOV-1", Kind: workflows.CivicBreachKindAck,
	}
	require.Error(t, a.ReportCivicSLABreach(context.Background(), rep))
	require.Empty(t, esc.rows)
}

// Contract guard: unknown breach kinds are rejected.
func TestReportCivicSLABreachBadKind(t *testing.T) {
	a := civicTestActivities(t, daprc.New("127.0.0.1", 1))
	rep := workflows.CivicSLABreachReport{TenantSlug: "ikeja-lga", Ref: "GOV-1", Kind: "bogus"}
	require.Error(t, a.ReportCivicSLABreach(context.Background(), rep))
}
