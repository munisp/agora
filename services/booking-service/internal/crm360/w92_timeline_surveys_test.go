package crm360

// SPEC-W92 B14: survey/CSAT responses appear in the CRM-360 timeline.
// Redirection: removing the timelineSurveyResponses call from Timeline
// fails this test (zero survey_response items); dropping the score
// projection fails the exact-summary assertions.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// w92SurveySchema mirrors the production surveys tables (surveys/store.go)
// WITHOUT their RLS, same as the other timeline fixture tables.
const w92SurveySchema = `
CREATE TABLE IF NOT EXISTS surveys (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL,
    name         TEXT NOT NULL,
    kind         TEXT NOT NULL DEFAULT 'nps',
    trigger_kind TEXT NOT NULL DEFAULT 'manual',
    channel      TEXT NOT NULL DEFAULT 'sms',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS survey_responses (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL,
    survey_id    UUID NOT NULL,
    invite_id    UUID,
    contact_id   UUID,
    answers      JSONB NOT NULL DEFAULT '{}',
    score        INTEGER,
    submitted_at TIMESTAMPTZ NOT NULL DEFAULT now()
);`

func TestW92TimelineSurveyResponses(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contactID := addContact(t, st, tenantID, "Survey Customer", "", "")

	if _, err := st.pool.Exec(ctx, w92SurveySchema); err != nil {
		t.Fatalf("survey schema: %v", err)
	}

	csatID := uuid.New()
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO surveys (id, tenant_id, name, kind) VALUES ($1,$2,$3,$4)`,
		csatID, tenantID, "Post-ticket CSAT", "csat"); err != nil {
		t.Fatalf("insert csat survey: %v", err)
	}
	npsID := uuid.New()
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO surveys (id, tenant_id, name, kind) VALUES ($1,$2,$3,$4)`,
		npsID, tenantID, "Quarterly NPS", "nps"); err != nil {
		t.Fatalf("insert nps survey: %v", err)
	}

	// Scored CSAT response + a score-less NPS comment response; a THIRD
	// response belongs to a different contact and must NOT appear.
	scoredID := uuid.New()
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO survey_responses (id, tenant_id, survey_id, contact_id, score, submitted_at)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		scoredID, tenantID, csatID, contactID, 5, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("insert scored response: %v", err)
	}
	unscoredID := uuid.New()
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO survey_responses (id, tenant_id, survey_id, contact_id, score, submitted_at)
		 VALUES ($1,$2,$3,$4,NULL,$5)`,
		unscoredID, tenantID, npsID, contactID, time.Now()); err != nil {
		t.Fatalf("insert unscored response: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO survey_responses (id, tenant_id, survey_id, contact_id, score)
		 VALUES ($1,$2,$3,$4,$5)`,
		uuid.New(), tenantID, csatID, uuid.New(), 1); err != nil {
		t.Fatalf("insert other-contact response: %v", err)
	}

	items, err := st.Timeline(ctx, tenantID, contactID, 50)
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	var surveyItems []TimelineItem
	for _, it := range items {
		if it.Kind == KindSurveyResponse {
			surveyItems = append(surveyItems, it)
		}
	}
	if len(surveyItems) != 2 {
		t.Fatalf("survey items = %d, want 2: %+v", len(surveyItems), items)
	}
	// Newest first: the unscored NPS response is the later one.
	if surveyItems[0].RefID != unscoredID.String() {
		t.Fatalf("first survey item = %+v, want the unscored NPS response", surveyItems[0])
	}
	if surveyItems[0].Summary != "Survey Quarterly NPS (nps)" {
		t.Fatalf("nil-score summary = %q", surveyItems[0].Summary)
	}
	if surveyItems[1].RefID != scoredID.String() ||
		surveyItems[1].Summary != "Survey Post-ticket CSAT (csat) — score 5" {
		t.Fatalf("scored item = %+v", surveyItems[1])
	}
}

func TestW92TimelineSurveyResponsesAbsentTables(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contactID := addContact(t, st, tenantID, "No Surveys", "", "")

	// Without the surveys tables the contributor degrades to nothing —
	// no error, no items (the base fixture schema has no surveys tables).
	items, err := st.Timeline(ctx, tenantID, contactID, 50)
	if err != nil {
		t.Fatalf("timeline without surveys tables: %v", err)
	}
	for _, it := range items {
		if it.Kind == KindSurveyResponse {
			t.Fatalf("survey item leaked without tables: %+v", it)
		}
	}
}
