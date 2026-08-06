package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Civic reporting (SPEC-W32 WS-A): civic_categories + civic_routing_rules +
// civic_cases + civic_ref_seqs, all tenant-scoped with RLS.
//
// Civic cases build on the W11 incidents lifecycle (SPEC-W32 §0.1): the
// civic status machine (new → triaged → assigned → in_progress → resolved →
// closed) maps onto the incidents lifecycle at the store layer as
//   new                  → new
//   triaged / assigned   → dispatched (routed to an MDA queue)
//   in_progress          → acknowledged
//   resolved / closed    → closed
// The civic statuses are stored verbatim on civic_cases (the incidents table
// keeps its own 4-state CHECK); the mapping above is the canonical
// translation for any cross-wave consumer.
// ---------------------------------------------------------------------------

// Civic case statuses (CHECK constraint of the civic_cases table).
const (
	CivicStatusNew        = "new"
	CivicStatusTriaged    = "triaged"
	CivicStatusAssigned   = "assigned"
	CivicStatusInProgress = "in_progress"
	CivicStatusResolved   = "resolved"
	CivicStatusClosed     = "closed"
)

// CivicCategory mirrors booking.civic_categories: a reportable issue class
// with its default MDA dispatch queue and SLA clocks (SPEC-W32 §2).
type CivicCategory struct {
	ID              uuid.UUID `json:"id"`
	TenantID        uuid.UUID `json:"tenant_id"`
	Name            string    `json:"name"`
	Slug            string    `json:"slug"`
	MDAQueue        string    `json:"mda_queue"`
	AckSLAHours     int       `json:"ack_sla_hours"`
	ResolveSLAHours int       `json:"resolve_sla_hours"`
	Active          bool      `json:"active"`
	CreatedAt       time.Time `json:"created_at"`
}

// CivicRoutingRule mirrors booking.civic_routing_rules: an optional
// ward-specific MDA override (empty Ward = category-wide rule); a
// ward-specific override wins over the category default (SPEC-W32 §2).
type CivicRoutingRule struct {
	ID         uuid.UUID `json:"id"`
	TenantID   uuid.UUID `json:"tenant_id"`
	Ward       string    `json:"ward"`
	CategoryID uuid.UUID `json:"category_id"`
	MDAQueue   string    `json:"mda_queue"`
	CreatedAt  time.Time `json:"created_at"`
}

// CivicCase mirrors booking.civic_cases. ReporterPhone/ReporterName are
// nullable (anonymous or contact-less reports); the API layer masks them by
// role — the store always returns the raw values.
type CivicCase struct {
	ID                uuid.UUID  `json:"id"`
	TenantID          uuid.UUID  `json:"tenant_id"`
	Ref               string     `json:"ref"` // GOV-{LGA}-{WARD}-YYYY-{seq6}
	CategoryID        uuid.UUID  `json:"category_id"`
	Status            string     `json:"status"`
	Description       string     `json:"description"`
	Ward              string     `json:"ward"`
	LGA               string     `json:"lga"`
	Lat               *float64   `json:"lat,omitempty"`
	Lon               *float64   `json:"lon,omitempty"`
	LocationText      string     `json:"location_text"`
	ReporterPhoneE164 *string    `json:"reporter_phone_e164,omitempty"`
	ReporterName      *string    `json:"reporter_name,omitempty"`
	Anonymous         bool       `json:"anonymous"`
	WantsUpdates      bool       `json:"wants_updates"`
	PhotoURL          string     `json:"photo_url,omitempty"`
	Channel           string     `json:"channel"` // web | pwa | whatsapp
	MDAQueue          string     `json:"mda_queue"`
	AssignedTo        *string    `json:"assigned_to,omitempty"`
	AckDueAt          *time.Time `json:"ack_due_at,omitempty"`
	ResolveDueAt      *time.Time `json:"resolve_due_at,omitempty"`
	AckedAt           *time.Time `json:"acked_at,omitempty"`
	ResolvedAt        *time.Time `json:"resolved_at,omitempty"`
	ClosedAt          *time.Time `json:"closed_at,omitempty"`
	MergedInto        *uuid.UUID `json:"merged_into,omitempty"`
	SLABreachAck      bool       `json:"sla_breach_ack"`
	SLABreachResolve  bool       `json:"sla_breach_resolve"`
	EventSeq          int64      `json:"-"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// CivicCaseFilter narrows ListCivicCases (zero values disable a filter).
type CivicCaseFilter struct {
	Status     string
	CategoryID *uuid.UUID
	Ward       string
	SLABreach  string // "" | any | ack | resolve
	Query      string // substring over ref / description / location_text
}

// CivicStatRow is one aggregate bucket of the public stats endpoint
// (aggregate-only: counts, never person data — SPEC-W32 §4.1).
type CivicStatRow struct {
	Key      string `json:"key"`
	Open     int    `json:"open"`
	Resolved int    `json:"resolved"`
}

// CivicStats is the aggregate-only public dashboard payload.
type CivicStats struct {
	Open       int            `json:"open"`
	Resolved   int            `json:"resolved"`
	ByCategory []CivicStatRow `json:"by_category"`
	ByWard     []CivicStatRow `json:"by_ward"`
}

// ensureCivicTables bootstraps the SPEC-W32 tables idempotently (same
// pattern as ensureIncidentTables). RLS mirrors the rest of the store:
// enabled + forced with the tenant_isolation policy, guarded by a
// pg_policies existence check.
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) ensureCivicTables(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS civic_categories (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        UUID NOT NULL,
    name             TEXT NOT NULL,
    slug             TEXT NOT NULL,
    mda_queue        TEXT NOT NULL DEFAULT '',
    ack_sla_hours    INTEGER NOT NULL DEFAULT 24,
    resolve_sla_hours INTEGER NOT NULL DEFAULT 72,
    active           BOOLEAN NOT NULL DEFAULT TRUE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, slug)
);
ALTER TABLE civic_categories ENABLE ROW LEVEL SECURITY;
ALTER TABLE civic_categories FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'civic_categories' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON civic_categories
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS civic_routing_rules (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL,
    ward        TEXT NOT NULL DEFAULT '',
    category_id UUID NOT NULL,
    mda_queue   TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_civic_routing_category ON civic_routing_rules (tenant_id, category_id);
ALTER TABLE civic_routing_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE civic_routing_rules FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'civic_routing_rules' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON civic_routing_rules
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS civic_cases (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           UUID NOT NULL,
    ref                 TEXT NOT NULL,
    category_id         UUID NOT NULL,
    status              TEXT NOT NULL DEFAULT 'new'
                        CHECK (status IN ('new','triaged','assigned','in_progress','resolved','closed')),
    description         TEXT NOT NULL DEFAULT '',
    ward                TEXT NOT NULL DEFAULT '',
    lga                 TEXT NOT NULL DEFAULT '',
    lat                 DOUBLE PRECISION,
    lon                 DOUBLE PRECISION,
    location_text       TEXT NOT NULL DEFAULT '',
    reporter_phone_e164 TEXT,
    reporter_name       TEXT,
    anonymous           BOOLEAN NOT NULL DEFAULT FALSE,
    wants_updates       BOOLEAN NOT NULL DEFAULT FALSE,
    photo_url           TEXT NOT NULL DEFAULT '',
    channel             TEXT NOT NULL DEFAULT 'web'
                        CHECK (channel IN ('web','pwa','whatsapp')),
    mda_queue           TEXT NOT NULL DEFAULT '',
    assigned_to         TEXT,
    ack_due_at          TIMESTAMPTZ,
    resolve_due_at      TIMESTAMPTZ,
    acked_at            TIMESTAMPTZ,
    resolved_at         TIMESTAMPTZ,
    closed_at           TIMESTAMPTZ,
    merged_into         UUID,
    sla_breach_ack      BOOLEAN NOT NULL DEFAULT FALSE,
    sla_breach_resolve  BOOLEAN NOT NULL DEFAULT FALSE,
    event_seq           BIGINT NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, ref)
);
CREATE INDEX IF NOT EXISTS idx_civic_cases_tenant_status ON civic_cases (tenant_id, status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_civic_cases_category ON civic_cases (tenant_id, category_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_civic_cases_geo ON civic_cases (tenant_id, category_id)
    WHERE lat IS NOT NULL AND lon IS NOT NULL;
ALTER TABLE civic_cases ENABLE ROW LEVEL SECURITY;
ALTER TABLE civic_cases FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'civic_cases' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON civic_cases
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END $$;

-- Per-tenant-per-year reference sequence for GOV-{LGA}-{WARD}-YYYY-{seq6}.
CREATE TABLE IF NOT EXISTS civic_ref_seqs (
    tenant_id UUID NOT NULL,
    year      INTEGER NOT NULL,
    seq       BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, year)
);
ALTER TABLE civic_ref_seqs ENABLE ROW LEVEL SECURITY;
ALTER TABLE civic_ref_seqs FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'civic_ref_seqs' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON civic_ref_seqs
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure civic tables: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Categories (+ lazy default seed, SPEC-W32 §2)
// ---------------------------------------------------------------------------

// civicDefaultCategories are seeded on first use per tenant. mda_queue
// defaults to the W11 dispatch-endpoint key convention "mda-{slug}".
var civicDefaultCategories = []struct {
	Slug, Name, Queue string
	Ack, Resolve      int
}{
	{"roads", "Roads & Potholes", "mda-works", 24, 168},
	{"water", "Water Supply", "mda-water", 12, 72},
	{"power", "Power & Streetlights", "mda-power", 12, 72},
	{"waste", "Waste & Sanitation", "mda-environment", 24, 96},
	{"health", "Public Health", "mda-health", 8, 48},
	{"security", "Security & Safety", "mda-security", 2, 24},
	{"education", "Education", "mda-education", 48, 240},
	{"environment", "Environment", "mda-environment", 24, 120},
	{"other", "Other", "mda-admin", 48, 168},
}

// EnsureCivicCategories seeds the SPEC-W32 default categories when the
// tenant has none yet (idempotent, safe to call on every read path).
func (s *Store) EnsureCivicCategories(ctx context.Context, tenantID uuid.UUID) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM civic_categories WHERE tenant_id=$1`, tenantID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		const ins = `INSERT INTO civic_categories (tenant_id, name, slug, mda_queue, ack_sla_hours, resolve_sla_hours, active)
		             VALUES ($1,$2,$3,$4,$5,$6,TRUE) ON CONFLICT (tenant_id, slug) DO NOTHING`
		for _, d := range civicDefaultCategories {
			if _, err := tx.Exec(ctx, ins, tenantID, d.Name, d.Slug, d.Queue, d.Ack, d.Resolve); err != nil {
				return fmt.Errorf("seed civic category %s: %w", d.Slug, err)
			}
		}
		return nil
	})
}

const civicCategoryCols = `id, tenant_id, name, slug, mda_queue, ack_sla_hours, resolve_sla_hours, active, created_at`

func scanCivicCategory(row pgx.Row) (CivicCategory, error) {
	var c CivicCategory
	err := row.Scan(&c.ID, &c.TenantID, &c.Name, &c.Slug, &c.MDAQueue,
		&c.AckSLAHours, &c.ResolveSLAHours, &c.Active, &c.CreatedAt)
	return c, err
}

// ListCivicCategories returns the tenant's categories (activeOnly filters
// to the public-intake subset), seeding defaults on first use.
func (s *Store) ListCivicCategories(ctx context.Context, tenantID uuid.UUID, activeOnly bool) ([]CivicCategory, error) {
	if err := s.EnsureCivicCategories(ctx, tenantID); err != nil {
		return nil, err
	}
	q := `SELECT ` + civicCategoryCols + ` FROM civic_categories WHERE tenant_id=$1`
	if activeOnly {
		q += ` AND active`
	}
	q += ` ORDER BY name`
	out := []CivicCategory{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanCivicCategory(rows)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// GetCivicCategoryBySlug fetches one category by slug (public intake lookup).
func (s *Store) GetCivicCategoryBySlug(ctx context.Context, tenantID uuid.UUID, slug string) (CivicCategory, error) {
	if err := s.EnsureCivicCategories(ctx, tenantID); err != nil {
		return CivicCategory{}, err
	}
	var c CivicCategory
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		c, err = scanCivicCategory(tx.QueryRow(ctx,
			`SELECT `+civicCategoryCols+` FROM civic_categories WHERE tenant_id=$1 AND slug=$2`, tenantID, slug))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// GetCivicCategory fetches one category by id.
func (s *Store) GetCivicCategory(ctx context.Context, tenantID, id uuid.UUID) (CivicCategory, error) {
	var c CivicCategory
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		c, err = scanCivicCategory(tx.QueryRow(ctx,
			`SELECT `+civicCategoryCols+` FROM civic_categories WHERE tenant_id=$1 AND id=$2`, tenantID, id))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// CreateCivicCategory inserts a category (slug unique per tenant).
func (s *Store) CreateCivicCategory(ctx context.Context, c *CivicCategory) error {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	const q = `INSERT INTO civic_categories (` + civicCategoryCols + `)
	           VALUES ($1,$2,$3,$4,$5,$6,$7,$8,now()) RETURNING created_at`
	return s.withTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, q, c.ID, c.TenantID, c.Name, c.Slug, c.MDAQueue,
			c.AckSLAHours, c.ResolveSLAHours, c.Active).Scan(&c.CreatedAt)
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	})
}

// UpdateCivicCategory replaces mutable category fields.
func (s *Store) UpdateCivicCategory(ctx context.Context, c *CivicCategory) error {
	const q = `UPDATE civic_categories SET name=$3, mda_queue=$4, ack_sla_hours=$5,
	           resolve_sla_hours=$6, active=$7 WHERE tenant_id=$1 AND id=$2`
	return s.withTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, q, c.TenantID, c.ID, c.Name, c.MDAQueue,
			c.AckSLAHours, c.ResolveSLAHours, c.Active)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Routing rules
// ---------------------------------------------------------------------------

const civicRoutingCols = `id, tenant_id, ward, category_id, mda_queue, created_at`

func scanCivicRoutingRule(row pgx.Row) (CivicRoutingRule, error) {
	var r CivicRoutingRule
	err := row.Scan(&r.ID, &r.TenantID, &r.Ward, &r.CategoryID, &r.MDAQueue, &r.CreatedAt)
	return r, err
}

// ListCivicRoutingRules returns all routing rules of a tenant (the service
// applies the ward-override > category-default precedence in Go).
func (s *Store) ListCivicRoutingRules(ctx context.Context, tenantID uuid.UUID) ([]CivicRoutingRule, error) {
	out := []CivicRoutingRule{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+civicRoutingCols+` FROM civic_routing_rules WHERE tenant_id=$1 ORDER BY created_at`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanCivicRoutingRule(rows)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// CreateCivicRoutingRule inserts one rule.
func (s *Store) CreateCivicRoutingRule(ctx context.Context, r *CivicRoutingRule) error {
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	const q = `INSERT INTO civic_routing_rules (` + civicRoutingCols + `)
	           VALUES ($1,$2,$3,$4,$5,now()) RETURNING created_at`
	return s.withTenant(ctx, r.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, r.ID, r.TenantID, r.Ward, r.CategoryID, r.MDAQueue).Scan(&r.CreatedAt)
	})
}

// UpdateCivicRoutingRule replaces mutable rule fields.
func (s *Store) UpdateCivicRoutingRule(ctx context.Context, r *CivicRoutingRule) error {
	const q = `UPDATE civic_routing_rules SET ward=$3, category_id=$4, mda_queue=$5 WHERE tenant_id=$1 AND id=$2`
	return s.withTenant(ctx, r.TenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, q, r.TenantID, r.ID, r.Ward, r.CategoryID, r.MDAQueue)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// DeleteCivicRoutingRule removes one rule.
func (s *Store) DeleteCivicRoutingRule(ctx context.Context, tenantID, id uuid.UUID) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM civic_routing_rules WHERE tenant_id=$1 AND id=$2`, tenantID, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Reference sequence + cases
// ---------------------------------------------------------------------------

// NextCivicRefSeq atomically increments and returns the per-tenant-per-year
// case reference sequence (1-based) backing GOV-{LGA}-{WARD}-YYYY-{seq6}.
func (s *Store) NextCivicRefSeq(ctx context.Context, tenantID uuid.UUID, year int) (int64, error) {
	var seq int64
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO civic_ref_seqs (tenant_id, year, seq) VALUES ($1,$2,1)
			 ON CONFLICT (tenant_id, year) DO UPDATE SET seq = civic_ref_seqs.seq + 1
			 RETURNING seq`, tenantID, year).Scan(&seq)
	})
	return seq, err
}

const civicCaseCols = `id, tenant_id, ref, category_id, status, description, ward, lga, lat, lon,
	location_text, reporter_phone_e164, reporter_name, anonymous, wants_updates, photo_url, channel,
	mda_queue, assigned_to, ack_due_at, resolve_due_at, acked_at, resolved_at, closed_at, merged_into,
	sla_breach_ack, sla_breach_resolve, event_seq, created_at, updated_at`

func scanCivicCase(row pgx.Row) (CivicCase, error) {
	var c CivicCase
	err := row.Scan(&c.ID, &c.TenantID, &c.Ref, &c.CategoryID, &c.Status, &c.Description,
		&c.Ward, &c.LGA, &c.Lat, &c.Lon, &c.LocationText, &c.ReporterPhoneE164, &c.ReporterName,
		&c.Anonymous, &c.WantsUpdates, &c.PhotoURL, &c.Channel, &c.MDAQueue, &c.AssignedTo,
		&c.AckDueAt, &c.ResolveDueAt, &c.AckedAt, &c.ResolvedAt, &c.ClosedAt, &c.MergedInto,
		&c.SLABreachAck, &c.SLABreachResolve, &c.EventSeq, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

// InsertCivicCase persists one case (ref unique per tenant — the sequence
// generator guarantees uniqueness; a violation maps to ErrConflict).
func (s *Store) InsertCivicCase(ctx context.Context, c *CivicCase) error {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	const q = `INSERT INTO civic_cases (` + civicCaseCols + `)
	           VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,now(),now())
	           RETURNING created_at, updated_at`
	return s.withTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, q, c.ID, c.TenantID, c.Ref, c.CategoryID, c.Status, c.Description,
			c.Ward, c.LGA, c.Lat, c.Lon, c.LocationText, c.ReporterPhoneE164, c.ReporterName,
			c.Anonymous, c.WantsUpdates, c.PhotoURL, c.Channel, c.MDAQueue, c.AssignedTo,
			c.AckDueAt, c.ResolveDueAt, c.AckedAt, c.ResolvedAt, c.ClosedAt, c.MergedInto,
			c.SLABreachAck, c.SLABreachResolve, c.EventSeq).Scan(&c.CreatedAt, &c.UpdatedAt)
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	})
}

// GetCivicCase fetches one case scoped to a tenant.
func (s *Store) GetCivicCase(ctx context.Context, tenantID, id uuid.UUID) (CivicCase, error) {
	var c CivicCase
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		c, err = scanCivicCase(tx.QueryRow(ctx,
			`SELECT `+civicCaseCols+` FROM civic_cases WHERE tenant_id=$1 AND id=$2`, tenantID, id))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// GetCivicCaseByRef fetches one case by its public reference (citizen
// tracking + the internal sla-breach callback).
func (s *Store) GetCivicCaseByRef(ctx context.Context, tenantID uuid.UUID, ref string) (CivicCase, error) {
	var c CivicCase
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		c, err = scanCivicCase(tx.QueryRow(ctx,
			`SELECT `+civicCaseCols+` FROM civic_cases WHERE tenant_id=$1 AND ref=$2`, tenantID, ref))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// ListCivicCases returns cases of a tenant, newest first, narrowed by the
// filter (SPEC-W32 WS-A: status / category / ward / sla_breach / q).
func (s *Store) ListCivicCases(ctx context.Context, tenantID uuid.UUID, f CivicCaseFilter) ([]CivicCase, error) {
	q := `SELECT ` + civicCaseCols + ` FROM civic_cases WHERE tenant_id=$1`
	args := []any{tenantID}
	n := 1
	if f.Status != "" {
		n++
		q += fmt.Sprintf(` AND status=$%d`, n)
		args = append(args, f.Status)
	}
	if f.CategoryID != nil {
		n++
		q += fmt.Sprintf(` AND category_id=$%d`, n)
		args = append(args, *f.CategoryID)
	}
	if f.Ward != "" {
		n++
		q += fmt.Sprintf(` AND ward=$%d`, n)
		args = append(args, f.Ward)
	}
	switch f.SLABreach {
	case "any", "true":
		q += ` AND (sla_breach_ack OR sla_breach_resolve)`
	case "ack":
		q += ` AND sla_breach_ack`
	case "resolve":
		q += ` AND sla_breach_resolve`
	}
	if f.Query != "" {
		n++
		q += fmt.Sprintf(` AND (ref ILIKE '%%'||$%d||'%%' OR description ILIKE '%%'||$%d||'%%' OR location_text ILIKE '%%'||$%d||'%%')`, n, n, n)
		args = append(args, f.Query)
	}
	q += ` ORDER BY created_at DESC LIMIT 500`
	out := []CivicCase{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanCivicCase(rows)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// SaveCivicCase rewrites the mutable fields of a case (status machine,
// SLA clocks, merge pointer, breach flags). event_seq is NOT touched here —
// it advances only via NextCivicEventSeq.
func (s *Store) SaveCivicCase(ctx context.Context, c *CivicCase) error {
	const q = `UPDATE civic_cases SET category_id=$3, status=$4, description=$5, ward=$6, lga=$7,
	           lat=$8, lon=$9, location_text=$10, reporter_phone_e164=$11, reporter_name=$12,
	           anonymous=$13, wants_updates=$14, photo_url=$15, channel=$16, mda_queue=$17,
	           assigned_to=$18, ack_due_at=$19, resolve_due_at=$20, acked_at=$21, resolved_at=$22,
	           closed_at=$23, merged_into=$24, sla_breach_ack=$25, sla_breach_resolve=$26,
	           updated_at=now()
	           WHERE tenant_id=$1 AND id=$2 RETURNING updated_at`
	return s.withTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, q, c.TenantID, c.ID, c.CategoryID, c.Status, c.Description,
			c.Ward, c.LGA, c.Lat, c.Lon, c.LocationText, c.ReporterPhoneE164, c.ReporterName,
			c.Anonymous, c.WantsUpdates, c.PhotoURL, c.Channel, c.MDAQueue, c.AssignedTo,
			c.AckDueAt, c.ResolveDueAt, c.AckedAt, c.ResolvedAt, c.ClosedAt, c.MergedInto,
			c.SLABreachAck, c.SLABreachResolve).Scan(&c.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
}

// NextCivicEventSeq atomically advances the per-case event sequence and
// returns the new value, backing the deterministic CloudEvent id
// tenant:civic:{ref}:{seq} (SPEC-W32 WS-A).
func (s *Store) NextCivicEventSeq(ctx context.Context, tenantID, caseID uuid.UUID) (int64, error) {
	var seq int64
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`UPDATE civic_cases SET event_seq = event_seq + 1 WHERE tenant_id=$1 AND id=$2 RETURNING event_seq`,
			tenantID, caseID).Scan(&seq)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return seq, err
}

// CivicCaseStats aggregates open/resolved counts by category and ward
// (aggregate-only — no person columns are selected, SPEC-W32 §4.1). Merged
// duplicates are excluded (their canonical case carries the count).
func (s *Store) CivicCaseStats(ctx context.Context, tenantID uuid.UUID) (CivicStats, error) {
	stats := CivicStats{ByCategory: []CivicStatRow{}, ByWard: []CivicStatRow{}}
	const openCase = `CASE WHEN status IN ('resolved','closed') THEN 'resolved' ELSE 'open' END`
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		// Totals.
		rows, err := tx.Query(ctx,
			`SELECT `+openCase+` AS bucket, count(*) FROM civic_cases
			 WHERE tenant_id=$1 AND merged_into IS NULL GROUP BY 1`, tenantID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var bucket string
			var n int
			if err := rows.Scan(&bucket, &n); err != nil {
				rows.Close()
				return err
			}
			if bucket == "resolved" {
				stats.Resolved = n
			} else {
				stats.Open = n
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		// By category (slug key).
		rows, err = tx.Query(ctx,
			`SELECT COALESCE(cat.slug, 'other'), `+openCase+` AS bucket, count(*)
			 FROM civic_cases c LEFT JOIN civic_categories cat
			   ON cat.tenant_id=c.tenant_id AND cat.id=c.category_id
			 WHERE c.tenant_id=$1 AND c.merged_into IS NULL GROUP BY 1, 2 ORDER BY 1, 2`, tenantID)
		if err != nil {
			return err
		}
		byCat := map[string]*CivicStatRow{}
		for rows.Next() {
			var key, bucket string
			var n int
			if err := rows.Scan(&key, &bucket, &n); err != nil {
				rows.Close()
				return err
			}
			row := byCat[key]
			if row == nil {
				row = &CivicStatRow{Key: key}
				byCat[key] = row
			}
			if bucket == "resolved" {
				row.Resolved = n
			} else {
				row.Open = n
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		// By ward.
		rows, err = tx.Query(ctx,
			`SELECT COALESCE(NULLIF(ward,''), 'unspecified'), `+openCase+` AS bucket, count(*)
			 FROM civic_cases WHERE tenant_id=$1 AND merged_into IS NULL GROUP BY 1, 2 ORDER BY 1, 2`, tenantID)
		if err != nil {
			return err
		}
		byWard := map[string]*CivicStatRow{}
		for rows.Next() {
			var key, bucket string
			var n int
			if err := rows.Scan(&key, &bucket, &n); err != nil {
				rows.Close()
				return err
			}
			row := byWard[key]
			if row == nil {
				row = &CivicStatRow{Key: key}
				byWard[key] = row
			}
			if bucket == "resolved" {
				row.Resolved = n
			} else {
				row.Open = n
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, r := range byCat {
			stats.ByCategory = append(stats.ByCategory, *r)
		}
		for _, r := range byWard {
			stats.ByWard = append(stats.ByWard, *r)
		}
		return nil
	})
	return stats, err
}

// DuplicateCivicCaseCandidates returns the coarse duplicate-candidate set
// for one case: same tenant + category, unmerged, created within ±72h of
// the reference time, excluding the case itself. The geo ≤500m filter runs
// in Go (haversine) in the civic service so no PostGIS dependency is added
// (SPEC-W32 WS-A; embedded-postgres test environments lack PostGIS).
func (s *Store) DuplicateCivicCaseCandidates(ctx context.Context, tenantID, categoryID, excludeID uuid.UUID, at time.Time) ([]CivicCase, error) {
	from := at.Add(-72 * time.Hour)
	to := at.Add(72 * time.Hour)
	out := []CivicCase{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+civicCaseCols+` FROM civic_cases
			 WHERE tenant_id=$1 AND category_id=$2 AND id<>$3 AND merged_into IS NULL
			   AND created_at BETWEEN $4 AND $5
			 ORDER BY created_at DESC LIMIT 200`,
			tenantID, categoryID, excludeID, from, to)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanCivicCase(rows)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}
