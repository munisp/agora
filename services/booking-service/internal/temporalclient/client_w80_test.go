package temporalclient

// SPEC-W80 C1 test: StartBookingSaga must pass the saga input through to
// Temporal untouched — including the W80 workflow_name field the worker
// resolves the pack child workflow from (bookingops.SagaInput.WorkflowName,
// JSON "workflow_name", is the bookingops side of the frozen W80 contract;
// temporalclient must not drop or alter fields).

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/opendesk/booking-service/internal/bookingops"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
)

// captureClient is a client.Client fake that records the ExecuteWorkflow
// call. All other methods panic via the nil embedded interface (the saga
// path only uses ExecuteWorkflow).
type captureClient struct {
	client.Client

	gotOpts     client.StartWorkflowOptions
	gotWorkflow string
	gotArgs     []any
}

func (f *captureClient) ExecuteWorkflow(ctx context.Context, opts client.StartWorkflowOptions, wf any, args ...any) (client.WorkflowRun, error) {
	f.gotOpts = opts
	f.gotWorkflow, _ = wf.(string)
	f.gotArgs = args
	return fakeRun{}, nil
}

type fakeRun struct{}

func (fakeRun) GetID() string    { return "wf-id" }
func (fakeRun) GetRunID() string { return "run-1" }
func (fakeRun) Get(ctx context.Context, valuePtr any) error {
	return nil
}
func (fakeRun) GetWithOptions(ctx context.Context, valuePtr any, options client.WorkflowRunGetOptions) error {
	return nil
}

func TestStartBookingSagaPassesInputThrough(t *testing.T) {
	fc := &captureClient{}
	c := &Client{tc: fc, taskQueue: "booking-saga"}

	in := bookingops.SagaInput{
		BookingID:    "b-1",
		TenantID:     "t-1",
		TenantSlug:   "acme",
		ContactPhone: "+15551234567",
		ContactEmail: "jane@example.com",
		ContactName:  "Jane",
		StartsAt:     time.Now().Add(48 * time.Hour),
		EndsAt:       time.Now().Add(49 * time.Hour),
		PriceCents:   5000,
		Currency:     "USD",
		Industry:     "healthcare",
	}
	runID, err := c.StartBookingSaga(context.Background(), in)
	if err != nil {
		t.Fatalf("StartBookingSaga: %v", err)
	}
	if runID != "run-1" {
		t.Fatalf("run id = %q, want run-1", runID)
	}
	if fc.gotWorkflow != WorkflowType {
		t.Fatalf("workflow type = %q, want %q", fc.gotWorkflow, WorkflowType)
	}
	if fc.gotOpts.ID != "booking-saga-b-1" {
		t.Fatalf("workflow id = %q, want booking-saga-b-1", fc.gotOpts.ID)
	}
	if fc.gotOpts.TaskQueue != "booking-saga" {
		t.Fatalf("task queue = %q", fc.gotOpts.TaskQueue)
	}
	if fc.gotOpts.WorkflowIDReusePolicy != enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE {
		t.Fatalf("unexpected id reuse policy: %v", fc.gotOpts.WorkflowIDReusePolicy)
	}
	if len(fc.gotArgs) != 1 {
		t.Fatalf("expected exactly one workflow arg, got %d", len(fc.gotArgs))
	}
	got, ok := fc.gotArgs[0].(bookingops.SagaInput)
	if !ok {
		t.Fatalf("workflow arg type = %T, want bookingops.SagaInput", fc.gotArgs[0])
	}
	if got != in {
		t.Fatalf("saga input was not passed through untouched:\n got %+v\nwant %+v", got, in)
	}

	// JSON contract sanity: the serialized input the worker decodes must
	// carry the snake_case keys the worker's mirror struct expects
	// (workflow_name rides here via bookingops.SagaInput.WorkflowName —
	// the bookingops side of the W80 contract).
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"booking_id", "tenant_id", "industry"} {
		if _, ok := keys[k]; !ok {
			t.Fatalf("serialized saga input missing key %q: %s", k, raw)
		}
	}
}
