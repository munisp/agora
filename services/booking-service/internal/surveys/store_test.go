package surveys

import (
	"context"
	"errors"
	"testing"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SPEC-W20 store tests run against embedded Postgres (same harness as the
// devices/helpdesk/campaignstudio tests; dedicated port 5564 avoids the
// postmaster.pid race with sibling packages under `go test ./...`;
// -short skips).

// testSchema provides the contacts fixture table (owned by the base schema
// in real deployments — mirrored here WITHOUT RLS so fixtures are easy to
// seed; the surveys tables' own RLS comes from ensureSchema and is
// exercised by TestRLSIsolation).
const testSchema = `
CREATE TABLE IF NOT EXISTS contacts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    name TEXT NOT NULL,
    phone TEXT,
    email TEXT,
    notes TEXT NOT NULL DEFAULT '',
    source TEXT,
    external_id TEXT
);`

func newTestStore(t *testing.T) *Store {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres surveys store test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_surveys_test").
		Port(5564).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })
	st, err := DialStore(context.Background(),
		"postgres://postgres:postgres@localhost:5564/booking_surveys_test?sslmode=disable")
	if err != nil {
		t.Fatalf("DialStore: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.pool.Exec(context.Background(), testSchema); err != nil {
		t.Fatalf("fixture schema: %v", err)
	}
	return st
}

func seedContact(t *testing.T, st *Store, tenantID uuid.UUID, name, phone string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := st.pool.Exec(context.Background(),
		`INSERT INTO contacts (id, tenant_id, name, phone) VALUES ($1,$2,$3,$4)`,
		id, tenantID, name, phone)
	if err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	return id
}

func mkSurvey(tenantID uuid.UUID) Survey {
	return Survey{
		TenantID:    tenantID,
		Name:        "NPS Q3",
		Status:      StatusDraft,
		Kind:        KindNPS,
		TriggerKind: TriggerManual,
		Channel:     ChannelSMS,
		Questions: []Question{
			{ID: "nps", Type: QTypeRating, Label: "How likely?", Required: true},
			{ID: "why", Type: QTypeText, Label: "Why?"},
			{ID: "channel", Type: QTypeSingle, Label: "Channel", Options: []string{"sms", "app"}},
		},
	}
}

func TestSurveyCRUD(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID, other := uuid.New(), uuid.New()

	sv := mkSurvey(tenantID)
	if err := st.CreateSurvey(ctx, &sv); err != nil {
		t.Fatalf("CreateSurvey: %v", err)
	}
	if sv.ID == uuid.Nil || sv.CreatedAt.IsZero() {
		t.Fatalf("survey not stamped: %+v", sv)
	}

	got, err := st.GetSurvey(ctx, tenantID, sv.ID)
	if err != nil || got.Name != "NPS Q3" || len(got.Questions) != 3 {
		t.Fatalf("GetSurvey = %+v, %v", got, err)
	}
	if _, err := st.GetSurvey(ctx, other, sv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant get = %v, want ErrNotFound", err)
	}
	if _, err := st.GetSurvey(ctx, tenantID, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing get = %v, want ErrNotFound", err)
	}

	// Filters.
	cs := mkSurvey(tenantID)
	cs.Name, cs.Kind = "CSAT", KindCSAT
	if err := st.CreateSurvey(ctx, &cs); err != nil {
		t.Fatalf("create csat: %v", err)
	}
	list, err := st.ListSurveys(ctx, tenantID, "", "")
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %d, %v", len(list), err)
	}
	list, err = st.ListSurveys(ctx, tenantID, "", KindCSAT)
	if err != nil || len(list) != 1 || list[0].ID != cs.ID {
		t.Fatalf("kind filter = %+v, %v", list, err)
	}
	list, err = st.ListSurveys(ctx, tenantID, StatusActive, "")
	if err != nil || len(list) != 0 {
		t.Fatalf("status filter = %d, %v", len(list), err)
	}

	// Update + stats rollup.
	sv.Status = StatusActive
	if err := st.UpdateSurvey(ctx, &sv); err != nil {
		t.Fatalf("UpdateSurvey: %v", err)
	}
	got, _ = st.GetSurvey(ctx, tenantID, sv.ID)
	if got.Status != StatusActive || got.UpdatedAt.Before(got.CreatedAt) {
		t.Fatalf("updated = %+v", got)
	}
	stats, err := st.Stats(ctx, tenantID, sv.ID)
	if err != nil || stats.Responses != 0 || stats.InvitesSent != 0 {
		t.Fatalf("stats = %+v, %v", stats, err)
	}
}

func TestInvitesAndSubmit(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	ada := seedContact(t, st, tenantID, "Ada", "+234801")
	bob := seedContact(t, st, tenantID, "Bob", "") // no phone -> skipped on sms
	ghost := uuid.New()                            // unknown -> skipped

	sv := mkSurvey(tenantID)
	sv.Status = StatusActive
	if err := st.CreateSurvey(ctx, &sv); err != nil {
		t.Fatalf("create: %v", err)
	}

	res, err := st.CreateInvites(ctx, tenantID, sv.ID, ChannelSMS, []uuid.UUID{ada, bob, ghost, ada})
	if err != nil {
		t.Fatalf("CreateInvites: %v", err)
	}
	if len(res.Invites) != 2 { // ada twice = two invites (dedupe is a handler concern)
		t.Fatalf("invites = %d, want 2", len(res.Invites))
	}
	if len(res.Skipped) != 2 {
		t.Fatalf("skipped = %+v, want 2", res.Skipped)
	}
	reasons := map[string]string{}
	for _, sk := range res.Skipped {
		reasons[sk.ContactID.String()] = sk.Reason
	}
	if reasons[bob.String()] != "no_phone" || reasons[ghost.String()] != "not_found" {
		t.Fatalf("skip reasons = %v", reasons)
	}
	inv := res.Invites[0]
	if inv.Status != InviteQueued || len(inv.Token) != 32 {
		t.Fatalf("invite = %+v", inv)
	}
	if res.Contacts[ada].Phone != "+234801" {
		t.Fatalf("resolved contact = %+v", res.Contacts[ada])
	}

	// push_marketing needs no phone: bob gets an invite there.
	res2, err := st.CreateInvites(ctx, tenantID, sv.ID, ChannelPushMarketing, []uuid.UUID{bob})
	if err != nil || len(res2.Invites) != 1 {
		t.Fatalf("push invites = %+v, %v", res2, err)
	}

	if err := st.MarkInviteSent(ctx, tenantID, inv.ID); err != nil {
		t.Fatalf("MarkInviteSent: %v", err)
	}

	// Unknown token -> ErrNotFound (the token RLS policy hides everything).
	if _, err := st.SubmitResponse(ctx, "deadbeefdeadbeefdeadbeefdeadbeef", map[string]any{"nps": float64(9)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown token = %v, want ErrNotFound", err)
	}

	// Invalid answers -> ErrInvalidInput (missing required rating).
	if _, err := st.SubmitResponse(ctx, inv.Token, map[string]any{"why": "x"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid answers = %v, want ErrInvalidInput", err)
	}

	// Happy submit: score computed, invite flipped, response persisted.
	out, err := st.SubmitResponse(ctx, inv.Token, map[string]any{
		"nps": float64(9), "why": "great", "channel": "sms",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if out.Response.Score == nil || *out.Response.Score != 9 {
		t.Fatalf("score = %v", out.Response.Score)
	}
	if out.Invite.Status != InviteAnswered || out.Invite.AnsweredAt == nil {
		t.Fatalf("invite = %+v", out.Invite)
	}
	if out.Response.InviteID == nil || *out.Response.InviteID != inv.ID {
		t.Fatalf("response invite link = %+v", out.Response.InviteID)
	}

	// Replay -> ErrAlreadyAnswered (idempotent per invite).
	if _, err := st.SubmitResponse(ctx, inv.Token, map[string]any{"nps": float64(1)}); !errors.Is(err, ErrAlreadyAnswered) {
		t.Fatalf("replay = %v, want ErrAlreadyAnswered", err)
	}

	// Expired invite -> ErrInviteExpired.
	exp := res.Invites[1]
	if _, err := st.pool.Exec(ctx, `UPDATE survey_invites SET status='expired' WHERE id=$1`, exp.ID); err != nil {
		t.Fatalf("expire invite: %v", err)
	}
	if _, err := st.SubmitResponse(ctx, exp.Token, map[string]any{"nps": float64(5)}); !errors.Is(err, ErrInviteExpired) {
		t.Fatalf("expired = %v, want ErrInviteExpired", err)
	}

	// Stats + ListResponses roll up.
	stats, err := st.Stats(ctx, tenantID, sv.ID)
	if err != nil || stats.Responses != 1 || stats.InvitesAnswered != 1 || stats.InvitesExpired != 1 {
		t.Fatalf("stats = %+v, %v", stats, err)
	}
	responses, total, truncated, err := st.ListResponses(ctx, tenantID, sv.ID)
	if err != nil || total != 1 || truncated || len(responses) != 1 {
		t.Fatalf("list responses = %d/%d/%v, %v", len(responses), total, truncated, err)
	}

	// Theme texts pick up the text answer only.
	texts, scanned, err := st.ThemeTexts(ctx, tenantID, &sv.ID)
	if err != nil || scanned != 1 || len(texts) != 1 || texts[0] != "great" {
		t.Fatalf("themes = %v (%d), %v", texts, scanned, err)
	}
	if _, _, err := st.ThemeTexts(ctx, tenantID, &[]uuid.UUID{uuid.New()}[0]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("themes unknown survey = %v, want ErrNotFound", err)
	}
}

// TestRLSIsolation proves tenant scoping is enforced by the database, not
// the app: a restricted role sees NOTHING without app.tenant_id, exactly
// its own rows with it, and exactly ONE invite row when only the public
// app.invite_token GUC is set (the invite_token_access policy).
func TestRLSIsolation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()

	sa := mkSurvey(tenantA)
	if err := st.CreateSurvey(ctx, &sa); err != nil {
		t.Fatalf("create A: %v", err)
	}
	sb := mkSurvey(tenantB)
	if err := st.CreateSurvey(ctx, &sb); err != nil {
		t.Fatalf("create B: %v", err)
	}
	ca := seedContact(t, st, tenantA, "A", "+234800")
	cb := seedContact(t, st, tenantB, "B", "+234801")
	ra, err := st.CreateInvites(ctx, tenantA, sa.ID, ChannelSMS, []uuid.UUID{ca})
	if err != nil || len(ra.Invites) != 1 {
		t.Fatalf("invites A: %v", err)
	}
	rb, err := st.CreateInvites(ctx, tenantB, sb.ID, ChannelSMS, []uuid.UUID{cb})
	if err != nil || len(rb.Invites) != 1 {
		t.Fatalf("invites B: %v", err)
	}
	tokenB := rb.Invites[0].Token

	if _, err := st.pool.Exec(ctx, `
		DO $$ BEGIN
		    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'surveys_rls') THEN
		        CREATE ROLE surveys_rls LOGIN PASSWORD 'surveys_rls';
		    END IF;
		END $$;
		GRANT USAGE ON SCHEMA public TO surveys_rls;
		GRANT SELECT, INSERT, UPDATE, DELETE ON surveys, survey_invites, survey_responses TO surveys_rls;`); err != nil {
		t.Fatalf("create rls role: %v", err)
	}
	pool, err := pgxpool.New(ctx,
		"postgres://surveys_rls:surveys_rls@localhost:5564/booking_surveys_test?sslmode=disable")
	if err != nil {
		t.Fatalf("dial rls role: %v", err)
	}
	defer pool.Close()

	// No tenant context, no token → zero rows everywhere.
	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM surveys`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("surveys visible without context: %d (%v)", n, err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM survey_invites`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("invites visible without context: %d (%v)", n, err)
	}

	// Token-only context (the PUBLIC respond path) → exactly the one
	// invite carrying that token; nothing else.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT set_config('app.invite_token', $1, true)`, tokenB); err != nil {
		t.Fatalf("set token: %v", err)
	}
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM survey_invites`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("token context sees %d invites (%v), want 1", n, err)
	}
	var gotToken string
	if err := tx.QueryRow(ctx, `SELECT token FROM survey_invites`).Scan(&gotToken); err != nil || gotToken != tokenB {
		t.Fatalf("token row = %q, %v", gotToken, err)
	}
	// ...but surveys stay invisible until the tenant GUC is set.
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM surveys`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("surveys visible with token only: %d (%v)", n, err)
	}
	// ...and the token policy is SELECT-only: UPDATE matches nothing.
	tag, err := tx.Exec(ctx, `UPDATE survey_invites SET status='expired' WHERE token=$1`, tokenB)
	if err != nil {
		t.Fatalf("token update: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Fatalf("token-only UPDATE affected %d rows", tag.RowsAffected())
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// Tenant A context → exactly A's rows; B's row invisible/untouchable.
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantA.String()); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM surveys`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("tenant A sees %d surveys (%v), want 1", n, err)
	}
	var cross string
	if err := tx.QueryRow(ctx, `SELECT name FROM surveys WHERE id=$1`, sb.ID).Scan(&cross); err == nil {
		t.Fatalf("cross-tenant survey visible: %q", cross)
	}
	tag, err = tx.Exec(ctx, `UPDATE surveys SET name='pwned' WHERE id=$1`, sb.ID)
	if err != nil {
		t.Fatalf("cross update: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Fatalf("cross-tenant UPDATE affected %d rows", tag.RowsAffected())
	}
	// INSERT under tenant A context carrying tenant B's id is rejected by
	// the WITH CHECK side of the policy.
	if _, err := tx.Exec(ctx, `INSERT INTO surveys (tenant_id, name) VALUES ($1, 'x')`, tenantB); err == nil {
		t.Fatal("cross-tenant INSERT accepted")
	}
}
