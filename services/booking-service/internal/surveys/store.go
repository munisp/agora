package surveys

// Store persists surveys, invites and responses. Same packaging idiom as
// the W16 devices / W19 workorders packages: NewStore wraps an existing
// pool (tests), DialStore opens a small dedicated pool (integrator wiring
// path). maxConns 4: survey admin + public respond are low-QPS paths.
//
// SECURITY (PUBLIC respond path): survey_invites carries a SECOND RLS
// policy, invite_token_access (FOR SELECT), which makes exactly the rows
// whose token equals the request-scoped GUC app.invite_token visible. The
// public SubmitResponse flow sets that GUC to the presented token, reads
// the invite (404 when the token is unknown — the policy hides every other
// row), THEN sets the standard app.tenant_id GUC from the invite row and
// proceeds fully tenant-scoped. The tenant is therefore resolved from the
// token, never from X-Tenant-Slug or a JWT. Tokens are 128-bit random hex
// (unguessable), the token policy is SELECT-only (writes always require
// the tenant context), and the respond mutation is guarded by the invite
// status flip so a replayed submit can never double-insert.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store wraps a pgx pool.
type Store struct {
	pool    *pgxpool.Pool
	ownPool bool // true when opened via DialStore
}

// NewStore wraps an existing pool and ensures the schema.
func NewStore(ctx context.Context, pool *pgxpool.Pool) (*Store, error) {
	s := &Store{pool: pool}
	if err := s.ensureSchema(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// DialStore opens a small dedicated pool and ensures the schema.
func DialStore(ctx context.Context, databaseURL string) (*Store, error) {
	poolCfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse postgres config: %w", err)
	}
	poolCfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	s, err := NewStore(ctx, pool)
	if err != nil {
		pool.Close()
		return nil, err
	}
	s.ownPool = true
	return s, nil
}

// Close releases the pool when this store opened it.
func (s *Store) Close() {
	if s.ownPool {
		s.pool.Close()
	}
}

// ensureSchema bootstraps the surveys tables idempotently (contract §1:
// RLS enabled + forced with the tenant_isolation policy, guarded by a
// pg_policies existence check — mirrors internal/devices/store.go).
//
// NOTE (RLS): bootstrap DDL is a migration path, not a tenant query — it
// intentionally runs outside withTenant.
func (s *Store) ensureSchema(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS surveys (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL,
    name         TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'draft'
                 CHECK (status IN ('draft','active','paused','archived')),
    kind         TEXT NOT NULL DEFAULT 'nps'
                 CHECK (kind IN ('nps','csat','ces','custom')),
    questions    JSONB NOT NULL DEFAULT '[]',
    trigger_kind TEXT NOT NULL DEFAULT 'manual'
                 CHECK (trigger_kind IN ('manual','ticket_resolved','booking_completed')),
    channel      TEXT NOT NULL DEFAULT 'sms'
                 CHECK (channel IN ('sms','push_marketing')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_surveys_status ON surveys (tenant_id, status);
ALTER TABLE surveys ENABLE ROW LEVEL SECURITY;
ALTER TABLE surveys FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'surveys' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON surveys
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS survey_invites (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL,
    survey_id   UUID NOT NULL,
    contact_id  UUID NOT NULL,
    token       TEXT NOT NULL UNIQUE,
    status      TEXT NOT NULL DEFAULT 'queued'
                CHECK (status IN ('queued','sent','answered','expired')),
    sent_at     TIMESTAMPTZ,
    answered_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_survey_invites_survey ON survey_invites (tenant_id, survey_id, status);
CREATE INDEX IF NOT EXISTS idx_survey_invites_contact ON survey_invites (tenant_id, contact_id);
ALTER TABLE survey_invites ENABLE ROW LEVEL SECURITY;
ALTER TABLE survey_invites FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'survey_invites' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON survey_invites
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END $$;
-- Public respond path (see the SECURITY note at the top of this file):
-- SELECT-only, matches only the row whose token the caller presented.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'survey_invites' AND policyname = 'invite_token_access') THEN
        CREATE POLICY invite_token_access ON survey_invites FOR SELECT
            USING (token = nullif(current_setting('app.invite_token', true), ''));
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS survey_responses (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL,
    survey_id    UUID NOT NULL,
    invite_id    UUID,
    contact_id   UUID,
    answers      JSONB NOT NULL DEFAULT '{}',
    score        INTEGER,
    submitted_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_survey_responses_survey ON survey_responses (tenant_id, survey_id);
CREATE INDEX IF NOT EXISTS idx_survey_responses_invite ON survey_responses (invite_id);
ALTER TABLE survey_responses ENABLE ROW LEVEL SECURITY;
ALTER TABLE survey_responses FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'survey_responses' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON survey_responses
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END $$;

-- Shared transactional outbox (identical shape to the base schema; the
-- IF NOT EXISTS makes standalone tests self-sufficient and real
-- deployments a no-op). Not RLS-scoped: the dispatcher drains cross-tenant.
CREATE TABLE IF NOT EXISTS outbox (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_id UUID NOT NULL,
    topic        TEXT NOT NULL,
    payload      JSONB NOT NULL,
    sent_at      TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_outbox_unsent ON outbox (id) WHERE sent_at IS NULL;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure surveys tables: %w", err)
	}
	return nil
}

// withTenant runs fn inside a transaction with `SET LOCAL app.tenant_id`
// applied (mirrors devices.Store.withTenant) so the RLS tenant_isolation
// policy scopes every statement of fn to the given tenant.
func (s *Store) withTenant(ctx context.Context, tenantID uuid.UUID, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID.String()); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ErrNotFound is returned when a row does not exist (mirrors
// devices.ErrNotFound so the API maps it to 404).
var ErrNotFound = errors.New("not found")

// EnqueueOutbox appends one row to the transactional outbox (mirrors
// referrals.PayoutStore.EnqueueOutbox; lifecycle events, the metered usage
// record and the per-invite PacedSend commands all ride this path).
//
// NOTE (RLS): the outbox table is not tenant-scoped (no RLS policy — the
// dispatcher drains it cross-tenant by design).
func (s *Store) EnqueueOutbox(ctx context.Context, aggregateID uuid.UUID, topic string, payload []byte) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO outbox (aggregate_id, topic, payload) VALUES ($1,$2,$3)`,
		aggregateID, topic, payload); err != nil {
		return fmt.Errorf("enqueue outbox: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Surveys CRUD
// ---------------------------------------------------------------------------

const surveyCols = `id, tenant_id, name, status, kind, questions, trigger_kind, channel, created_at, updated_at`

func scanSurvey(row pgx.Row) (Survey, error) {
	var sv Survey
	var questions []byte
	err := row.Scan(&sv.ID, &sv.TenantID, &sv.Name, &sv.Status, &sv.Kind,
		&questions, &sv.TriggerKind, &sv.Channel, &sv.CreatedAt, &sv.UpdatedAt)
	if err != nil {
		return sv, err
	}
	sv.Questions = []Question{}
	if len(questions) > 0 {
		if err := json.Unmarshal(questions, &sv.Questions); err != nil {
			return sv, fmt.Errorf("decode questions: %w", err)
		}
	}
	return sv, nil
}

// CreateSurvey inserts one survey (validated by the caller) and stamps
// id/timestamps back onto sv.
func (s *Store) CreateSurvey(ctx context.Context, sv *Survey) error {
	questions, err := json.Marshal(sv.Questions)
	if err != nil {
		return fmt.Errorf("encode questions: %w", err)
	}
	const q = `INSERT INTO surveys (tenant_id, name, status, kind, questions, trigger_kind, channel)
		           VALUES ($1,$2,$3,$4,$5,$6,$7)
		           RETURNING ` + surveyCols
	return s.withTenant(ctx, sv.TenantID, func(tx pgx.Tx) error {
		row, err := scanSurvey(tx.QueryRow(ctx, q,
			sv.TenantID, sv.Name, sv.Status, sv.Kind, questions, sv.TriggerKind, sv.Channel))
		if err != nil {
			return fmt.Errorf("insert survey: %w", err)
		}
		*sv = row
		return nil
	})
}

// GetSurvey loads one survey scoped to the tenant (ErrNotFound when
// missing or cross-tenant).
func (s *Store) GetSurvey(ctx context.Context, tenantID, id uuid.UUID) (Survey, error) {
	var sv Survey
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+surveyCols+` FROM surveys WHERE tenant_id=$1 AND id=$2`, tenantID, id)
		var err error
		sv, err = scanSurvey(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return sv, err
}

// ListSurveys returns the tenant's surveys (newest first), optionally
// filtered by status / kind ("" disables a filter).
func (s *Store) ListSurveys(ctx context.Context, tenantID uuid.UUID, status, kind string) ([]Survey, error) {
	q := `SELECT ` + surveyCols + ` FROM surveys WHERE tenant_id=$1`
	args := []any{tenantID}
	n := 1
	if status != "" {
		n++
		q += fmt.Sprintf(` AND status=$%d`, n)
		args = append(args, status)
	}
	if kind != "" {
		n++
		q += fmt.Sprintf(` AND kind=$%d`, n)
		args = append(args, kind)
	}
	q += ` ORDER BY created_at DESC LIMIT 500`
	out := []Survey{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			sv, err := scanSurvey(rows)
			if err != nil {
				return err
			}
			out = append(out, sv)
		}
		return rows.Err()
	})
	return out, err
}

// UpdateSurvey persists every mutable field of sv (status transitions are
// validated by the caller). updated_at is stamped; ErrNotFound when the
// row is missing/cross-tenant.
func (s *Store) UpdateSurvey(ctx context.Context, sv *Survey) error {
	questions, err := json.Marshal(sv.Questions)
	if err != nil {
		return fmt.Errorf("encode questions: %w", err)
	}
	return s.withTenant(ctx, sv.TenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE surveys SET
				name=$3, status=$4, kind=$5, questions=$6, trigger_kind=$7, channel=$8, updated_at=now()
			WHERE tenant_id=$1 AND id=$2`,
			sv.TenantID, sv.ID, sv.Name, sv.Status, sv.Kind, questions, sv.TriggerKind, sv.Channel)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// SurveyStats rolls up invite counts + the response count for one survey
// (surfaced by GET /surveys/{id} for the dashboard header).
type SurveyStats struct {
	InvitesQueued   int `json:"invites_queued"`
	InvitesSent     int `json:"invites_sent"`
	InvitesAnswered int `json:"invites_answered"`
	InvitesExpired  int `json:"invites_expired"`
	Responses       int `json:"responses"`
}

// Stats returns the invite/response rollup for one survey.
func (s *Store) Stats(ctx context.Context, tenantID, surveyID uuid.UUID) (SurveyStats, error) {
	var st SurveyStats
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT
				COUNT(*) FILTER (WHERE status='queued'),
				COUNT(*) FILTER (WHERE status='sent'),
				COUNT(*) FILTER (WHERE status='answered'),
				COUNT(*) FILTER (WHERE status='expired')
			FROM survey_invites WHERE tenant_id=$1 AND survey_id=$2`, tenantID, surveyID).
			Scan(&st.InvitesQueued, &st.InvitesSent, &st.InvitesAnswered, &st.InvitesExpired); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM survey_responses WHERE tenant_id=$1 AND survey_id=$2`,
			tenantID, surveyID).Scan(&st.Responses)
	})
	return st, err
}

// ---------------------------------------------------------------------------
// Invites (send flow)
// ---------------------------------------------------------------------------

const inviteCols = `id, tenant_id, survey_id, contact_id, token, status, sent_at, answered_at, created_at`

func scanInvite(row pgx.Row) (Invite, error) {
	var inv Invite
	err := row.Scan(&inv.ID, &inv.TenantID, &inv.SurveyID, &inv.ContactID,
		&inv.Token, &inv.Status, &inv.SentAt, &inv.AnsweredAt, &inv.CreatedAt)
	return inv, err
}

// ResolvedContact carries the contact attributes needed to render one
// invite message (name/phone; push resolves device tokens by contact id
// worker-side).
type ResolvedContact struct {
	ContactID uuid.UUID `json:"contact_id"`
	Name      string    `json:"name"`
	Phone     string    `json:"phone"`
}

// SkippedContact reports one requested contact that got no invite.
type SkippedContact struct {
	ContactID uuid.UUID `json:"contact_id"`
	Reason    string    `json:"reason"` // not_found | no_phone
}

// CreateInvitesResult is the CreateInvites outcome.
type CreateInvitesResult struct {
	Invites []Invite
	// Contacts maps contact id to its resolved attributes (only for
	// contacts that got an invite) — the send handler renders messages.
	Contacts map[uuid.UUID]ResolvedContact
	Skipped  []SkippedContact
}

// CreateInvites resolves the requested contacts (tenant-scoped) and
// inserts one QUEUED invite per resolvable contact, each with a fresh
// 128-bit token. Contacts that do not exist in the tenant are skipped
// (not_found); on the sms channel, contacts without a phone are skipped
// (no_phone — push_marketing needs no phone: device tokens resolve by
// contact id worker-side, and the phone only feeds the DND guard).
func (s *Store) CreateInvites(ctx context.Context, tenantID, surveyID uuid.UUID, channel string, contactIDs []uuid.UUID) (CreateInvitesResult, error) {
	res := CreateInvitesResult{
		Invites:  []Invite{},
		Contacts: map[uuid.UUID]ResolvedContact{},
		Skipped:  []SkippedContact{},
	}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		// Resolve every requested contact in one query.
		rows, err := tx.Query(ctx,
			`SELECT id, name, COALESCE(phone,'') FROM contacts WHERE tenant_id=$1 AND id = ANY($2)`,
			tenantID, contactIDs)
		if err != nil {
			return err
		}
		found := map[uuid.UUID]ResolvedContact{}
		defer rows.Close()
		for rows.Next() {
			var c ResolvedContact
			if err := rows.Scan(&c.ContactID, &c.Name, &c.Phone); err != nil {
				return err
			}
			found[c.ContactID] = c
		}
		if err := rows.Err(); err != nil {
			return err
		}

		const ins = `INSERT INTO survey_invites (tenant_id, survey_id, contact_id, token)
			             VALUES ($1,$2,$3,$4)
			             RETURNING ` + inviteCols
		for _, cid := range contactIDs {
			c, ok := found[cid]
			if !ok {
				res.Skipped = append(res.Skipped, SkippedContact{ContactID: cid, Reason: "not_found"})
				continue
			}
			if channel == ChannelSMS && c.Phone == "" {
				res.Skipped = append(res.Skipped, SkippedContact{ContactID: cid, Reason: "no_phone"})
				continue
			}
			// Token collision is cryptographically negligible; retry a few
			// times anyway so a unique-violation can never 500 the send.
			var inv Invite
			inserted := false
			for attempt := 0; attempt < 3 && !inserted; attempt++ {
				token, err := NewToken()
				if err != nil {
					return err
				}
				inv, err = scanInvite(tx.QueryRow(ctx, ins, tenantID, surveyID, cid, token))
				if err != nil {
					if isUniqueViolation(err) {
						continue
					}
					return fmt.Errorf("insert invite: %w", err)
				}
				inserted = true
			}
			if !inserted {
				return fmt.Errorf("insert invite: token collision after retries")
			}
			res.Invites = append(res.Invites, inv)
			res.Contacts[cid] = c
		}
		return nil
	})
	return res, err
}

// isUniqueViolation reports a Postgres unique-constraint error (SQLSTATE
// 23505) without dragging in pgconn for one code.
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}

// MarkInviteSent flips one queued invite to sent (sent_at stamped). A
// no-op when the invite is no longer queued (e.g. already answered).
func (s *Store) MarkInviteSent(ctx context.Context, tenantID, inviteID uuid.UUID) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE survey_invites SET status='sent', sent_at=now()
			 WHERE tenant_id=$1 AND id=$2 AND status='queued'`,
			tenantID, inviteID)
		return err
	})
}

// ---------------------------------------------------------------------------
// Public respond flow (token-resolved tenant — see the SECURITY note)
// ---------------------------------------------------------------------------

// SubmitResult bundles the persisted response with its invite + survey so
// the handler can emit the answered event / metering without re-reading.
type SubmitResult struct {
	Response Response `json:"response"`
	Invite   Invite   `json:"invite"`
	Survey   Survey   `json:"survey"`
}

// SubmitResponse persists one public response:
//
//  1. set app.invite_token and read the invite BY TOKEN (the
//     invite_token_access RLS policy exposes only that row; an unknown
//     token sees nothing → ErrNotFound → 404);
//  2. reject answered (ErrAlreadyAnswered → 409) and expired
//     (ErrInviteExpired → 410) invites;
//  3. set app.tenant_id from the INVITE ROW and load the survey
//     tenant-scoped;
//  4. validate answers against the survey definition (ErrInvalidInput →
//  400. and compute the score;
//  5. insert the response and flip the invite to answered in ONE
//     transaction — the status-guarded UPDATE makes a concurrent double
//     submit lose the race and map to ErrAlreadyAnswered (idempotent per
//     invite).
func (s *Store) SubmitResponse(ctx context.Context, token string, answers map[string]any) (SubmitResult, error) {
	var out SubmitResult
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx, `SELECT set_config('app.invite_token', $1, true)`, token); err != nil {
		return out, fmt.Errorf("set invite token context: %w", err)
	}
	inv, err := scanInvite(tx.QueryRow(ctx,
		`SELECT `+inviteCols+` FROM survey_invites WHERE token=$1`, token))
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	switch inv.Status {
	case InviteAnswered:
		return out, ErrAlreadyAnswered
	case InviteExpired:
		return out, ErrInviteExpired
	}

	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, inv.TenantID.String()); err != nil {
		return out, fmt.Errorf("set tenant context: %w", err)
	}
	sv, err := scanSurvey(tx.QueryRow(ctx,
		`SELECT `+surveyCols+` FROM surveys WHERE tenant_id=$1 AND id=$2`, inv.TenantID, inv.SurveyID))
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}

	score, err := ValidateAnswers(sv, answers)
	if err != nil {
		return out, err
	}
	payload, err := json.Marshal(answers)
	if err != nil {
		return out, fmt.Errorf("encode answers: %w", err)
	}
	var resp Response
	var answersRaw []byte
	err = tx.QueryRow(ctx, `INSERT INTO survey_responses
			(tenant_id, survey_id, invite_id, contact_id, answers, score)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id, tenant_id, survey_id, invite_id, contact_id, answers, score, submitted_at`,
		inv.TenantID, inv.SurveyID, inv.ID, inv.ContactID, payload, score).
		Scan(&resp.ID, &resp.TenantID, &resp.SurveyID, &resp.InviteID, &resp.ContactID,
			&answersRaw, &resp.Score, &resp.SubmittedAt)
	if err != nil {
		return out, fmt.Errorf("insert response: %w", err)
	}
	resp.Answers = map[string]any{}
	if len(answersRaw) > 0 {
		if err := json.Unmarshal(answersRaw, &resp.Answers); err != nil {
			return out, fmt.Errorf("decode stored answers: %w", err)
		}
	}
	inv.Status = InviteAnswered
	now := resp.SubmittedAt
	inv.AnsweredAt = &now

	tag, err := tx.Exec(ctx,
		`UPDATE survey_invites SET status='answered', answered_at=now()
		 WHERE tenant_id=$1 AND id=$2 AND status IN ('queued','sent')`,
		inv.TenantID, inv.ID)
	if err != nil {
		return out, err
	}
	if tag.RowsAffected() == 0 {
		// Lost a concurrent-submit race: the other tx already answered.
		return out, ErrAlreadyAnswered
	}
	if err := tx.Commit(ctx); err != nil {
		return out, err
	}
	out = SubmitResult{Response: resp, Invite: inv, Survey: sv}
	return out, nil
}

// ---------------------------------------------------------------------------
// Responses (results + themes)
// ---------------------------------------------------------------------------

// maxResultResponses caps the responses scanned for results/themes
// aggregation (the count itself stays exact via COUNT(*)).
const maxResultResponses = 10000

// ListResponses returns up to maxResultResponses responses of one survey
// (oldest first) plus the EXACT total count; truncated is true when the
// cap clipped the aggregation input.
func (s *Store) ListResponses(ctx context.Context, tenantID, surveyID uuid.UUID) (responses []Response, total int, truncated bool, err error) {
	responses = []Response{}
	err = s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM survey_responses WHERE tenant_id=$1 AND survey_id=$2`,
			tenantID, surveyID).Scan(&total); err != nil {
			return err
		}
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, survey_id, invite_id, contact_id, answers, score, submitted_at
			   FROM survey_responses WHERE tenant_id=$1 AND survey_id=$2
			   ORDER BY submitted_at ASC LIMIT $3`,
			tenantID, surveyID, maxResultResponses)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanResponse(rows)
			if err != nil {
				return err
			}
			responses = append(responses, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, 0, false, err
	}
	return responses, total, total > len(responses), nil
}

func scanResponse(row pgx.Row) (Response, error) {
	var r Response
	var answers []byte
	err := row.Scan(&r.ID, &r.TenantID, &r.SurveyID, &r.InviteID, &r.ContactID,
		&answers, &r.Score, &r.SubmittedAt)
	if err != nil {
		return r, err
	}
	r.Answers = map[string]any{}
	if len(answers) > 0 {
		if err := json.Unmarshal(answers, &r.Answers); err != nil {
			return r, fmt.Errorf("decode answers: %w", err)
		}
	}
	return r, nil
}

// maxThemeResponses caps the responses scanned by the themes endpoint
// (naive keyword frequency — bounded work per call).
const maxThemeResponses = 5000

// ThemeTexts collects every text-question answer across the tenant's
// survey responses, optionally restricted to one survey. Returns the
// flattened texts plus how many responses were scanned.
func (s *Store) ThemeTexts(ctx context.Context, tenantID uuid.UUID, surveyID *uuid.UUID) (texts []string, scanned int, err error) {
	texts = []string{}
	err = s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		// Load the survey definitions in scope (text question ids).
		q := `SELECT ` + surveyCols + ` FROM surveys WHERE tenant_id=$1`
		args := []any{tenantID}
		if surveyID != nil {
			q += ` AND id=$2`
			args = append(args, *surveyID)
		}
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defs := map[uuid.UUID]Survey{}
		for rows.Next() {
			sv, err := scanSurvey(rows)
			if err != nil {
				rows.Close()
				return err
			}
			defs[sv.ID] = sv
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if surveyID != nil && len(defs) == 0 {
			return ErrNotFound
		}

		rq := `SELECT id, tenant_id, survey_id, invite_id, contact_id, answers, score, submitted_at
		         FROM survey_responses WHERE tenant_id=$1`
		rargs := []any{tenantID}
		if surveyID != nil {
			rq += ` AND survey_id=$2`
			rargs = append(rargs, *surveyID)
		}
		rq += fmt.Sprintf(` ORDER BY submitted_at DESC LIMIT %d`, maxThemeResponses)
		rrows, err := tx.Query(ctx, rq, rargs...)
		if err != nil {
			return err
		}
		defer rrows.Close()
		for rrows.Next() {
			r, err := scanResponse(rrows)
			if err != nil {
				return err
			}
			scanned++
			sv, ok := defs[r.SurveyID]
			if !ok {
				continue
			}
			texts = append(texts, TextAnswers(sv, r)...)
		}
		return rrows.Err()
	})
	if err != nil {
		return nil, 0, err
	}
	return texts, scanned, nil
}
