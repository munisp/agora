package campaignstudio

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

// Workflow tests use the Temporal testsuite with stub activities
// registered under their PRODUCTION names (mirroring the geo workflow
// tests): "NotifyPaced" stands in for the notification-worker's paced
// wrapper, "StudioRecordSendOutcome" captures the outcome records,
// "StudioAssignVariant" (SPEC-W45 U6) stubs the registry arm resolution.

type studioWfEnv struct {
	env      *testsuite.TestWorkflowEnvironment
	recorded []RecordSendRequest
	// NotifyPaced behavior knobs (keyed by paced kind).
	suppressKinds map[string]string // kind → suppression reason
	failKinds     map[string]error  // kind → activity error
	seenRequests  []PacedSendRequest
	// StudioAssignVariant stub knobs (SPEC-W45 U6).
	assignVariant string                 // arm the stub returns (default champion)
	assignErr     error                  // stub activity error (Temporal-level)
	seenAssign    []AssignVariantRequest // assignment calls observed
}

func newStudioWfEnv(t *testing.T) *studioWfEnv {
	t.Helper()
	s := &studioWfEnv{
		suppressKinds: map[string]string{},
		failKinds:     map[string]error{},
	}
	s.env = (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()

	s.env.RegisterActivityWithOptions(func(ctx context.Context, req PacedSendRequest) (PacedSendResult, error) {
		s.seenRequests = append(s.seenRequests, req)
		if err, ok := s.failKinds[req.Kind]; ok {
			return PacedSendResult{}, err
		}
		if reason, ok := s.suppressKinds[req.Kind]; ok {
			return PacedSendResult{Status: PacedSendStatusSuppressedDND, Reason: reason}, nil
		}
		return PacedSendResult{Status: PacedSendStatusSent}, nil
	}, activity.RegisterOptions{Name: ActivityNotifyPaced})

	s.env.RegisterActivityWithOptions(func(ctx context.Context, req RecordSendRequest) error {
		s.recorded = append(s.recorded, req)
		return nil
	}, activity.RegisterOptions{Name: ActivityStudioRecordSend})

	// SPEC-W45 U6: the variant-assignment activity is stubbed under its
	// production name; the fail-open default is the control arm.
	s.env.RegisterActivityWithOptions(func(ctx context.Context, req AssignVariantRequest) (AssignVariantResult, error) {
		s.seenAssign = append(s.seenAssign, req)
		if s.assignErr != nil {
			return AssignVariantResult{}, s.assignErr
		}
		if s.assignVariant != "" {
			return AssignVariantResult{Variant: s.assignVariant}, nil
		}
		return AssignVariantResult{Variant: ControlArm}, nil
	}, activity.RegisterOptions{Name: ActivityStudioAssignVariant})
	return s
}

func mkBatch(kinds ...string) StudioSendBatchInput {
	in := StudioSendBatchInput{
		BatchID:     uuid.NewString(),
		TenantID:    uuid.NewString(),
		TenantSlug:  "acme",
		JourneyID:   uuid.NewString(),
		JourneyName: "Winback",
		// No quiet-hours window override: a window that never contains the
		// test clock would be flaky, so use a narrow midday window — the
		// testsuite auto-advances timers anyway, but staying outside the
		// window keeps the runs fast and deterministic.
		QuietHoursWindow:   "03:00-04:00",
		QuietHoursTimezone: "Africa/Lagos",
	}
	for _, kind := range kinds {
		in.Sends = append(in.Sends, QueuedSend{
			EnrollmentID: uuid.New(),
			ContactID:    uuid.New(),
			StepIdx:      0,
			Kind:         kind,
			Phone:        "+2348012345678",
			Name:         "Ada",
			Text:         "Hi Ada",
		})
	}
	return in
}

func TestStudioSendWorkflowAllSent(t *testing.T) {
	s := newStudioWfEnv(t)
	in := mkBatch(KindSMS, KindPushMarketing)
	s.env.ExecuteWorkflow(StudioSendWorkflow, in)
	if !s.env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := s.env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if len(s.seenRequests) != 2 {
		t.Fatalf("NotifyPaced calls = %d, want 2", len(s.seenRequests))
	}
	if s.seenRequests[0].Kind != PacedSendGeoCampaign || s.seenRequests[1].Kind != PacedSendPushMarketing {
		t.Fatalf("paced kinds = %q, %q", s.seenRequests[0].Kind, s.seenRequests[1].Kind)
	}
	if len(s.recorded) != 2 {
		t.Fatalf("recorded outcomes = %d, want 2", len(s.recorded))
	}
	for _, rec := range s.recorded {
		if rec.Status != PacedSendStatusSent || rec.TenantID != in.TenantID || rec.JourneyID != in.JourneyID {
			t.Fatalf("recorded = %+v, want sent with batch ids", rec)
		}
	}
}

func TestStudioSendWorkflowSuppressedAndFailed(t *testing.T) {
	s := newStudioWfEnv(t)
	s.suppressKinds[PacedSendGeoCampaign] = "global_dnd"
	s.failKinds[PacedSendPushMarketing] = errors.New("provider boom")
	in := mkBatch(KindSMS, KindPushMarketing, KindPushMarketing)
	s.env.ExecuteWorkflow(StudioSendWorkflow, in)
	if err := s.env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow must not fail on send errors: %v", err)
	}
	if len(s.recorded) != 3 {
		t.Fatalf("recorded = %d, want 3 (failure must not abort the batch)", len(s.recorded))
	}
	if s.recorded[0].Status != PacedSendStatusSuppressedDND || s.recorded[0].Reason != "global_dnd" {
		t.Fatalf("recorded[0] = %+v, want suppressed_dnd/global_dnd", s.recorded[0])
	}
	for i := 1; i <= 2; i++ {
		if s.recorded[i].Status != "failed" {
			t.Fatalf("recorded[%d] = %+v, want failed", i, s.recorded[i])
		}
	}
}

// SPEC-W45 U6: an experiment-tagged send resolves its variant through the
// StudioAssignVariant activity (person = enrollment id) and the resolved
// arm + experiment id ride the outcome record; plain sends skip the
// assignment entirely.
func TestStudioSendWorkflowExperimentVariant(t *testing.T) {
	s := newStudioWfEnv(t)
	s.assignVariant = ChallengerArm
	in := mkBatch(KindSMS, KindPushMarketing)
	experimentID := uuid.New()
	in.Sends[0].ExperimentID = &experimentID // only the first send is experiment-tagged
	s.env.ExecuteWorkflow(StudioSendWorkflow, in)
	if err := s.env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if len(s.seenAssign) != 1 {
		t.Fatalf("assignment calls = %d, want 1 (experiment sends only)", len(s.seenAssign))
	}
	if s.seenAssign[0].ExperimentID != experimentID.String() ||
		s.seenAssign[0].PersonID != in.Sends[0].EnrollmentID.String() ||
		s.seenAssign[0].TenantID != in.TenantID {
		t.Fatalf("assignment = %+v", s.seenAssign[0])
	}
	if len(s.recorded) != 2 {
		t.Fatalf("recorded = %d, want 2", len(s.recorded))
	}
	if s.recorded[0].Variant != ChallengerArm || s.recorded[0].ExperimentID != experimentID.String() {
		t.Fatalf("recorded[0] = %+v, want challenger variant + experiment id", s.recorded[0])
	}
	if s.recorded[1].Variant != "" || s.recorded[1].ExperimentID != "" {
		t.Fatalf("recorded[1] = %+v, want no variant (non-experiment send)", s.recorded[1])
	}
}

// U6 fail-open end to end: even when the assignment ACTIVITY itself errors
// (Temporal-level failure), the send still goes out and the outcome is
// recorded against the control arm.
func TestStudioSendWorkflowAssignmentFailOpen(t *testing.T) {
	s := newStudioWfEnv(t)
	s.assignErr = errors.New("temporal activity boom")
	in := mkBatch(KindSMS)
	experimentID := uuid.New()
	in.Sends[0].ExperimentID = &experimentID
	s.env.ExecuteWorkflow(StudioSendWorkflow, in)
	if err := s.env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow must not fail on assignment errors: %v", err)
	}
	if len(s.seenRequests) != 1 {
		t.Fatalf("NotifyPaced calls = %d, want 1 (the send must still go out)", len(s.seenRequests))
	}
	if len(s.recorded) != 1 || s.recorded[0].Variant != ControlArm || s.recorded[0].ExperimentID != experimentID.String() {
		t.Fatalf("recorded = %+v, want control-arm fail-open with the experiment id", s.recorded)
	}
}
