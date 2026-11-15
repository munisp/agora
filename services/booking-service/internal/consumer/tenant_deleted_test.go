package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go.uber.org/zap"
)

type fakeDeleter struct {
	slugs []string
	ids   []string
	err   error
}

func (f *fakeDeleter) DeleteTenantData(_ context.Context, slug, tenantID string) error {
	if f.err != nil {
		return f.err
	}
	f.slugs = append(f.slugs, slug)
	f.ids = append(f.ids, tenantID)
	return nil
}

func tenantDeletedRaw(t *testing.T, env tenantDeletedEnvelope) []byte {
	t.Helper()
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestTenantDeleted_CallsDeleteTenantData(t *testing.T) {
	del := &fakeDeleter{}
	c := &TenantDeletedConsumer{deleter: del, log: zap.NewNop()}
	env := tenantDeletedEnvelope{ID: "evt-td-1", Type: "com.opendesk.identity.TenantDeleted", Subject: "acme"}
	env.Data.TenantSlug = "acme"
	env.Data.TenantID = "t-1"
	env.Data.Actor = "platform-admin"
	if err := c.Process(context.Background(), tenantDeletedRaw(t, env)); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(del.slugs) != 1 || del.slugs[0] != "acme" {
		t.Fatalf("DeleteTenantData not called with slug: %+v", del.slugs)
	}
}

func TestTenantDeleted_SlugFallsBackToSubject(t *testing.T) {
	del := &fakeDeleter{}
	c := &TenantDeletedConsumer{deleter: del, log: zap.NewNop()}
	env := tenantDeletedEnvelope{ID: "evt-td-2", Type: TenantDeletedEventType, Subject: "acme"}
	if err := c.Process(context.Background(), tenantDeletedRaw(t, env)); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(del.slugs) != 1 || del.slugs[0] != "acme" {
		t.Fatalf("subject fallback failed: %+v", del.slugs)
	}
}

func TestTenantDeleted_OtherTypesAcked(t *testing.T) {
	del := &fakeDeleter{}
	c := &TenantDeletedConsumer{deleter: del, log: zap.NewNop()}
	env := tenantDeletedEnvelope{ID: "evt-mi-1", Type: "com.opendesk.identity.MemberInvited"}
	if err := c.Process(context.Background(), tenantDeletedRaw(t, env)); err != nil {
		t.Fatalf("other event types must ack: %v", err)
	}
	if len(del.slugs) != 0 {
		t.Fatalf("no deletion expected for MemberInvited: %+v", del.slugs)
	}
}

func TestTenantDeleted_MissingSlug_Permanent(t *testing.T) {
	c := &TenantDeletedConsumer{deleter: &fakeDeleter{}, log: zap.NewNop()}
	env := tenantDeletedEnvelope{ID: "evt-td-3", Type: TenantDeletedEventType}
	err := c.Process(context.Background(), tenantDeletedRaw(t, env))
	if !errors.Is(err, errPermanentTenantDeleted) {
		t.Fatalf("expected permanent error, got %v", err)
	}
}

func TestTenantDeleted_Malformed_Permanent(t *testing.T) {
	c := &TenantDeletedConsumer{deleter: &fakeDeleter{}, log: zap.NewNop()}
	err := c.Process(context.Background(), []byte("{not json"))
	if !errors.Is(err, errPermanentTenantDeleted) {
		t.Fatalf("poison payload must be permanent, got %v", err)
	}
}

func TestTenantDeleted_DeleterError_Retries(t *testing.T) {
	del := &fakeDeleter{err: errors.New("db down")}
	c := &TenantDeletedConsumer{deleter: del, log: zap.NewNop()}
	env := tenantDeletedEnvelope{ID: "evt-td-4", Type: TenantDeletedEventType, Subject: "acme"}
	err := c.Process(context.Background(), tenantDeletedRaw(t, env))
	if err == nil || errors.Is(err, errPermanentTenantDeleted) {
		t.Fatalf("transient store error must be retryable, got %v", err)
	}
}


func TestTenantDeletedPurgesTheEventsTenantID(t *testing.T) {
	// A same-slug re-create left an older site row for the slug; purging by slug hit that old tenant, not the one
	// just deleted. The event's tenant_id must reach the deleter.
	del := &fakeDeleter{}
	c := &TenantDeletedConsumer{deleter: del, log: zap.NewNop()}
	var env tenantDeletedEnvelope
	env.Type = "com.opendesk.identity.TenantDeleted"
	env.Subject = "harbour"
	env.Data.TenantSlug = "harbour"
	env.Data.TenantID = "55ec7166-8dfc-4508-a4b7-f703f8e1312f"
	if err := c.Process(context.Background(), tenantDeletedRaw(t, env)); err != nil {
		t.Fatal(err)
	}
	if len(del.ids) != 1 || del.ids[0] != env.Data.TenantID {
		t.Fatalf("deleter got tenant ids %v, want [%s]", del.ids, env.Data.TenantID)
	}
}
