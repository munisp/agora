package store

// SPEC-W45 CODER-A store tests: contact (tenant_id, phone) dedupe migration
// + booking-path upsert (item 5), the QR scan store (item 9), and the
// tenant-deletion prep helper (item 10 / K9).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The store bootstrap runs ensureContactDedupe BEFORE the test schema
// exists (a no-op); tests re-run it explicitly against seeded duplicates —
// exactly the existing-database migration path.
func TestContactDedupeMigrationAndUpsert(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	// Seed duplicates: two contacts share a phone; the booking references
	// the SECOND one.
	c1 := Contact{TenantID: tenantID, Name: "First", Phone: "+2348055550001"}
	c2 := Contact{TenantID: tenantID, Name: "Second", Phone: "+2348055550001", Email: "ada@example.com"}
	phoneless1 := Contact{TenantID: tenantID, Name: "No Phone A"}
	phoneless2 := Contact{TenantID: tenantID, Name: "No Phone B"}
	for _, c := range []*Contact{&c1, &c2, &phoneless1, &phoneless2} {
		if err := st.CreateContact(ctx, c); err != nil {
			t.Fatalf("seed contact: %v", err)
		}
	}
	offering := Offering{TenantID: tenantID, Name: "Cut", DurationMin: 30, PriceCents: 5000, Currency: "NGN"}
	if err := st.CreateOffering(ctx, &offering); err != nil {
		t.Fatal(err)
	}
	keeper, dup := c1, c2
	if c2.ID.String() < c1.ID.String() { // migration keeps MIN(id)
		keeper, dup = c2, c1
	}
	b := Booking{TenantID: tenantID, OfferingID: offering.ID, ContactID: dup.ID,
		StartsAt: time.Now().UTC().Add(48 * time.Hour), EndsAt: time.Now().UTC().Add(48*time.Hour + 30*time.Minute),
		Status: "confirmed", Source: "api"}
	if err := st.CreateBookingTx(ctx, &b, SlotGuard{}, "test.events", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}

	// Run the migration: duplicates folded, booking re-pointed, unique
	// index created.
	if err := st.ensureContactDedupe(ctx); err != nil {
		t.Fatalf("ensureContactDedupe: %v", err)
	}
	var count int
	if err := st.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM contacts WHERE tenant_id=$1`, tenantID).Scan(&count)
	}); err != nil {
		t.Fatal(err)
	}
	if count != 3 { // keeper + two phoneless rows (phone-less rows are exempt)
		t.Fatalf("contacts after dedupe = %d, want 3", count)
	}
	got, err := st.GetBooking(ctx, tenantID, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ContactID != keeper.ID {
		t.Fatalf("booking.contact_id = %v, want re-pointed to keeper %v", got.ContactID, keeper.ID)
	}
	// The UNIQUE index now rejects a duplicate (tenant, phone).
	dupContact := Contact{TenantID: tenantID, Name: "Dup", Phone: "+2348055550001"}
	if err := st.CreateContact(ctx, &dupContact); !isUniqueViolation(err) {
		t.Fatalf("duplicate insert after migration: err=%v, want unique violation", err)
	}

	// UpsertBookingContact: create-then-match on (tenant, phone). The
	// fixture email must be unique per test — reusing the seeded duplicate's
	// "ada@example.com" collides whenever c2 wins the MIN(id) keeper race
	// (email match would fold the upsert into the keeper, created=false).
	in := Contact{Name: "Ada Lovelace", Phone: "+2348055559999", Email: "ada.upsert@example.com"}
	created, err := st.UpsertBookingContact(ctx, tenantID, &in)
	if err != nil || !created {
		t.Fatalf("first upsert: created=%v err=%v", created, err)
	}
	again := Contact{Name: "Ada L. Updated", Phone: "+2348055559999"}
	created, err = st.UpsertBookingContact(ctx, tenantID, &again)
	if err != nil || created {
		t.Fatalf("second upsert: created=%v err=%v", created, err)
	}
	if again.ID != in.ID {
		t.Fatalf("upsert matched a different contact: %v vs %v", again.ID, in.ID)
	}
	if again.Name != "Ada L. Updated" || again.Email != "ada.upsert@example.com" {
		t.Fatalf("merge lost data: %+v", again)
	}
	if err := st.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM contacts WHERE tenant_id=$1`, tenantID).Scan(&count)
	}); err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Fatalf("contacts after upserts = %d, want 4 (no duplicate person)", count)
	}
}

// QR scans: insert + the analytics read (item 9).
func TestQRScanStore(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID, otherTenant := uuid.New(), uuid.New()

	for i, ref := range []string{"", "poster-a", "poster-b"} {
		sc := QRScan{TenantID: tenantID, SiteSlug: "acme-ng", Ref: ref, UserAgent: "test-agent"}
		if err := st.InsertQRScan(ctx, &sc); err != nil {
			t.Fatalf("insert scan %d: %v", i, err)
		}
		if sc.ScannedAt.IsZero() {
			t.Fatal("scanned_at not returned")
		}
	}
	// Other tenant's scans are invisible.
	if sum, err := st.ListQRScans(ctx, otherTenant, nil, nil); err != nil || sum.TotalScans != 0 || len(sum.Scans) != 0 {
		t.Fatalf("cross-tenant scans: %+v err=%v", sum, err)
	}
	sum, err := st.ListQRScans(ctx, tenantID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.TotalScans != 3 || len(sum.Scans) != 3 {
		t.Fatalf("summary: %+v", sum)
	}
	// Time filter excludes everything.
	future := time.Now().UTC().Add(time.Hour)
	if sum, err := st.ListQRScans(ctx, tenantID, &future, nil); err != nil || sum.TotalScans != 3 || len(sum.Scans) != 0 {
		t.Fatalf("filtered summary: %+v err=%v (TotalScans is the all-time total)", sum, err)
	}
}

// DeleteTenantData (item 10 / K9): contacts anonymized, open bookings
// cancelled, terminal bookings untouched, idempotent.
func TestDeleteTenantData(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	withPhone := Contact{TenantID: tenantID, Name: "Ada", Phone: "+2348066660001", Email: "ada@example.com", Notes: "vip"}
	phoneless := Contact{TenantID: tenantID, Name: "Mystery"}
	if err := st.CreateContact(ctx, &withPhone); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateContact(ctx, &phoneless); err != nil {
		t.Fatal(err)
	}
	offering := Offering{TenantID: tenantID, Name: "Cut", DurationMin: 30, PriceCents: 5000, Currency: "NGN"}
	if err := st.CreateOffering(ctx, &offering); err != nil {
		t.Fatal(err)
	}
	mkBooking := func(status string, hours int) Booking {
		b := Booking{TenantID: tenantID, OfferingID: offering.ID, ContactID: withPhone.ID,
			StartsAt: time.Now().UTC().Add(time.Duration(hours) * time.Hour),
			EndsAt:   time.Now().UTC().Add(time.Duration(hours)*time.Hour + 30*time.Minute),
			Status:   status, Source: "api"}
		if err := st.CreateBookingTx(ctx, &b, SlotGuard{}, "test.events", []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
		return b
	}
	pending := mkBooking("pending", 100)
	confirmed := mkBooking("confirmed", 110)
	completed := mkBooking("completed", 120)

	sum, err := st.DeleteTenantData(ctx, tenantID)
	if err != nil {
		t.Fatalf("DeleteTenantData: %v", err)
	}
	if sum.ContactsAnonymized != 2 || sum.BookingsCancelled != 2 {
		t.Fatalf("summary: %+v, want {2 contacts, 2 bookings}", sum)
	}
	got, err := st.GetContact(ctx, tenantID, withPhone.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "erased" || got.Notes != "" ||
		len(got.Phone) < 7 || got.Phone[:7] != "sha256:" ||
		len(got.Email) < 7 || got.Email[:7] != "sha256:" {
		t.Fatalf("contact not anonymized: %+v", got)
	}
	for id, want := range map[uuid.UUID]string{pending.ID: "cancelled", confirmed.ID: "cancelled", completed.ID: "completed"} {
		b, err := st.GetBooking(ctx, tenantID, id)
		if err != nil {
			t.Fatal(err)
		}
		if b.Status != want {
			t.Fatalf("booking %v status = %q, want %q", id, b.Status, want)
		}
	}
	// Idempotent: a cascade redelivery changes nothing.
	sum2, err := st.DeleteTenantData(ctx, tenantID)
	if err != nil || sum2.ContactsAnonymized != 0 || sum2.BookingsCancelled != 0 {
		t.Fatalf("second run: %+v err=%v, want all-zero", sum2, err)
	}

	// Slug wrapper: resolves via the sites registry (unpublished included).
	slugTenant := uuid.New()
	site := Site{TenantID: slugTenant, TenantSlug: "gone-ng", Slug: "gone-ng", DisplayName: "Gone"}
	if err := st.CreateSite(ctx, &site); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteTenantDataBySlug(ctx, "gone-ng"); err != nil {
		t.Fatalf("DeleteTenantDataBySlug: %v", err)
	}
	if _, err := st.DeleteTenantDataBySlug(ctx, "never-existed"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown slug: err=%v, want ErrNotFound", err)
	}
}
