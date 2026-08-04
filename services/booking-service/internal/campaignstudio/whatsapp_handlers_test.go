package campaignstudio

import (
	"net/http"
	"testing"
)

// SPEC-W21 Agent A: handler-level validation — a whatsapp send step
// without template_name is a 400 at the API.

func TestWhatsAppStepValidationAPI(t *testing.T) {
	e := newStudioTestEnv(t)

	// Missing template_name → 400.
	code, resp := e.do(t, http.MethodPost, "/v1/studio/journeys", map[string]any{
		"name": "WA", "trigger_kind": "manual",
		"steps": []map[string]any{{"type": "send", "kind": "whatsapp"}},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("whatsapp without template_name = %d %v, want 400", code, resp)
	}

	// With template_name (+ optional language/params, no free-text
	// template) → 201.
	code, resp = e.do(t, http.MethodPost, "/v1/studio/journeys", map[string]any{
		"name": "WA", "trigger_kind": "manual",
		"steps": []map[string]any{{
			"type": "send", "kind": "whatsapp",
			"template_name": "vote_reminder", "language": "en_US",
			"params": []string{"{name}", "Ward 3"},
		}},
	})
	if code != http.StatusCreated {
		t.Fatalf("whatsapp journey = %d %v, want 201", code, resp)
	}
	step := resp["journey"].(map[string]any)["steps"].([]any)[0].(map[string]any)
	if step["template_name"] != "vote_reminder" || step["language"] != "en_US" {
		t.Fatalf("stored step mismatch: %+v", step)
	}
}
