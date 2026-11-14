package store

// SPEC-W66 WS2 tests: the caption BK-ref hint on media_inbox — corroborate,
// narrow, contradict-park, zero-candidate G4 proof, case normalization,
// malformed-as-absent, replay-unchanged, the P8 no-caption-column DB grep
// and the G7 erasure NULL. Embedded Postgres (newTestStore; -short skips).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// miSetPublicRef pins a booking's public_ref (the W51 column is added by
// the ensure* re-run in newTestStore).
func miSetPublicRef(t *testing.T, st *Store, tenantID, bookingID uuid.UUID, ref string) {
	t.Helper()
	execTenant(t, st, tenantID,
		`UPDATE bookings SET public_ref=$3 WHERE tenant_id=$1 AND id=$2`, bookingID, ref)
}

// miIngestHint builds an ingest input carrying a caption-extracted hint.
func miIngestHint(tenantID uuid.UUID, msgID, rawPhone, hint string) MediaInboxIngest {
	in := miIngestInput(tenantID, msgID, rawPhone)
	in.BookingRefHint = hint
	return in
}

// miHintRow reads back the row's hint-bearing projection.
func miHintRow(t *testing.T, st *Store, tenantID uuid.UUID, msgID string) string {
	t.Helper()
	return queryTenant(t, st, tenantID,
		`SELECT state || '|' || coalesce(anchor_method,'') || '|' ||
		        coalesce(booking_id::text,'') || '|' || coalesce(booking_ref_hint,'')
		   FROM media_inbox WHERE tenant_id=$1 AND provider_message_id=$2`, msgID)
}

// Case 1: the hint CORROBORATES the single phone candidate => anchored,
// anchor_method 'phone_recency' UNCHANGED, hint recorded on the row.
func TestMediaInboxHintCorroboratesUniqueCandidate(t *testing.T) {
	st := newMediaInboxTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contactID := miSeedContact(t, st, tenantID, "+15551231001")
	bookingID := miSeedBooking(t, st, tenantID, contactID, "confirmed", time.Now().Add(24*time.Hour))
	miSetPublicRef(t, st, tenantID, bookingID, "BK-C0RR0001")

	item, err := st.IngestMediaInbox(ctx, miIngestHint(tenantID, "wamid.w66-corroborate", "15551231001", "BK-C0RR0001"), "")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if item.State != "anchored" || item.AnchorMethod != "phone_recency" {
		t.Fatalf("corroborated single candidate must anchor as phone_recency, got %+v", item)
	}
	if item.BookingID == nil || *item.BookingID != bookingID {
		t.Fatalf("booking_id %v, want %s", item.BookingID, bookingID)
	}
	if item.BookingRefHint != "BK-C0RR0001" {
		t.Fatalf("hint must be recorded, got %q", item.BookingRefHint)
	}
	if got := miHintRow(t, st, tenantID, "wamid.w66-corroborate"); got != "anchored|phone_recency|"+bookingID.String()+"|BK-C0RR0001" {
		t.Fatalf("stored row %q", got)
	}
}

// Case 2: the hint NARROWS 2+ phone candidates to exactly one => anchored,
// anchor_method 'phone_recency+ref_hint'.
func TestMediaInboxHintNarrowsAmbiguousCandidates(t *testing.T) {
	st := newMediaInboxTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contactID := miSeedContact(t, st, tenantID, "+15551231002")
	b1 := miSeedBooking(t, st, tenantID, contactID, "pending", time.Now().Add(24*time.Hour))
	b2 := miSeedBooking(t, st, tenantID, contactID, "confirmed", time.Now().Add(48*time.Hour))
	miSetPublicRef(t, st, tenantID, b1, "BK-NARR0W01")
	miSetPublicRef(t, st, tenantID, b2, "BK-NARR0W02")

	item, err := st.IngestMediaInbox(ctx, miIngestHint(tenantID, "wamid.w66-narrow", "15551231002", "BK-NARR0W02"), "")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if item.State != "anchored" || item.AnchorMethod != "phone_recency+ref_hint" {
		t.Fatalf("narrowed candidate must anchor as phone_recency+ref_hint, got %+v", item)
	}
	if item.BookingID == nil || *item.BookingID != b2 {
		t.Fatalf("booking_id %v, want the hint-matched %s", item.BookingID, b2)
	}
	if item.BookingRefHint != "BK-NARR0W02" {
		t.Fatalf("hint must be recorded, got %q", item.BookingRefHint)
	}
}

// Case 3: the hint CONTRADICTS the phone candidate(s) => PARKED,
// anchor_method 'ref_hint_mismatch', booking NOT touched. Covered for the
// single-candidate and the multi-candidate shapes.
func TestMediaInboxHintContradictsParks(t *testing.T) {
	st := newMediaInboxTestStore(t)
	ctx := context.Background()

	t.Run("single candidate", func(t *testing.T) {
		tenantID := uuid.New()
		contactID := miSeedContact(t, st, tenantID, "+15551231003")
		bookingID := miSeedBooking(t, st, tenantID, contactID, "confirmed", time.Now().Add(24*time.Hour))
		miSetPublicRef(t, st, tenantID, bookingID, "BK-C0NTRAD1")
		bookingBefore := queryTenant(t, st, tenantID,
			`SELECT count(*)::text || '|' || string_agg(status, ',' ORDER BY id) FROM bookings WHERE tenant_id=$1`)

		item, err := st.IngestMediaInbox(ctx, miIngestHint(tenantID, "wamid.w66-contra1", "15551231003", "BK-0THERREF"), "")
		if err != nil {
			t.Fatalf("ingest: %v", err)
		}
		if item.State != "parked" || item.AnchorMethod != "ref_hint_mismatch" {
			t.Fatalf("contradicting hint must park with ref_hint_mismatch, got %+v", item)
		}
		if item.BookingID != nil || item.ContactID != nil {
			t.Fatalf("mismatch park must not set anchor columns: %+v", item)
		}
		if item.BookingRefHint != "BK-0THERREF" {
			t.Fatalf("hint must be recorded on the parked row, got %q", item.BookingRefHint)
		}
		bookingAfter := queryTenant(t, st, tenantID,
			`SELECT count(*)::text || '|' || string_agg(status, ',' ORDER BY id) FROM bookings WHERE tenant_id=$1`)
		if bookingBefore != bookingAfter {
			t.Fatalf("booking row touched: %s -> %s", bookingBefore, bookingAfter)
		}
	})

	t.Run("multiple candidates hint matches none", func(t *testing.T) {
		tenantID := uuid.New()
		contactID := miSeedContact(t, st, tenantID, "+15551231004")
		b1 := miSeedBooking(t, st, tenantID, contactID, "pending", time.Now().Add(24*time.Hour))
		b2 := miSeedBooking(t, st, tenantID, contactID, "confirmed", time.Now().Add(48*time.Hour))
		miSetPublicRef(t, st, tenantID, b1, "BK-MVT100AA")
		miSetPublicRef(t, st, tenantID, b2, "BK-MVT100AB")

		item, err := st.IngestMediaInbox(ctx, miIngestHint(tenantID, "wamid.w66-contra2", "15551231004", "BK-N0NE0FEM"), "")
		if err != nil {
			t.Fatalf("ingest: %v", err)
		}
		if item.State != "parked" || item.AnchorMethod != "ref_hint_mismatch" || item.BookingID != nil {
			t.Fatalf("hint matching none of >1 candidates must park+flag, got %+v", item)
		}
	})
}

// Case 4 (G4 proof): a hint with ZERO phone candidates parks the row with
// the hint recorded — and NEVER creates contacts, leads or bookings.
func TestMediaInboxHintZeroCandidatesNeverCreates(t *testing.T) {
	st := newMediaInboxTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	miSeedContact(t, st, tenantID, "+15551231005") // unrelated existing contact
	miSeedBooking(t, st, tenantID, miSeedContact(t, st, tenantID, "+15551231006"), "pending", time.Now().Add(24*time.Hour))

	before := queryTenant(t, st, tenantID,
		`SELECT (SELECT count(*) FROM contacts WHERE tenant_id=$1)::text || '|' ||
		        (SELECT count(*) FROM leads WHERE tenant_id=$1)::text || '|' ||
		        (SELECT count(*) FROM bookings WHERE tenant_id=$1)::text`)
	item, err := st.IngestMediaInbox(ctx, miIngestHint(tenantID, "wamid.w66-zero", "+15559998877", "BK-ZER0CAND"), "")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if item.State != "parked" || item.AnchorMethod != "" || item.BookingID != nil {
		t.Fatalf("zero candidates must park WITHOUT a method flag, got %+v", item)
	}
	if item.BookingRefHint != "BK-ZER0CAND" {
		t.Fatalf("hint must be recorded even with zero candidates, got %q", item.BookingRefHint)
	}
	after := queryTenant(t, st, tenantID,
		`SELECT (SELECT count(*) FROM contacts WHERE tenant_id=$1)::text || '|' ||
		        (SELECT count(*) FROM leads WHERE tenant_id=$1)::text || '|' ||
		        (SELECT count(*) FROM bookings WHERE tenant_id=$1)::text`)
	if before != after {
		t.Fatalf("G4 violation: contacts|leads|bookings changed %s -> %s", before, after)
	}
	if before != "2|0|1" {
		t.Fatalf("fixture broken: counts %q, want 2|0|1", before)
	}
}

// Case 5 (no-hint byte-identical): without a hint the W61 behavior is
// unchanged — unique candidate anchors 'phone_recency', ambiguity parks
// WITHOUT a flag, and booking_ref_hint is NULL.
func TestMediaInboxNoHintByteIdentical(t *testing.T) {
	st := newMediaInboxTestStore(t)
	ctx := context.Background()

	tenantID := uuid.New()
	contactID := miSeedContact(t, st, tenantID, "+15551231007")
	bookingID := miSeedBooking(t, st, tenantID, contactID, "confirmed", time.Now().Add(24*time.Hour))
	miSetPublicRef(t, st, tenantID, bookingID, "BK-N0H1NT01")

	item, err := st.IngestMediaInbox(ctx, miIngestInput(tenantID, "wamid.w66-nohint", "15551231007"), "")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if item.State != "anchored" || item.AnchorMethod != "phone_recency" || item.BookingRefHint != "" {
		t.Fatalf("no-hint must be byte-identical W61: %+v", item)
	}
	if got := miHintRow(t, st, tenantID, "wamid.w66-nohint"); !strings.HasSuffix(got, "|") {
		t.Fatalf("booking_ref_hint must be NULL without a hint, row %q", got)
	}

	// Ambiguity without a hint still parks WITHOUT an anchor_method flag.
	tenant2 := uuid.New()
	c2 := miSeedContact(t, st, tenant2, "+15551231008")
	miSeedBooking(t, st, tenant2, c2, "pending", time.Now().Add(24*time.Hour))
	miSeedBooking(t, st, tenant2, c2, "confirmed", time.Now().Add(48*time.Hour))
	item2, err := st.IngestMediaInbox(ctx, miIngestInput(tenant2, "wamid.w66-nohint-amb", "15551231008"), "")
	if err != nil {
		t.Fatalf("ingest 2: %v", err)
	}
	if item2.State != "parked" || item2.AnchorMethod != "" || item2.BookingRefHint != "" {
		t.Fatalf("ambiguous no-hint must park unflagged (W61): %+v", item2)
	}
}

// Case 6: a lowercase / mixed-case hint is uppercase-normalized before
// matching and storage.
func TestMediaInboxHintCaseNormalization(t *testing.T) {
	st := newMediaInboxTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contactID := miSeedContact(t, st, tenantID, "+15551231009")
	bookingID := miSeedBooking(t, st, tenantID, contactID, "confirmed", time.Now().Add(24*time.Hour))
	miSetPublicRef(t, st, tenantID, bookingID, "BK-CASE0N01")

	item, err := st.IngestMediaInbox(ctx, miIngestHint(tenantID, "wamid.w66-case", "15551231009", "bk-case0n01"), "")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if item.State != "anchored" || item.AnchorMethod != "phone_recency" {
		t.Fatalf("normalized hint must corroborate, got %+v", item)
	}
	if item.BookingRefHint != "BK-CASE0N01" {
		t.Fatalf("hint must be stored uppercase-normalized, got %q", item.BookingRefHint)
	}
}

// Case 8: a malformed hint (bad charset, wrong length, junk) is treated as
// ABSENT — never an error, never stored, and resolution falls back to the
// no-hint W61 behavior.
func TestMediaInboxHintMalformedTreatedAbsent(t *testing.T) {
	st := newMediaInboxTestStore(t)
	ctx := context.Background()
	for i, bad := range []string{
		"BK-7C3X9Q2U", // excluded Crockford letter U
		"BK-7C3X9Q2I", // excluded I
		"BK-7C3X9Q2L", // excluded L
		"BK-7C3X9Q2O", // excluded O
		"BK-SHORT",    // too short
		"BK-",         // prefix only
		"XX-7C3X9Q2M", // wrong prefix
		"BK7C3X9Q2M",  // no dash
		"not a ref at all",
		strings.Repeat("B", 65), // absurd length
	} {
		tenantID := uuid.New()
		contactID := miSeedContact(t, st, tenantID, "+15551232001")
		bookingID := miSeedBooking(t, st, tenantID, contactID, "confirmed", time.Now().Add(24*time.Hour))
		miSetPublicRef(t, st, tenantID, bookingID, "BK-MAVF0RM1")

		item, err := st.IngestMediaInbox(ctx, miIngestHint(tenantID, "wamid.w66-malformed-"+string(rune('a'+i)), "15551232001", bad), "")
		if err != nil {
			t.Fatalf("hint %q: malformed must never error, got %v", bad, err)
		}
		if item.State != "anchored" || item.AnchorMethod != "phone_recency" {
			t.Fatalf("hint %q: must fall back to no-hint resolution, got %+v", bad, item)
		}
		if item.BookingRefHint != "" {
			t.Fatalf("hint %q: malformed must NOT be stored, got %q", bad, item.BookingRefHint)
		}
	}
}

// Case 10: replay (ON CONFLICT) returns the EXISTING row unchanged — a
// replayed ingest carrying a DIFFERENT hint neither re-resolves nor
// rewrites the stored hint.
func TestMediaInboxHintReplayUnchanged(t *testing.T) {
	st := newMediaInboxTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contactID := miSeedContact(t, st, tenantID, "+15551231010")
	bookingID := miSeedBooking(t, st, tenantID, contactID, "confirmed", time.Now().Add(24*time.Hour))
	miSetPublicRef(t, st, tenantID, bookingID, "BK-REP1AY01")

	// First ingest: hint CONTRADICTS => parked + ref_hint_mismatch.
	first, err := st.IngestMediaInbox(ctx, miIngestHint(tenantID, "wamid.w66-replay", "15551231010", "BK-WR0NGREF"), "")
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if first.State != "parked" || first.AnchorMethod != "ref_hint_mismatch" {
		t.Fatalf("first ingest fixture broken: %+v", first)
	}
	// Replay with the CORRECT hint must NOT re-resolve or mutate the row.
	second, err := st.IngestMediaInbox(ctx, miIngestHint(tenantID, "wamid.w66-replay", "15551231010", "BK-REP1AY01"), "")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if second.ID != first.ID || second.State != first.State || second.AnchorMethod != first.AnchorMethod {
		t.Fatalf("replay mutated the row: %+v vs %+v", second, first)
	}
	if second.BookingRefHint != "BK-WR0NGREF" {
		t.Fatalf("replayed hint must be ignored (original kept), got %q", second.BookingRefHint)
	}
	if second.BookingID != nil {
		t.Fatalf("replay must never anchor after the fact: %+v", second)
	}
	if got := miCount(t, st, tenantID, "TRUE"); got != "1" {
		t.Fatalf("row count %s after replay, want 1", got)
	}
}

// Case 11 (P8, DB grep-style proof): the media_inbox schema has NO caption
// column — the caption string cannot be persisted even by accident.
func TestMediaInboxNoCaptionColumn(t *testing.T) {
	st := newMediaInboxTestStore(t)
	got := queryTenant(t, st, uuid.New(),
		`SELECT count(*)::text FROM information_schema.columns
		  WHERE table_name='media_inbox' AND column_name ILIKE '%caption%' AND $1::uuid IS NOT NULL`)
	if got != "0" {
		t.Fatalf("media_inbox must have NO caption column (P8), found %s", got)
	}
	// Sanity: the hint column DOES exist.
	got = queryTenant(t, st, uuid.New(),
		`SELECT count(*)::text FROM information_schema.columns
		  WHERE table_name='media_inbox' AND column_name='booking_ref_hint' AND $1::uuid IS NOT NULL`)
	if got != "1" {
		t.Fatalf("booking_ref_hint column missing, count %s", got)
	}
}

// Case 12 (G7): AnonymizeSubject NULLs booking_ref_hint; booking_id and
// the other anchor columns stay for audit; the step stays idempotent.
func TestMediaInboxHintErasureNulls(t *testing.T) {
	st := newMediaInboxTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	phone := "+15551231011"

	contactID := miSeedContact(t, st, tenantID, phone)
	bookingID := miSeedBooking(t, st, tenantID, contactID, "confirmed", time.Now().Add(24*time.Hour))
	miSetPublicRef(t, st, tenantID, bookingID, "BK-ERAS0RE1")
	item, err := st.IngestMediaInbox(ctx, miIngestHint(tenantID, "wamid.w66-erasure", "15551231011", "BK-ERAS0RE1"), "")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if item.State != "anchored" || item.BookingRefHint != "BK-ERAS0RE1" {
		t.Fatalf("fixture broken: %+v", item)
	}

	sum, err := st.AnonymizeSubject(ctx, tenantID, phone, "")
	if err != nil {
		t.Fatalf("AnonymizeSubject: %v", err)
	}
	if sum.Details["media_inbox"] != 1 {
		t.Fatalf("media_inbox tombstoned %d, want 1", sum.Details["media_inbox"])
	}
	got := queryTenant(t, st, tenantID,
		`SELECT coalesce(booking_ref_hint,'<NULL>') || '|' || coalesce(booking_id::text,'null') || '|' || state
		   FROM media_inbox WHERE tenant_id=$1 AND id=$2`, item.ID)
	want := "<NULL>|" + bookingID.String() + "|anchored"
	if got != want {
		t.Fatalf("post-erasure row %q, want %q (hint NULLed, booking kept for audit)", got, want)
	}
	// Idempotent: second erase matches nothing.
	sum2, err := st.AnonymizeSubject(ctx, tenantID, phone, "")
	if err != nil {
		t.Fatalf("second AnonymizeSubject: %v", err)
	}
	if sum2.Details["media_inbox"] != 0 {
		t.Fatalf("second erase not idempotent: %d", sum2.Details["media_inbox"])
	}
}

// Case 7 companion (store-side): the hint NEVER anchors without phone
// corroboration even when the ref exists in the tenant — a sender whose
// phone matches NO booking contact but who quotes a VALID tenant ref still
// parks (G6).
func TestMediaInboxHintNeverAnchorsWithoutPhoneMatch(t *testing.T) {
	st := newMediaInboxTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contactID := miSeedContact(t, st, tenantID, "+15551231012")
	bookingID := miSeedBooking(t, st, tenantID, contactID, "confirmed", time.Now().Add(24*time.Hour))
	miSetPublicRef(t, st, tenantID, bookingID, "BK-REA1REF1")

	// A DIFFERENT sender quotes the real ref: phone matches no candidate.
	item, err := st.IngestMediaInbox(ctx, miIngestHint(tenantID, "wamid.w66-nocorro", "+15559990001", "BK-REA1REF1"), "")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if item.State != "parked" || item.BookingID != nil || item.AnchorMethod != "" {
		t.Fatalf("hint must NEVER anchor without phone corroboration, got %+v", item)
	}
	if item.BookingRefHint != "BK-REA1REF1" {
		t.Fatalf("hint recorded on the parked row, got %q", item.BookingRefHint)
	}
}
