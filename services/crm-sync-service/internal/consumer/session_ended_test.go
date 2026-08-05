package consumer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/crm-sync-service/internal/events"
	"github.com/opendesk/crm-sync-service/internal/metrics"
	"github.com/opendesk/crm-sync-service/internal/syncmap"
	"github.com/opendesk/crm-sync-service/internal/twentyc"
	"go.uber.org/zap"
)

// callQualityStub answers Twenty lookups: people are found only for phones in
// personByPhone; note/noteTarget creates are recorded.
type callQualityStub struct {
	mu            sync.Mutex
	personByPhone map[string]string
	requests      []stubReq
}

func (s *callQualityStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		s.requests = append(s.requests, stubReq{r.Method, r.URL.Path, r.URL.Query().Get("filter"), body})
		n := len(s.requests)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		write := func(key string, v any) {
			b, _ := json.Marshal(map[string]any{"data": map[string]any{key: v}})
			_, _ = w.Write(b)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/rest/people":
			for phone, id := range s.personByPhone {
				if strings.Contains(r.URL.Query().Get("filter"), phone) {
					write("people", []map[string]any{{"id": id}})
					return
				}
			}
			write("people", []map[string]any{})
		case r.Method == http.MethodPost && r.URL.Path == "/rest/notes":
			write("createNote", map[string]any{"id": fmt.Sprintf("note-%d", n)})
		case r.Method == http.MethodPost && r.URL.Path == "/rest/noteTargets":
			write("created", map[string]any{"id": "target-1"})
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/rest/notes/"):
			write("updateNote", map[string]any{"id": strings.TrimPrefix(r.URL.Path, "/rest/notes/")})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func (s *callQualityStub) notes() (created, linked []stubReq) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.requests {
		if r.method == http.MethodPost && r.path == "/rest/notes" {
			created = append(created, r)
		}
		if r.method == http.MethodPost && r.path == "/rest/noteTargets" {
			linked = append(linked, r)
		}
	}
	return created, linked
}

func newCallQualitySyncer(t *testing.T, personByPhone map[string]string) (*Syncer, *fakeMap, *callQualityStub) {
	t.Helper()
	stub := &callQualityStub{personByPhone: personByPhone}
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	fm := newFakeMap()
	s := &Syncer{
		Twenty:  twentyc.New(srv.URL, "k", 0),
		Map:     fm,
		Metrics: metrics.New(),
		Log:     zap.NewNop(),
	}
	return s, fm, stub
}

func sessionEndedEvent(data map[string]any) events.CloudEvent {
	return events.CloudEvent{
		SpecVersion: "1.0",
		ID:          uuid.NewString(),
		Source:      "voice-agent-runtime",
		Type:        events.TypeSessionEnded,
		Subject:     "acme-salon",
		Time:        time.Now().UTC(),
		TenantID:    uuid.NewString(),
		Data:        data,
	}
}

func qualityData(phone string) map[string]any {
	avg, max := 820, 1400
	q := map[string]any{
		"duration_s":         95.2,
		"turn_count":         6,
		"tool_calls":         map[string]any{"book_appointment": 1, "lookup_appointment": 2},
		"avg_llm_latency_ms": avg,
		"max_llm_latency_ms": max,
		"stt_calls":          6,
		"tts_calls":          5,
		"llm_fallback_used":  false,
		"escalated":          true,
		"confirmed_phone":    nil,
	}
	if phone != "" {
		q["confirmed_phone"] = phone
	}
	return q
}

func TestSessionEndedCreatesCallSummaryNoteViaPhoneLookup(t *testing.T) {
	s, _, stub := newCallQualitySyncer(t, map[string]string{"+1555000111": "person-77"})
	evt := sessionEndedEvent(map[string]any{
		"conversationId": "conv-1",
		"channel":        "voice",
		"siteSlug":       "acme-salon",
		"quality":        qualityData("+1555000111"),
	})
	if err := s.HandleConversation(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	notes, links := stub.notes()
	if len(notes) != 1 {
		t.Fatalf("expected 1 note, got %d; requests=%+v", len(notes), stub.requests)
	}
	body, _ := notes[0].body["body"].(string)
	for _, want := range []string{
		"📞 AI call summary",
		"duration 95s",
		"6 turns",
		"book_appointment×1",
		"lookup_appointment×2",
		"avg LLM 820ms (max 1400ms)",
		"escalated: yes",
		"fallback used: no",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("note body missing %q:\n%s", want, body)
		}
	}
	if notes[0].body["title"] != twentyc.CallSummaryNoteTitle {
		t.Errorf("note title = %v", notes[0].body["title"])
	}
	if len(links) != 1 || links[0].body["personId"] != "person-77" {
		t.Errorf("noteTarget link = %+v", links)
	}
}

func TestSessionEndedFallsBackToSyncMapContactPhone(t *testing.T) {
	// Twenty phone lookup misses, but the booking sync wrote a contact_phone
	// mapping for the confirmed number.
	s, fm, stub := newCallQualitySyncer(t, nil)
	tid, _ := uuid.Parse(uuid.NewString())
	if err := fm.Put(context.Background(), syncmap.KindContactPhone, "+1555000222", "person-88", &tid); err != nil {
		t.Fatal(err)
	}
	evt := sessionEndedEvent(map[string]any{
		"conversationId": "conv-2",
		"channel":        "voice",
		"siteSlug":       "acme-salon",
		"quality":        qualityData("+1555000222"),
	})
	evt.TenantID = tid.String()
	if err := s.HandleConversation(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	notes, links := stub.notes()
	if len(notes) != 1 {
		t.Fatalf("expected 1 note via sync_map fallback, got %d", len(notes))
	}
	if len(links) != 1 || links[0].body["personId"] != "person-88" {
		t.Errorf("noteTarget link = %+v", links)
	}
}

func TestSessionEndedSkipsWhenPersonUnresolvable(t *testing.T) {
	s, _, stub := newCallQualitySyncer(t, nil)
	evt := sessionEndedEvent(map[string]any{
		"conversationId": "conv-3",
		"channel":        "voice",
		"siteSlug":       "acme-salon",
		"quality":        qualityData("+1555000999"),
	})
	if err := s.HandleConversation(context.Background(), evt); err != nil {
		t.Fatalf("unresolvable person must be acked, got %v", err)
	}
	if notes, _ := stub.notes(); len(notes) != 0 {
		t.Fatalf("no note expected without a person; requests=%+v", stub.requests)
	}
}

func TestSessionEndedWithoutQualityIsAcked(t *testing.T) {
	s, _, stub := newCallQualitySyncer(t, map[string]string{"+1555000111": "person-77"})
	evt := sessionEndedEvent(map[string]any{
		"conversationId": "conv-4",
		"channel":        "voice",
		"siteSlug":       "acme-salon",
	})
	if err := s.HandleConversation(context.Background(), evt); err != nil {
		t.Fatalf("missing quality must be acked, got %v", err)
	}
	if len(stub.requests) != 0 {
		t.Fatalf("no Twenty calls expected without quality; requests=%+v", stub.requests)
	}
}

func TestSessionEndedWithoutConfirmedPhoneIsAcked(t *testing.T) {
	s, _, stub := newCallQualitySyncer(t, map[string]string{"+1555000111": "person-77"})
	evt := sessionEndedEvent(map[string]any{
		"conversationId": "conv-5",
		"channel":        "voice",
		"siteSlug":       "acme-salon",
		"quality":        qualityData(""), // confirmed_phone null
	})
	if err := s.HandleConversation(context.Background(), evt); err != nil {
		t.Fatalf("missing confirmed_phone must be acked, got %v", err)
	}
	if len(stub.requests) != 0 {
		t.Fatalf("no Twenty calls expected without a phone; requests=%+v", stub.requests)
	}
}

func TestBookingUpsertWritesContactPhoneMapping(t *testing.T) {
	s, fm, _ := newTestSyncer(t)
	tid := uuid.New()
	phone := "+1555000333"
	evt := bookingEvent(events.TypeBookingCreated, map[string]any{
		"booking_id":   uuid.NewString(),
		"starts_at":    "2026-03-01T10:00:00Z",
		"ends_at":      "2026-03-01T10:30:00Z",
		"status":       "pending",
		"source":       "voice",
		"offering_id":  uuid.NewString(),
		"contact_name": "Jane Doe",
		"phone":        phone,
	})
	evt.TenantID = tid.String()
	if err := s.HandleBooking(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	m, err := fm.Get(context.Background(), syncmap.KindContactPhone, phone, &tid)
	if err != nil {
		t.Fatalf("contact_phone mapping missing: %v", err)
	}
	if m.TwentyID == "" {
		t.Error("contact_phone mapping should point at the Twenty person id")
	}
}
