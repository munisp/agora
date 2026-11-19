package campaignstudio

// SPEC-W92 B13: studio segment + event triggers.
// Redirections:
//   - removing the segment evaluation from FireSegmentJourney -> the
//     member set is wrong (zero enrollments) and TestW92FireSegment fails.
//   - loosening the event match (no event_name predicate) -> the
//     other-event active journey enrolls and TestW92FireEvent fails.
//   - dropping the active-only predicate -> the draft journey enrolls.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestW92FireSegmentJourney(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	// 3 matching contacts + 1 non-matching + 1 cross-tenant decoy.
	in1 := seedContact(t, st, tenantID, "Ada Lovelace", "+2348011111111", "ada@example.com", "twenty")
	in2 := seedContact(t, st, tenantID, "Alan Turing", "+2348033333333", "alan@example.com", "twenty")
	in3 := seedContact(t, st, tenantID, "No Phone", "", "nophone@example.com", "twenty")
	seedContact(t, st, tenantID, "Grace Hopper", "+2348022222222", "grace@example.com", "field")
	seedContact(t, st, uuid.New(), "Decoy", "+2348099999999", "decoy@example.com", "twenty")

	seg := Segment{TenantID: tenantID, Name: "twenty-source", Definition: SegmentDefinition{
		Filters: []SegmentFilter{{Field: FieldSource, Op: OpEq, Value: "twenty"}},
	}}
	if err := st.CreateSegment(ctx, &seg); err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}

	j := Journey{TenantID: tenantID, Name: "onboard", TriggerKind: TriggerSegment, SegmentID: &seg.ID,
		Steps: Steps{{Type: StepSend, Kind: KindSMS, Template: "hi"}}}
	if err := st.CreateJourney(ctx, &j); err != nil {
		t.Fatalf("CreateJourney: %v", err)
	}

	// Draft journey refuses to fire (409 at the API).
	if _, _, _, err := st.FireSegmentJourney(ctx, tenantID, j.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("draft fire: err = %v, want ErrConflict", err)
	}
	if _, err := st.TransitionJourney(ctx, tenantID, j.ID, StatusActive); err != nil {
		t.Fatalf("activate: %v", err)
	}

	// First fire enrolls exactly the 3 matching contacts.
	created, existing, truncated, err := st.FireSegmentJourney(ctx, tenantID, j.ID)
	if err != nil {
		t.Fatalf("fire: %v", err)
	}
	if truncated {
		t.Fatal("small fixture must not truncate")
	}
	if len(created) != 3 || existing != 0 {
		t.Fatalf("first fire: created=%d existing=%d, want 3/0", len(created), existing)
	}
	got := map[uuid.UUID]bool{}
	for _, e := range created {
		got[e.ContactID] = true
	}
	for _, want := range []uuid.UUID{in1, in2, in3} {
		if !got[want] {
			t.Fatalf("member %s not enrolled: %v", want, got)
		}
	}

	// Re-fire is idempotent — a scheduled re-fire enrolls only NEW members.
	newbie := seedContact(t, st, tenantID, "New Member", "+2348055555555", "new@example.com", "twenty")
	created, existing, truncated, err = st.FireSegmentJourney(ctx, tenantID, j.ID)
	if err != nil {
		t.Fatalf("re-fire: %v", err)
	}
	if truncated || len(created) != 1 || created[0].ContactID != newbie || existing != 3 {
		t.Fatalf("re-fire: created=%v existing=%d truncated=%v, want [newbie]/3/false", created, existing, truncated)
	}

	// A journey WITHOUT a bound segment refuses (409), even when active.
	j2 := Journey{TenantID: tenantID, Name: "manual", TriggerKind: TriggerManual,
		Steps: Steps{{Type: StepSend, Kind: KindSMS, Template: "hi"}}}
	if err := st.CreateJourney(ctx, &j2); err != nil {
		t.Fatalf("CreateJourney 2: %v", err)
	}
	if _, err := st.TransitionJourney(ctx, tenantID, j2.ID, StatusActive); err != nil {
		t.Fatalf("activate 2: %v", err)
	}
	if _, _, _, err := st.FireSegmentJourney(ctx, tenantID, j2.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("no-segment fire: err = %v, want ErrConflict", err)
	}
}

func TestW92FireEventJourneys(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contactID := seedContact(t, st, tenantID, "Event Customer", "+2348066666666", "ev@example.com", "web")

	mk := func(name, trigger, eventName string, activate bool) Journey {
		j := Journey{TenantID: tenantID, Name: name, TriggerKind: trigger, EventName: eventName,
			Steps: Steps{{Type: StepSend, Kind: KindSMS, Template: "hi"}}}
		if err := st.CreateJourney(ctx, &j); err != nil {
			t.Fatalf("CreateJourney %s: %v", name, err)
		}
		if activate {
			if _, err := st.TransitionJourney(ctx, tenantID, j.ID, StatusActive); err != nil {
				t.Fatalf("activate %s: %v", name, err)
			}
		}
		return j
	}
	match1 := mk("booking-completed-a", TriggerEvent, "booking_completed", true)
	match2 := mk("booking-completed-b", TriggerEvent, "booking_completed", true)
	mk("other-event", TriggerEvent, "ticket_resolved", true)       // wrong event
	mk("draft-listener", TriggerEvent, "booking_completed", false) // not active

	results, created, err := st.FireEventJourneys(ctx, tenantID, "booking_completed", contactID)
	if err != nil {
		t.Fatalf("fire event: %v", err)
	}
	if len(results) != 2 || len(created) != 2 {
		t.Fatalf("results = %+v created=%d, want 2/2", results, len(created))
	}
	enrolled := map[uuid.UUID]bool{}
	for _, r := range results {
		if !r.Enrolled {
			t.Fatalf("first fire must enroll: %+v", r)
		}
		enrolled[r.JourneyID] = true
	}
	if !enrolled[match1.ID] || !enrolled[match2.ID] {
		t.Fatalf("matched journeys = %v, want %s + %s", enrolled, match1.ID, match2.ID)
	}

	// Replay: same event re-fires nothing (idempotent, meters nothing).
	results, created, err = st.FireEventJourneys(ctx, tenantID, "booking_completed", contactID)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(results) != 2 || len(created) != 0 {
		t.Fatalf("replay results = %+v created=%d, want 2/0", results, len(created))
	}
	for _, r := range results {
		if r.Enrolled {
			t.Fatalf("replay must not re-enroll: %+v", r)
		}
	}

	// Zero-match event: valid empty result, never an error.
	results, created, err = st.FireEventJourneys(ctx, tenantID, "no_such_event", contactID)
	if err != nil || len(results) != 0 || len(created) != 0 {
		t.Fatalf("zero-match: %+v/%v/%v", results, created, err)
	}
}

func TestW92JourneyEventNameRoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	// event_name persists through create + patch; the ALTER ... ADD COLUMN
	// keeps pre-W92 rows at ''.
	j := Journey{TenantID: tenantID, Name: "evt", TriggerKind: TriggerEvent, EventName: "booking_completed",
		Steps: Steps{{Type: StepSend, Kind: KindSMS, Template: "hi"}}}
	if err := st.CreateJourney(ctx, &j); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := st.GetJourney(ctx, tenantID, j.ID)
	if err != nil || got.EventName != "booking_completed" {
		t.Fatalf("round-trip: %+v, %v", got, err)
	}
	newName := "ticket_resolved"
	got, err = st.UpdateJourney(ctx, tenantID, j.ID, nil, nil, nil, &newName, nil)
	if err != nil || got.EventName != "ticket_resolved" {
		t.Fatalf("patch: %+v, %v", got, err)
	}
	// nil leaves the column untouched.
	got, err = st.UpdateJourney(ctx, tenantID, j.ID, nil, nil, nil, nil, nil)
	if err != nil || got.EventName != "ticket_resolved" {
		t.Fatalf("nil patch clobbered event_name: %+v, %v", got, err)
	}
}
