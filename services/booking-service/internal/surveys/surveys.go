// Package surveys implements SPEC-W20 Agent B: the SURVEYS / VoC enterprise
// app (NPS / CSAT / CES / custom surveys), app_id "surveys-voc".
//
// Model (PostgreSQL, RLS tenant_isolation on every table, mirroring the
// W16 devices store idiom):
//
//	surveys          {id, tenant_id, name, status, kind, questions jsonb,
//	                  trigger_kind, channel, created_at, updated_at}
//	survey_invites   {id, tenant_id, survey_id, contact_id, token unique,
//	                  status, sent_at, answered_at, created_at}
//	survey_responses {id, tenant_id, survey_id, invite_id, contact_id,
//	                  answers jsonb, score, submitted_at}
//
// Survey status machine (mirrors campaignstudio): draft-active-paused
// active-archived; archived is terminal; draft may archive directly.
//
// Invite sends ride the fire-and-forget PacedSend CloudEvent contract
// (notification-worker internal/notifyoutbox/consumer.go +
// internal/workflows/paced.go — mirrored EXACTLY in events.go): one
// com.opendesk.notifications.PacedSend CloudEvent per invite on the
// notifications outbox topic, data IS a PacedSendRequest of kind
// "geo_campaign" (channel sms — the worker's only SMS marketing route) or
// "push_marketing". Both kinds are MARKETING-class in the worker's pacer
// table, so DND suppression (activity-side) and quiet-hours deferral
// (workflow-side, PacedSendWorkflow / GuardedPacedSend) apply AUTOMATICALLY.
//
// The respond path (POST /v1/surveys/respond) is PUBLIC: it resolves the
// tenant from the invite TOKEN, never from X-Tenant-Slug or a JWT (see the
// SECURITY note in store.go on the invite_token_access RLS policy).
//
// Trigger automation (ticket_resolved / booking_completed auto-send) is
// OUT of scope this wave (manual send only) — documented as a follow-up in
// docs/apps/surveys-voc.md. The trigger_kind column is stored for it.
//
// Anti-collision contract (SPEC-W20): this package is SELF-CONTAINED — it
// exposes NewStore/DialStore (mirror internal/devices) and
// RegisterRoutes(r, d, mw...) (see handlers.go); the integrator wires Deps,
// route mounting, config envs and the appgate entitlement flag
// (app_id "surveys-voc"). This package touches NO shared files.
//
// Config envs (documented for the integrator — no config code here; every
// one is optional and the app is functional with zero config):
//
//	SURVEYS_DATABASE_URL       postgres DSN for the dedicated store pool
//	                           (DialStore; integrator may fall back to DATABASE_URL)
//	SURVEYS_EVENTS_TOPIC       lifecycle CloudEvents topic
//	                           (default opendesk.surveys.events.v1; empty disables)
//	SURVEYS_NOTIFICATIONS_TOPIC notifications command topic
//	                           (default opendesk.notifications.outbox; empty
//	                           disables invite sends — invites stay queued)
//	USAGE_EVENTS_TOPIC         existing metering topic (opendesk.usage.events);
//	                           empty disables the survey_response_received meter
//	SURVEYS_PUBLIC_BASE_URL    public base URL embedded in invite messages
//	                           (default https://app.opendesk.ng/s; the invite
//	                           link is <base>?t=<token>)
package surveys

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrInvalidInput marks deterministic validation failures (400 at the API).
var ErrInvalidInput = errors.New("invalid survey input")

// ErrInvalidTransition marks survey status-machine violations (409).
var ErrInvalidTransition = errors.New("invalid survey status transition")

// ErrSurveyNotActive marks send attempts against non-active surveys (409).
var ErrSurveyNotActive = errors.New("survey is not active")

// ErrAlreadyAnswered marks a second submit against an answered invite
// (409 already_answered at the API).
var ErrAlreadyAnswered = errors.New("already_answered")

// ErrInviteExpired marks submits against an expired invite (410 at the API).
var ErrInviteExpired = errors.New("invite expired")

// ---------------------------------------------------------------------------
// Surveys
// ---------------------------------------------------------------------------

// Survey statuses (draft-active-paused-active-archived machine).
const (
	StatusDraft    = "draft"
	StatusActive   = "active"
	StatusPaused   = "paused"
	StatusArchived = "archived"
)

// Survey kinds.
const (
	KindNPS    = "nps"
	KindCSAT   = "csat"
	KindCES    = "ces"
	KindCustom = "custom"
)

// Trigger kinds. Only TriggerManual is actionable this wave (POST /send);
// ticket_resolved / booking_completed are stored for the automation
// follow-up.
const (
	TriggerManual           = "manual"
	TriggerTicketResolved   = "ticket_resolved"
	TriggerBookingCompleted = "booking_completed"
)

// Invite channels. Both map to MARKETING-class paced kinds in the
// notification-worker (DND/quiet-hours automatic).
const (
	ChannelSMS           = "sms"
	ChannelPushMarketing = "push_marketing"
)

// Question types.
const (
	QTypeRating = "rating"
	QTypeText   = "text"
	QTypeSingle = "single"
	QTypeMulti  = "multi"
)

// RatingScaleMin / RatingScaleMax bound rating answers. One scale covers
// every kind: NPS is 0-10 (promoters 9-10, detractors 0-6); CSAT/CES are
// conventionally 1-5 or 0-10 subsets of the same range (the mean is scale
// agnostic). Documented in docs/apps/surveys-voc.md.
const (
	RatingScaleMin = 0
	RatingScaleMax = 10
)

// Bounds for definitions and answers.
const (
	maxSurveyNameLen = 200
	maxQuestions     = 50
	maxQuestionIDLen = 64
	maxLabelLen      = 300
	maxOptions       = 20
	maxOptionLen     = 120
	maxTextAnswerLen = 4000
	maxAnswerKeys    = 100
	maxSendContacts  = 500
)

// Question is one entry of surveys.questions jsonb
// ({id, type, label, options, required}).
type Question struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"` // rating | text | single | multi
	Label    string   `json:"label"`
	Options  []string `json:"options,omitempty"` // single | multi only (>= 2)
	Required bool     `json:"required"`
}

// ValidateQuestions enforces the SPEC-W20 question contract: known types,
// non-empty unique ids (auto-assigned q1..qn when omitted), single/multi
// require >= 2 options, per-type field discipline.
func ValidateQuestions(qs []Question) error {
	if len(qs) == 0 {
		return fmt.Errorf("%w: at least one question is required", ErrInvalidInput)
	}
	if len(qs) > maxQuestions {
		return fmt.Errorf("%w: at most %d questions", ErrInvalidInput, maxQuestions)
	}
	seen := map[string]bool{}
	for i := range qs {
		q := &qs[i]
		q.ID = strings.TrimSpace(q.ID)
		if q.ID == "" {
			q.ID = fmt.Sprintf("q%d", i+1)
		}
		if len(q.ID) > maxQuestionIDLen {
			return fmt.Errorf("%w: questions[%d].id exceeds %d bytes", ErrInvalidInput, i, maxQuestionIDLen)
		}
		if seen[q.ID] {
			return fmt.Errorf("%w: duplicate question id %q", ErrInvalidInput, q.ID)
		}
		seen[q.ID] = true
		q.Label = strings.TrimSpace(q.Label)
		if q.Label == "" {
			return fmt.Errorf("%w: questions[%d].label is required", ErrInvalidInput, i)
		}
		if len(q.Label) > maxLabelLen {
			return fmt.Errorf("%w: questions[%d].label exceeds %d bytes", ErrInvalidInput, i, maxLabelLen)
		}
		switch q.Type {
		case QTypeRating, QTypeText:
			if len(q.Options) != 0 {
				return fmt.Errorf("%w: questions[%d] %s takes no options", ErrInvalidInput, i, q.Type)
			}
		case QTypeSingle, QTypeMulti:
			if len(q.Options) < 2 {
				return fmt.Errorf("%w: questions[%d] %s requires at least 2 options", ErrInvalidInput, i, q.Type)
			}
			if len(q.Options) > maxOptions {
				return fmt.Errorf("%w: questions[%d] allows at most %d options", ErrInvalidInput, i, maxOptions)
			}
			optSeen := map[string]bool{}
			for j, o := range q.Options {
				o = strings.TrimSpace(o)
				if o == "" {
					return fmt.Errorf("%w: questions[%d].options[%d] is empty", ErrInvalidInput, i, j)
				}
				if len(o) > maxOptionLen {
					return fmt.Errorf("%w: questions[%d].options[%d] exceeds %d bytes", ErrInvalidInput, i, j, maxOptionLen)
				}
				if optSeen[o] {
					return fmt.Errorf("%w: questions[%d] duplicate option %q", ErrInvalidInput, i, o)
				}
				optSeen[o] = true
				q.Options[j] = o
			}
		default:
			return fmt.Errorf("%w: questions[%d].type %q (want rating|text|single|multi)", ErrInvalidInput, i, q.Type)
		}
	}
	return nil
}

// Survey mirrors booking.surveys (SPEC-W20 Agent B).
type Survey struct {
	ID          uuid.UUID  `json:"id"`
	TenantID    uuid.UUID  `json:"tenant_id"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	Kind        string     `json:"kind"`
	Questions   []Question `json:"questions"`
	TriggerKind string     `json:"trigger_kind"`
	Channel     string     `json:"channel"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// CanTransition reports whether the survey status machine allows from->to
// (mirror of campaignstudio.CanTransition): draft->active->paused<->active
// ->archived; archived is terminal; draft may archive directly. A
// same-state request is not a transition — handlers treat it as a no-op
// before consulting this.
func CanTransition(from, to string) bool {
	switch from {
	case StatusDraft:
		return to == StatusActive || to == StatusArchived
	case StatusActive:
		return to == StatusPaused || to == StatusArchived
	case StatusPaused:
		return to == StatusActive || to == StatusArchived
	default: // archived
		return false
	}
}

// ValidateTransition returns ErrInvalidTransition when from->to is illegal.
func ValidateTransition(from, to string) error {
	if !validSurveyStatus(from) || !validSurveyStatus(to) {
		return fmt.Errorf("%w: %q -> %q (unknown status)", ErrInvalidTransition, from, to)
	}
	if !CanTransition(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	return nil
}

func validSurveyStatus(s string) bool {
	switch s {
	case StatusDraft, StatusActive, StatusPaused, StatusArchived:
		return true
	}
	return false
}

func validKind(k string) bool {
	switch k {
	case KindNPS, KindCSAT, KindCES, KindCustom:
		return true
	}
	return false
}

func validTriggerKind(k string) bool {
	switch k {
	case TriggerManual, TriggerTicketResolved, TriggerBookingCompleted:
		return true
	}
	return false
}

func validChannel(c string) bool {
	switch c {
	case ChannelSMS, ChannelPushMarketing:
		return true
	}
	return false
}

// Validate checks the minimal field set required for persistence
// (questions are validated separately via ValidateQuestions so the
// auto-assigned ids are stamped back onto the caller's slice first).
func (s *Survey) Validate() error {
	if s.TenantID == uuid.Nil {
		return fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	s.Name = strings.TrimSpace(s.Name)
	if s.Name == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	if len(s.Name) > maxSurveyNameLen {
		return fmt.Errorf("%w: name exceeds %d bytes", ErrInvalidInput, maxSurveyNameLen)
	}
	if !validSurveyStatus(s.Status) {
		return fmt.Errorf("%w: status %q (want draft|active|paused|archived)", ErrInvalidInput, s.Status)
	}
	if !validKind(s.Kind) {
		return fmt.Errorf("%w: kind %q (want nps|csat|ces|custom)", ErrInvalidInput, s.Kind)
	}
	if !validTriggerKind(s.TriggerKind) {
		return fmt.Errorf("%w: trigger_kind %q (want manual|ticket_resolved|booking_completed)", ErrInvalidInput, s.TriggerKind)
	}
	if !validChannel(s.Channel) {
		return fmt.Errorf("%w: channel %q (want sms|push_marketing)", ErrInvalidInput, s.Channel)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Invites
// ---------------------------------------------------------------------------

// Invite statuses.
const (
	InviteQueued   = "queued"
	InviteSent     = "sent"
	InviteAnswered = "answered"
	InviteExpired  = "expired"
)

// Invite mirrors booking.survey_invites. Token is a 128-bit random hex
// string (32 chars) — the PUBLIC respond capability; it is never accepted
// from a tenant header and never predictable.
type Invite struct {
	ID         uuid.UUID  `json:"id"`
	TenantID   uuid.UUID  `json:"tenant_id"`
	SurveyID   uuid.UUID  `json:"survey_id"`
	ContactID  uuid.UUID  `json:"contact_id"`
	Token      string     `json:"token"`
	Status     string     `json:"status"`
	SentAt     *time.Time `json:"sent_at"`
	AnsweredAt *time.Time `json:"answered_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

// NewToken returns a fresh 128-bit random hex invite token (crypto/rand).
func NewToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("draw invite token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ---------------------------------------------------------------------------
// Responses + answer validation + scoring
// ---------------------------------------------------------------------------

// Response mirrors booking.survey_responses. Answers is keyed by question
// id ({question_id: value}); value is a number (rating), a string
// (text/single) or an array of strings (multi). Score is the first rating
// answer for nps/csat/ces kinds (null otherwise / for custom).
type Response struct {
	ID          uuid.UUID      `json:"id"`
	TenantID    uuid.UUID      `json:"tenant_id"`
	SurveyID    uuid.UUID      `json:"survey_id"`
	InviteID    *uuid.UUID     `json:"invite_id"`
	ContactID   *uuid.UUID     `json:"contact_id"`
	Answers     map[string]any `json:"answers"`
	Score       *int           `json:"score"`
	SubmittedAt time.Time      `json:"submitted_at"`
}

// ValidateAnswers checks a respond payload against the survey definition:
// every required question must be answered, rating answers must be
// integral within [RatingScaleMin, RatingScaleMax], single answers must be
// one of the options, multi answers a (possibly empty, unless required)
// array of valid options, text answers bounded strings. Keys that do not
// match a question are IGNORED (forward-compatible with surveys edited
// after the invite went out). Returns the computed score (nil for custom
// kind or when no rating question was answered).
func ValidateAnswers(sv Survey, answers map[string]any) (*int, error) {
	if len(answers) > maxAnswerKeys {
		return nil, fmt.Errorf("%w: answers carry more than %d keys", ErrInvalidInput, maxAnswerKeys)
	}
	for _, q := range sv.Questions {
		v, present := answers[q.ID]
		if !present || answerEmpty(v) {
			if q.Required {
				return nil, fmt.Errorf("%w: question %q is required", ErrInvalidInput, q.ID)
			}
			continue
		}
		switch q.Type {
		case QTypeRating:
			n, ok := answerNumber(v)
			if !ok {
				return nil, fmt.Errorf("%w: question %q wants a numeric rating", ErrInvalidInput, q.ID)
			}
			if n != math.Trunc(n) || n < RatingScaleMin || n > RatingScaleMax {
				return nil, fmt.Errorf("%w: question %q rating must be an integer %d-%d", ErrInvalidInput, q.ID, RatingScaleMin, RatingScaleMax)
			}
		case QTypeText:
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("%w: question %q wants a text answer", ErrInvalidInput, q.ID)
			}
			if len(s) > maxTextAnswerLen {
				return nil, fmt.Errorf("%w: question %q text exceeds %d bytes", ErrInvalidInput, q.ID, maxTextAnswerLen)
			}
		case QTypeSingle:
			s, ok := v.(string)
			if !ok || !optionOf(q, s) {
				return nil, fmt.Errorf("%w: question %q answer must be one of its options", ErrInvalidInput, q.ID)
			}
		case QTypeMulti:
			arr, ok := v.([]any)
			if !ok {
				return nil, fmt.Errorf("%w: question %q wants an array of options", ErrInvalidInput, q.ID)
			}
			for _, item := range arr {
				s, ok := item.(string)
				if !ok || !optionOf(q, s) {
					return nil, fmt.Errorf("%w: question %q answers must be options of the question", ErrInvalidInput, q.ID)
				}
			}
		}
	}
	return ComputeScore(sv, answers), nil
}

// answerEmpty reports whether an answer counts as "not given" (null, empty
// string after trim, empty array).
func answerEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(t) == ""
	case []any:
		return len(t) == 0
	}
	return false
}

// answerNumber coerces a JSON-decoded numeric answer (encoding/json yields
// float64 with a default decoder, json.Number with UseNumber, ints when a
// Go caller builds the map directly).
func answerNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func optionOf(q Question, s string) bool {
	for _, o := range q.Options {
		if o == s {
			return true
		}
	}
	return false
}

// ComputeScore derives the response score: for nps/csat/ces kinds it is
// the FIRST rating question's answer (question order), nil when no rating
// question exists or it was left unanswered; custom kind never scores.
func ComputeScore(sv Survey, answers map[string]any) *int {
	if sv.Kind == KindCustom {
		return nil
	}
	for _, q := range sv.Questions {
		if q.Type != QTypeRating {
			continue
		}
		if v, ok := answers[q.ID]; ok {
			if n, ok := answerNumber(v); ok {
				score := int(n)
				return &score
			}
		}
		return nil // first rating question unanswered
	}
	return nil
}

// ---------------------------------------------------------------------------
// Results + VoC themes (pure computation — unit-tested without Postgres)
// ---------------------------------------------------------------------------

// OptionCount is one option's tally in a single/multi breakdown.
type OptionCount struct {
	Option string `json:"option"`
	Count  int    `json:"count"`
}

// QuestionBreakdown is the per-question result block for single/multi
// questions.
type QuestionBreakdown struct {
	ID          string        `json:"id"`
	Type        string        `json:"type"`
	Label       string        `json:"label"`
	AnswerCount int           `json:"answer_count"`
	Options     []OptionCount `json:"options"`
}

// Results is the GET /surveys/{id}/results payload: response count, score
// distribution, NPS (kind=nps) or mean (csat/ces), and per-question
// breakdowns for single/multi questions.
type Results struct {
	SurveyID          uuid.UUID           `json:"survey_id"`
	Kind              string              `json:"kind"`
	ResponseCount     int                 `json:"response_count"`
	ScoreDistribution map[string]int      `json:"score_distribution"`
	ScoredCount       int                 `json:"scored_count"`
	NPS               *float64            `json:"nps"` // kind=nps only
	Promoters         int                 `json:"promoters"`
	Passives          int                 `json:"passives"`
	Detractors        int                 `json:"detractors"`
	MeanScore         *float64            `json:"mean_score"`
	Questions         []QuestionBreakdown `json:"questions"`
}

// BuildResults aggregates one survey's responses (score + answers already
// loaded by the store). NPS = %promoters(9-10) - %detractors(0-6) over
// scored responses (SPEC-W20); mean is the plain average of scores.
func BuildResults(sv Survey, responses []Response) Results {
	res := Results{
		SurveyID:          sv.ID,
		Kind:              sv.Kind,
		ResponseCount:     len(responses),
		ScoreDistribution: map[string]int{},
		Questions:         []QuestionBreakdown{},
	}
	breakdowns := map[string]*QuestionBreakdown{}
	for _, q := range sv.Questions {
		if q.Type != QTypeSingle && q.Type != QTypeMulti {
			continue
		}
		qb := QuestionBreakdown{ID: q.ID, Type: q.Type, Label: q.Label, Options: []OptionCount{}}
		for _, o := range q.Options {
			qb.Options = append(qb.Options, OptionCount{Option: o})
		}
		breakdowns[q.ID] = &qb
	}
	var scoreSum int
	for _, r := range responses {
		if r.Score != nil {
			res.ScoredCount++
			scoreSum += *r.Score
			res.ScoreDistribution[fmt.Sprintf("%d", *r.Score)]++
			switch {
			case *r.Score >= 9:
				res.Promoters++
			case *r.Score <= 6:
				res.Detractors++
			default:
				res.Passives++
			}
		}
		for qid, v := range r.Answers {
			qb, ok := breakdowns[qid]
			if !ok {
				continue
			}
			counted := false
			add := func(s string) {
				for i := range qb.Options {
					if qb.Options[i].Option == s {
						qb.Options[i].Count++
						counted = true
						return
					}
				}
			}
			switch t := v.(type) {
			case string:
				add(t)
			case []any:
				for _, item := range t {
					if s, ok := item.(string); ok {
						add(s)
					}
				}
			}
			if counted {
				qb.AnswerCount++
			}
		}
	}
	if res.ScoredCount > 0 {
		mean := float64(scoreSum) / float64(res.ScoredCount)
		res.MeanScore = &mean
		if sv.Kind == KindNPS {
			nps := 100 * (float64(res.Promoters) - float64(res.Detractors)) / float64(res.ScoredCount)
			res.NPS = &nps
		}
	}
	for _, q := range sv.Questions {
		if qb, ok := breakdowns[q.ID]; ok {
			res.Questions = append(res.Questions, *qb)
		}
	}
	return res
}

// Theme is one naive VoC keyword-frequency row.
type Theme struct {
	Term  string `json:"term"`
	Count int    `json:"count"`
}

// stopwords is the small English stoplist stripped before counting. The
// themes endpoint is deliberately NAIVE keyword frequency — documented as
// such (not NLP) in docs/apps/surveys-voc.md.
var stopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "been": true, "but": true, "by": true, "for": true, "from": true,
	"had": true, "has": true, "have": true, "he": true, "her": true, "his": true,
	"i": true, "if": true, "in": true, "is": true, "it": true, "its": true,
	"me": true, "my": true, "no": true, "not": true, "of": true, "on": true,
	"or": true, "our": true, "she": true, "so": true, "that": true, "the": true,
	"their": true, "them": true, "they": true, "this": true, "to": true, "too": true,
	"us": true, "was": true, "we": true, "were": true, "what": true, "when": true,
	"with": true, "you": true, "your": true, "very": true, "really": true,
	"just": true, "about": true, "would": true, "could": true, "should": true,
	"there": true, "here": true, "all": true, "also": true, "am": true,
}

// minThemeTermLen drops one/two-letter tokens that survive the stoplist.
const minThemeTermLen = 3

// MaxThemes caps the themes response (SPEC-W20: top 20).
const MaxThemes = 20

// BuildThemes computes the naive keyword frequency over the given text
// answers: lowercase, split on non-letters/digits, strip stopwords and
// short tokens, return the top MaxThemes terms by count (ties broken
// alphabetically for determinism).
func BuildThemes(texts []string) []Theme {
	counts := map[string]int{}
	for _, text := range texts {
		words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
			return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
		})
		for _, tok := range words {
			if len(tok) < minThemeTermLen || stopwords[tok] {
				continue
			}
			counts[tok]++
		}
	}
	themes := make([]Theme, 0, len(counts))
	for term, n := range counts {
		themes = append(themes, Theme{Term: term, Count: n})
	}
	sort.Slice(themes, func(i, j int) bool {
		if themes[i].Count != themes[j].Count {
			return themes[i].Count > themes[j].Count
		}
		return themes[i].Term < themes[j].Term
	})
	if len(themes) > MaxThemes {
		themes = themes[:MaxThemes]
	}
	return themes
}

// TextAnswers extracts the text-question answers of one response for the
// theme pipeline (question types come from the survey definition; answers
// for questions no longer on the survey are ignored).
func TextAnswers(sv Survey, r Response) []string {
	textQ := map[string]bool{}
	for _, q := range sv.Questions {
		if q.Type == QTypeText {
			textQ[q.ID] = true
		}
	}
	out := []string{}
	for qid, v := range r.Answers {
		if !textQ[qid] {
			continue
		}
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}
