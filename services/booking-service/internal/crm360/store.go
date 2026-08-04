package crm360

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store persists crm_notes + crm_tags and serves the read-only 360
// aggregation over the W13–W19 domain tables. Same packaging idiom as the
// W19 packages (NewStore wraps an existing pool for tests; DialStore opens
// a small dedicated pool for the integrator wiring path — the shared
// store.Store does not expose its pool). maxConns 4: CRM-360 is an
// operator-paced, low-QPS surface.
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

// ensureSchema bootstraps crm_notes + crm_tags idempotently (SPEC-W20
// contract §1: RLS enabled + forced with the tenant_isolation policy,
// guarded by a pg_policies existence check — mirrors
// internal/devices/store.go and the W19 stores).
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) ensureSchema(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS crm_notes (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID NOT NULL,
    contact_id UUID NOT NULL,
    author     TEXT NOT NULL DEFAULT '',
    body       TEXT NOT NULL,
    pinned     BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_crm_notes_contact ON crm_notes (tenant_id, contact_id, pinned DESC, created_at DESC);
ALTER TABLE crm_notes ENABLE ROW LEVEL SECURITY;
ALTER TABLE crm_notes FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'crm_notes' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON crm_notes
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS crm_tags (
    tenant_id  UUID NOT NULL,
    contact_id UUID NOT NULL,
    tag        TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, contact_id, tag)
);
CREATE INDEX IF NOT EXISTS idx_crm_tags_tag ON crm_tags (tenant_id, tag);
ALTER TABLE crm_tags ENABLE ROW LEVEL SECURITY;
ALTER TABLE crm_tags FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'crm_tags' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON crm_tags
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure crm360 tables: %w", err)
	}
	return nil
}

// withTenant runs fn inside a transaction with `SET LOCAL app.tenant_id`
// applied (mirrors devices.Store.withTenant) so the RLS tenant_isolation
// policy scopes every statement of fn to the given tenant. The aggregation
// joins touch OTHER W13–W19 tables, which carry the same policy — one
// tenant context covers them all.
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

// ErrNotFound is returned when a row does not exist (or is cross-tenant —
// RLS makes the two indistinguishable, which is the point).
var ErrNotFound = errors.New("not found")

// tableExists reports whether name is a table in the current schema. The
// 360 aggregation guards every OPTIONAL source with this so a partially
// deployed booking DB (e.g. helpdesk not yet rolled out) degrades to an
// empty section instead of a 500 (SPEC-W20 Agent A contract).
func tableExists(ctx context.Context, tx pgx.Tx, name string) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables
		                 WHERE table_schema = current_schema() AND table_name = $1)`,
		name).Scan(&ok)
	return ok, err
}

// ---------------------------------------------------------------------------
// Notes
// ---------------------------------------------------------------------------

const noteCols = `id, tenant_id, contact_id, author, body, pinned, created_at, updated_at`

// CreateNote inserts a note and stamps id/timestamps back onto n.
func (s *Store) CreateNote(ctx context.Context, n *Note) error {
	const q = `INSERT INTO crm_notes (` + noteCols + `)
		           VALUES (COALESCE($1, gen_random_uuid()), $2,$3,$4,$5,$6,now(),now())
		           RETURNING id, created_at, updated_at`
	return s.withTenant(ctx, n.TenantID, func(tx pgx.Tx) error {
		var id any
		if n.ID != uuid.Nil {
			id = n.ID
		}
		return tx.QueryRow(ctx, q, id, n.TenantID, n.ContactID, n.Author, n.Body, n.Pinned).
			Scan(&n.ID, &n.CreatedAt, &n.UpdatedAt)
	})
}

// GetNote loads one note scoped to the tenant (ErrNotFound when missing
// or cross-tenant).
func (s *Store) GetNote(ctx context.Context, tenantID, id uuid.UUID) (Note, error) {
	var n Note
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`SELECT `+noteCols+` FROM crm_notes WHERE tenant_id=$1 AND id=$2`, tenantID, id).
			Scan(&n.ID, &n.TenantID, &n.ContactID, &n.Author, &n.Body, &n.Pinned, &n.CreatedAt, &n.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return n, err
}

// ListNotes returns a contact's notes, pinned first then newest first.
func (s *Store) ListNotes(ctx context.Context, tenantID, contactID uuid.UUID) ([]Note, error) {
	out := []Note{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+noteCols+` FROM crm_notes
			 WHERE tenant_id=$1 AND contact_id=$2
			 ORDER BY pinned DESC, created_at DESC LIMIT 500`, tenantID, contactID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n Note
			if err := rows.Scan(&n.ID, &n.TenantID, &n.ContactID, &n.Author, &n.Body, &n.Pinned, &n.CreatedAt, &n.UpdatedAt); err != nil {
				return err
			}
			out = append(out, n)
		}
		return rows.Err()
	})
	return out, err
}

// UpdateNote persists body+pinned (the only mutable fields), stamping
// updated_at. ErrNotFound when the row is missing/cross-tenant.
func (s *Store) UpdateNote(ctx context.Context, n *Note) error {
	return s.withTenant(ctx, n.TenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE crm_notes SET body=$3, pinned=$4, updated_at=now()
			 WHERE tenant_id=$1 AND id=$2`,
			n.TenantID, n.ID, n.Body, n.Pinned)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return tx.QueryRow(ctx,
			`SELECT updated_at FROM crm_notes WHERE tenant_id=$1 AND id=$2`, n.TenantID, n.ID).
			Scan(&n.UpdatedAt)
	})
}

// ---------------------------------------------------------------------------
// Tags
// ---------------------------------------------------------------------------

// AddTag attaches a (validated, normalized) tag to a contact. Idempotent:
// re-adding the same tag is a no-op (PK conflict) so tag add retries and
// double-clicks never error. maxTagsPerContact bounds the label set.
func (s *Store) AddTag(ctx context.Context, tenantID, contactID uuid.UUID, tag string) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM crm_tags WHERE tenant_id=$1 AND contact_id=$2`,
			tenantID, contactID).Scan(&count); err != nil {
			return err
		}
		if count >= maxTagsPerContact {
			return fmt.Errorf("%w: contact already has %d tags", ErrInvalidInput, maxTagsPerContact)
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO crm_tags (tenant_id, contact_id, tag) VALUES ($1,$2,$3)
			 ON CONFLICT (tenant_id, contact_id, tag) DO NOTHING`,
			tenantID, contactID, tag)
		return err
	})
}

// RemoveTag detaches a tag. ErrNotFound when the tag is not attached.
func (s *Store) RemoveTag(ctx context.Context, tenantID, contactID uuid.UUID, tag string) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		cmd, err := tx.Exec(ctx,
			`DELETE FROM crm_tags WHERE tenant_id=$1 AND contact_id=$2 AND tag=$3`,
			tenantID, contactID, tag)
		if err != nil {
			return err
		}
		if cmd.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ListTags returns a contact's tags in lexical (deterministic) order.
func (s *Store) ListTags(ctx context.Context, tenantID, contactID uuid.UUID) ([]string, error) {
	out := []string{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tag FROM crm_tags WHERE tenant_id=$1 AND contact_id=$2 ORDER BY tag`,
			tenantID, contactID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var tag string
			if err := rows.Scan(&tag); err != nil {
				return err
			}
			out = append(out, tag)
		}
		return rows.Err()
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Contacts (read-only over the base contacts table)
// ---------------------------------------------------------------------------

// contactCols: base contacts columns + the W12 reverse-sync columns
// (source/external_id — added by the shared store's ensureCRMColumns).
const contactCols = `id, tenant_id, name, COALESCE(phone,''), COALESCE(email,''), COALESCE(notes,''),
	COALESCE(source,''), COALESCE(external_id,'')`

// GetContact loads one contact scoped to the tenant (ErrNotFound when
// missing or cross-tenant). contacts is the 360 BASE table — not an
// optional source — so an absent contacts table surfaces as an error.
func (s *Store) GetContact(ctx context.Context, tenantID, contactID uuid.UUID) (Contact, error) {
	var c Contact
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`SELECT `+contactCols+` FROM contacts WHERE tenant_id=$1 AND id=$2`, tenantID, contactID).
			Scan(&c.ID, &c.TenantID, &c.Name, &c.Phone, &c.Email, &c.Notes, &c.Source, &c.ExternalID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return c, err
}

// SearchContacts runs the name/phone/email PREFIX search with an optional
// tag filter (SPEC-W20 Agent A). Empty q matches every contact (the tag
// filter then does the selecting). Each row carries the contact's tags.
func (s *Store) SearchContacts(ctx context.Context, tenantID uuid.UUID, q, tag string, limit int) ([]ContactSearchResult, error) {
	if limit <= 0 || limit > maxSearchLimit {
		limit = defaultSearchLimit
	}
	q = strings.TrimSpace(q)
	out := []ContactSearchResult{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		sql := `SELECT c.id, c.tenant_id, c.name, COALESCE(c.phone,''), COALESCE(c.email,''),
		               COALESCE(c.notes,''), COALESCE(c.source,''), COALESCE(c.external_id,'')
		           FROM contacts c`
		args := []any{tenantID}
		n := 1
		if tag != "" {
			sql += ` JOIN crm_tags ct ON ct.tenant_id = c.tenant_id AND ct.contact_id = c.id AND ct.tag = $2`
			n = 2
			args = append(args, tag)
		}
		sql += ` WHERE c.tenant_id = $1`
		if q != "" {
			n++
			sql += fmt.Sprintf(` AND (c.name ILIKE $%[1]d || '%%' OR c.phone LIKE $%[1]d || '%%' OR c.email ILIKE $%[1]d || '%%')`, n)
			args = append(args, q)
		}
		n++
		sql += fmt.Sprintf(` ORDER BY c.name LIMIT $%d`, n)
		args = append(args, limit)

		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		ids := []uuid.UUID{}
		for rows.Next() {
			var r ContactSearchResult
			if err := rows.Scan(&r.ID, &r.TenantID, &r.Name, &r.Phone, &r.Email, &r.Notes, &r.Source, &r.ExternalID); err != nil {
				return err
			}
			r.Tags = []string{}
			out = append(out, r)
			ids = append(ids, r.ID)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		// Batch-attach tags for the result page (one query, no N+1).
		tagRows, err := tx.Query(ctx,
			`SELECT contact_id, tag FROM crm_tags
			 WHERE tenant_id=$1 AND contact_id = ANY($2) ORDER BY tag`, tenantID, ids)
		if err != nil {
			return err
		}
		defer tagRows.Close()
		byContact := map[uuid.UUID][]string{}
		for tagRows.Next() {
			var cid uuid.UUID
			var t string
			if err := tagRows.Scan(&cid, &t); err != nil {
				return err
			}
			byContact[cid] = append(byContact[cid], t)
		}
		if err := tagRows.Err(); err != nil {
			return err
		}
		for i := range out {
			if ts, ok := byContact[out[i].ID]; ok {
				out[i].Tags = ts
			}
		}
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// 360 aggregation (read-only joins; every optional source degrades empty)
// ---------------------------------------------------------------------------

// Profile360 builds the unified customer profile. The contact record and
// crm tags are authoritative (contact missing → ErrNotFound); every
// domain section is guarded by tableExists and degrades to an empty
// array / nil when its source table is absent (SPEC-W20 Agent A: never
// 500 on a missing optional source).
func (s *Store) Profile360(ctx context.Context, tenantID, contactID uuid.UUID) (Profile360, error) {
	p := Profile360{
		Tags:       []string{},
		Tickets:    []TicketSummary{},
		Bookings:   []BookingSummary{},
		WorkOrders: []WorkOrderSummary{},
	}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		// Base contact (required).
		err := tx.QueryRow(ctx,
			`SELECT `+contactCols+` FROM contacts WHERE tenant_id=$1 AND id=$2`, tenantID, contactID).
			Scan(&p.Contact.ID, &p.Contact.TenantID, &p.Contact.Name, &p.Contact.Phone,
				&p.Contact.Email, &p.Contact.Notes, &p.Contact.Source, &p.Contact.ExternalID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		// Tags (own table — always present).
		tagRows, err := tx.Query(ctx,
			`SELECT tag FROM crm_tags WHERE tenant_id=$1 AND contact_id=$2 ORDER BY tag`, tenantID, contactID)
		if err != nil {
			return err
		}
		for tagRows.Next() {
			var t string
			if err := tagRows.Scan(&t); err != nil {
				tagRows.Close()
				return err
			}
			p.Tags = append(p.Tags, t)
		}
		tagRows.Close()
		if err := tagRows.Err(); err != nil {
			return err
		}

		if err := loadTicketSection(ctx, tx, tenantID, contactID, &p); err != nil {
			return err
		}
		if err := loadBookingSection(ctx, tx, tenantID, contactID, &p); err != nil {
			return err
		}
		if err := loadWorkOrderSection(ctx, tx, tenantID, contactID, &p); err != nil {
			return err
		}
		return loadWalletSection(ctx, tx, tenantID, contactID, &p)
	})
	return p, err
}

// loadTicketSection fills OpenTicketCount + the latest 5 tickets
// (open = status open|pending, mirroring the helpdesk queue semantics).
func loadTicketSection(ctx context.Context, tx pgx.Tx, tenantID, contactID uuid.UUID, p *Profile360) error {
	ok, err := tableExists(ctx, tx, "tickets")
	if err != nil || !ok {
		return err
	}
	if err := tx.QueryRow(ctx,
		`SELECT COUNT(*) FROM tickets
		 WHERE tenant_id=$1 AND contact_id=$2 AND status IN ('open','pending')`,
		tenantID, contactID).Scan(&p.OpenTicketCount); err != nil {
		return err
	}
	rows, err := tx.Query(ctx,
		`SELECT id, subject, status, priority, created_at FROM tickets
		 WHERE tenant_id=$1 AND contact_id=$2
		 ORDER BY created_at DESC LIMIT 5`, tenantID, contactID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var t TicketSummary
		if err := rows.Scan(&t.ID, &t.Subject, &t.Status, &t.Priority, &t.CreatedAt); err != nil {
			return err
		}
		p.Tickets = append(p.Tickets, t)
	}
	return rows.Err()
}

// loadBookingSection fills the latest 5 bookings (by scheduled start).
func loadBookingSection(ctx context.Context, tx pgx.Tx, tenantID, contactID uuid.UUID, p *Profile360) error {
	ok, err := tableExists(ctx, tx, "bookings")
	if err != nil || !ok {
		return err
	}
	rows, err := tx.Query(ctx,
		`SELECT id, status, source, starts_at, ends_at, created_at FROM bookings
		 WHERE tenant_id=$1 AND contact_id=$2
		 ORDER BY starts_at DESC LIMIT 5`, tenantID, contactID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var b BookingSummary
		if err := rows.Scan(&b.ID, &b.Status, &b.Source, &b.StartsAt, &b.EndsAt, &b.CreatedAt); err != nil {
			return err
		}
		p.Bookings = append(p.Bookings, b)
	}
	return rows.Err()
}

// activeWorkOrderStatuses mirrors workorders' non-terminal states.
var activeWorkOrderStatuses = []string{"created", "assigned", "en_route", "on_site"}

// loadWorkOrderSection fills the ACTIVE work orders for the contact.
func loadWorkOrderSection(ctx context.Context, tx pgx.Tx, tenantID, contactID uuid.UUID, p *Profile360) error {
	ok, err := tableExists(ctx, tx, "work_orders")
	if err != nil || !ok {
		return err
	}
	rows, err := tx.Query(ctx,
		`SELECT id, title, status, scheduled_start FROM work_orders
		 WHERE tenant_id=$1 AND contact_id=$2 AND status = ANY($3)
		 ORDER BY scheduled_start ASC NULLS LAST, created_at DESC LIMIT 50`,
		tenantID, contactID, activeWorkOrderStatuses)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var w WorkOrderSummary
		if err := rows.Scan(&w.ID, &w.Title, &w.Status, &w.ScheduledStart); err != nil {
			return err
		}
		p.WorkOrders = append(p.WorkOrders, w)
	}
	return rows.Err()
}

// loadWalletSection fills Wallet when a loyalty_wallets row exists for
// the contact (nil otherwise — SPEC: "loyalty wallet {balance, tier} if
// any").
func loadWalletSection(ctx context.Context, tx pgx.Tx, tenantID, contactID uuid.UUID, p *Profile360) error {
	ok, err := tableExists(ctx, tx, "loyalty_wallets")
	if err != nil || !ok {
		return err
	}
	var w WalletSummary
	err = tx.QueryRow(ctx,
		`SELECT balance, tier FROM loyalty_wallets WHERE tenant_id=$1 AND contact_id=$2`,
		tenantID, contactID).Scan(&w.Balance, &w.Tier)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	p.Wallet = &w
	return nil
}

// ---------------------------------------------------------------------------
// Timeline (merged chronological feed)
// ---------------------------------------------------------------------------

// Timeline builds the merged chronological feed across bookings,
// ticket_events, work order lifecycle, loyalty ledger entries and
// crm_notes (SPEC-W20 Agent A), newest first, capped at limit (default
// 50, max 200). Every source except crm_notes is guarded by tableExists
// and degrades to nothing when absent.
func (s *Store) Timeline(ctx context.Context, tenantID, contactID uuid.UUID, limit int) ([]TimelineItem, error) {
	if limit <= 0 || limit > maxTimelineLimit {
		limit = defaultTimelineLimit
	}
	items := []TimelineItem{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		// crm_notes (own table — always present).
		noteRows, err := tx.Query(ctx,
			`SELECT id, author, body, pinned, created_at FROM crm_notes
			 WHERE tenant_id=$1 AND contact_id=$2 ORDER BY created_at DESC LIMIT $3`,
			tenantID, contactID, limit)
		if err != nil {
			return err
		}
		for noteRows.Next() {
			var id uuid.UUID
			var author, body string
			var pinned bool
			var ts time.Time
			if err := noteRows.Scan(&id, &author, &body, &pinned, &ts); err != nil {
				noteRows.Close()
				return err
			}
			summary := "Note"
			if pinned {
				summary = "Pinned note"
			}
			if author != "" {
				summary += " by " + author
			}
			summary += ": " + truncate(body, 120)
			items = append(items, TimelineItem{TS: ts, Kind: KindNote, Summary: summary, RefID: id.String()})
		}
		noteRows.Close()
		if err := noteRows.Err(); err != nil {
			return err
		}

		if err := timelineBookings(ctx, tx, tenantID, contactID, limit, &items); err != nil {
			return err
		}
		if err := timelineTicketEvents(ctx, tx, tenantID, contactID, limit, &items); err != nil {
			return err
		}
		if err := timelineWorkOrders(ctx, tx, tenantID, contactID, limit, &items); err != nil {
			return err
		}
		return timelineLoyalty(ctx, tx, tenantID, contactID, limit, &items)
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].TS.After(items[j].TS) })
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

// timelineBookings contributes one item per booking (at creation time).
func timelineBookings(ctx context.Context, tx pgx.Tx, tenantID, contactID uuid.UUID, limit int, items *[]TimelineItem) error {
	ok, err := tableExists(ctx, tx, "bookings")
	if err != nil || !ok {
		return err
	}
	rows, err := tx.Query(ctx,
		`SELECT id, status, starts_at, created_at FROM bookings
		 WHERE tenant_id=$1 AND contact_id=$2 ORDER BY created_at DESC LIMIT $3`,
		tenantID, contactID, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var status string
		var startsAt, createdAt time.Time
		if err := rows.Scan(&id, &status, &startsAt, &createdAt); err != nil {
			return err
		}
		*items = append(*items, TimelineItem{
			TS:      createdAt,
			Kind:    KindBooking,
			Summary: fmt.Sprintf("Booking %s (starts %s)", status, startsAt.UTC().Format("2006-01-02 15:04 UTC")),
			RefID:   id.String(),
		})
	}
	return rows.Err()
}

// timelineTicketEvents contributes helpdesk ticket_events for tickets
// linked to the contact (created / assigned / status_changed / note /
// first_response / resolved / reopened).
func timelineTicketEvents(ctx context.Context, tx pgx.Tx, tenantID, contactID uuid.UUID, limit int, items *[]TimelineItem) error {
	ok, err := tableExists(ctx, tx, "ticket_events")
	if err != nil || !ok {
		return err
	}
	ok, err = tableExists(ctx, tx, "tickets")
	if err != nil || !ok {
		return err
	}
	rows, err := tx.Query(ctx,
		`SELECT te.ts, te.kind, te.ticket_id, t.subject FROM ticket_events te
		 JOIN tickets t ON t.tenant_id = te.tenant_id AND t.id = te.ticket_id
		 WHERE te.tenant_id=$1 AND t.contact_id=$2
		 ORDER BY te.ts DESC LIMIT $3`, tenantID, contactID, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ts time.Time
		var kind string
		var ticketID uuid.UUID
		var subject string
		if err := rows.Scan(&ts, &kind, &ticketID, &subject); err != nil {
			return err
		}
		*items = append(*items, TimelineItem{
			TS:      ts,
			Kind:    KindTicketEvent,
			Summary: fmt.Sprintf("Ticket %s — %s", strings.ReplaceAll(kind, "_", " "), subject),
			RefID:   ticketID.String(),
		})
	}
	return rows.Err()
}

// timelineWorkOrders contributes a creation item per work order plus a
// completion item when completed_at is set. Intermediate status changes
// are not recorded in the booking DB (no work-order history table), so
// they cannot appear here — documented in docs/apps/crm-360.md.
func timelineWorkOrders(ctx context.Context, tx pgx.Tx, tenantID, contactID uuid.UUID, limit int, items *[]TimelineItem) error {
	ok, err := tableExists(ctx, tx, "work_orders")
	if err != nil || !ok {
		return err
	}
	rows, err := tx.Query(ctx,
		`SELECT id, title, status, created_at, completed_at FROM work_orders
		 WHERE tenant_id=$1 AND contact_id=$2 ORDER BY created_at DESC LIMIT $3`,
		tenantID, contactID, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var title, status string
		var createdAt time.Time
		var completedAt *time.Time
		if err := rows.Scan(&id, &title, &status, &createdAt, &completedAt); err != nil {
			return err
		}
		*items = append(*items, TimelineItem{
			TS:      createdAt,
			Kind:    KindWorkOrder,
			Summary: fmt.Sprintf("Work order created (%s): %s", status, title),
			RefID:   id.String(),
		})
		if completedAt != nil {
			*items = append(*items, TimelineItem{
				TS:      *completedAt,
				Kind:    KindWorkOrder,
				Summary: "Work order completed: " + title,
				RefID:   id.String(),
			})
		}
	}
	return rows.Err()
}

// timelineLoyalty contributes one item per loyalty_ledger entry for the
// contact (beneficiary_id carries the contact id as text — see
// internal/loyalty/ledger.go).
func timelineLoyalty(ctx context.Context, tx pgx.Tx, tenantID, contactID uuid.UUID, limit int, items *[]TimelineItem) error {
	ok, err := tableExists(ctx, tx, "loyalty_ledger")
	if err != nil || !ok {
		return err
	}
	rows, err := tx.Query(ctx,
		`SELECT id, account_code, debit_points, credit_points, ref_type, created_at
		 FROM loyalty_ledger
		 WHERE tenant_id=$1 AND beneficiary_id=$2
		 ORDER BY created_at DESC LIMIT $3`, tenantID, contactID.String(), limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var code int
		var debit, credit int64
		var refType string
		var ts time.Time
		if err := rows.Scan(&id, &code, &debit, &credit, &refType, &ts); err != nil {
			return err
		}
		summary := fmt.Sprintf("Loyalty +%d pts (%s)", credit, refType)
		if debit > 0 {
			summary = fmt.Sprintf("Loyalty -%d pts (%s)", debit, refType)
		}
		*items = append(*items, TimelineItem{TS: ts, Kind: KindLoyalty, Summary: summary, RefID: id.String()})
	}
	return rows.Err()
}

// truncate shortens s to max bytes on a rune boundary (timeline previews).
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && len(string(r)) > max-1 {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

// EnqueueOutbox appends one row to the transactional outbox (mirrors
// workorders.Store.EnqueueOutbox; the note/pin/tag lifecycle CloudEvents
// ride this path — the W5 outbox dispatcher drains rows to Kafka via
// Dapr).
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
