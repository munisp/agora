package incidents

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// HMAC known-vector (SPEC-W11 Part B §4): expected value computed with
// python3 -c "import hmac,hashlib;print(hmac.new(b'top-secret', BODY, hashlib.sha256).hexdigest())".
func TestSignatureHexKnownVector(t *testing.T) {
	body := []byte(`{"incident_id":"11111111-1111-1111-1111-111111111111","severity":"critical"}`)
	got := SignatureHex("top-secret", body)
	want := "2b514d4c2cd1a494ad8b4a05145975ec3cedb88fbff4017e3e20bdab0a618ca6"
	if got != want {
		t.Fatalf("SignatureHex = %s, want %s", got, want)
	}
	if SignatureHex("", body) != "" {
		t.Fatal("empty secret must yield an empty (unsigned) signature")
	}
}

func TestCompleteFillsDefaults(t *testing.T) {
	p := (&IDP{TenantID: uuid.New(), IncidentType: "fire", Severity: "high"}).Complete()
	if p.IncidentID == uuid.Nil {
		t.Fatal("incident_id must be minted")
	}
	if p.SchemaVersion != SchemaVersion || p.Channel != ChannelWebhook {
		t.Fatalf("schema/channel defaults: %+v", p)
	}
	if p.CapturedAt.IsZero() {
		t.Fatal("captured_at must default to now")
	}
	if !strings.HasPrefix(p.ReferenceNumber, "INC-") {
		t.Fatalf("reference_number must be derived, got %q", p.ReferenceNumber)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("completed IDP must validate: %v", err)
	}
}

func TestValidateRejectsBadSeverityAndMissingIDs(t *testing.T) {
	p := (&IDP{TenantID: uuid.New(), IncidentType: "fire", Severity: "catastrophic"}).Complete()
	if err := p.Validate(); err == nil {
		t.Fatal("bad severity must be rejected")
	}
	p2 := &IDP{}
	if err := p2.Validate(); err == nil {
		t.Fatal("missing ids must be rejected")
	}
	long := &IDP{IncidentID: uuid.New(), TenantID: uuid.New(), IncidentType: "other", Severity: "low",
		NarrativeSummary: strings.Repeat("x", 501)}
	if err := long.Validate(); err == nil {
		t.Fatal("narrative > 500 chars must be rejected")
	}
}

func TestNeedsOutreach(t *testing.T) {
	phone := "+2348012345678"
	cases := []struct {
		name string
		idp  IDP
		want bool
	}{
		{"critical with callback", IDP{Severity: "critical", CallbackNumber: &phone}, true},
		{"high with contact", IDP{Severity: "high", ContactID: ptrUUID(uuid.New())}, true},
		{"medium with callback", IDP{Severity: "medium", CallbackNumber: &phone}, false},
		{"critical no contact", IDP{Severity: "critical"}, false},
	}
	for _, c := range cases {
		if got := c.idp.NeedsOutreach(); got != c.want {
			t.Errorf("%s: NeedsOutreach = %v, want %v", c.name, got, c.want)
		}
	}
}

func ptrUUID(u uuid.UUID) *uuid.UUID { return &u }

func TestOutreachChannelAndText(t *testing.T) {
	ref := "INC-2026-000123"
	p := &IDP{
		Severity:        "critical",
		IncidentType:    "fire",
		ReferenceNumber: ref,
		Channel:         ChannelWhatsApp,
		Location:        &Location{AddressText: "12 Marina Rd, Lagos"},
	}
	if got := p.OutreachChannel(); got != ChannelWhatsApp {
		t.Fatalf("whatsapp incidents alert on whatsapp, got %q", got)
	}
	p.Channel = ChannelWebhook
	if got := p.OutreachChannel(); got != ChannelSMS {
		t.Fatalf("webhook incidents alert on sms, got %q", got)
	}
	text := p.OutreachText()
	for _, want := range []string{ref, "fire", "12 Marina Rd, Lagos"} {
		if !strings.Contains(text, want) {
			t.Fatalf("outreach text %q must contain %q", text, want)
		}
	}
	// No address → template still carries ref + type, no dangling "near".
	p.Location = nil
	text = p.OutreachText()
	if !strings.Contains(text, ref) || !strings.Contains(text, "fire") || strings.Contains(text, "near") {
		t.Fatalf("address-less outreach text malformed: %q", text)
	}
}

func TestReferenceNumberShape(t *testing.T) {
	ref := ReferenceNumber(uuid.New(), time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	if !strings.HasPrefix(ref, "INC-2026-") || len(ref) != len("INC-2026-000000") {
		t.Fatalf("reference shape: %q", ref)
	}
}
