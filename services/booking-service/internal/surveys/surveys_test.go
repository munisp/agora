package surveys

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Pure unit tests (no Postgres): validation, status machine, scoring,
// results aggregation, themes, token shape, paced-send contract shape.

func TestValidateQuestions(t *testing.T) {
	ok := []Question{
		{ID: "nps", Type: QTypeRating, Label: "How likely?", Required: true},
		{ID: "why", Type: QTypeText, Label: "Why?"},
		{ID: "channel", Type: QTypeSingle, Label: "Where?", Options: []string{"sms", "app"}},
		{ID: "tags", Type: QTypeMulti, Label: "Pick", Options: []string{"a", "b", "c"}},
	}
	if err := ValidateQuestions(ok); err != nil {
		t.Fatalf("valid questions: %v", err)
	}

	cases := []struct {
		name string
		qs   []Question
	}{
		{"empty", nil},
		{"unknown type", []Question{{ID: "x", Type: "matrix", Label: "L"}}},
		{"missing label", []Question{{ID: "x", Type: QTypeRating}}},
		{"single needs 2 options", []Question{{ID: "x", Type: QTypeSingle, Label: "L", Options: []string{"only"}}}},
		{"multi needs 2 options", []Question{{ID: "x", Type: QTypeMulti, Label: "L"}}},
		{"rating takes no options", []Question{{ID: "x", Type: QTypeRating, Label: "L", Options: []string{"a", "b"}}}},
		{"duplicate id", []Question{
			{ID: "x", Type: QTypeRating, Label: "L"},
			{ID: "x", Type: QTypeText, Label: "M"},
		}},
		{"duplicate option", []Question{{ID: "x", Type: QTypeSingle, Label: "L", Options: []string{"a", "a"}}}},
		{"empty option", []Question{{ID: "x", Type: QTypeSingle, Label: "L", Options: []string{"a", "  "}}}},
	}
	for _, tc := range cases {
		if err := ValidateQuestions(tc.qs); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%s: got %v, want ErrInvalidInput", tc.name, err)
		}
	}

	// Missing ids auto-assign q1..qn.
	auto := []Question{{Type: QTypeRating, Label: "L"}, {Type: QTypeText, Label: "M"}}
	if err := ValidateQuestions(auto); err != nil {
		t.Fatalf("auto ids: %v", err)
	}
	if auto[0].ID != "q1" || auto[1].ID != "q2" {
		t.Fatalf("auto ids = %q,%q, want q1,q2", auto[0].ID, auto[1].ID)
	}
}

func TestStatusMachine(t *testing.T) {
	legal := [][2]string{
		{StatusDraft, StatusActive}, {StatusDraft, StatusArchived},
		{StatusActive, StatusPaused}, {StatusActive, StatusArchived},
		{StatusPaused, StatusActive}, {StatusPaused, StatusArchived},
	}
	for _, tr := range legal {
		if !CanTransition(tr[0], tr[1]) {
			t.Fatalf("%s -> %s should be legal", tr[0], tr[1])
		}
		if err := ValidateTransition(tr[0], tr[1]); err != nil {
			t.Fatalf("%s -> %s: %v", tr[0], tr[1], err)
		}
	}
	illegal := [][2]string{
		{StatusDraft, StatusPaused}, {StatusActive, StatusDraft},
		{StatusPaused, StatusDraft}, {StatusArchived, StatusActive},
		{StatusArchived, StatusDraft}, {StatusActive, StatusActive},
	}
	for _, tr := range illegal {
		if CanTransition(tr[0], tr[1]) {
			t.Fatalf("%s -> %s should be illegal", tr[0], tr[1])
		}
		if err := ValidateTransition(tr[0], tr[1]); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("%s -> %s: got %v, want ErrInvalidTransition", tr[0], tr[1], err)
		}
	}
}

func testSurvey() Survey {
	return Survey{
		ID:       uuid.New(),
		TenantID: uuid.New(),
		Name:     "NPS",
		Kind:     KindNPS,
		Status:   StatusActive,
		Channel:  ChannelSMS,
		Questions: []Question{
			{ID: "nps", Type: QTypeRating, Label: "How likely are you to recommend us?", Required: true},
			{ID: "why", Type: QTypeText, Label: "Why that score?"},
			{ID: "channel", Type: QTypeSingle, Label: "Preferred channel", Options: []string{"sms", "app", "email"}},
			{ID: "topics", Type: QTypeMulti, Label: "Topics", Options: []string{"price", "service", "speed"}},
		},
	}
}

func TestValidateAnswers(t *testing.T) {
	sv := testSurvey()

	// Happy path: rating + single + multi + text.
	score, err := ValidateAnswers(sv, map[string]any{
		"nps": float64(9), "why": "great service", "channel": "sms", "topics": []any{"price", "service"},
	})
	if err != nil {
		t.Fatalf("valid answers: %v", err)
	}
	if score == nil || *score != 9 {
		t.Fatalf("score = %v, want 9", score)
	}

	cases := []struct {
		name    string
		answers map[string]any
	}{
		{"missing required rating", map[string]any{"why": "x"}},
		{"rating out of range", map[string]any{"nps": float64(11)}},
		{"rating fractional", map[string]any{"nps": float64(7.5)}},
		{"rating wrong type", map[string]any{"nps": "9"}},
		{"single not an option", map[string]any{"nps": float64(9), "channel": "pigeon"}},
		{"multi not an array", map[string]any{"nps": float64(9), "topics": "price"}},
		{"multi bad member", map[string]any{"nps": float64(9), "topics": []any{"price", "junk"}}},
	}
	for _, tc := range cases {
		if _, err := ValidateAnswers(sv, tc.answers); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%s: got %v, want ErrInvalidInput", tc.name, err)
		}
	}

	// Optional questions may be omitted; unknown keys are ignored.
	score, err = ValidateAnswers(sv, map[string]any{"nps": float64(3), "unknown_q": "ignored"})
	if err != nil || score == nil || *score != 3 {
		t.Fatalf("optional omitted = %v, %v", score, err)
	}

	// Custom kind never scores.
	sv.Kind = KindCustom
	score, err = ValidateAnswers(sv, map[string]any{"nps": float64(9)})
	if err != nil || score != nil {
		t.Fatalf("custom kind score = %v, %v; want nil", score, err)
	}
}

func TestComputeScoreFirstRating(t *testing.T) {
	sv := Survey{Kind: KindCSAT, Questions: []Question{
		{ID: "a", Type: QTypeText, Label: "t"},
		{ID: "b", Type: QTypeRating, Label: "first"},
		{ID: "c", Type: QTypeRating, Label: "second"},
	}}
	score := ComputeScore(sv, map[string]any{"b": float64(4), "c": float64(1)})
	if score == nil || *score != 4 {
		t.Fatalf("score = %v, want 4 (first rating question)", score)
	}
	if ComputeScore(sv, map[string]any{"c": float64(1)}) != nil {
		t.Fatal("first rating unanswered -> nil score")
	}
}

func TestBuildResults(t *testing.T) {
	sv := testSurvey()
	mk := func(score int, channel string, topics ...string) Response {
		topicsAny := make([]any, 0, len(topics))
		for _, tp := range topics {
			topicsAny = append(topicsAny, tp)
		}
		return Response{
			Score: &score,
			Answers: map[string]any{
				"nps": float64(score), "channel": channel, "topics": topicsAny,
			},
		}
	}
	// 2 promoters (9, 10), 1 passive (8), 1 detractor (2) -> NPS = 50-25 = 25.
	res := BuildResults(sv, []Response{
		mk(9, "sms", "price"), mk(10, "app", "service", "speed"), mk(8, "sms"), mk(2, "sms", "price"),
	})
	if res.ResponseCount != 4 || res.ScoredCount != 4 {
		t.Fatalf("counts = %d/%d, want 4/4", res.ResponseCount, res.ScoredCount)
	}
	if res.NPS == nil || *res.NPS != 25 {
		t.Fatalf("nps = %v, want 25", res.NPS)
	}
	if res.Promoters != 2 || res.Passives != 1 || res.Detractors != 1 {
		t.Fatalf("p/p/d = %d/%d/%d", res.Promoters, res.Passives, res.Detractors)
	}
	if res.MeanScore == nil || *res.MeanScore != 7.25 {
		t.Fatalf("mean = %v, want 7.25", res.MeanScore)
	}
	if res.ScoreDistribution["9"] != 1 || res.ScoreDistribution["2"] != 1 {
		t.Fatalf("distribution = %v", res.ScoreDistribution)
	}
	var channel, topics *QuestionBreakdown
	for i := range res.Questions {
		switch res.Questions[i].ID {
		case "channel":
			channel = &res.Questions[i]
		case "topics":
			topics = &res.Questions[i]
		}
	}
	if channel == nil || topics == nil {
		t.Fatalf("breakdowns missing: %+v", res.Questions)
	}
	if channel.AnswerCount != 4 {
		t.Fatalf("channel answers = %d, want 4", channel.AnswerCount)
	}
	for _, oc := range channel.Options {
		if oc.Option == "sms" && oc.Count != 3 {
			t.Fatalf("sms count = %d, want 3", oc.Count)
		}
	}
	for _, oc := range topics.Options {
		if oc.Option == "price" && oc.Count != 2 {
			t.Fatalf("price count = %d, want 2", oc.Count)
		}
	}

	// csat: no NPS, mean only.
	sv.Kind = KindCSAT
	res = BuildResults(sv, []Response{mk(4, "sms"), mk(2, "app")})
	if res.NPS != nil {
		t.Fatalf("csat nps = %v, want nil", res.NPS)
	}
	if res.MeanScore == nil || *res.MeanScore != 3 {
		t.Fatalf("csat mean = %v, want 3", res.MeanScore)
	}

	// Empty: nulls, zeroes.
	res = BuildResults(sv, nil)
	if res.ResponseCount != 0 || res.NPS != nil || res.MeanScore != nil {
		t.Fatalf("empty results = %+v", res)
	}
}

func TestBuildThemes(t *testing.T) {
	themes := BuildThemes([]string{
		"The delivery was late and the driver was rude. Very late!",
		"Late again — delivery took forever. The app is great though.",
		"Great service, will book again.",
	})
	top := map[string]int{}
	for _, th := range themes {
		top[th.Term] = th.Count
	}
	if top["late"] != 3 {
		t.Fatalf("late = %d, want 3 (%v)", top["late"], themes)
	}
	if top["delivery"] != 2 {
		t.Fatalf("delivery = %d, want 2", top["delivery"])
	}
	for _, th := range themes {
		if stopwords[th.Term] || len(th.Term) < minThemeTermLen {
			t.Fatalf("stopword/short term leaked: %q", th.Term)
		}
	}
	if themes[0].Term != "late" {
		t.Fatalf("top term = %q, want late", themes[0].Term)
	}

	// Cap at MaxThemes with deterministic tie-break.
	many := []string{}
	for i := 0; i < 40; i++ {
		many = append(many, strings.Repeat("zz", 1)+" term"+strings.Repeat("x", i+3))
	}
	if got := BuildThemes(many); len(got) > MaxThemes {
		t.Fatalf("themes = %d, want <= %d", len(got), MaxThemes)
	}
	if got := BuildThemes(nil); len(got) != 0 {
		t.Fatalf("empty themes = %v", got)
	}
}

func TestNewToken(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok, err := NewToken()
		if err != nil {
			t.Fatalf("NewToken: %v", err)
		}
		if len(tok) != 32 {
			t.Fatalf("token len = %d, want 32 (128-bit hex)", len(tok))
		}
		if _, err := json.Marshal(tok); err != nil {
			t.Fatalf("token not JSON-safe: %v", err)
		}
		for _, r := range tok {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
				t.Fatalf("token not hex: %q", tok)
			}
		}
		if seen[tok] {
			t.Fatal("token collision")
		}
		seen[tok] = true
	}
}

// The paced-send envelope MUST stay field-compatible with
// notification-worker workflows.PacedSendRequest (contract mirror).
func TestMarshalInvitePacedSendContract(t *testing.T) {
	h := &Handlers{PublicBaseURL: "https://s.example.com/"}
	sv := Survey{ID: uuid.New(), TenantID: uuid.New(), Name: "NPS Q3", Channel: ChannelSMS}
	inv := Invite{ID: uuid.New(), TenantID: sv.TenantID, SurveyID: sv.ID, ContactID: uuid.New(), Token: "abc123"}
	c := ResolvedContact{ContactID: inv.ContactID, Name: "Ada", Phone: "+234801"}

	raw, err := h.MarshalInvitePacedSend("acme", sv, inv, c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var env struct {
		SpecVersion string         `json:"specversion"`
		ID          string         `json:"id"`
		Type        string         `json:"type"`
		Subject     string         `json:"subject"`
		TenantID    string         `json:"tenantid"`
		Data        map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if env.Type != "com.opendesk.notifications.PacedSend" || env.ID == "" || env.TenantID != sv.TenantID.String() {
		t.Fatalf("envelope = %+v", env)
	}
	if env.Data["kind"] != "geo_campaign" {
		t.Fatalf("kind = %v, want geo_campaign (sms route)", env.Data["kind"])
	}
	geo, ok := env.Data["geo_campaign"].(map[string]any)
	if !ok {
		t.Fatalf("geo_campaign missing: %v", env.Data)
	}
	if geo["tenant_slug"] != "acme" || geo["campaign_id"] != sv.ID.String() ||
		geo["channel"] != "sms" || geo["phone"] != "+234801" || geo["name"] != "Ada" {
		t.Fatalf("geo payload = %v", geo)
	}
	text, _ := geo["text"].(string)
	if !strings.Contains(text, "https://s.example.com?t=abc123") || !strings.Contains(text, "Ada") {
		t.Fatalf("text = %q", text)
	}

	// push_marketing shape.
	sv.Channel = ChannelPushMarketing
	raw, err = h.MarshalInvitePacedSend("acme", sv, inv, c)
	if err != nil {
		t.Fatalf("marshal push: %v", err)
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if env.Data["kind"] != "push_marketing" {
		t.Fatalf("kind = %v, want push_marketing", env.Data["kind"])
	}
	push, ok := env.Data["push"].(map[string]any)
	if !ok {
		t.Fatalf("push missing: %v", env.Data)
	}
	if push["tenant_slug"] != "acme" || push["contact_id"] != inv.ContactID.String() ||
		push["phone"] != "+234801" || push["title"] != "NPS Q3" || push["body"] == "" {
		t.Fatalf("push payload = %v", push)
	}

	// Default base URL when unset.
	h.PublicBaseURL = ""
	if got := h.inviteLink("tok"); got != DefaultPublicBaseURL+"?t=tok" {
		t.Fatalf("default link = %q", got)
	}
}
