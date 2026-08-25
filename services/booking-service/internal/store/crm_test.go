package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// --- pure merge logic (no DB) ---

func TestMergeExternalContactNonEmptyWins(t *testing.T) {
	existing := Contact{Name: "Old Name", Phone: "+1", Email: "old@x.com", Notes: "old notes"}
	in := Contact{Name: "New Name", Email: "new@x.com", Source: "twenty", ExternalID: "person-1"}
	out := MergeExternalContact(existing, in)
	if out.Name != "New Name" || out.Email != "new@x.com" {
		t.Errorf("merged = %+v", out)
	}
	if out.Phone != "+1" || out.Notes != "old notes" {
		t.Errorf("empty inbound fields must keep stored values: %+v", out)
	}
	if out.Source != "twenty" || out.ExternalID != "person-1" {
		t.Errorf("source/external_id not merged: %+v", out)
	}
}

func TestMergeExternalContactKeepsExternalStampWhenEmpty(t *testing.T) {
	existing := Contact{Name: "A", Source: "twenty", ExternalID: "p-1"}
	out := MergeExternalContact(existing, Contact{Name: "B"})
	if out.Source != "twenty" || out.ExternalID != "p-1" {
		t.Errorf("external stamp lost: %+v", out)
	}
}

// --- DB-backed upsert / note tests (embedded Postgres, skippable) ---

func TestUpsertExternalContactCreateThenUpdateByPhoneAndEmail(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	// 1) Create: no existing contact -> created=true, stamped with twenty ids.
	in := Contact{Name: "Jane Doe", Phone: "+15550001", Email: "jane@x.com", Source: "twenty", ExternalID: "person-1"}
	created, err := st.UpsertExternalContact(ctx, tenantID, &in)
	if err != nil {
		t.Fatalf("create upsert: %v", err)
	}
	if !created {
		t.Fatal("first upsert should create")
	}

	// 2) Update by phone: same phone, new name -> created=false, same row id.
	byPhone := Contact{Name: "Jane D.", Phone: "+15550001", Source: "twenty", ExternalID: "person-1"}
	created, err = st.UpsertExternalContact(ctx, tenantID, &byPhone)
	if err != nil {
		t.Fatalf("update-by-phone upsert: %v", err)
	}
	if created {
		t.Fatal("upsert with matching phone should update, not create")
	}
	if byPhone.ID != in.ID {
		t.Errorf("update-by-phone hit a different row: %s != %s", byPhone.ID, in.ID)
	}
	// Merge semantics: email kept from the first write (empty inbound).
	if byPhone.Email != "jane@x.com" {
		t.Errorf("email lost on merge: %+v", byPhone)
	}

	// 3) Update by email: phone-less inbound matches on email.
	byEmail := Contact{Name: "Janet Doe", Email: "jane@x.com", Source: "twenty", ExternalID: "person-1"}
	created, err = st.UpsertExternalContact(ctx, tenantID, &byEmail)
	if err != nil {
		t.Fatalf("update-by-email upsert: %v", err)
	}
	if created || byEmail.ID != in.ID {
		t.Fatalf("update-by-email created=%v id=%s", created, byEmail.ID)
	}
	got, err := st.GetContact(ctx, tenantID, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Janet Doe" {
		t.Errorf("stored name = %q, want merged update", got.Name)
	}

	// 4) No match -> a second contact row is created.
	other := Contact{Name: "Bob", Phone: "+1999", Source: "twenty", ExternalID: "person-2"}
	created, err = st.UpsertExternalContact(ctx, tenantID, &other)
	if err != nil || !created {
		t.Fatalf("unmatched upsert created=%v err=%v", created, err)
	}
	list, err := st.ListContacts(ctx, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("contacts = %d, want 2", len(list))
	}
}

func TestUpsertExternalContactTenantIsolation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	t1, t2 := uuid.New(), uuid.New()

	in := Contact{Name: "Jane", Phone: "+15550001", Source: "twenty", ExternalID: "person-1"}
	if _, err := st.UpsertExternalContact(ctx, t1, &in); err != nil {
		t.Fatal(err)
	}
	// Same phone in another tenant must create a separate row.
	other := Contact{Name: "Jane", Phone: "+15550001", Source: "twenty", ExternalID: "person-1"}
	created, err := st.UpsertExternalContact(ctx, t2, &other)
	if err != nil {
		t.Fatal(err)
	}
	if !created || other.ID == in.ID {
		t.Fatalf("cross-tenant upsert created=%v ids %s/%s", created, other.ID, in.ID)
	}
}

func TestAppendBookingCRMNote(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	offering := Offering{TenantID: tenantID, Name: "Cut", DurationMin: 30, Capacity: 1}
	if err := st.CreateOffering(ctx, &offering); err != nil {
		t.Fatal(err)
	}
	contact := Contact{TenantID: tenantID, Name: "Jane", Phone: "+1"}
	if err := st.CreateContact(ctx, &contact); err != nil {
		t.Fatal(err)
	}
	b := &Booking{
		TenantID: tenantID, OfferingID: offering.ID, ContactID: contact.ID,
		StartsAt: time.Now().UTC().Add(24 * time.Hour), EndsAt: time.Now().UTC().Add(25 * time.Hour),
		Status: StatusConfirmed, Source: "web",
	}
	if err := st.CreateBookingTx(ctx, b, SlotGuard{}, "test.topic", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}

	n1 := CRMNote{At: time.Now().UTC(), Source: "twenty", Text: "Twenty task task-7 marked DONE"}
	if err := st.AppendBookingCRMNote(ctx, tenantID, b.ID, n1); err != nil {
		t.Fatalf("append note 1: %v", err)
	}
	n2 := CRMNote{At: time.Now().UTC(), Source: "twenty", Text: "second note"}
	if err := st.AppendBookingCRMNote(ctx, tenantID, b.ID, n2); err != nil {
		t.Fatalf("append note 2: %v", err)
	}
	notes, err := st.ListBookingCRMNotes(ctx, tenantID, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 2 || notes[0].Text != n1.Text || notes[1].Text != n2.Text {
		t.Fatalf("notes = %+v", notes)
	}

	// Unknown booking -> ErrNotFound; no outbox event written for notes.
	if err := st.AppendBookingCRMNote(ctx, tenantID, uuid.New(), n1); err != ErrNotFound {
		t.Fatalf("unknown booking err = %v, want ErrNotFound", err)
	}
	var outboxRows int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&outboxRows); err != nil {
		t.Fatal(err)
	}
	if outboxRows != 1 { // only the CreateBookingTx event
		t.Fatalf("outbox rows = %d, want 1 (crm notes must not emit events)", outboxRows)
	}
}
