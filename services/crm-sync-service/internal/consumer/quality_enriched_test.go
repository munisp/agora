package consumer

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/crm-sync-service/internal/events"
)

// CallQualityEnriched (Wave 5 #2): sentiment-enriched note path, merge +
// dedupe against the plain SessionEnded fallback via sync_map
// kind=quality_note. Stubs live in session_ended_test.go (same package).

func callQualityEnrichedEvent(data map[string]any) events.CloudEvent {
	evt := sessionEndedEvent(data)
	evt.Type = events.TypeCallQualityEnriched
	evt.Source = "conversation-service"
	return evt
}

func enrichedData(phone string, avgSentiment float64, scoredTurns int) map[string]any {
	return map[string]any{
		"conversationId":       "conv-enriched-1",
		"channel":              "voice",
		"siteSlug":             "acme-salon",
		"quality":              qualityData(phone),
		"avg_sentiment":        avgSentiment,
		"turn_sentiment_count": scoredTurns,
	}
}

func notePatches(stub *callQualityStub) []stubReq {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	var out []stubReq
	for _, r := range stub.requests {
		if r.method == http.MethodPatch && strings.HasPrefix(r.path, "/rest/notes/") {
			out = append(out, r)
		}
	}
	return out
}

func TestCallQualityEnrichedCreatesNoteWithSentiment(t *testing.T) {
	s, fm, stub := newCallQualitySyncer(t, map[string]string{"+1555000111": "person-77"})
	evt := callQualityEnrichedEvent(enrichedData("+1555000111", 0.42, 5))
	if err := s.HandleQuality(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	notes, links := stub.notes()
	if len(notes) != 1 {
		t.Fatalf("expected 1 note, got %d; requests=%+v", len(notes), stub.requests)
	}
	body, _ := noteText(notes[0].body), true
	for _, want := range []string{
		"- Call length: 1 min 35 s",
		"Caller's mood: Positive",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("note body missing %q:\n%s", want, body)
		}
	}
	if len(links) != 1 || links[0].body["personId"] != "person-77" {
		t.Errorf("noteTarget link = %+v", links)
	}
	// the mapping lets a late plain SessionEnded dedupe instead of duplicating
	m, err := fm.Get(context.Background(), KindQualityNote, "conv-enriched-1", parseUUID(evt.TenantID))
	if err != nil {
		t.Fatalf("quality_note mapping missing: %v", err)
	}
	if m.TwentyID == "" {
		t.Error("quality_note mapping has empty twenty id")
	}
}

func TestCallQualityEnrichedPatchesExistingFallbackNote(t *testing.T) {
	// Eventual-consistency merge: plain SessionEnded lands first and creates
	// the note without sentiment; the enriched event then PATCHes the same
	// note instead of creating a second one.
	s, fm, stub := newCallQualitySyncer(t, map[string]string{"+1555000111": "person-77"})
	plain := sessionEndedEvent(map[string]any{
		"conversationId": "conv-enriched-2",
		"channel":        "voice",
		"siteSlug":       "acme-salon",
		"quality":        qualityData("+1555000111"),
	})
	if err := s.HandleConversation(context.Background(), plain); err != nil {
		t.Fatal(err)
	}
	notes, _ := stub.notes()
	if len(notes) != 1 {
		t.Fatalf("fallback should create 1 note, got %d", len(notes))
	}
	if body, _ := noteText(notes[0].body), true; strings.Contains(body, "sentiment") {
		t.Fatalf("fallback note must not contain sentiment yet:\n%s", body)
	}

	enriched := callQualityEnrichedEvent(enrichedData("+1555000111", -0.35, 4))
	enriched.Data["conversationId"] = "conv-enriched-2"
	enriched.TenantID = plain.TenantID
	if err := s.HandleQuality(context.Background(), enriched); err != nil {
		t.Fatal(err)
	}
	notes, _ = stub.notes()
	if len(notes) != 1 {
		t.Fatalf("enriched event must not create a second note, got %d", len(notes))
	}
	patches := notePatches(stub)
	if len(patches) != 1 {
		t.Fatalf("expected 1 note patch, got %d; requests=%+v", len(patches), stub.requests)
	}
	body, _ := noteText(patches[0].body), true
	if !strings.Contains(body, "Caller's mood: Negative") {
		t.Errorf("patched body missing sentiment:\n%s", body)
	}
	// mapping still points at the same note
	if _, err := fm.Get(context.Background(), KindQualityNote, "conv-enriched-2", parseUUID(plain.TenantID)); err != nil {
		t.Fatalf("quality_note mapping missing: %v", err)
	}
}

func TestPlainSessionEndedSkippedAfterEnrichedNote(t *testing.T) {
	// Reverse ordering: enriched event arrives first and creates the note;
	// the late plain SessionEnded must NOT create a duplicate.
	s, _, stub := newCallQualitySyncer(t, map[string]string{"+1555000111": "person-77"})
	enriched := callQualityEnrichedEvent(enrichedData("+1555000111", 0.9, 6))
	enriched.Data["conversationId"] = "conv-enriched-3"
	if err := s.HandleQuality(context.Background(), enriched); err != nil {
		t.Fatal(err)
	}
	plain := sessionEndedEvent(map[string]any{
		"conversationId": "conv-enriched-3",
		"channel":        "voice",
		"siteSlug":       "acme-salon",
		"quality":        qualityData("+1555000111"),
	})
	plain.TenantID = enriched.TenantID
	if err := s.HandleConversation(context.Background(), plain); err != nil {
		t.Fatal(err)
	}
	notes, _ := stub.notes()
	if len(notes) != 1 {
		t.Fatalf("expected exactly 1 note across both events, got %d", len(notes))
	}
	body, _ := noteText(notes[0].body), true
	if !strings.Contains(body, "Caller's mood: Positive") {
		t.Errorf("enriched-created note missing sentiment:\n%s", body)
	}
}

func TestCallQualityEnrichedWithoutQualityIsAcked(t *testing.T) {
	s, _, stub := newCallQualitySyncer(t, map[string]string{"+1555000111": "person-77"})
	evt := callQualityEnrichedEvent(map[string]any{
		"conversationId": "conv-enriched-4",
		"avg_sentiment":  0.5,
	})
	if err := s.HandleQuality(context.Background(), evt); err != nil {
		t.Fatalf("missing quality must be acked, got %v", err)
	}
	if len(stub.requests) != 0 {
		t.Fatalf("no Twenty calls expected without quality; requests=%+v", stub.requests)
	}
}

func TestCallQualityEnrichedWithoutConfirmedPhoneIsAcked(t *testing.T) {
	s, _, stub := newCallQualitySyncer(t, map[string]string{"+1555000111": "person-77"})
	evt := callQualityEnrichedEvent(enrichedData("", 0.5, 3)) // confirmed_phone null
	if err := s.HandleQuality(context.Background(), evt); err != nil {
		t.Fatalf("missing confirmed_phone must be acked, got %v", err)
	}
	if len(stub.requests) != 0 {
		t.Fatalf("no Twenty calls expected without a phone; requests=%+v", stub.requests)
	}
}

func TestCallQualityEnrichedSkipsWhenPersonUnresolvable(t *testing.T) {
	s, _, stub := newCallQualitySyncer(t, nil)
	evt := callQualityEnrichedEvent(enrichedData("+1555000999", 0.5, 3))
	if err := s.HandleQuality(context.Background(), evt); err != nil {
		t.Fatalf("unresolvable person must be acked, got %v", err)
	}
	if notes, _ := stub.notes(); len(notes) != 0 {
		t.Fatalf("no note expected without a person; requests=%+v", stub.requests)
	}
}

func TestHandleQualityIgnoresOtherEventTypes(t *testing.T) {
	s, _, stub := newCallQualitySyncer(t, nil)
	evt := callQualityEnrichedEvent(enrichedData("+1555000111", 0.5, 3))
	evt.Type = events.TypeSessionEnded // wrong type on the quality topic
	if err := s.HandleQuality(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	if len(stub.requests) != 0 {
		t.Fatalf("unexpected Twenty calls; requests=%+v", stub.requests)
	}
}

func TestEnrichedNoteUsesNestedAvgSentiment(t *testing.T) {
	// conversation-service also sets quality.avg_sentiment; when the
	// top-level field is absent the nested one must be used.
	s, _, stub := newCallQualitySyncer(t, map[string]string{"+1555000111": "person-77"})
	q := qualityData("+1555000111")
	q["avg_sentiment"] = 0.77
	evt := events.CloudEvent{
		SpecVersion: "1.0",
		ID:          uuid.NewString(),
		Source:      "conversation-service",
		Type:        events.TypeCallQualityEnriched,
		Subject:     "acme-salon",
		Time:        time.Now().UTC(),
		TenantID:    callQualityTenant.String(), // matches the seeded tenant-scoped mapping (SPEC-W80 A1)
		Data: map[string]any{
			"conversationId": "conv-enriched-5",
			"quality":        q,
		},
	}
	if err := s.HandleQuality(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	notes, _ := stub.notes()
	if len(notes) != 1 {
		t.Fatalf("expected 1 note, got %d", len(notes))
	}
	body, _ := noteText(notes[0].body), true
	if !strings.Contains(body, "Caller's mood: Positive") {
		t.Errorf("note body missing nested avg sentiment:\n%s", body)
	}
}
