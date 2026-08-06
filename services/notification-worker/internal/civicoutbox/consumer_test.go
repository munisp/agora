package civicoutbox

// SPEC-W32 WS-B: civic events consumer — topic gating, ReportReceived →
// deterministic SLA workflow start, StatusChanged → signal + citizen
// notify, Merged → canonical-ref handling.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/opendesk/notification-worker/internal/workflows"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.uber.org/zap"
)

// fakeTemporal records signals + workflow starts.
type fakeTemporal struct {
	mu      sync.Mutex
	signals []struct {
		workflowID string
		signal     string
		arg        interface{}
	}
	starts []struct {
		id           string
		workflowType string
		arg          interface{}
	}
	signalErr error
	startErr  error
}

func (f *fakeTemporal) SignalWorkflow(_ context.Context, workflowID, _ string, signalName string, arg interface{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.signalErr != nil {
		return f.signalErr
	}
	f.signals = append(f.signals, struct {
		workflowID string
		signal     string
		arg        interface{}
	}{workflowID, signalName, arg})
	return nil
}

func (f *fakeTemporal) ExecuteWorkflow(_ context.Context, opts client.StartWorkflowOptions, workflowType interface{}, args ...interface{}) (client.WorkflowRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return nil, f.startErr
	}
	name, _ := workflowType.(string)
	var arg interface{}
	if len(args) > 0 {
		arg = args[0]
	}
	f.starts = append(f.starts, struct {
		id           string
		workflowType string
		arg          interface{}
	}{opts.ID, name, arg})
	return nil, nil
}

func newTestConsumer(ft *fakeTemporal) *Consumer {
	// kafka.NewReader validates a non-empty broker list at construction;
	// Process never touches the reader, so a dead address is fine.
	return New([]string{"localhost:9092"}, DefaultTopic, "test-group", ft, "opendesk-main", zap.NewNop())
}

func civicEvent(t *testing.T, id, typ, subject, tenantID string, data map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"specversion": "1.0",
		"id":          id,
		"source":      "booking-service",
		"type":        typ,
		"subject":     subject,
		"time":        time.Now().UTC().Format(time.RFC3339),
		"tenantid":    tenantID,
		"data":        data,
	})
	require.NoError(t, err)
	return raw
}

// Topic gating (SPEC-W32: CIVIC_EVENTS_TOPIC default
// opendesk.civic.events.v1; empty/"off" disables the consumer).
func TestTopicEnabled(t *testing.T) {
	require.Equal(t, DefaultTopic, TopicEnabled(""), "unset → default topic")
	require.Equal(t, "opendesk.civic.events.v1", DefaultTopic)
	require.Equal(t, "", TopicEnabled("off"), "off disables the consumer")
	require.Equal(t, "", TopicEnabled("OFF"))
	require.Equal(t, "", TopicEnabled("disabled"))
	require.Equal(t, "custom.topic", TopicEnabled("custom.topic"))
}

// ReportReceived starts CivicSLAWorkflow with the deterministic ID and the
// parsed SLA due times.
func TestProcessReportReceivedStartsSLAWorkflow(t *testing.T) {
	ft := &fakeTemporal{}
	c := newTestConsumer(ft)
	raw := civicEvent(t, "t-1:civic:ref:1", EventTypeReportReceived, "ikeja-lga", "t-1", map[string]any{
		"ref":            "GOV-IKEJA-03-2026-000042",
		"mda_queue":      "roads-dept",
		"ack_due_at":     "2026-08-05T14:00:00Z",
		"resolve_due_at": "2026-08-07T12:00:00Z",
	})
	require.NoError(t, c.Process(context.Background(), raw))
	require.Len(t, ft.starts, 1)
	start := ft.starts[0]
	require.Equal(t, "civic-sla-ikeja-lga-GOV-IKEJA-03-2026-000042", start.id)
	require.Equal(t, workflows.WorkflowTypeCivicSLA, start.workflowType)
	in, ok := start.arg.(workflows.CivicSLAInput)
	require.True(t, ok)
	require.Equal(t, "GOV-IKEJA-03-2026-000042", in.Ref)
	require.Equal(t, "ikeja-lga", in.TenantSlug)
	require.Equal(t, "t-1", in.TenantID)
	require.Equal(t, "roads-dept", in.MDAQueue)
	require.Equal(t, time.Date(2026, 8, 5, 14, 0, 0, 0, time.UTC), in.AckDueAt)
	require.Equal(t, time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC), in.ResolveDueAt)
}

// Redelivered ReportReceived (AlreadyStarted) is acknowledged, not failed.
func TestProcessReportReceivedAlreadyStartedAcked(t *testing.T) {
	ft := &fakeTemporal{startErr: &serviceerror.WorkflowExecutionAlreadyStarted{}}
	c := newTestConsumer(ft)
	raw := civicEvent(t, "e-1", EventTypeReportReceived, "ikeja-lga", "t-1", map[string]any{
		"ref": "GOV-1", "ack_due_at": "2026-08-05T14:00:00Z",
	})
	require.NoError(t, c.Process(context.Background(), raw))
}

// ReportReceived without a ref is acknowledged (bad producer data, never
// hot-looped).
func TestProcessReportReceivedWithoutRefAcked(t *testing.T) {
	ft := &fakeTemporal{}
	c := newTestConsumer(ft)
	raw := civicEvent(t, "e-1", EventTypeReportReceived, "ikeja-lga", "t-1", map[string]any{})
	require.NoError(t, c.Process(context.Background(), raw))
	require.Empty(t, ft.starts)
}

// StatusChanged with reporter_phone: SLA workflow signalled AND the
// citizen notify workflow started with the deterministic per-event ID.
func TestProcessStatusChangedSignalsAndNotifies(t *testing.T) {
	ft := &fakeTemporal{}
	c := newTestConsumer(ft)
	raw := civicEvent(t, "t-1:civic:ref:7", EventTypeStatusChanged, "ikeja-lga", "t-1", map[string]any{
		"ref":            "GOV-42",
		"status":         "assigned",
		"reporter_phone": "+2348012345678",
	})
	require.NoError(t, c.Process(context.Background(), raw))

	require.Len(t, ft.signals, 1)
	sig := ft.signals[0]
	require.Equal(t, "civic-sla-ikeja-lga-GOV-42", sig.workflowID)
	require.Equal(t, workflows.SignalCivicStatus, sig.signal)
	statusSig, ok := sig.arg.(workflows.CivicStatusSignal)
	require.True(t, ok)
	require.Equal(t, "assigned", statusSig.Status)

	require.Len(t, ft.starts, 1)
	start := ft.starts[0]
	require.Equal(t, "civic-notify-t-1:civic:ref:7", start.id)
	require.Equal(t, workflows.WorkflowTypeCivicStatusNotify, start.workflowType)
	in, ok := start.arg.(workflows.CivicStatusNotifyInput)
	require.True(t, ok)
	require.Equal(t, "GOV-42", in.Ref)
	require.Equal(t, "assigned", in.Status)
	require.Equal(t, "+2348012345678", in.Phone)
	require.Equal(t, "sms", in.Channel)
	require.Equal(t, "ikeja-lga", in.TenantSlug)
}

// StatusChanged WITHOUT reporter_phone (no wants_updates): signal only,
// no citizen notification.
func TestProcessStatusChangedNoPhoneSkipsNotify(t *testing.T) {
	ft := &fakeTemporal{}
	c := newTestConsumer(ft)
	raw := civicEvent(t, "e-2", EventTypeStatusChanged, "ikeja-lga", "t-1", map[string]any{
		"ref": "GOV-42", "status": "triaged",
	})
	require.NoError(t, c.Process(context.Background(), raw))
	require.Len(t, ft.signals, 1)
	require.Empty(t, ft.starts)
}

// A signal for a completed/absent SLA workflow is tolerated (NotFound).
func TestProcessStatusChangedSignalNotFoundTolerated(t *testing.T) {
	ft := &fakeTemporal{signalErr: &serviceerror.NotFound{}}
	c := newTestConsumer(ft)
	raw := civicEvent(t, "e-3", EventTypeStatusChanged, "ikeja-lga", "t-1", map[string]any{
		"ref": "GOV-42", "status": "triaged", "reporter_phone": "+2348012345678",
	})
	require.NoError(t, c.Process(context.Background(), raw))
	require.Len(t, ft.starts, 1, "notification still starts when the SLA workflow is gone")
}

// Merged: the merged case's SLA workflow is signalled, and a later
// StatusChanged for the merged ref notifies against the CANONICAL ref
// (SPEC-W32 §4.3: notifications follow the canonical case).
func TestProcessMergedThenStatusChangedUsesCanonicalRef(t *testing.T) {
	ft := &fakeTemporal{}
	c := newTestConsumer(ft)
	merged := civicEvent(t, "e-4", EventTypeMerged, "ikeja-lga", "t-1", map[string]any{
		"ref":           "GOV-99",
		"canonical_ref": "GOV-7",
	})
	require.NoError(t, c.Process(context.Background(), merged))
	require.Len(t, ft.signals, 1)
	require.Equal(t, "civic-sla-ikeja-lga-GOV-99", ft.signals[0].workflowID)
	require.Equal(t, workflows.SignalCivicMerged, ft.signals[0].signal)
	mergedSig, ok := ft.signals[0].arg.(workflows.CivicMergedSignal)
	require.True(t, ok)
	require.Equal(t, "GOV-7", mergedSig.CanonicalRef)

	status := civicEvent(t, "e-5", EventTypeStatusChanged, "ikeja-lga", "t-1", map[string]any{
		"ref": "GOV-99", "status": "in_progress", "reporter_phone": "+2348012345678",
	})
	require.NoError(t, c.Process(context.Background(), status))
	require.Len(t, ft.starts, 1)
	in, ok := ft.starts[0].arg.(workflows.CivicStatusNotifyInput)
	require.True(t, ok)
	require.Equal(t, "GOV-99", in.Ref)
	require.Equal(t, "GOV-7", in.CanonicalRef)
	require.Equal(t, "GOV-7", in.EffectiveRef(), "notification must reference the canonical ref")
}

// SPEC-W32 W3 (d): recomputed due times in the event data ride the signal.
func TestProcessStatusChangedExtractsDueTimes(t *testing.T) {
	ft := &fakeTemporal{}
	c := newTestConsumer(ft)
	raw := civicEvent(t, "e-9", EventTypeStatusChanged, "ikeja-lga", "t-1", map[string]any{
		"ref":            "GOV-42",
		"status":         "triaged",
		"ack_due_at":     "2026-08-05T15:00:00Z",
		"resolve_due_at": "2026-08-06T12:30:00Z",
	})
	require.NoError(t, c.Process(context.Background(), raw))
	require.Len(t, ft.signals, 1)
	sig, ok := ft.signals[0].arg.(workflows.CivicStatusSignal)
	require.True(t, ok)
	require.NotNil(t, sig.AckDueAt)
	require.Equal(t, time.Date(2026, 8, 5, 15, 0, 0, 0, time.UTC), *sig.AckDueAt)
	require.NotNil(t, sig.ResolveDueAt)
	require.Equal(t, time.Date(2026, 8, 6, 12, 30, 0, 0, time.UTC), *sig.ResolveDueAt)
}

// SPEC-W32 W3 (d): absent, null, empty and malformed due times all decode
// to nil ("don't re-arm") and NEVER drop the signal.
func TestProcessStatusChangedToleratesBadDueTimes(t *testing.T) {
	ft := &fakeTemporal{}
	c := newTestConsumer(ft)
	cases := []map[string]any{
		{"ref": "GOV-1", "status": "triaged"},                                                             // absent
		{"ref": "GOV-1", "status": "triaged", "ack_due_at": nil, "resolve_due_at": nil},                   // null
		{"ref": "GOV-1", "status": "triaged", "ack_due_at": "", "resolve_due_at": "  "},                   // empty
		{"ref": "GOV-1", "status": "triaged", "ack_due_at": "not-a-time", "resolve_due_at": "2026-13-99"}, // malformed
	}
	for i, data := range cases {
		raw := civicEvent(t, fmt.Sprintf("bad-%d", i), EventTypeStatusChanged, "ikeja-lga", "t-1", data)
		require.NoError(t, c.Process(context.Background(), raw), "case %d", i)
	}
	require.Len(t, ft.signals, 4, "every signal must still be delivered")
	for i, s := range ft.signals {
		sig, ok := s.arg.(workflows.CivicStatusSignal)
		require.True(t, ok, "case %d", i)
		require.Nil(t, sig.AckDueAt, "case %d", i)
		require.Nil(t, sig.ResolveDueAt, "case %d", i)
		require.Equal(t, "triaged", sig.Status, "case %d", i)
	}
}

// Unknown types and malformed payloads are acknowledged
// (forward-compatible).
func TestProcessUnknownAndMalformedAcked(t *testing.T) {
	ft := &fakeTemporal{}
	c := newTestConsumer(ft)
	require.NoError(t, c.Process(context.Background(), []byte(`{"type":"com.opendesk.civic.SomethingNew","data":{}}`)))
	require.NoError(t, c.Process(context.Background(), []byte(`not json`)))
	require.Empty(t, ft.signals)
	require.Empty(t, ft.starts)
}

// A transient signal failure propagates (Run logs + acks, Process reports).
func TestProcessStatusChangedSignalFailurePropagates(t *testing.T) {
	ft := &fakeTemporal{signalErr: errors.New("temporal unreachable")}
	c := newTestConsumer(ft)
	raw := civicEvent(t, "e-6", EventTypeStatusChanged, "ikeja-lga", "t-1", map[string]any{
		"ref": "GOV-1", "status": "triaged",
	})
	require.Error(t, c.Process(context.Background(), raw))
}

// Merged via the merged_into alias field (booking-service store column
// name) also decodes.
func TestProcessMergedIntoAlias(t *testing.T) {
	ft := &fakeTemporal{}
	c := newTestConsumer(ft)
	raw := civicEvent(t, "e-7", EventTypeMerged, "ikeja-lga", "t-1", map[string]any{
		"ref": "GOV-99", "merged_into": "GOV-7",
	})
	require.NoError(t, c.Process(context.Background(), raw))
	require.Equal(t, "GOV-7", c.canonicalRef("ikeja-lga", "GOV-99"))
}

// Notify already-running is acknowledged (at-least-once delivery).
func TestProcessStatusChangedNotifyAlreadyStartedAcked(t *testing.T) {
	ft := &fakeTemporal{startErr: fmt.Errorf("workflow execution already started")}
	c := newTestConsumer(ft)
	raw := civicEvent(t, "e-8", EventTypeStatusChanged, "ikeja-lga", "t-1", map[string]any{
		"ref": "GOV-1", "status": "resolved", "reporter_phone": "+2348012345678",
	})
	require.NoError(t, c.Process(context.Background(), raw))
}
