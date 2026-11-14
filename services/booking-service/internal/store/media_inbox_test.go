package store

// SPEC-W61 (Scope 2) media_inbox store tests — embedded Postgres
// (newTestStore, -short skips): G3 both-sides digits matching against
// UNNORMALIZED stored phones, unique-candidate anchoring, zero/ambiguous
// candidate parking (ambiguity NEVER anchors), idempotent replay, the
// parked-cap auto-discard, staff link/discard transitions, G1 erasure
// coverage (subject tombstone + tenant purge) and the G4 proof that media
// ingest NEVER writes to contacts or leads.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// newMediaInboxTestStore boots the shared harness and re-runs the media
// DDL: store.New inside newTestStore ran BEFORE the bare contacts fixture
// existed, so the to_regclass-guarded idx_contacts_phone_digits index was
// skipped — the re-run converges it (and doubles as an idempotency proof).
func newMediaInboxTestStore(t *testing.T) *Store {
	t.Helper()
	st := newTestStore(t)
	if err := st.ensureMediaInboxDDL(context.Background()); err != nil {
		t.Fatalf("ensure media_inbox ddl (idempotent re-run): %v", err)
	}
	return st
}

func miSeedContact(t *testing.T, st *Store, tenantID uuid.UUID, phone string) uuid.UUID {
	t.Helper()
	c := Contact{TenantID: tenantID, Name: "Sender", Phone: phone}
	if err := st.CreateContact(context.Background(), &c); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	return c.ID
}

func miSeedBooking(t *testing.T, st *Store, tenantID, contactID uuid.UUID, status string, startsAt time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	execTenant(t, st, tenantID,
		`INSERT INTO bookings (id, tenant_id, offering_id, contact_id, starts_at, ends_at, status, source)
		 VALUES ($2,$1,$3,$4,$5,$6,$7,'api')`,
		id, uuid.New(), contactID, startsAt, startsAt.Add(time.Hour), status)
	return id
}

func miIngestInput(tenantID uuid.UUID, msgID, rawPhone string) MediaInboxIngest {
	return MediaInboxIngest{
		TenantID:          tenantID,
		Channel:           "whatsapp",
		ProviderMessageID: msgID,
		DocumentID:        uuid.New(),
		SenderPhone:       rawPhone,
		SenderPhoneDigits: rawPhone, // gateway-computed field; the store re-derives SQL-side
		Mime:              "image/jpeg",
		Bytes:             12345,
		SHA256Prefix:      "0123456789abcdef",
	}
}

// miRawInsert inserts a media_inbox row directly (bypasses resolution) with
// an explicit age for deterministic list ordering.
func miRawInsert(t *testing.T, st *Store, tenantID uuid.UUID, msgID, state string, ageSeconds float64) {
	t.Helper()
	execTenant(t, st, tenantID,
		`INSERT INTO media_inbox (tenant_id, channel, provider_message_id, document_id, sender_phone,
		                          sender_phone_digits, mime, bytes, sha256_prefix, state, created_at)
		 VALUES ($1,'whatsapp',$2,gen_random_uuid(),'+15550000000','15550000000','image/jpeg',10,'deadbeefcafe',$3,
		         now() - make_interval(secs => $4))`,
		msgID, state, ageSeconds)
}

func miCount(t *testing.T, st *Store, tenantID uuid.UUID, where string) string {
	t.Helper()
	return queryTenant(t, st, tenantID,
		`SELECT count(*)::text FROM media_inbox WHERE tenant_id=$1 AND `+where)
}

// TestMediaInboxAnchorsOnUnnormalizedStoredPhones (G3): the contact match
// is BOTH-SIDES digits-normalized — legacy unnormalized contacts.phone rows
// match Meta's digits-only sender ids, a leading "00" strips once on either
// side, and there is NO national folding ('0801...' never matches
// '234801...').
func TestMediaInboxAnchorsOnUnnormalizedStoredPhones(t *testing.T) {
	st := newMediaInboxTestStore(t)
	ctx := context.Background()
	cases := []struct {
		name        string
		storedPhone string
		senderPhone string
		wantState   string
		wantDigits  string
	}{
		{"spaced e164 stored vs bare digits", "+234 801 234 5678", "2348012345678", "anchored", "2348012345678"},
		{"00-prefix stored vs e164 sender", "002348012345678", "+234 801 234 5678", "anchored", "2348012345678"},
		{"bare digits stored vs 00-prefix sender", "2348012345678", "002348012345678", "anchored", "2348012345678"},
		{"us parens stored vs digits", "+1 (415) 555-0132", "14155550132", "anchored", "14155550132"},
		{"no national folding", "08012345678", "2348012345678", "parked", "2348012345678"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tenantID := uuid.New()
			contactID := miSeedContact(t, st, tenantID, tc.storedPhone)
			bookingID := miSeedBooking(t, st, tenantID, contactID, "confirmed", time.Now().Add(72*time.Hour))
			in := miIngestInput(tenantID, "wamid."+tc.name, tc.senderPhone)
			in.SenderPhoneDigits = "GATEWAY-VALUE-IGNORED" // trust-but-verify: store re-derives SQL-side
			item, err := st.IngestMediaInbox(ctx, in, "")
			if err != nil {
				t.Fatalf("ingest: %v", err)
			}
			if item.State != tc.wantState {
				t.Fatalf("state %q, want %q (stored %q, sender %q)", item.State, tc.wantState, tc.storedPhone, tc.senderPhone)
			}
			if item.SenderPhone != tc.senderPhone {
				t.Fatalf("sender_phone %q rewritten, want raw %q", item.SenderPhone, tc.senderPhone)
			}
			if item.SenderPhoneDigits != tc.wantDigits {
				t.Fatalf("sender_phone_digits %q, want SQL-derived %q", item.SenderPhoneDigits, tc.wantDigits)
			}
			if tc.wantState == "anchored" {
				if item.AnchorMethod != "phone_recency" {
					t.Fatalf("anchor_method %q, want phone_recency", item.AnchorMethod)
				}
				if item.BookingID == nil || *item.BookingID != bookingID {
					t.Fatalf("booking_id %v, want %s", item.BookingID, bookingID)
				}
				if item.ContactID == nil || *item.ContactID != contactID {
					t.Fatalf("contact_id %v, want %s", item.ContactID, contactID)
				}
			} else if item.BookingID != nil || item.ContactID != nil || item.AnchorMethod != "" {
				t.Fatalf("parked row must carry no anchor: %+v", item)
			}
		})
	}
}

// TestMediaInboxParksZeroCandidates: a matching contact whose bookings all
// fall outside the status/window filter parks the row, as does having no
// contact at all.
func TestMediaInboxParksZeroCandidates(t *testing.T) {
	st := newMediaInboxTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contactID := miSeedContact(t, st, tenantID, "+15551230001")
	// Cancelled status and out-of-window starts never anchor.
	miSeedBooking(t, st, tenantID, contactID, "cancelled", time.Now().Add(24*time.Hour))
	miSeedBooking(t, st, tenantID, contactID, "confirmed", time.Now().Add(45*24*time.Hour))
	miSeedBooking(t, st, tenantID, contactID, "pending", time.Now().Add(-10*24*time.Hour))

	item, err := st.IngestMediaInbox(ctx, miIngestInput(tenantID, "wamid.zero-candidates", "15551230001"), "")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if item.State != "parked" || item.BookingID != nil || item.ContactID != nil {
		t.Fatalf("zero candidates must park: %+v", item)
	}

	// No contact at all => parked.
	item2, err := st.IngestMediaInbox(ctx, miIngestInput(tenantID, "wamid.no-contact", "15559999999"), "")
	if err != nil {
		t.Fatalf("ingest unknown: %v", err)
	}
	if item2.State != "parked" || item2.BookingID != nil {
		t.Fatalf("unknown sender must park: %+v", item2)
	}
}

// TestMediaInboxParksAmbiguousCandidates: two candidate bookings for the
// matched contact => parked. Ambiguity NEVER anchors (R1: a wrong anchor is
// media PII on the wrong customer's booking).
func TestMediaInboxParksAmbiguousCandidates(t *testing.T) {
	st := newMediaInboxTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contactID := miSeedContact(t, st, tenantID, "+15551230002")
	miSeedBooking(t, st, tenantID, contactID, "pending", time.Now().Add(24*time.Hour))
	miSeedBooking(t, st, tenantID, contactID, "confirmed", time.Now().Add(48*time.Hour))

	item, err := st.IngestMediaInbox(ctx, miIngestInput(tenantID, "wamid.ambiguous", "15551230002"), "")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if item.State != "parked" || item.BookingID != nil || item.ContactID != nil || item.AnchorMethod != "" {
		t.Fatalf("ambiguity must park without anchor: %+v", item)
	}
}

// TestMediaInboxIngestIdempotentReplay: the UNIQUE(tenant,channel,
// provider_message_id) key makes N-05 redeliveries converge — a replayed
// ingest returns the EXISTING row (same id, same state, original
// document_id) and never inserts a second row.
func TestMediaInboxIngestIdempotentReplay(t *testing.T) {
	st := newMediaInboxTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contactID := miSeedContact(t, st, tenantID, "+15551230003")
	miSeedBooking(t, st, tenantID, contactID, "confirmed", time.Now().Add(24*time.Hour))

	first, err := st.IngestMediaInbox(ctx, miIngestInput(tenantID, "wamid.replay", "15551230003"), "")
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if first.State != "anchored" {
		t.Fatalf("first state %q, want anchored", first.State)
	}
	// Redelivery with a DIFFERENT document_id must still return the
	// original row untouched.
	replay := miIngestInput(tenantID, "wamid.replay", "15551230003")
	second, err := st.IngestMediaInbox(ctx, replay, "")
	if err != nil {
		t.Fatalf("replay ingest: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("replay returned id %s, want existing %s", second.ID, first.ID)
	}
	if second.State != first.State || second.DocumentID != first.DocumentID {
		t.Fatalf("replay mutated the row: %+v vs %+v", second, first)
	}
	if got := miCount(t, st, tenantID, "TRUE"); got != "1" {
		t.Fatalf("row count %s after replay, want 1", got)
	}
}

// TestMediaInboxParkedCapAutoDiscard: with mediaInboxParkedCap rows already
// parked, one more parked ingest auto-discards the OLDEST excess row
// (state='discarded', anchor_method='auto_cap') and keeps the fresh one.
func TestMediaInboxParkedCapAutoDiscard(t *testing.T) {
	st := newMediaInboxTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	execTenant(t, st, tenantID,
		`INSERT INTO media_inbox (tenant_id, channel, provider_message_id, document_id, sender_phone,
		                          sender_phone_digits, mime, bytes, sha256_prefix, state)
		 SELECT $1, 'whatsapp', 'wamid.seed.' || g, gen_random_uuid(), '+15550000000', '15550000000',
		        'image/jpeg', 10, 'deadbeefcafe', 'parked'
		   FROM generate_series(1, $2) AS g`, mediaInboxParkedCap)
	if got := miCount(t, st, tenantID, "state='parked'"); got != "500" {
		t.Fatalf("seeded parked %s, want 500", got)
	}

	fresh, err := st.IngestMediaInbox(ctx, miIngestInput(tenantID, "wamid.fresh", "+447700900123"), "")
	if err != nil {
		t.Fatalf("ingest over cap: %v", err)
	}
	if fresh.State != "parked" {
		t.Fatalf("fresh row state %q, want parked (cap must never discard the fresh row)", fresh.State)
	}
	if got := miCount(t, st, tenantID, "TRUE"); got != "501" {
		t.Fatalf("total %s, want 501", got)
	}
	if got := miCount(t, st, tenantID, "state='parked'"); got != "500" {
		t.Fatalf("parked %s after cap, want 500", got)
	}
	if got := miCount(t, st, tenantID, "state='discarded' AND anchor_method='auto_cap'"); got != "1" {
		t.Fatalf("auto_cap discarded %s, want 1", got)
	}
	discarded := queryTenant(t, st, tenantID,
		`SELECT provider_message_id FROM media_inbox WHERE tenant_id=$1 AND state='discarded'`)
	if !strings.HasPrefix(discarded, "wamid.seed.") {
		t.Fatalf("discarded %q, want an oldest seeded row (not the fresh ingest)", discarded)
	}
}

// TestMediaInboxLinkAndDiscard covers the staff transitions: link validates
// the booking in-tenant and fills contact_id from the booking; discard
// records the actor; both report ErrNotFound for missing rows.
func TestMediaInboxLinkAndDiscard(t *testing.T) {
	st := newMediaInboxTestStore(t)
	ctx := context.Background()
	tenantID, otherTenant := uuid.New(), uuid.New()
	contactID := miSeedContact(t, st, tenantID, "+15551230004")
	bookingID := miSeedBooking(t, st, tenantID, contactID, "pending", time.Now().Add(24*time.Hour))
	otherContact := miSeedContact(t, st, otherTenant, "+15551230005")
	otherBooking := miSeedBooking(t, st, otherTenant, otherContact, "pending", time.Now().Add(24*time.Hour))

	parked, err := st.IngestMediaInbox(ctx, miIngestInput(tenantID, "wamid.link", "+49666123456"), "")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if parked.State != "parked" {
		t.Fatalf("state %q, want parked", parked.State)
	}

	// Linking to a booking of ANOTHER tenant fails and leaves the row parked.
	if _, err := st.LinkMediaInbox(ctx, tenantID, parked.ID, otherBooking, "staff-x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant link err %v, want ErrNotFound", err)
	}
	if got := queryTenant(t, st, tenantID,
		`SELECT state FROM media_inbox WHERE tenant_id=$1 AND id=$2`, parked.ID); got != "parked" {
		t.Fatalf("row mutated by failed link: state %q", got)
	}
	// Missing inbox row.
	if _, err := st.LinkMediaInbox(ctx, tenantID, uuid.New(), bookingID, "staff-x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing-row link err %v, want ErrNotFound", err)
	}

	linked, err := st.LinkMediaInbox(ctx, tenantID, parked.ID, bookingID, "staff-1")
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if linked.State != "linked" || linked.AnchorMethod != "staff" || linked.LinkedBy != "staff-1" {
		t.Fatalf("linked row: %+v", linked)
	}
	if linked.BookingID == nil || *linked.BookingID != bookingID {
		t.Fatalf("linked booking_id %v, want %s", linked.BookingID, bookingID)
	}
	if linked.ContactID == nil || *linked.ContactID != contactID {
		t.Fatalf("linked contact_id %v, want booking contact %s", linked.ContactID, contactID)
	}

	// Discard a second parked row.
	parked2, err := st.IngestMediaInbox(ctx, miIngestInput(tenantID, "wamid.discard", "+49666777777"), "")
	if err != nil {
		t.Fatalf("ingest 2: %v", err)
	}
	if err := st.DiscardMediaInbox(ctx, tenantID, parked2.ID, "staff-2"); err != nil {
		t.Fatalf("discard: %v", err)
	}
	got := queryTenant(t, st, tenantID,
		`SELECT state || '|' || linked_by FROM media_inbox WHERE tenant_id=$1 AND id=$2`, parked2.ID)
	if got != "discarded|staff-2" {
		t.Fatalf("discarded row %q, want discarded|staff-2", got)
	}
	if err := st.DiscardMediaInbox(ctx, tenantID, uuid.New(), "staff-2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing-row discard err %v, want ErrNotFound", err)
	}
}

// TestMediaInboxNeverWritesContactsOrLeads (G4, binding ruling): ingesting
// an UNKNOWN sender leaves the contacts and leads tables byte-for-byte
// untouched — the ingest path only ever writes media_inbox.
func TestMediaInboxNeverWritesContactsOrLeads(t *testing.T) {
	st := newMediaInboxTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	miSeedContact(t, st, tenantID, "+15551230006") // unrelated existing contact

	before := queryTenant(t, st, tenantID,
		`SELECT (SELECT count(*) FROM contacts WHERE tenant_id=$1)::text || '|' ||
		        (SELECT count(*) FROM leads WHERE tenant_id=$1)::text`)
	item, err := st.IngestMediaInbox(ctx, miIngestInput(tenantID, "wamid.g4", "+15557770001"), "")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if item.State != "parked" {
		t.Fatalf("unknown sender state %q, want parked", item.State)
	}
	after := queryTenant(t, st, tenantID,
		`SELECT (SELECT count(*) FROM contacts WHERE tenant_id=$1)::text || '|' ||
		        (SELECT count(*) FROM leads WHERE tenant_id=$1)::text`)
	if before != after {
		t.Fatalf("G4 violation: contacts|leads counts changed %s -> %s on unknown-sender ingest", before, after)
	}
	if before != "1|0" {
		t.Fatalf("fixture broken: counts %q, want 1|0", before)
	}
}

// TestMediaInboxListPagination: state filter ("" = all), limit/offset and
// newest-first ordering.
func TestMediaInboxListPagination(t *testing.T) {
	st := newMediaInboxTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	miRawInsert(t, st, tenantID, "wamid.oldest", "parked", 300)
	miRawInsert(t, st, tenantID, "wamid.mid-anchored", "anchored", 200)
	miRawInsert(t, st, tenantID, "wamid.mid-discarded", "discarded", 100)
	miRawInsert(t, st, tenantID, "wamid.newest", "parked", 0)

	all, err := st.ListMediaInbox(ctx, tenantID, "", 0, 0)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("list all len %d, want 4", len(all))
	}
	gotOrder := []string{all[0].ProviderMessageID, all[1].ProviderMessageID, all[2].ProviderMessageID, all[3].ProviderMessageID}
	wantOrder := []string{"wamid.newest", "wamid.mid-discarded", "wamid.mid-anchored", "wamid.oldest"}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Fatalf("order %v, want %v (newest first)", gotOrder, wantOrder)
		}
	}

	parked, err := st.ListMediaInbox(ctx, tenantID, "parked", 0, 0)
	if err != nil {
		t.Fatalf("list parked: %v", err)
	}
	if len(parked) != 2 || parked[0].ProviderMessageID != "wamid.newest" || parked[1].ProviderMessageID != "wamid.oldest" {
		t.Fatalf("parked filter: %+v", parked)
	}

	page, err := st.ListMediaInbox(ctx, tenantID, "", 1, 1)
	if err != nil {
		t.Fatalf("list page: %v", err)
	}
	if len(page) != 1 || page[0].ProviderMessageID != "wamid.mid-discarded" {
		t.Fatalf("limit/offset page: %+v", page)
	}

	// State filter never leaks across tenants.
	other, err := st.ListMediaInbox(ctx, uuid.New(), "", 0, 0)
	if err != nil {
		t.Fatalf("list other tenant: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("cross-tenant leak: %+v", other)
	}
}

// TestMediaInboxSubjectErasure (G1): AnonymizeSubject tombstones the
// media_inbox sender columns keyed on the subject phone DIGITS (needle
// derived with the same G3 normalization), preserves the anchor columns for
// audit, and is idempotent.
func TestMediaInboxSubjectErasure(t *testing.T) {
	st := newMediaInboxTestStore(t)
	ctx := context.Background()
	tenantID, otherTenant := uuid.New(), uuid.New()
	phone := "+15551234567"

	contactID := miSeedContact(t, st, tenantID, phone)
	bookingID := miSeedBooking(t, st, tenantID, contactID, "confirmed", time.Now().Add(24*time.Hour))
	// Sender formatting differs from the stored contact phone — the digits
	// key must still line up.
	item, err := st.IngestMediaInbox(ctx, miIngestInput(tenantID, "wamid.erase", "+1 (555) 123-4567"), "")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if item.State != "anchored" {
		t.Fatalf("state %q, want anchored (fixture broken)", item.State)
	}
	// Same subject, other tenant — must survive.
	if _, err := st.IngestMediaInbox(ctx, miIngestInput(otherTenant, "wamid.erase-other", "+15551234567"), ""); err != nil {
		t.Fatalf("ingest other tenant: %v", err)
	}

	ids, err := st.MediaDocumentIDsForSubject(ctx, tenantID, "15551234567")
	if err != nil {
		t.Fatalf("MediaDocumentIDsForSubject: %v", err)
	}
	if len(ids) != 1 || ids[0] != item.DocumentID.String() {
		t.Fatalf("document ids %v, want [%s]", ids, item.DocumentID)
	}

	sum, err := st.AnonymizeSubject(ctx, tenantID, phone, "")
	if err != nil {
		t.Fatalf("AnonymizeSubject: %v", err)
	}
	if sum.Details["media_inbox"] != 1 {
		t.Fatalf("media_inbox tombstoned %d, want 1 (details=%+v)", sum.Details["media_inbox"], sum.Details)
	}
	got := queryTenant(t, st, tenantID,
		`SELECT sender_phone || '|' || sender_phone_digits || '|' ||
		        coalesce(booking_id::text,'null') || '|' || coalesce(contact_id::text,'null') || '|' ||
		        document_id::text || '|' || state
		   FROM media_inbox WHERE tenant_id=$1 AND id=$2`, item.ID)
	want := hashPii(phone) + "||" + bookingID.String() + "|" + contactID.String() + "|" + item.DocumentID.String() + "|anchored"
	if got != want {
		t.Fatalf("tombstoned row %q, want %q (sender_phone salted, digits blanked, anchors preserved)", got, want)
	}
	// Idempotent: the second erase matches nothing.
	sum2, err := st.AnonymizeSubject(ctx, tenantID, phone, "")
	if err != nil {
		t.Fatalf("second AnonymizeSubject: %v", err)
	}
	if sum2.Details["media_inbox"] != 0 {
		t.Fatalf("second erase not idempotent: media_inbox=%d", sum2.Details["media_inbox"])
	}
	// Tombstoned rows no longer resolve document ids (digits blanked).
	ids2, err := st.MediaDocumentIDsForSubject(ctx, tenantID, "15551234567")
	if err != nil {
		t.Fatalf("MediaDocumentIDsForSubject post-erase: %v", err)
	}
	if len(ids2) != 0 {
		t.Fatalf("post-erase document ids %v, want none", ids2)
	}
	// Cross-tenant isolation.
	other := queryTenant(t, st, otherTenant,
		`SELECT sender_phone || '|' || sender_phone_digits FROM media_inbox WHERE tenant_id=$1`)
	if other != "+15551234567|15551234567" {
		t.Fatalf("other tenant row touched: %q", other)
	}
}

// TestMediaInboxTenantPurge (G1): DeleteTenantData hard-deletes the
// tenant's media_inbox rows and leaves other tenants untouched.
func TestMediaInboxTenantPurge(t *testing.T) {
	st := newMediaInboxTestStore(t)
	ctx := context.Background()
	tenantID, otherTenant := uuid.New(), uuid.New()
	miRawInsert(t, st, tenantID, "wamid.purge-1", "parked", 10)
	miRawInsert(t, st, tenantID, "wamid.purge-2", "anchored", 5)
	miRawInsert(t, st, otherTenant, "wamid.purge-other", "parked", 5)

	sum, err := st.DeleteTenantData(ctx, tenantID)
	if err != nil {
		t.Fatalf("DeleteTenantData: %v", err)
	}
	if sum.Details["media_inbox"] != 2 {
		t.Fatalf("media_inbox purged %d, want 2 (details=%+v)", sum.Details["media_inbox"], sum.Details)
	}
	if got := miCount(t, st, tenantID, "TRUE"); got != "0" {
		t.Fatalf("tenant rows left: %s", got)
	}
	if got := miCount(t, st, otherTenant, "TRUE"); got != "1" {
		t.Fatalf("other tenant rows touched: %s", got)
	}
	// Idempotent re-run.
	sum2, err := st.DeleteTenantData(ctx, tenantID)
	if err != nil {
		t.Fatalf("second DeleteTenantData: %v", err)
	}
	if sum2.Details["media_inbox"] != 0 {
		t.Fatalf("second purge not idempotent: %d", sum2.Details["media_inbox"])
	}
}

// TestMediaDocumentIDsForSubjectEmptyNeedle: an empty digits needle matches
// NOTHING — tombstoned rows carry sender_phone_digits='' and must never be
// re-collected for ocr erasure.
func TestMediaDocumentIDsForSubjectEmptyNeedle(t *testing.T) {
	st := newMediaInboxTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	miRawInsert(t, st, tenantID, "wamid.docs", "parked", 0)

	ids, err := st.MediaDocumentIDsForSubject(ctx, tenantID, "")
	if err != nil {
		t.Fatalf("empty needle: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("empty needle matched %v", ids)
	}
	ids, err = st.MediaDocumentIDsForSubject(ctx, tenantID, "15550000000")
	if err != nil {
		t.Fatalf("digits needle: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("digits needle found %v, want 1 row", ids)
	}
}
