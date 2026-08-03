package devices

import (
	"context"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
)

// SPEC-W16 Agent B store tests run against embedded Postgres (same harness
// as the leads/referrals tests; dedicated port 5561 avoids the
// postmaster.pid race with sibling packages under `go test ./...`;
// -short skips).

func newTestStore(t *testing.T) *Store {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres devices store test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_devices_test").
		Port(5561).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })
	st, err := DialStore(context.Background(),
		"postgres://postgres:postgres@localhost:5561/booking_devices_test?sslmode=disable")
	if err != nil {
		t.Fatalf("DialStore: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

func mkDevice(tenantID uuid.UUID, contactID *uuid.UUID, token, platform, app string) DeviceToken {
	return DeviceToken{
		TenantID:  tenantID,
		ContactID: contactID,
		Token:     token,
		Platform:  platform,
		App:       app,
	}
}

// Upsert idempotency (contract §1): re-registering the same token refreshes
// contact_id/platform/app + last_seen_at; created=false; one row only.
func TestUpsertDeviceIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contactA := uuid.New()
	contactB := uuid.New()

	d := mkDevice(tenantID, &contactA, "fcm-token-1", PlatformAndroid, AppField)
	created, err := st.Upsert(ctx, &d)
	if err != nil || !created {
		t.Fatalf("first upsert: created=%v err=%v", created, err)
	}
	if d.CreatedAt.IsZero() || d.LastSeenAt.IsZero() {
		t.Fatalf("timestamps not stamped: %+v", d)
	}
	firstSeen := d.LastSeenAt

	time.Sleep(10 * time.Millisecond) // make the last_seen_at bump observable
	re := mkDevice(tenantID, &contactB, "fcm-token-1", PlatformIOS, AppAdmin)
	created, err = st.Upsert(ctx, &re)
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if created {
		t.Fatal("re-registration must report created=false")
	}
	if re.ContactID == nil || *re.ContactID != contactB || re.Platform != PlatformIOS || re.App != AppAdmin {
		t.Fatalf("refresh did not update fields: %+v", re)
	}
	if !re.LastSeenAt.After(firstSeen) {
		t.Fatalf("last_seen_at not refreshed: %v vs %v", re.LastSeenAt, firstSeen)
	}
	if !re.CreatedAt.Equal(d.CreatedAt) {
		t.Fatalf("created_at must survive re-registration: %v vs %v", re.CreatedAt, d.CreatedAt)
	}

	all, err := st.List(ctx, tenantID, "", "")
	if err != nil || len(all) != 1 {
		t.Fatalf("one row expected after re-registration: %+v, %v", all, err)
	}
}

// ListByContact backs GET /internal/devices?contact_id= (Agent A contract):
// only the contact's devices, empty slice (never nil) when none.
func TestListByContact(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contact := uuid.New()
	other := uuid.New()

	for _, d := range []DeviceToken{
		mkDevice(tenantID, &contact, "tok-a", PlatformAndroid, AppField),
		mkDevice(tenantID, &contact, "tok-b", PlatformWeb, AppField),
		mkDevice(tenantID, &other, "tok-c", PlatformIOS, AppAdmin),
		mkDevice(tenantID, nil, "tok-d", PlatformWeb, AppAdmin), // unlinked device
	} {
		d := d
		if _, err := st.Upsert(ctx, &d); err != nil {
			t.Fatalf("upsert %s: %v", d.Token, err)
		}
	}

	devs, err := st.ListByContact(ctx, tenantID, contact)
	if err != nil || len(devs) != 2 {
		t.Fatalf("contact devices: %+v, %v", devs, err)
	}
	for _, d := range devs {
		if d.ContactID == nil || *d.ContactID != contact {
			t.Fatalf("wrong contact leaked: %+v", d)
		}
	}

	none, err := st.ListByContact(ctx, tenantID, uuid.New())
	if err != nil {
		t.Fatalf("unknown contact: %v", err)
	}
	if none == nil || len(none) != 0 {
		t.Fatalf("empty result must be a non-nil empty array (Agent A contract): %#v", none)
	}

	// Cross-tenant isolation (app-level + RLS belt-and-braces).
	cross, err := st.ListByContact(ctx, uuid.New(), contact)
	if err != nil || len(cross) != 0 {
		t.Fatalf("cross-tenant list: %+v, %v", cross, err)
	}
}

// List filters (GET /v1/devices?platform=&app=).
func TestListFilters(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	for _, d := range []DeviceToken{
		mkDevice(tenantID, nil, "t1", PlatformAndroid, AppField),
		mkDevice(tenantID, nil, "t2", PlatformAndroid, AppAdmin),
		mkDevice(tenantID, nil, "t3", PlatformWeb, AppField),
	} {
		d := d
		if _, err := st.Upsert(ctx, &d); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	android, err := st.List(ctx, tenantID, PlatformAndroid, "")
	if err != nil || len(android) != 2 {
		t.Fatalf("platform filter: %+v, %v", android, err)
	}
	field, err := st.List(ctx, tenantID, "", AppField)
	if err != nil || len(field) != 2 {
		t.Fatalf("app filter: %+v, %v", field, err)
	}
	androidField, err := st.List(ctx, tenantID, PlatformAndroid, AppField)
	if err != nil || len(androidField) != 1 || androidField[0].Token != "t1" {
		t.Fatalf("combined filter: %+v, %v", androidField, err)
	}
	none, err := st.List(ctx, tenantID, PlatformIOS, "")
	if err != nil || len(none) != 0 {
		t.Fatalf("no-match filter: %+v, %v", none, err)
	}
}

// Delete scoping: tenant-scoped delete; missing / cross-tenant → ErrNotFound.
func TestDeleteDevice(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	d := mkDevice(tenantID, nil, "tok-del", PlatformAndroid, AppField)
	if _, err := st.Upsert(ctx, &d); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.Delete(ctx, uuid.New(), "tok-del"); err != ErrNotFound {
		t.Fatalf("cross-tenant delete = %v, want ErrNotFound", err)
	}
	if err := st.Delete(ctx, tenantID, "tok-del"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := st.Delete(ctx, tenantID, "tok-del"); err != ErrNotFound {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
}

// Validation: contract §1 enums + required fields (no DB needed, but kept
// beside the store tests for one-package coverage).
func TestValidate(t *testing.T) {
	tenantID := uuid.New()
	good := mkDevice(tenantID, nil, "tok", PlatformAndroid, AppField)
	if err := Validate(&good); err != nil {
		t.Fatalf("valid device rejected: %v", err)
	}
	for name, mutate := range map[string]func(*DeviceToken){
		"empty token":     func(d *DeviceToken) { d.Token = "  " },
		"bad platform":    func(d *DeviceToken) { d.Platform = "windows" },
		"bad app":         func(d *DeviceToken) { d.App = "crm" },
		"missing tenant":  func(d *DeviceToken) { d.TenantID = uuid.Nil },
		"oversized token": func(d *DeviceToken) { d.Token = string(make([]byte, maxTokenLen+1)) },
	} {
		d := mkDevice(tenantID, nil, "tok", PlatformAndroid, AppField)
		mutate(&d)
		if err := Validate(&d); err == nil {
			t.Fatalf("%s: expected validation error", name)
		}
	}
}
