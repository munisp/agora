package twentyc

import (
	"strings"
	"testing"

	"github.com/opendesk/crm-sync-service/internal/events"
)

func floatPtr(v float64) *float64 { return &v }
func intPtr(v int) *int           { return &v }

func TestCallSummaryNoteReadsLikeASummary(t *testing.T) {
	body := CallSummaryNote(events.CallQuality{
		DurationS:       341.9,
		TurnCount:       8,
		ToolCalls:       map[string]int{"get_availability": 2, "book_appointment": 1},
		AvgLLMLatencyMs: intPtr(820),
		TtsCalls:        12,
		ConfirmedPhone:  "08065553108",
	}, "The caller wanted to open a retirement savings account. The receptionist booked an RSA Registration call for Tuesday 24 November at 9:00 am.")
	want := "The caller wanted to open a retirement savings account. The receptionist booked an RSA Registration call for Tuesday 24 November at 9:00 am.\n\n" +
		"- Call length: 5 min 42 s\n" +
		"- Phone confirmed: 0806 555 3108\n" +
		"- Appointment booked: Yes\n" +
		"- Passed to a person: No"
	if body != want {
		t.Fatalf("CallSummaryNote =\n%q\nwant\n%q", body, want)
	}
}

func TestCallSummaryNoteHasNoEngineeringJargon(t *testing.T) {
	body := CallSummaryNote(events.CallQuality{
		DurationS: 95, TurnCount: 6, ToolCalls: map[string]int{"lookup_appointment": 2},
		AvgLLMLatencyMs: intPtr(820), MaxLLMLatencyMs: intPtr(1400), SttCalls: 6, TtsCalls: 5, LLMFallbackUsed: true,
	}, "")
	for _, bad := range []string{"turns", "tools", "×", "LLM", "stt", "tts", "fallback", "ms"} {
		if strings.Contains(body, bad) {
			t.Errorf("summary should not mention %q: %q", bad, body)
		}
	}
	if !strings.HasPrefix(body, "- Call length: 1 min 35 s") {
		t.Errorf("without sentences the note starts with the facts: %q", body)
	}
}

func TestCallSummaryNoteBookingOutcomeAndHandover(t *testing.T) {
	cases := map[string]events.CallQuality{
		"Appointment booked: No":          {DurationS: 40},
		"Appointment booked: Rescheduled": {DurationS: 40, ToolCalls: map[string]int{"reschedule_appointment": 1}},
		"Passed to a person: Yes":         {DurationS: 40, Escalated: true},
		"Call length: 40 s":               {DurationS: 40},
	}
	for want, q := range cases {
		if body := CallSummaryNote(q, ""); !strings.Contains(body, want) {
			t.Errorf("want %q in %q", want, body)
		}
	}
}

func TestCallSummaryNoteMood(t *testing.T) {
	for v, want := range map[float64]string{0.42: "Positive", -0.35: "Negative", 0.1: "Neutral"} {
		body := CallSummaryNote(events.CallQuality{DurationS: 30, AvgSentiment: floatPtr(v)}, "")
		if !strings.Contains(body, "Caller's mood: "+want) {
			t.Errorf("sentiment %v: want mood %s in %q", v, want, body)
		}
	}
	if strings.Contains(CallSummaryNote(events.CallQuality{DurationS: 30}, ""), "mood") {
		t.Error("no mood line without sentiment")
	}
}
