package campaignstudio

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Pure validation / evaluation unit tests (no database).

func TestValidateSegmentDefinition(t *testing.T) {
	good := &SegmentDefinition{Filters: []SegmentFilter{
		{Field: FieldSource, Op: OpEq, Value: "twenty"},
		{Field: FieldLeadStatus, Op: OpIn, Value: []any{"new", "qualified"}},
		{Field: FieldName, Op: OpContains, Value: "ada"},
	}}
	if err := ValidateSegmentDefinition(good); err != nil {
		t.Fatalf("valid definition rejected: %v", err)
	}
	cases := map[string]*SegmentDefinition{
		"no filters":    {Filters: nil},
		"bad field":     {Filters: []SegmentFilter{{Field: "password", Op: OpEq, Value: "x"}}},
		"bad op":        {Filters: []SegmentFilter{{Field: FieldName, Op: "regex", Value: "x"}}},
		"missing value": {Filters: []SegmentFilter{{Field: FieldName, Op: OpEq}}},
		"empty value":   {Filters: []SegmentFilter{{Field: FieldName, Op: OpEq, Value: "  "}}},
		"in not array":  {Filters: []SegmentFilter{{Field: FieldName, Op: OpIn, Value: "x"}}},
		"in empty":      {Filters: []SegmentFilter{{Field: FieldName, Op: OpIn, Value: []any{}}}},
	}
	for name, def := range cases {
		if err := ValidateSegmentDefinition(def); err == nil {
			t.Fatalf("%s: expected validation error", name)
		}
	}
}

func TestValidateSteps(t *testing.T) {
	cond := &SegmentDefinition{Filters: []SegmentFilter{{Field: FieldLeadStatus, Op: OpEq, Value: "qualified"}}}
	good := Steps{
		{Type: StepSend, Kind: KindSMS, Template: "Hi {name}"},
		{Type: StepWait, WaitHours: 24},
		{Type: StepBranch, Condition: cond},
		{Type: StepSend, Kind: KindPushMarketing, Template: "Push body", AbVariant: "A"},
		{Type: StepSend, Kind: KindUSSD, Template: "USSD text"},
	}
	if err := ValidateSteps(good); err != nil {
		t.Fatalf("valid steps rejected: %v", err)
	}
	cases := map[string]Steps{
		"empty":            {},
		"bad type":         {{Type: "email"}},
		"bad kind":         {{Type: StepSend, Kind: "whatsapp", Template: "x"}},
		"send no template": {{Type: StepSend, Kind: KindSMS}},
		"send with wait":   {{Type: StepSend, Kind: KindSMS, Template: "x", WaitHours: 2}},
		"wait negative":    {{Type: StepWait, WaitHours: -1}},
		"wait with kind":   {{Type: StepWait, WaitHours: 1, Kind: KindSMS}},
		"branch no cond":   {{Type: StepBranch}},
		"branch bad cond":  {{Type: StepBranch, Condition: &SegmentDefinition{}}},
		"branch with kind": {{Type: StepBranch, Condition: cond, Kind: KindSMS}},
		"bad ab variant":   {{Type: StepSend, Kind: KindSMS, Template: "x", AbVariant: "C"}},
	}
	for name, steps := range cases {
		if err := ValidateSteps(steps); err == nil {
			t.Fatalf("%s: expected validation error", name)
		}
	}
}

func TestStatusMachine(t *testing.T) {
	allowed := [][2]string{
		{StatusDraft, StatusActive},
		{StatusDraft, StatusArchived},
		{StatusActive, StatusPaused},
		{StatusPaused, StatusActive},
		{StatusActive, StatusArchived},
		{StatusPaused, StatusArchived},
	}
	for _, tr := range allowed {
		if !CanTransition(tr[0], tr[1]) {
			t.Fatalf("%s → %s must be allowed", tr[0], tr[1])
		}
	}
	denied := [][2]string{
		{StatusDraft, StatusPaused},
		{StatusPaused, StatusDraft},
		{StatusActive, StatusDraft},
		{StatusArchived, StatusActive},
		{StatusArchived, StatusDraft},
		{StatusArchived, StatusPaused},
		{StatusArchived, StatusArchived},
	}
	for _, tr := range denied {
		if CanTransition(tr[0], tr[1]) {
			t.Fatalf("%s → %s must be denied", tr[0], tr[1])
		}
	}
}

func TestBuildSegmentQuery(t *testing.T) {
	def := &SegmentDefinition{Filters: []SegmentFilter{
		{Field: FieldSource, Op: OpEq, Value: "twenty"},
		{Field: FieldLeadStatus, Op: OpIn, Value: []any{"new", "qualified"}},
		{Field: FieldEmail, Op: OpContains, Value: "@example.com"},
		{Field: FieldLeadCreatedAt, Op: OpGte, Value: "2025-01-01T00:00:00Z"},
	}}
	where, args, err := buildSegmentQuery(def)
	if err != nil {
		t.Fatalf("buildSegmentQuery: %v", err)
	}
	if len(args) != 4 {
		t.Fatalf("args = %v, want 4 bound values", args)
	}
	// Values must be BOUND, never interpolated.
	wantParts := []string{
		"COALESCE(c.source,'') = $2",
		"EXISTS (SELECT 1 FROM leads l WHERE l.tenant_id=$1",
		"l.status = ANY($3)",
		"ILIKE '%' || $4 || '%'",
		"l.created_at >= $5::timestamptz",
	}
	for _, part := range wantParts {
		if !strings.Contains(where, part) {
			t.Fatalf("where clause missing %q:\n%s", part, where)
		}
	}
	if strings.Contains(where, "twenty") || strings.Contains(where, "@example.com") {
		t.Fatalf("where clause interpolates user values (SQL injection risk):\n%s", where)
	}
}

func TestEvaluateCondition(t *testing.T) {
	attrs := ContactAttrs{
		FieldName:       "Ada Lovelace",
		FieldPhone:      "+2348012345678",
		FieldEmail:      "ada@example.com",
		FieldSource:     "twenty",
		FieldLeadStatus: "qualified",
	}
	cases := []struct {
		name string
		def  *SegmentDefinition
		want bool
	}{
		{"eq true", &SegmentDefinition{Filters: []SegmentFilter{{Field: FieldSource, Op: OpEq, Value: "twenty"}}}, true},
		{"eq false", &SegmentDefinition{Filters: []SegmentFilter{{Field: FieldSource, Op: OpEq, Value: "field"}}}, false},
		{"neq", &SegmentDefinition{Filters: []SegmentFilter{{Field: FieldSource, Op: OpNeq, Value: "field"}}}, true},
		{"in hit", &SegmentDefinition{Filters: []SegmentFilter{{Field: FieldLeadStatus, Op: OpIn, Value: []any{"new", "qualified"}}}}, true},
		{"in miss", &SegmentDefinition{Filters: []SegmentFilter{{Field: FieldLeadStatus, Op: OpIn, Value: []any{"lost"}}}}, false},
		{"contains ci", &SegmentDefinition{Filters: []SegmentFilter{{Field: FieldName, Op: OpContains, Value: "LOVELACE"}}}, true},
		{"gte numeric", &SegmentDefinition{Filters: []SegmentFilter{{Field: FieldPhone, Op: OpGte, Value: "100"}}}, true},       // +234... parses numeric → 2.3e12 >= 100
		{"gte lexicographic", &SegmentDefinition{Filters: []SegmentFilter{{Field: FieldName, Op: OpGte, Value: "Bob"}}}, false}, // "Ada Lovelace" < "Bob"
		{"missing attr eq", &SegmentDefinition{Filters: []SegmentFilter{{Field: FieldLeadChannel, Op: OpEq, Value: "web"}}}, false},
		{"and semantics", &SegmentDefinition{Filters: []SegmentFilter{
			{Field: FieldSource, Op: OpEq, Value: "twenty"},
			{Field: FieldLeadStatus, Op: OpEq, Value: "lost"},
		}}, false},
	}
	for _, tc := range cases {
		if got := EvaluateCondition(tc.def, attrs); got != tc.want {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBuildPacedRequest(t *testing.T) {
	in := StudioSendBatchInput{
		TenantSlug:  "acme",
		JourneyID:   uuid.NewString(),
		JourneyName: "Winback",
	}
	send := QueuedSend{
		EnrollmentID: uuid.New(),
		ContactID:    uuid.New(),
		StepIdx:      1,
		Kind:         KindSMS,
		Phone:        "+2348012345678",
		Name:         "Ada",
		Text:         "Hi Ada, we miss you",
	}
	req, err := buildPacedRequest(in, send)
	if err != nil {
		t.Fatalf("sms build: %v", err)
	}
	if req.Kind != PacedSendGeoCampaign || req.Geo == nil {
		t.Fatalf("sms must route via geo_campaign paced kind: %+v", req)
	}
	if req.Geo.Channel != "sms" || req.Geo.Phone != send.Phone || req.Geo.Text != send.Text ||
		req.Geo.TenantSlug != "acme" || req.Geo.CampaignID != in.JourneyID {
		t.Fatalf("geo payload mismatch: %+v", req.Geo)
	}

	send.Kind = KindPushMarketing
	req, err = buildPacedRequest(in, send)
	if err != nil {
		t.Fatalf("push build: %v", err)
	}
	if req.Kind != PacedSendPushMarketing || req.Push == nil {
		t.Fatalf("push must route via push_marketing paced kind: %+v", req)
	}
	if req.Push.ContactID != send.ContactID.String() || req.Push.Phone != send.Phone ||
		req.Push.Title != "Winback" || req.Push.Body != send.Text {
		t.Fatalf("push payload mismatch: %+v", req.Push)
	}
	if req.Push.Data["journey_id"] != in.JourneyID || req.Push.Data["enrollment_id"] != send.EnrollmentID.String() {
		t.Fatalf("push data mismatch: %+v", req.Push.Data)
	}

	send.Kind = KindUSSD
	if _, err := buildPacedRequest(in, send); err == nil {
		t.Fatal("ussd must have no paced route")
	}
}
