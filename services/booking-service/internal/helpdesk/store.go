package helpdesk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store persists SLA policies, tickets and ticket events. Same packaging
// idiom as the W16 devices.Store: NewStore wraps an existing pool (tests),
// DialStore opens a small dedicated pool (main wiring path — the shared
// store.Store does not expose its pool). maxConns 4: helpdesk is an
// operator-QPS surface (queue board + ticket mutations).
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
func DialStore(ctx context.Context, databaseURL string, maxConns int32) (*Store, error) {
	poolCfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse postgres config: %w", err)
	}
	if maxConns <= 0 {
		maxConns = 4
	}
	poolCfg.MaxConns = maxConns
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

// ensureSchema bootstraps sla_policies + tickets + ticket_events
// idempotently (same pattern as devices.ensureSchema, SPEC-W19 contract §1):
// RLS enabled + forced with the tenant_isolation policy, guarded by a
// pg_policies existence check.
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) ensureSchema(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS sla_policies (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID NOT NULL,
    name                  TEXT NOT NULL,
    priority              TEXT NOT NULL
                          CHECK (priority IN ('low','normal','high','urgent')),
    first_response_minutes INT NOT NULL CHECK (first_response_minutes > 0),
    resolve_minutes       INT NOT NULL CHECK (resolve_minutes > 0),
    active                BOOLEAN NOT NULL DEFAULT TRUE,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_sla_policies_tenant ON sla_policies (tenant_id, priority) WHERE active;
ALTER TABLE sla_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE sla_policies FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'sla_policies' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON sla_policies
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS tickets (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID NOT NULL,
    contact_id            UUID,
    conversation_id       UUID,
    subject               TEXT NOT NULL,
    channel               TEXT NOT NULL DEFAULT 'web',
    priority              TEXT NOT NULL DEFAULT 'normal'
                          CHECK (priority IN ('low','normal','high','urgent')),
    status                TEXT NOT NULL DEFAULT 'open'
                          CHECK (status IN ('open','pending','resolved','closed')),
    assignee_id           UUID,
    sla_policy_id         UUID,
    due_first_response_at TIMESTAMPTZ,
    due_resolve_at        TIMESTAMPTZ,
    first_response_at     TIMESTAMPTZ,
    resolved_at           TIMESTAMPTZ,
    csat_rating           INT CHECK (csat_rating BETWEEN 1 AND 5),
    csat_comment          TEXT,
    csat_at               TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_tickets_tenant_status   ON tickets (tenant_id, status);
CREATE INDEX IF NOT EXISTS idx_tickets_tenant_assignee ON tickets (tenant_id, assignee_id);
CREATE INDEX IF NOT EXISTS idx_tickets_tenant_created  ON tickets (tenant_id, created_at DESC);
ALTER TABLE tickets ENABLE ROW LEVEL SECURITY;
ALTER TABLE tickets FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'tickets' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON tickets
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS ticket_events (
    id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    ticket_id UUID NOT NULL,
    kind      TEXT NOT NULL
              CHECK (kind IN ('created','assigned','status_changed','note','first_response','resolved','reopened')),
    actor     TEXT,
    payload   JSONB NOT NULL DEFAULT '{}',
    ts        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_ticket_events_ticket ON ticket_events (tenant_id, ticket_id, ts);
ALTER TABLE ticket_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE ticket_events FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'ticket_events' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON ticket_events
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure helpdesk tables: %w", err)
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

// ErrNoAssignee is returned when auto-assignment finds no active team
// member (the API maps it to 409).
var ErrNoAssignee = errors.New("no active team member available for auto-assignment")

// ErrInvalidTransition is returned for CSAT before resolution (409).
var ErrInvalidTransition = errors.New("invalid ticket state for this action")

const ticketCols = `id, tenant_id, contact_id, conversation_id, subject, channel, priority, status,
                    assignee_id, sla_policy_id, due_first_response_at, due_resolve_at,
                    first_response_at, resolved_at, csat_rating, csat_comment, csat_at,
                    created_at, updated_at`

func scanTicket(row pgx.Row) (Ticket, error) {
	var t Ticket
	err := row.Scan(&t.ID, &t.TenantID, &t.ContactID, &t.ConversationID, &t.Subject,
		&t.Channel, &t.Priority, &t.Status, &t.AssigneeID, &t.SLAPolicyID,
		&t.DueFirstResponseAt, &t.DueResolveAt, &t.FirstResponseAt, &t.ResolvedAt,
		&t.CSATRating, &t.CSATComment, &t.CSATAt, &t.CreatedAt, &t.UpdatedAt)
	return t, err
}

const policyCols = `id, tenant_id, name, priority, first_response_minutes, resolve_minutes, active, created_at, updated_at`

func scanPolicy(row pgx.Row) (SLAPolicy, error) {
	var p SLAPolicy
	err := row.Scan(&p.ID, &p.TenantID, &p.Name, &p.Priority, &p.FirstResponseMinute,
		&p.ResolveMinutes, &p.Active, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

// ---------------------------------------------------------------------------
// SLA policies
// ---------------------------------------------------------------------------

// CreatePolicy inserts one SLA policy (POST /v1/helpdesk/sla-policies).
func (s *Store) CreatePolicy(ctx context.Context, p *SLAPolicy) error {
	const q = `INSERT INTO sla_policies (tenant_id, name, priority, first_response_minutes, resolve_minutes, active)
	           VALUES ($1,$2,$3,$4,$5,$6)
	           RETURNING id, created_at, updated_at`
	return s.withTenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, p.TenantID, p.Name, p.Priority, p.FirstResponseMinute, p.ResolveMinutes, p.Active).
			Scan(&p.ID, &p.CreatedAt, &p.UpdatedAt)
	})
}

// ListPolicies returns the tenant's SLA policies (active first, then by
// priority rank). Backs GET /v1/helpdesk/sla-policies.
func (s *Store) ListPolicies(ctx context.Context, tenantID uuid.UUID) ([]SLAPolicy, error) {
	const q = `SELECT ` + policyCols + ` FROM sla_policies
	           WHERE tenant_id=$1
	           ORDER BY active DESC,
	                    CASE priority WHEN 'urgent' THEN 0 WHEN 'high' THEN 1 WHEN 'normal' THEN 2 ELSE 3 END,
	                    name`
	out := []SLAPolicy{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanPolicy(rows)
			if err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// UpdatePolicy applies a partial update to one policy (PATCH
// /v1/helpdesk/sla-policies/{id}). Only non-nil fields are applied.
func (s *Store) UpdatePolicy(ctx context.Context, tenantID, id uuid.UUID, name *string, priority *string, firstResponse, resolve *int, active *bool) (SLAPolicy, error) {
	var out SLAPolicy
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`UPDATE sla_policies SET
			     name                   = COALESCE($3, name),
			     priority               = COALESCE($4, priority),
			     first_response_minutes = COALESCE($5, first_response_minutes),
			     resolve_minutes        = COALESCE($6, resolve_minutes),
			     active                 = COALESCE($7, active),
			     updated_at             = now()
			 WHERE tenant_id=$1 AND id=$2
			 RETURNING `+policyCols,
			tenantID, id, name, priority, firstResponse, resolve, active).Scan(
			&out.ID, &out.TenantID, &out.Name, &out.Priority, &out.FirstResponseMinute,
			&out.ResolveMinutes, &out.Active, &out.CreatedAt, &out.UpdatedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return SLAPolicy{}, ErrNotFound
	}
	return out, err
}

// policyByID / activePolicyForPriority read policies inside an open tenant
// tx (callers hold withTenant).
func policyByID(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (SLAPolicy, error) {
	p, err := scanPolicy(tx.QueryRow(ctx,
		`SELECT `+policyCols+` FROM sla_policies WHERE tenant_id=$1 AND id=$2`, tenantID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return SLAPolicy{}, ErrNotFound
	}
	return p, err
}

func activePolicyForPriority(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, priority string) (*SLAPolicy, error) {
	p, err := scanPolicy(tx.QueryRow(ctx,
		`SELECT `+policyCols+` FROM sla_policies
		 WHERE tenant_id=$1 AND priority=$2 AND active
		 ORDER BY created_at LIMIT 1`, tenantID, priority))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ---------------------------------------------------------------------------
// Tickets
// ---------------------------------------------------------------------------

// CreateTicket inserts a ticket plus its `created` timeline event in one tx.
// The effective SLA policy is the explicit sla_policy_id when given, else
// the active policy matching the ticket priority (auto-attach by tier);
// due_* are computed from it (clock base: created_at). An explicit
// assignee_id also writes an `assigned` event.
func (s *Store) CreateTicket(ctx context.Context, t *Ticket, actor string) error {
	return s.withTenant(ctx, t.TenantID, func(tx pgx.Tx) error {
		var policy *SLAPolicy
		if t.SLAPolicyID != nil {
			p, err := policyByID(ctx, tx, t.TenantID, *t.SLAPolicyID)
			if err != nil {
				return err
			}
			policy = &p
		} else {
			p, err := activePolicyForPriority(ctx, tx, t.TenantID, t.Priority)
			if err != nil {
				return err
			}
			if p != nil {
				t.SLAPolicyID = &p.ID
			}
			policy = p
		}
		const q = `INSERT INTO tickets (tenant_id, contact_id, conversation_id, subject, channel, priority, status,
		                                assignee_id, sla_policy_id)
		           VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		           RETURNING id, created_at, updated_at`
		if err := tx.QueryRow(ctx, q, t.TenantID, t.ContactID, t.ConversationID, t.Subject, t.Channel,
			t.Priority, t.Status, t.AssigneeID, t.SLAPolicyID).
			Scan(&t.ID, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return err
		}
		// The SLA clock starts at the true creation timestamp.
		if policy != nil {
			t.DueFirstResponseAt, t.DueResolveAt = computeDues(t.CreatedAt, policy)
			if _, err := tx.Exec(ctx,
				`UPDATE tickets SET due_first_response_at=$3, due_resolve_at=$4 WHERE tenant_id=$1 AND id=$2`,
				t.TenantID, t.ID, t.DueFirstResponseAt, t.DueResolveAt); err != nil {
				return err
			}
		}
		if err := insertEvent(ctx, tx, t.TenantID, t.ID, EventCreated, actor, map[string]any{
			"subject": t.Subject, "channel": t.Channel, "priority": t.Priority, "status": t.Status,
		}); err != nil {
			return err
		}
		if t.AssigneeID != nil {
			return insertEvent(ctx, tx, t.TenantID, t.ID, EventAssigned, actor, map[string]any{
				"assignee_id": t.AssigneeID.String(), "auto": false,
			})
		}
		return nil
	})
}

// TicketFilters carries the GET /v1/helpdesk/tickets query filters
// (empty string / nil disables a filter).
type TicketFilters struct {
	Status     string
	Priority   string
	AssigneeID *uuid.UUID
	Channel    string
	Q          string // case-insensitive substring over subject
}

// ListTickets returns the tenant's tickets (newest first, LIMIT 500).
func (s *Store) ListTickets(ctx context.Context, tenantID uuid.UUID, f TicketFilters) ([]Ticket, error) {
	q := `SELECT ` + ticketCols + ` FROM tickets WHERE tenant_id=$1`
	args := []any{tenantID}
	n := 1
	if f.Status != "" {
		n++
		q += fmt.Sprintf(` AND status=$%d`, n)
		args = append(args, f.Status)
	}
	if f.Priority != "" {
		n++
		q += fmt.Sprintf(` AND priority=$%d`, n)
		args = append(args, f.Priority)
	}
	if f.AssigneeID != nil {
		n++
		q += fmt.Sprintf(` AND assignee_id=$%d`, n)
		args = append(args, *f.AssigneeID)
	}
	if f.Channel != "" {
		n++
		q += fmt.Sprintf(` AND channel=$%d`, n)
		args = append(args, f.Channel)
	}
	if f.Q != "" {
		n++
		q += fmt.Sprintf(` AND subject ILIKE '%%' || $%d || '%%'`, n)
		args = append(args, f.Q)
	}
	q += ` ORDER BY created_at DESC LIMIT 500`
	out := []Ticket{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t, err := scanTicket(rows)
			if err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// GetTicket returns one ticket scoped to the tenant.
func (s *Store) GetTicket(ctx context.Context, tenantID, id uuid.UUID) (Ticket, error) {
	var t Ticket
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		t, err = scanTicket(tx.QueryRow(ctx,
			`SELECT `+ticketCols+` FROM tickets WHERE tenant_id=$1 AND id=$2`, tenantID, id))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, ErrNotFound
	}
	return t, err
}

// ListEvents returns the ticket's timeline (oldest first) — the tenant
// scoping makes a cross-tenant ticket_id read return an empty list.
func (s *Store) ListEvents(ctx context.Context, tenantID, ticketID uuid.UUID) ([]TicketEvent, error) {
	out := []TicketEvent{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, ticket_id, kind, actor, payload, ts
			 FROM ticket_events WHERE tenant_id=$1 AND ticket_id=$2 ORDER BY ts, id`, tenantID, ticketID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e TicketEvent
			var actor *string
			var payload []byte
			if err := rows.Scan(&e.ID, &e.TenantID, &e.TicketID, &e.Kind, &actor, &payload, &e.Ts); err != nil {
				return err
			}
			if actor != nil {
				e.Actor = *actor
			}
			e.Payload = map[string]any{}
			if len(payload) > 0 {
				if err := json.Unmarshal(payload, &e.Payload); err != nil {
					return fmt.Errorf("decode ticket event payload: %w", err)
				}
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// insertEvent appends one timeline row (caller holds the tenant tx).
func insertEvent(ctx context.Context, tx pgx.Tx, tenantID, ticketID uuid.UUID, kind, actor string, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal ticket event payload: %w", err)
	}
	var actorArg *string
	if strings.TrimSpace(actor) != "" {
		actorArg = &actor
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO ticket_events (tenant_id, ticket_id, kind, actor, payload) VALUES ($1,$2,$3,$4,$5)`,
		tenantID, ticketID, kind, actorArg, raw)
	return err
}

// PatchInput is the PATCH /v1/helpdesk/tickets/{id} body after decoding.
// Pointer fields distinguish "absent" from "explicit null" (unassign /
// detach policy).
type PatchInput struct {
	AssigneeID   *uuid.UUID // nil = unchanged; use AssigneeSet+nil via Unassign
	Unassign     bool       // explicit assignee_id: null
	AutoAssign   bool       // assignee_id: "auto"
	Status       *string
	Note         *string
	Priority     *string
	SLAPolicyID  *uuid.UUID
	DetachPolicy bool // explicit sla_policy_id: null
}

// PatchResult reports what the patch changed so the handler can emit
// metering / CloudEvents for the transitions that actually happened.
type PatchResult struct {
	Ticket           Ticket
	ResolvedNow      bool // transitioned INTO resolved on this patch
	AutoAssignedTo   *uuid.UUID
	FirstResponseNow bool // first_response_at stamped on this patch
}

// PatchTicket applies one operator mutation and writes the matching
// timeline events in the same tx:
//   - assign (explicit / "auto" least-open-tickets / null unassign) → assigned
//   - status change → status_changed; resolved_at set entering resolved
//     (+ resolved event, PatchResult.ResolvedNow), cleared on reopen
//     (+ reopened event)
//   - note → note event
//   - the first staff note or status change stamps first_response_at
//     (+ first_response event)
//   - priority / sla_policy_id change recomputes due_* from the effective
//     policy (clock base stays created_at)
func (s *Store) PatchTicket(ctx context.Context, tenantID, id uuid.UUID, in PatchInput, actor string) (PatchResult, error) {
	var res PatchResult
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		t, err := scanTicket(tx.QueryRow(ctx,
			`SELECT `+ticketCols+` FROM tickets WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenantID, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}

		// --- assignment -------------------------------------------------
		if in.AutoAssign {
			member, err := leastLoadedMember(ctx, tx, tenantID)
			if err != nil {
				return err
			}
			t.AssigneeID = &member.ID
			res.AutoAssignedTo = &member.ID
			if err := insertEvent(ctx, tx, tenantID, id, EventAssigned, actor, map[string]any{
				"assignee_id": member.ID.String(), "assignee_name": member.Name, "auto": true,
			}); err != nil {
				return err
			}
		} else if in.Unassign {
			t.AssigneeID = nil
			if err := insertEvent(ctx, tx, tenantID, id, EventAssigned, actor, map[string]any{
				"assignee_id": nil, "auto": false,
			}); err != nil {
				return err
			}
		} else if in.AssigneeID != nil {
			t.AssigneeID = in.AssigneeID
			if err := insertEvent(ctx, tx, tenantID, id, EventAssigned, actor, map[string]any{
				"assignee_id": in.AssigneeID.String(), "auto": false,
			}); err != nil {
				return err
			}
		}

		// --- priority / policy → recompute dues -------------------------
		policyChanged := false
		if in.Priority != nil && *in.Priority != t.Priority {
			t.Priority = *in.Priority
			policyChanged = true
		}
		var policy *SLAPolicy
		switch {
		case in.DetachPolicy:
			t.SLAPolicyID = nil
			policyChanged = true
		case in.SLAPolicyID != nil:
			p, err := policyByID(ctx, tx, tenantID, *in.SLAPolicyID)
			if err != nil {
				return err
			}
			t.SLAPolicyID = &p.ID
			policy = &p
			policyChanged = true
		}
		if policyChanged && policy == nil && !in.DetachPolicy {
			// Priority-driven policy selection: explicit policy stays when
			// it still matches the (new) priority tier; otherwise re-attach
			// the active policy for the tier.
			if t.SLAPolicyID != nil {
				p, err := policyByID(ctx, tx, tenantID, *t.SLAPolicyID)
				if err != nil {
					return err
				}
				if p.Priority == t.Priority {
					policy = &p
				}
			}
			if policy == nil {
				p, err := activePolicyForPriority(ctx, tx, tenantID, t.Priority)
				if err != nil {
					return err
				}
				if p != nil {
					t.SLAPolicyID = &p.ID
				} else if in.Priority != nil {
					// No policy for the new tier: drop the stale one.
					t.SLAPolicyID = nil
				}
				policy = p
			}
		}
		if policyChanged {
			if in.DetachPolicy || policy == nil {
				t.DueFirstResponseAt, t.DueResolveAt = nil, nil
			} else {
				t.DueFirstResponseAt, t.DueResolveAt = computeDues(t.CreatedAt, policy)
			}
		}

		// --- status machine ---------------------------------------------
		staffTouched := false
		if in.Status != nil && *in.Status != t.Status {
			from := t.Status
			t.Status = *in.Status
			staffTouched = true
			if err := insertEvent(ctx, tx, tenantID, id, EventStatusChanged, actor, map[string]any{
				"from": from, "to": t.Status,
			}); err != nil {
				return err
			}
			switch t.Status {
			case StatusResolved:
				if t.ResolvedAt == nil {
					now := time.Now().UTC()
					t.ResolvedAt = &now
					res.ResolvedNow = true
					if err := insertEvent(ctx, tx, tenantID, id, EventResolved, actor, map[string]any{
						"resolved_at": now,
					}); err != nil {
						return err
					}
				}
			case StatusOpen, StatusPending:
				if from == StatusResolved || from == StatusClosed {
					t.ResolvedAt = nil // reopen: the resolve clock restarts
					if err := insertEvent(ctx, tx, tenantID, id, EventReopened, actor, map[string]any{
						"from": from, "to": t.Status,
					}); err != nil {
						return err
					}
				}
			}
		}

		// --- note ---------------------------------------------------------
		if in.Note != nil && strings.TrimSpace(*in.Note) != "" {
			staffTouched = true
			if err := insertEvent(ctx, tx, tenantID, id, EventNote, actor, map[string]any{
				"body": strings.TrimSpace(*in.Note),
			}); err != nil {
				return err
			}
		}

		// --- first response -------------------------------------------------
		if staffTouched && t.FirstResponseAt == nil {
			now := time.Now().UTC()
			t.FirstResponseAt = &now
			res.FirstResponseNow = true
			if err := insertEvent(ctx, tx, tenantID, id, EventFirstResponse, actor, map[string]any{
				"first_response_at": now,
			}); err != nil {
				return err
			}
		}

		// --- persist ------------------------------------------------------
		const upd = `UPDATE tickets SET
		                 priority=$3, status=$4, assignee_id=$5, sla_policy_id=$6,
		                 due_first_response_at=$7, due_resolve_at=$8,
		                 first_response_at=$9, resolved_at=$10, updated_at=now()
		             WHERE tenant_id=$1 AND id=$2
		             RETURNING updated_at`
		if err := tx.QueryRow(ctx, upd, tenantID, id, t.Priority, t.Status, t.AssigneeID, t.SLAPolicyID,
			t.DueFirstResponseAt, t.DueResolveAt, t.FirstResponseAt, t.ResolvedAt).
			Scan(&t.UpdatedAt); err != nil {
			return err
		}
		res.Ticket = t
		return nil
	})
	return res, err
}

// RecordCSAT stores the customer satisfaction rating (1-5). Only a resolved
// or closed ticket can be rated (ErrInvalidTransition otherwise) — CSAT
// measures the completed service interaction.
func (s *Store) RecordCSAT(ctx context.Context, tenantID, id uuid.UUID, rating int, comment string) (Ticket, error) {
	var t Ticket
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		t, err = scanTicket(tx.QueryRow(ctx,
			`SELECT `+ticketCols+` FROM tickets WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenantID, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if t.Status != StatusResolved && t.Status != StatusClosed {
			return fmt.Errorf("%w: csat can only be recorded on a resolved or closed ticket", ErrInvalidTransition)
		}
		var commentArg *string
		if c := strings.TrimSpace(comment); c != "" {
			commentArg = &c
		}
		return tx.QueryRow(ctx,
			`UPDATE tickets SET csat_rating=$3, csat_comment=$4, csat_at=now(), updated_at=now()
			 WHERE tenant_id=$1 AND id=$2
			 RETURNING csat_at, updated_at`, tenantID, id, rating, commentArg).
			Scan(&t.CSATAt, &t.UpdatedAt)
	})
	if err != nil {
		return Ticket{}, err
	}
	t.CSATRating = &rating
	if c := strings.TrimSpace(comment); c != "" {
		t.CSATComment = &c
	} else {
		t.CSATComment = nil
	}
	return t, nil
}

// leastLoadedMember picks the active team member with the fewest
// open|pending tickets (ties broken by name) — the SPEC-W19 "auto"
// assignment rule. team_members is the core booking table (read-only here);
// it carries the same tenant_isolation RLS, so this runs inside withTenant.
func leastLoadedMember(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (TeamMemberView, error) {
	var m TeamMemberView
	err := tx.QueryRow(ctx,
		`SELECT tm.id, tm.name, COALESCE(tm.email, '')
		 FROM team_members tm
		 LEFT JOIN tickets t
		   ON t.tenant_id = tm.tenant_id AND t.assignee_id = tm.id AND t.status IN ('open','pending')
		 WHERE tm.tenant_id = $1 AND tm.active
		 GROUP BY tm.id, tm.name, tm.email
		 ORDER BY COUNT(t.id) ASC, tm.name ASC
		 LIMIT 1`, tenantID).Scan(&m.ID, &m.Name, &m.Email)
	if errors.Is(err, pgx.ErrNoRows) {
		return TeamMemberView{}, ErrNoAssignee
	}
	return m, err
}

// ListTeamMembers returns the tenant's active team members for the assignee
// picker (GET /v1/helpdesk/team-members — read-only projection of the core
// team_members table).
func (s *Store) ListTeamMembers(ctx context.Context, tenantID uuid.UUID) ([]TeamMemberView, error) {
	out := []TeamMemberView{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, name, COALESCE(email, '') FROM team_members
			 WHERE tenant_id=$1 AND active ORDER BY name`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m TeamMemberView
			if err := rows.Scan(&m.ID, &m.Name, &m.Email); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Stats & breaches
// ---------------------------------------------------------------------------

// Stats computes GET /v1/helpdesk/stats: open tickets by priority, current
// breach count (now > due_*, status not resolved|closed) and the 30-day
// averages for first response / resolve (and CSAT, for the UI tiles).
func (s *Store) Stats(ctx context.Context, tenantID uuid.UUID) (Stats, error) {
	st := Stats{OpenByPriority: map[string]int{
		PriorityLow: 0, PriorityNormal: 0, PriorityHigh: 0, PriorityUrgent: 0,
	}}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT priority, COUNT(*) FROM tickets
			 WHERE tenant_id=$1 AND status IN ('open','pending')
			 GROUP BY priority`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p string
			var c int
			if err := rows.Scan(&p, &c); err != nil {
				return err
			}
			st.OpenByPriority[p] = c
			st.OpenCount += c
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM tickets
			 WHERE tenant_id=$1 AND status NOT IN ('resolved','closed')
			   AND ((first_response_at IS NULL AND due_first_response_at IS NOT NULL AND now() > due_first_response_at)
			        OR (due_resolve_at IS NOT NULL AND now() > due_resolve_at))`, tenantID).
			Scan(&st.BreachedCount); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`SELECT COUNT(*) FILTER (WHERE resolved_at IS NOT NULL),
			        AVG(EXTRACT(EPOCH FROM (first_response_at - created_at)) / 60)
			          FILTER (WHERE first_response_at IS NOT NULL),
			        AVG(EXTRACT(EPOCH FROM (resolved_at - created_at)) / 60)
			          FILTER (WHERE resolved_at IS NOT NULL),
			        AVG(csat_rating) FILTER (WHERE csat_rating IS NOT NULL)
			 FROM tickets
			 WHERE tenant_id=$1 AND created_at > now() - interval '30 days'`, tenantID).
			Scan(&st.Resolved30d, &st.AvgFirstResponseMin30d, &st.AvgResolveMinutes30d, &st.AvgCSAT30d)
	})
	return st, err
}

// ListBreaches returns the currently SLA-breached tickets (now > due_*,
// status not in resolved|closed) with per-deadline breach flags. Backs GET
// /v1/helpdesk/breaches.
func (s *Store) ListBreaches(ctx context.Context, tenantID uuid.UUID) ([]BreachTicket, error) {
	out := []BreachTicket{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+ticketCols+`,
			        (first_response_at IS NULL AND due_first_response_at IS NOT NULL AND now() > due_first_response_at)
			          AS breached_first_response,
			        (due_resolve_at IS NOT NULL AND now() > due_resolve_at)
			          AS breached_resolve
			 FROM tickets
			 WHERE tenant_id=$1 AND status NOT IN ('resolved','closed')
			   AND ((first_response_at IS NULL AND due_first_response_at IS NOT NULL AND now() > due_first_response_at)
			        OR (due_resolve_at IS NOT NULL AND now() > due_resolve_at))
			 ORDER BY COALESCE(due_first_response_at, due_resolve_at) ASC LIMIT 500`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b BreachTicket
			if err := rows.Scan(&b.ID, &b.TenantID, &b.ContactID, &b.ConversationID, &b.Subject,
				&b.Channel, &b.Priority, &b.Status, &b.AssigneeID, &b.SLAPolicyID,
				&b.DueFirstResponseAt, &b.DueResolveAt, &b.FirstResponseAt, &b.ResolvedAt,
				&b.CSATRating, &b.CSATComment, &b.CSATAt, &b.CreatedAt, &b.UpdatedAt,
				&b.BreachedFirstResponse, &b.BreachedResolve); err != nil {
				return err
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Outbox (metering + CloudEvents)
// ---------------------------------------------------------------------------

// EnqueueOutbox appends one row to the shared transactional outbox (drained
// by the outbox dispatcher to Kafka) — same idiom as
// store.Store.EnqueueOutbox (SPEC-W11).
//
// NOTE (RLS): the outbox table is not tenant-scoped (no RLS policy — the
// dispatcher drains it cross-tenant by design), so this runs outside
// withTenant.
func (s *Store) EnqueueOutbox(ctx context.Context, aggregateID uuid.UUID, topic string, payload []byte) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO outbox (aggregate_id, topic, payload) VALUES ($1,$2,$3)`,
		aggregateID, topic, payload); err != nil {
		return fmt.Errorf("enqueue outbox: %w", err)
	}
	return nil
}
