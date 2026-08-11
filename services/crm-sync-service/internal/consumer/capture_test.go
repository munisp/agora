package consumer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/crm-sync-service/internal/events"
	"github.com/opendesk/crm-sync-service/internal/syncmap"
	"github.com/opendesk/crm-sync-service/internal/twentyc"
)

// CaptureExtracted (SPEC-W38 F3): captured-fields note path, dedupe via
// sync_map kind=capture_note keyed by record_id. Stubs live in
// session_ended_test.go (same package).

func captureExtractedEvent(data map[string]any) events.CloudEvent {
	return events.CloudEvent{
		SpecVersion: "1.0",
		ID:          uuid.NewString(),
		Source:      "conversation-service",
		Type:        events.TypeCaptureExtracted,
		Subject:     "acme-salon",
		Time:        time.Now().UTC(),
		TenantID:    uuid.NewString(),
		Data:        data,
	}
}

func captureData(recordID string, fields map[string]any) map[string]any {
	return map[string]any{
		"record_id":       recordID,
		"tenant_id":       uuid.NewString(),
		"agent_id":        uuid.NewString(),
		"conversation_id": "conv-capture-1",
		"schema_id":       uuid.NewString(),
		"data":            fields,
	}
}

func TestCaptureCreatesNoteWithCapturedFields(t *testing.T) {
	s, fm, stub := newCallQualitySyncer(t, map[string]string{"+1555000111": "person-77"})
	evt := captureExtractedEvent(captureData("rec-1", map[string]any{
		"caller_name":  "Ada",
		"caller_phone": "+1555000111",
		"party_size":   4.0,
		"day":          "Friday",
	}))
	if err := s.HandleCapture(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	notes, links := stub.notes()
	if len(notes) != 1 {
		t.Fatalf("expected 1 note, got %d; requests=%+v", len(notes), stub.requests)
	}
	if notes[0].body["title"] != twentyc.CaptureNoteTitle {
		t.Errorf("note title = %v", notes[0].body["title"])
	}
	body, _ := notes[0].body["body"].(string)
	for _, want := range []string{
		"📋 AI captured fields",
		"caller_name: Ada",
		"caller_phone: +1555000111",
		"party_size: 4",
		"day: Friday",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("note body missing %q:\n%s", want, body)
		}
	}
	if len(links) != 1 || links[0].body["personId"] != "person-77" {
		t.Errorf("noteTarget link = %+v", links)
	}
	// the mapping lets a redelivery dedupe instead of duplicating
	m, err := fm.Get(context.Background(), KindCaptureNote, "rec-1", parseUUID(evt.TenantID))
	if err != nil {
		t.Fatalf("capture_note mapping missing: %v", err)
	}
	if m.TwentyID == "" {
		t.Error("capture_note mapping has empty twenty id")
	}
}

func TestCaptureRedeliveryIsDeduped(t *testing.T) {
	s, _, stub := newCallQualitySyncer(t, map[string]string{"+1555000111": "person-77"})
	evt := captureExtractedEvent(captureData("rec-2", map[string]any{
		"caller_phone": "+1555000111",
	}))
	for i := 0; i < 2; i++ {
		if err := s.HandleCapture(context.Background(), evt); err != nil {
			t.Fatal(err)
		}
	}
	notes, _ := stub.notes()
	if len(notes) != 1 {
		t.Fatalf("redelivery must not create a second note, got %d", len(notes))
	}
}

func TestCaptureResolvesViaEmailField(t *testing.T) {
	// The stub answers /rest/people by phone substring; register the email
	// under the phone-filter path by keying on the email string itself (the
	// stub matches ANY filter containing the key).
	s, _, stub := newCallQualitySyncer(t, map[string]string{"ada@example.com": "person-78"})
	evt := captureExtractedEvent(captureData("rec-3", map[string]any{
		"caller_email": "ada@example.com",
	}))
	if err := s.HandleCapture(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	notes, links := stub.notes()
	if len(notes) != 1 || len(links) != 1 || links[0].body["personId"] != "person-78" {
		t.Fatalf("email-resolved note/link wrong; requests=%+v", stub.requests)
	}
}

func TestCaptureFallsBackToSyncMapContactPhone(t *testing.T) {
	// Twenty phone lookup misses, but the booking sync wrote a contact_phone
	// mapping for the captured number (mirrors resolvePersonForCall).
	s, fm, stub := newCallQualitySyncer(t, nil)
	tid, _ := uuid.Parse(uuid.NewString())
	if err := fm.Put(context.Background(), syncmap.KindContactPhone, "+1555000222", "person-88", &tid); err != nil {
		t.Fatal(err)
	}
	evt := captureExtractedEvent(captureData("rec-4", map[string]any{
		"caller_phone": "+1555000222",
	}))
	evt.TenantID = tid.String()
	if err := s.HandleCapture(context.Background(), evt); err != nil {
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

func TestCaptureWithoutRecordIDIsPermanent(t *testing.T) {
	s, _, stub := newCallQualitySyncer(t, map[string]string{"+1555000111": "person-77"})
	d := captureData("", map[string]any{"caller_phone": "+1555000111"})
	if err := s.HandleCapture(context.Background(), captureExtractedEvent(d)); err == nil {
		t.Fatal("missing record_id must be an error")
	} else if !errors.Is(err, errPermanent) {
		t.Fatalf("missing record_id must be permanent (DLQ at once), got %v", err)
	}
	if len(stub.requests) != 0 {
		t.Fatalf("no Twenty calls expected; requests=%+v", stub.requests)
	}
}

func TestCaptureWithoutContactRefsIsAcked(t *testing.T) {
	s, _, stub := newCallQualitySyncer(t, map[string]string{"+1555000111": "person-77"})
	evt := captureExtractedEvent(captureData("rec-5", map[string]any{
		"caller_name": "Ada", // no phone/email key
	}))
	if err := s.HandleCapture(context.Background(), evt); err != nil {
		t.Fatalf("no contact refs must be acked, got %v", err)
	}
	if len(stub.requests) != 0 {
		t.Fatalf("no Twenty calls expected without contact refs; requests=%+v", stub.requests)
	}
}

func TestCaptureSkipsWhenPersonUnresolvable(t *testing.T) {
	s, _, stub := newCallQualitySyncer(t, nil)
	evt := captureExtractedEvent(captureData("rec-6", map[string]any{
		"caller_phone": "+1555000999",
	}))
	if err := s.HandleCapture(context.Background(), evt); err != nil {
		t.Fatalf("unresolvable person must be acked, got %v", err)
	}
	if notes, _ := stub.notes(); len(notes) != 0 {
		t.Fatalf("no note expected without a person; requests=%+v", stub.requests)
	}
}

func TestCaptureWithEmptyDataIsAcked(t *testing.T) {
	s, _, stub := newCallQualitySyncer(t, nil)
	evt := captureExtractedEvent(captureData("rec-7", map[string]any{}))
	if err := s.HandleCapture(context.Background(), evt); err != nil {
		t.Fatalf("empty data must be acked, got %v", err)
	}
	if len(stub.requests) != 0 {
		t.Fatalf("no Twenty calls expected with empty data; requests=%+v", stub.requests)
	}
}

func TestHandleCaptureIgnoresOtherEventTypes(t *testing.T) {
	s, _, stub := newCallQualitySyncer(t, nil)
	evt := captureExtractedEvent(captureData("rec-8", map[string]any{
		"caller_phone": "+1555000111",
	}))
	evt.Type = events.TypeSessionEnded // wrong type on the captures topic
	if err := s.HandleCapture(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	if len(stub.requests) != 0 {
		t.Fatalf("unexpected Twenty calls; requests=%+v", stub.requests)
	}
}

func TestCaptureContactRefsDeterministic(t *testing.T) {
	phone, email := captureContactRefs(map[string]any{
		"caller_email": "ada@example.com",
		"caller_phone": "+1555000111",
		"party_size":   4.0,
		"callback":     "ignored",
	})
	if phone != "+1555000111" || email != "ada@example.com" {
		t.Errorf("refs = %q, %q", phone, email)
	}
	p, e := captureContactRefs(map[string]any{"caller_name": "Ada"})
	if p != "" || e != "" {
		t.Errorf("refs = %q, %q; want empty", p, e)
	}
}
