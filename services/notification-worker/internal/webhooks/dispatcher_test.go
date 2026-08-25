package webhooks

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/opendesk/notification-worker/internal/store"
	"github.com/opendesk/notification-worker/internal/workflows"
	"go.temporal.io/sdk/client"
	"go.uber.org/zap"
)

// fakeStore is an in-memory SubscriptionStore.
type fakeStore struct {
	subs       []store.WebhookSubscription
	deliveries []store.WebhookDelivery
	err        error
}

func (f *fakeStore) ActiveSubscriptions(_ context.Context, tenantID uuid.UUID) ([]store.WebhookSubscription, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []store.WebhookSubscription
	for _, s := range f.subs {
		if s.TenantID == tenantID && s.Active {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeStore) CreateDelivery(_ context.Context, d *store.WebhookDelivery) error {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	f.deliveries = append(f.deliveries, *d)
	return nil
}

// fakeRun is a no-op client.WorkflowRun.
type fakeRun struct{ id string }

func (r fakeRun) GetID() string    { return r.id }
func (r fakeRun) GetRunID() string { return "run-1" }
func (r fakeRun) Get(_ context.Context, _ interface{}) error {
	return nil
}

func (r fakeRun) GetWithOptions(_ context.Context, _ interface{}, _ client.WorkflowRunGetOptions) error {
	return nil
}

// fakeStarter records ExecuteWorkflow calls.
type fakeStarter struct {
	started []workflows.WebhookDeliveryInput
	ids     []string
	err     error
}

func (f *fakeStarter) ExecuteWorkflow(_ context.Context, opts client.StartWorkflowOptions, _ interface{}, args ...interface{}) (client.WorkflowRun, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.ids = append(f.ids, opts.ID)
	if in, ok := args[0].(workflows.WebhookDeliveryInput); ok {
		f.started = append(f.started, in)
	}
	return fakeRun{id: opts.ID}, nil
}

func newTestDispatcher(st SubscriptionStore, starter WorkflowStarter) *Dispatcher {
	return &Dispatcher{store: st, starter: starter, taskQueue: "test-queue", log: zap.NewNop()}
}

func bookingEvent(tenantID, eventType string) []byte {
	return []byte(fmt.Sprintf(`{"specversion":"1.0","id":"evt-1","source":"booking-service","type":%q,"subject":"acme","tenantid":%q,"data":{"booking_id":"b-1"}}`, eventType, tenantID))
}

func TestDispatcherFansOutToMatchingSubscriptions(t *testing.T) {
	tenantA := uuid.New()
	tenantB := uuid.New()
	st := &fakeStore{subs: []store.WebhookSubscription{
		{ID: uuid.New(), TenantID: tenantA, URL: "https://a.example/hook", Secret: "s1", Events: []string{"com.opendesk.booking.*"}, Active: true},
		{ID: uuid.New(), TenantID: tenantA, URL: "https://a2.example/hook", Secret: "s2", Events: []string{"com.opendesk.conversation.*"}, Active: true}, // no match
		{ID: uuid.New(), TenantID: tenantA, URL: "https://off.example/hook", Secret: "s3", Events: []string{"*"}, Active: false},                         // inactive
		{ID: uuid.New(), TenantID: tenantB, URL: "https://b.example/hook", Secret: "s4", Events: []string{"*"}, Active: true},                            // other tenant
	}}
	starter := &fakeStarter{}
	d := newTestDispatcher(st, starter)

	if err := d.Process(context.Background(), bookingEvent(tenantA.String(), "com.opendesk.booking.BookingCreated")); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(st.deliveries) != 1 {
		t.Fatalf("deliveries = %d, want exactly 1 (matching active sub of tenant A)", len(st.deliveries))
	}
	dlv := st.deliveries[0]
	if dlv.SubID != st.subs[0].ID || dlv.EventType != "com.opendesk.booking.BookingCreated" || dlv.EventID != "evt-1" {
		t.Fatalf("delivery = %+v", dlv)
	}
	if len(starter.started) != 1 {
		t.Fatalf("workflow starts = %d", len(starter.started))
	}
	in := starter.started[0]
	if in.URL != "https://a.example/hook" || in.Secret != "s1" || in.DeliveryID != dlv.ID.String() {
		t.Fatalf("workflow input = %+v", in)
	}
	if starter.ids[0] != "whd-"+st.subs[0].ID.String()+"-evt-1" {
		t.Fatalf("workflow id = %q, want whd-{sub}-{event} (N-03)", starter.ids[0])
	}
	// The raw envelope is the delivery body (signed verbatim downstream).
	if string(in.Body) != string(bookingEvent(tenantA.String(), "com.opendesk.booking.BookingCreated")) {
		t.Fatal("workflow body is not the raw event envelope")
	}
}

func TestDispatcherSkipsEventsWithoutTenant(t *testing.T) {
	st := &fakeStore{}
	starter := &fakeStarter{}
	d := newTestDispatcher(st, starter)
	for _, raw := range [][]byte{
		[]byte(`{"type":"com.opendesk.booking.BookingCreated"}`), // no tenantid
		[]byte(`{"tenantid":"not-a-uuid","type":"x"}`),
		[]byte(`not json`),
	} {
		if err := d.Process(context.Background(), raw); err != nil {
			t.Fatalf("process %s: %v", raw, err)
		}
	}
	if len(st.deliveries) != 0 || len(starter.started) != 0 {
		t.Fatal("tenant-less events must not fan out")
	}
}

func TestDispatcherAlreadyStartedIsIdempotent(t *testing.T) {
	tenant := uuid.New()
	subID := uuid.New()
	st := &fakeStore{subs: []store.WebhookSubscription{
		{ID: subID, TenantID: tenant, URL: "https://a.example/hook", Events: []string{"*"}, Active: true},
	}}
	starter := &fakeStarter{err: errors.New("workflow execution already started")}
	d := newTestDispatcher(st, starter)
	if err := d.Process(context.Background(), bookingEvent(tenant.String(), "com.opendesk.booking.BookingCancelled")); err != nil {
		t.Fatalf("already-started must be acknowledged, got %v", err)
	}
}

func TestDispatcherStoreFailurePropagates(t *testing.T) {
	st := &fakeStore{err: errors.New("db down")}
	d := newTestDispatcher(st, &fakeStarter{})
	if err := d.Process(context.Background(), bookingEvent(uuid.NewString(), "com.opendesk.booking.BookingCreated")); err == nil {
		t.Fatal("expected store failure to propagate")
	}
}
