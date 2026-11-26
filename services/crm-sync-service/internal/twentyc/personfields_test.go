package twentyc

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/opendesk/crm-sync-service/internal/events"
)

const leadwayFields = `{"advisor":"assignedAdvisor",
 "first_channel":{"field":"firstContactChannel","values":{"voice":"PHONE","phone":"PHONE","web":"BOOKING_PAGE"}},
 "language":{"field":"preferredLanguage","values":{"en":"ENGLISH","yo":"YORUBA"}},
 "offering_status":{"field":"rsaStatus","match":[["transfer","TRANSFER"],["registration","REGISTERING"],["retirement benefits","RETIREE"]]},
 "site":"leadway-pensure"}`

func TestPersonFieldPatches(t *testing.T) {
	var f PersonFields
	if err := json.Unmarshal([]byte(leadwayFields), &f); err != nil {
		t.Fatal(err)
	}
	latest, ifEmpty := f.Patches(PersonFacts{Advisor: " Aisha Bello ", Channel: "Voice", Language: "yo",
		Offering: "RSA Transfer & Account Review"})
	if latest["assignedAdvisor"] != "Aisha Bello" || latest["preferredLanguage"] != "YORUBA" || len(latest) != 2 {
		t.Fatalf("latest = %v", latest)
	}
	if ifEmpty["firstContactChannel"] != "PHONE" || ifEmpty["rsaStatus"] != "TRANSFER" || len(ifEmpty) != 2 {
		t.Fatalf("ifEmpty = %v", ifEmpty)
	}
	// Unknown values and empty facts write nothing.
	latest, ifEmpty = f.Patches(PersonFacts{Channel: "telegram", Language: "fr", Offering: "Employer Onboarding Session"})
	if len(latest) != 0 || len(ifEmpty) != 0 {
		t.Fatalf("expected nothing, got %v %v", latest, ifEmpty)
	}
	// A business without person_fields writes nothing.
	var none *PersonFields
	if l, e := none.Patches(PersonFacts{Advisor: "x", Language: "en"}); len(l) != 0 || len(e) != 0 {
		t.Fatal("nil fields should patch nothing")
	}
}

func TestCallSummaryNoteOutcome(t *testing.T) {
	q := events.CallQuality{Direction: "outbound", Purpose: "confirm_appointment", CallOutcome: "confirmed"}
	if n := CallSummaryNote(q, ""); !strings.Contains(n, "- Outcome: Confirmed they will attend") || strings.Contains(n, "Appointment booked") {
		t.Fatalf("confirm call note:\n%s", n)
	}
	q.ToolCalls = map[string]int{"book_appointment": 1} // a booking made on the call still reads as booked
	if n := CallSummaryNote(q, ""); !strings.Contains(n, "- Appointment booked: Yes") {
		t.Fatalf("booked call note:\n%s", n)
	}
}
