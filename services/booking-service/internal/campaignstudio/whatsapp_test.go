package campaignstudio

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// SPEC-W21 Agent A: whatsapp send-step kind — validation, paced-request
// build, workflow dispatch.

func sendStep(kind string) JourneyStep {
	return JourneyStep{Type: StepSend, Kind: kind, Template: "Hi {name}"}
}

// kind whatsapp REQUIRES template_name (400 at the API via ErrInvalidInput).
func TestValidateStepsWhatsAppRequiresTemplateName(t *testing.T) {
	st := sendStep(KindWhatsApp)
	if err := ValidateSteps(Steps{st}); err == nil {
		t.Fatal("whatsapp without template_name must fail validation")
	} else if !strings.Contains(err.Error(), "template_name") {
		t.Fatalf("error must name template_name, got %v", err)
	}

	st.TemplateName = "vote_reminder"
	if err := ValidateSteps(Steps{st}); err != nil {
		t.Fatalf("whatsapp with template_name must validate: %v", err)
	}
}

// The free-text template stays optional for whatsapp (repurposed params
// hint); language and params (≤10) are accepted.
func TestValidateStepsWhatsAppFields(t *testing.T) {
	st := JourneyStep{
		Type:         StepSend,
		Kind:         KindWhatsApp,
		TemplateName: "vote_reminder",
		Language:     "en_US",
		Params:       []string{"{name}", "Ward 3"},
	}
	if err := ValidateSteps(Steps{st}); err != nil {
		t.Fatalf("whatsapp step without free-text template must validate: %v", err)
	}

	st.Params = make([]string, maxWhatsAppParams+1)
	if err := ValidateSteps(Steps{st}); err == nil {
		t.Fatal("params > 10 must fail validation")
	}

	st.Params = nil
	st.TemplateName = strings.Repeat("t", maxTemplateNameLen+1)
	if err := ValidateSteps(Steps{st}); err == nil {
		t.Fatal("oversized template_name must fail validation")
	}
}

// template_name/language/params are whatsapp-only: other send kinds (and
// wait/branch steps) reject them; unknown kinds stay rejected.
func TestValidateStepsWhatsAppFieldDiscipline(t *testing.T) {
	st := sendStep(KindSMS)
	st.TemplateName = "x"
	if err := ValidateSteps(Steps{st}); err == nil {
		t.Fatal("sms step with template_name must fail validation")
	}

	st = sendStep(KindSMS)
	st.Params = []string{"a"}
	if err := ValidateSteps(Steps{st}); err == nil {
		t.Fatal("sms step with params must fail validation")
	}

	w := JourneyStep{Type: StepWait, WaitHours: 1, TemplateName: "x"}
	if err := ValidateSteps(Steps{w}); err == nil {
		t.Fatal("wait step with template_name must fail validation")
	}

	if err := ValidateSteps(Steps{{Type: StepSend, Kind: "carrier_pigeon", Template: "hi"}}); err == nil {
		t.Fatal("unknown kinds stay rejected")
	}
}

// buildPacedRequest routes whatsapp to the whatsapp_campaign paced kind
// with the contract payload (campaign_id = journey id).
func TestBuildPacedRequestWhatsApp(t *testing.T) {
	in := StudioSendBatchInput{
		TenantSlug: "acme",
		JourneyID:  uuid.NewString(),
	}
	send := QueuedSend{
		EnrollmentID: uuid.New(),
		ContactID:    uuid.New(),
		StepIdx:      0,
		Kind:         KindWhatsApp,
		Phone:        "+2348012345678",
		Name:         "Ada",
		TemplateName: "vote_reminder",
		Language:     "en_US",
		Params:       []string{"Ada", "Ward 3"},
	}
	req, err := buildPacedRequest(in, send)
	if err != nil {
		t.Fatalf("whatsapp build: %v", err)
	}
	if req.Kind != PacedSendWhatsAppCampaign || req.WhatsApp == nil {
		t.Fatalf("whatsapp must route via whatsapp_campaign paced kind: %+v", req)
	}
	w := req.WhatsApp
	if w.TenantSlug != "acme" || w.ContactID != send.ContactID.String() ||
		w.Phone != send.Phone || w.TemplateName != "vote_reminder" ||
		w.Language != "en_US" || w.CampaignID != in.JourneyID {
		t.Fatalf("whatsapp payload mismatch: %+v", w)
	}
	if len(w.Params) != 2 || w.Params[0] != "Ada" || w.Params[1] != "Ward 3" {
		t.Fatalf("params mismatch: %+v", w.Params)
	}
	if got := pacedSendChannel(req); got != "whatsapp" {
		t.Fatalf("quiet-hours channel key must be whatsapp, got %q", got)
	}
}

// End-to-end workflow dispatch: a whatsapp queued send reaches the stubbed
// NotifyPaced as a whatsapp_campaign request and records send_sent.
func TestStudioSendWorkflowDispatchesWhatsApp(t *testing.T) {
	s := newStudioWfEnv(t)
	in := mkBatch(KindWhatsApp)
	in.Sends[0].TemplateName = "vote_reminder"
	in.Sends[0].Params = []string{"Ada"}

	s.env.ExecuteWorkflow(StudioSendWorkflow, in)
	if !s.env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := s.env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if len(s.seenRequests) != 1 {
		t.Fatalf("expected 1 paced request, got %d", len(s.seenRequests))
	}
	got := s.seenRequests[0]
	if got.Kind != PacedSendWhatsAppCampaign || got.WhatsApp == nil {
		t.Fatalf("whatsapp_campaign request expected: %+v", got)
	}
	if got.WhatsApp.TemplateName != "vote_reminder" || len(got.WhatsApp.Params) != 1 {
		t.Fatalf("template payload mismatch: %+v", got.WhatsApp)
	}
	if len(s.recorded) != 1 || s.recorded[0].Status != PacedSendStatusSent {
		t.Fatalf("send_sent outcome expected: %+v", s.recorded)
	}
}

// DND suppression of a whatsapp send surfaces as send_suppressed, exactly
// like the sms/push marketing kinds.
func TestStudioSendWorkflowWhatsAppSuppressed(t *testing.T) {
	s := newStudioWfEnv(t)
	s.suppressKinds[PacedSendWhatsAppCampaign] = "tenant_optout"
	in := mkBatch(KindWhatsApp)
	in.Sends[0].TemplateName = "vote_reminder"

	s.env.ExecuteWorkflow(StudioSendWorkflow, in)
	if err := s.env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if len(s.recorded) != 1 || s.recorded[0].Status != PacedSendStatusSuppressedDND ||
		s.recorded[0].Reason != "tenant_optout" {
		t.Fatalf("suppressed outcome expected: %+v", s.recorded)
	}
}
