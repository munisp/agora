package store

// SPEC-W44 store tests: N-03 delivery idempotency (UNIQUE(sub_id,event_id)
// + ON CONFLICT read-back) and the K3 ops_alerts table.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestCreateDeliveryIdempotent(t *testing.T) {
	st := newDNDTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	sub := &WebhookSubscription{TenantID: tenantID, TenantSlug: "acme", URL: "https://a.example/hook", Events: []string{"*"}}
	require.NoError(t, st.CreateSubscription(ctx, sub))

	d1 := &WebhookDelivery{SubID: sub.ID, TenantID: tenantID, EventID: "evt-1", EventType: "booking.created"}
	require.NoError(t, st.CreateDelivery(ctx, d1))
	require.NotEqual(t, uuid.Nil, d1.ID)

	// Redelivery with the same (sub, event) → the EXISTING row, no dup.
	d2 := &WebhookDelivery{SubID: sub.ID, TenantID: tenantID, EventID: "evt-1", EventType: "booking.created"}
	require.NoError(t, st.CreateDelivery(ctx, d2))
	require.Equal(t, d1.ID, d2.ID, "idempotent create must return the existing delivery (N-03)")

	var n int64
	require.NoError(t, st.pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_deliveries WHERE sub_id=$1`, sub.ID).Scan(&n))
	require.EqualValues(t, 1, n)

	// A different event id creates a second row.
	d3 := &WebhookDelivery{SubID: sub.ID, TenantID: tenantID, EventID: "evt-2", EventType: "booking.created"}
	require.NoError(t, st.CreateDelivery(ctx, d3))
	require.NotEqual(t, d1.ID, d3.ID)

	// UpdateDelivery (id-keyed, internal-pool path) still works.
	require.NoError(t, st.UpdateDelivery(ctx, d1.ID, StatusDelivered, 1, nil, nil))
	got, err := st.ListDeliveries(ctx, tenantID, sub.ID, 10)
	require.NoError(t, err)
	require.Len(t, got, 2)
	var delivered WebhookDelivery
	for _, d := range got {
		if d.ID == d1.ID {
			delivered = d
		}
	}
	require.Equal(t, StatusDelivered, delivered.Status)
}

func TestOpsAlertsStore(t *testing.T) {
	st := newDNDTestStore(t)
	ctx := context.Background()

	a := &OpsAlert{EventID: "evt-1", TenantID: "acme", Source: "notification-worker",
		Type: "opendesk.payments.webhook.exhausted", Severity: "critical", Payload: []byte(`{"severity":"critical"}`)}
	inserted, err := st.InsertOpsAlert(ctx, a)
	require.NoError(t, err)
	require.True(t, inserted)
	require.NotEqual(t, uuid.Nil, a.ID)
	require.False(t, a.ReceivedAt.IsZero())

	// Idempotent on the CloudEvent id: redelivery is a no-op.
	dup := &OpsAlert{EventID: "evt-1", TenantID: "acme", Severity: "critical", Payload: []byte(`{}`)}
	inserted, err = st.InsertOpsAlert(ctx, dup)
	require.NoError(t, err)
	require.False(t, inserted)

	// event_id is required (no idempotency anchor → error).
	_, err = st.InsertOpsAlert(ctx, &OpsAlert{Payload: []byte(`{}`)})
	require.Error(t, err)

	// A second alert lands; ListOpsAlerts is newest-first and limit-capped.
	_, err = st.InsertOpsAlert(ctx, &OpsAlert{EventID: "evt-2", TenantID: "beta", Severity: "info", Payload: []byte(`{}`)})
	require.NoError(t, err)

	all, err := st.ListOpsAlerts(ctx, "", 100)
	require.NoError(t, err)
	require.Len(t, all, 2)
	require.Equal(t, "evt-2", all[0].EventID, "newest first")

	only, err := st.ListOpsAlerts(ctx, "acme", 100)
	require.NoError(t, err)
	require.Len(t, only, 1)
	require.Equal(t, "evt-1", only[0].EventID)

	capped, err := st.ListOpsAlerts(ctx, "", 1)
	require.NoError(t, err)
	require.Len(t, capped, 1)
}
