package notifyoutbox

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/opendesk/notification-worker/internal/workflows"
	"go.temporal.io/sdk/client"
	"go.uber.org/zap"
)

type fakeSender struct {
	sent []struct {
		channel, dest, subject, text string
	}
	err error
}

func (f *fakeSender) Send(_ context.Context, channel, destination, subject, text string) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, struct {
		channel, dest, subject, text string
	}{channel, destination, subject, text})
	return nil
}

func TestProcessSendPortalCodeSMS(t *testing.T) {
	sender := &fakeSender{}
	c := &Consumer{sender: sender, log: zap.NewNop()}
	raw := []byte(`{"specversion":"1.0","id":"e-1","type":"com.opendesk.notifications.SendPortalCode","subject":"acme","tenantid":"t-1","data":{"channel":"sms","destination":"+15550101","code":"482910","contact_name":"Pia","site_slug":"acme-books","expires_in_minutes":10}}`)
	if err := c.Process(context.Background(), raw); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("sent = %d, want 1", len(sender.sent))
	}
	msg := sender.sent[0]
	if msg.channel != "sms" || msg.dest != "+15550101" {
		t.Fatalf("msg = %+v", msg)
	}
	if want := "482910"; !strings.Contains(msg.text, want) {
		t.Fatalf("text %q does not contain the code", msg.text)
	}
}

func TestProcessSendPortalCodeEmail(t *testing.T) {
	sender := &fakeSender{}
	c := &Consumer{sender: sender, log: zap.NewNop()}
	raw := []byte(`{"type":"com.opendesk.notifications.SendPortalCode","data":{"channel":"email","destination":"pia@example.com","code":"111222"}}`)
	if err := c.Process(context.Background(), raw); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(sender.sent) != 1 || sender.sent[0].channel != "email" || sender.sent[0].subject == "" {
		t.Fatalf("sent = %+v", sender.sent)
	}
}

func TestProcessRejectsInvalidPayload(t *testing.T) {
	sender := &fakeSender{}
	c := &Consumer{sender: sender, log: zap.NewNop()}
	for _, raw := range [][]byte{
		[]byte(`{"type":"com.opendesk.notifications.SendPortalCode","data":{"channel":"pigeon","destination":"x","code":"123456"}}`),
		[]byte(`{"type":"com.opendesk.notifications.SendPortalCode","data":{"channel":"sms","destination":"","code":"123456"}}`),
		[]byte(`{"type":"com.opendesk.notifications.SendPortalCode","data":{"channel":"sms","destination":"+1","code":""}}`),
	} {
		if err := c.Process(context.Background(), raw); err == nil {
			t.Fatalf("expected invalid payload error for %s", raw)
		}
	}
	if len(sender.sent) != 0 {
		t.Fatal("invalid payloads must not send")
	}
}

func TestProcessUnknownTypeIsAcknowledged(t *testing.T) {
	sender := &fakeSender{}
	c := &Consumer{sender: sender, log: zap.NewNop()}
	for _, raw := range [][]byte{
		[]byte(`{"type":"com.opendesk.notifications.SendReminder","data":{}}`),
		[]byte(`not json`),
	} {
		if err := c.Process(context.Background(), raw); err != nil {
			t.Fatalf("unknown/malformed commands must be acknowledged: %v", err)
		}
	}
	if len(sender.sent) != 0 {
		t.Fatal("unknown commands must not send")
	}
}

// fakeStarter records ExecuteWorkflow calls (signals.Bridge test idiom).
type fakeStarter struct {
	started []struct {
		id           string
		workflowType string
		req          workflows.PacedSendRequest
	}
	err error
}

func (f *fakeStarter) ExecuteWorkflow(_ context.Context, opts client.StartWorkflowOptions, workflowType interface{}, args ...interface{}) (client.WorkflowRun, error) {
	if f.err != nil {
		return nil, f.err
	}
	wt, _ := workflowType.(string)
	var req workflows.PacedSendRequest
	if len(args) > 0 {
		req, _ = args[0].(workflows.PacedSendRequest)
	}
	f.started = append(f.started, struct {
		id           string
		workflowType string
		req          workflows.PacedSendRequest
	}{opts.ID, wt, req})
	return nil, nil
}

// SPEC-W19 integrator: a PacedSend command (field-service dispatch push
// envelope) starts one PacedSendWorkflow carrying the unmarshaled W16
// PacedSendRequest; the workflow ID derives from the CloudEvent id.
func TestProcessPacedSendStartsWorkflow(t *testing.T) {
	starter := &fakeStarter{}
	c := &Consumer{sender: &fakeSender{}, starter: starter, taskQueue: "opendesk-main", log: zap.NewNop()}
	raw := []byte(`{"specversion":"1.0","id":"evt-42","type":"com.opendesk.notifications.PacedSend","subject":"acme","tenantid":"t-1","data":{"kind":"push_notification","push":{"tenant_slug":"acme","contact_id":"9b1c","title":"Work order dispatched","body":"You have a new work order: Fix pump","app":"field","data":{"kind":"dispatch"}}}}`)
	if err := c.Process(context.Background(), raw); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(starter.started) != 1 {
		t.Fatalf("started = %d, want 1", len(starter.started))
	}
	got := starter.started[0]
	if got.id != "paced-send-evt-42" {
		t.Fatalf("workflow id = %q, want paced-send-evt-42", got.id)
	}
	if got.workflowType != workflows.WorkflowTypePacedSend {
		t.Fatalf("workflow type = %q, want %s", got.workflowType, workflows.WorkflowTypePacedSend)
	}
	if got.req.Kind != workflows.PacedSendPushNotification {
		t.Fatalf("req.Kind = %q, want %s", got.req.Kind, workflows.PacedSendPushNotification)
	}
	if got.req.Push == nil || got.req.Push.Title != "Work order dispatched" || got.req.Push.App != "field" {
		t.Fatalf("req.Push = %+v", got.req.Push)
	}
}

// Without a starter the PacedSend command is acknowledged gracefully (the
// same posture as unknown types — never hot-looped).
func TestProcessPacedSendWithoutStarterIsAcked(t *testing.T) {
	sender := &fakeSender{}
	c := &Consumer{sender: sender, log: zap.NewNop()}
	raw := []byte(`{"id":"e-9","type":"com.opendesk.notifications.PacedSend","data":{"kind":"push_notification","push":{"tenant_slug":"acme","title":"t","body":"b"}}}`)
	if err := c.Process(context.Background(), raw); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(sender.sent) != 0 {
		t.Fatal("PacedSend must not use the binding sender")
	}
}

// An already-started workflow (redelivered command) is acknowledged without
// error — no duplicate send, no hot loop.
func TestProcessPacedSendAlreadyStartedIsAcked(t *testing.T) {
	starter := &fakeStarter{err: errors.New("workflow execution already started")}
	c := &Consumer{sender: &fakeSender{}, starter: starter, taskQueue: "q", log: zap.NewNop()}
	raw := []byte(`{"id":"e-dup","type":"com.opendesk.notifications.PacedSend","data":{"kind":"push_notification","push":{"tenant_slug":"acme","title":"t","body":"b"}}}`)
	if err := c.Process(context.Background(), raw); err != nil {
		t.Fatalf("process: %v", err)
	}
}

// A malformed PacedSend payload (no kind) is acknowledged, not retried.
func TestProcessPacedSendMalformedIsAcked(t *testing.T) {
	starter := &fakeStarter{}
	c := &Consumer{sender: &fakeSender{}, starter: starter, taskQueue: "q", log: zap.NewNop()}
	for _, raw := range [][]byte{
		[]byte(`{"id":"e-1","type":"com.opendesk.notifications.PacedSend","data":{}}`),
		[]byte(`{"id":"e-2","type":"com.opendesk.notifications.PacedSend"}`),
	} {
		if err := c.Process(context.Background(), raw); err != nil {
			t.Fatalf("malformed PacedSend must be acknowledged: %v", err)
		}
	}
	if len(starter.started) != 0 {
		t.Fatal("malformed PacedSend must not start a workflow")
	}
}

// A Temporal start failure (not already-started) propagates so Run logs it
// (and still acks, per the consumer's never-hot-loop posture).
func TestProcessPacedSendStarterFailurePropagates(t *testing.T) {
	starter := &fakeStarter{err: errors.New("temporal down")}
	c := &Consumer{sender: &fakeSender{}, starter: starter, taskQueue: "q", log: zap.NewNop()}
	raw := []byte(`{"id":"e-3","type":"com.opendesk.notifications.PacedSend","data":{"kind":"push_notification","push":{"tenant_slug":"acme","title":"t","body":"b"}}}`)
	if err := c.Process(context.Background(), raw); err == nil {
		t.Fatal("expected starter failure to propagate")
	}
}

func TestProcessSenderFailurePropagates(t *testing.T) {
	sender := &fakeSender{err: errors.New("twilio down")}
	c := &Consumer{sender: sender, log: zap.NewNop()}
	raw := []byte(`{"type":"com.opendesk.notifications.SendPortalCode","data":{"channel":"sms","destination":"+1","code":"123456"}}`)
	if err := c.Process(context.Background(), raw); err == nil {
		t.Fatal("expected sender failure to propagate")
	}
}
