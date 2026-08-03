// Package store provides Postgres persistence for the booking DB per SPEC §7:
// offerings, team_members, availability_rules, contacts, bookings, outbox.
// A small `sites` table (public booking pages) is bootstrapped here because
// SPEC §7 does not define it — see README "Schema notes".
//
// RLS: every tenant-scoped query runs inside a transaction that first sets
// `SET LOCAL app.tenant_id` (via set_config(..., true)) so the FORCE ROW
// LEVEL SECURITY policies of 01-booking-schema.sql enforce tenant isolation
// at the database layer, even if an application-level filter regresses.
// The only exceptions are explicitly annotated superuser/cross-tenant paths
// (schema bootstrap, the outbox dispatcher, public site-slug resolution).
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned on unique-constraint violations
// (e.g. duplicate idempotency_key).
var ErrConflict = errors.New("conflict")

// Store wraps a pgx connection pool.
type Store struct {
	pool *pgxpool.Pool
	// geoEnabled reports whether the PostGIS geo tables (SPEC-W8) were
	// bootstrapped; false when the server lacks the postgis extension
	// (e.g. embedded-Postgres tests) — geo store methods then return
	// ErrPostGISUnavailable instead of failing at query time.
	geoEnabled bool
}

// New connects to Postgres, verifies connectivity and ensures the `sites`
// table exists (see package doc). maxConns sizes the pool; <= 0 keeps the
// pgx default (min(4, NumCPU)). Size for PEAK concurrent calls, not average:
// docs/runbooks/capacity-planning.md §DB connection pools
// (peak_calls × peak turns_per_call); a pool sized for average injects dead
// air mid-call while every dashboard looks green.
func New(ctx context.Context, databaseURL string, maxConns int32) (*Store, error) {
	poolCfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse postgres config: %w", err)
	}
	if maxConns > 0 {
		poolCfg.MaxConns = maxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	s := &Store{pool: pool}
	if err := s.ensureSitesTable(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if err := s.ensureWaitlistTable(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if err := s.ensureCRMColumns(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if err := s.ensurePortalTokensTable(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	// SPEC-W11 Part B: incidents + incident_deliveries +
	// incident_dispatch_endpoints (idempotent bootstrap, RLS-enabled).
	if err := s.ensureIncidentTables(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	// SPEC-W13 Agent A: leads + promo_codes + promo_redemptions +
	// campaigns + campaign_spend (idempotent bootstrap, RLS-enabled).
	if err := s.ensureLeadTables(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	// Geo tables (SPEC-W8): best-effort — a missing postgis extension
	// disables geo features without breaking the rest of the service.
	if err := s.ensureGeoTables(ctx); err != nil {
		if !errors.Is(err, ErrPostGISUnavailable) {
			pool.Close()
			return nil, err
		}
	} else {
		s.geoEnabled = true
	}
	return s, nil
}

// withTenant runs fn inside a transaction with `SET LOCAL app.tenant_id`
// applied, so Postgres RLS policies (tenant_isolation on every tenant table)
// scope every statement of fn to the given tenant. This is the ONLY way
// tenant-scoped queries should run: defence-in-depth on top of the
// application-level tenant_id filters.
func (s *Store) withTenant(ctx context.Context, tenantID uuid.UUID, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// set_config(name, value, is_local=true) == SET LOCAL: parameter binding
	// keeps this injection-safe (unlike fmt.Sprintf into a SET statement).
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID.String()); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ensureSitesTable creates the public-site registry when missing. It is
// IF NOT EXISTS so it never conflicts with infra-managed init scripts.
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) ensureSitesTable(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS sites (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL,
    tenant_slug TEXT NOT NULL,
    slug        TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL DEFAULT '',
    published   BOOLEAN NOT NULL DEFAULT TRUE,
    theme       JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS sites_tenant_idx ON sites(tenant_id);
ALTER TABLE sites ADD COLUMN IF NOT EXISTS theme JSONB NOT NULL DEFAULT '{}';`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure sites table: %w", err)
	}
	return nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping checks database liveness (used by /healthz).
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ---------------------------------------------------------------------------
// Catalog: offerings
// ---------------------------------------------------------------------------

// Offering mirrors booking.offerings.
type Offering struct {
	ID          uuid.UUID `json:"id"`
	TenantID    uuid.UUID `json:"tenant_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	DurationMin int       `json:"duration_min"`
	BufferMin   int       `json:"buffer_min"`
	PriceCents  int64     `json:"price_cents"`
	Currency    string    `json:"currency"`
	Capacity    int       `json:"capacity"`
	Bookable    bool      `json:"bookable"`
	CreatedAt   time.Time `json:"created_at"`
}

const offeringCols = `id, tenant_id, name, description, duration_min, buffer_min, price_cents, currency, capacity, bookable, created_at`

func scanOffering(row pgx.Row) (Offering, error) {
	var o Offering
	err := row.Scan(&o.ID, &o.TenantID, &o.Name, &o.Description, &o.DurationMin,
		&o.BufferMin, &o.PriceCents, &o.Currency, &o.Capacity, &o.Bookable, &o.CreatedAt)
	return o, err
}

// CreateOffering inserts an offering.
func (s *Store) CreateOffering(ctx context.Context, o *Offering) error {
	if o.ID == uuid.Nil {
		o.ID = uuid.New()
	}
	const q = `INSERT INTO offerings (` + offeringCols + `) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,now()) RETURNING created_at`
	return s.withTenant(ctx, o.TenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, q, o.ID, o.TenantID, o.Name, o.Description, o.DurationMin,
			o.BufferMin, o.PriceCents, o.Currency, o.Capacity, o.Bookable).Scan(&o.CreatedAt)
		if err != nil {
			return fmt.Errorf("insert offering: %w", err)
		}
		return nil
	})
}

// ListOfferings returns all offerings of a tenant.
func (s *Store) ListOfferings(ctx context.Context, tenantID uuid.UUID) ([]Offering, error) {
	const q = `SELECT ` + offeringCols + ` FROM offerings WHERE tenant_id = $1 ORDER BY created_at`
	var out []Offering
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			o, err := scanOffering(rows)
			if err != nil {
				return err
			}
			out = append(out, o)
		}
		return rows.Err()
	})
	return out, err
}

// GetOffering fetches one offering scoped to a tenant.
func (s *Store) GetOffering(ctx context.Context, tenantID, id uuid.UUID) (Offering, error) {
	const q = `SELECT ` + offeringCols + ` FROM offerings WHERE tenant_id = $1 AND id = $2`
	var o Offering
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		o, err = scanOffering(tx.QueryRow(ctx, q, tenantID, id))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return o, ErrNotFound
	}
	return o, err
}

// UpdateOffering replaces mutable offering fields.
func (s *Store) UpdateOffering(ctx context.Context, o *Offering) error {
	const q = `UPDATE offerings SET name=$3, description=$4, duration_min=$5, buffer_min=$6,
	           price_cents=$7, currency=$8, capacity=$9, bookable=$10
	           WHERE tenant_id=$1 AND id=$2`
	return s.withTenant(ctx, o.TenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, q, o.TenantID, o.ID, o.Name, o.Description, o.DurationMin,
			o.BufferMin, o.PriceCents, o.Currency, o.Capacity, o.Bookable)
		if err != nil {
			return fmt.Errorf("update offering: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// DeleteOffering removes an offering.
func (s *Store) DeleteOffering(ctx context.Context, tenantID, id uuid.UUID) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM offerings WHERE tenant_id=$1 AND id=$2`, tenantID, id)
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
// Team members + availability rules
// ---------------------------------------------------------------------------

// TeamMember mirrors booking.team_members.
type TeamMember struct {
	ID       uuid.UUID `json:"id"`
	TenantID uuid.UUID `json:"tenant_id"`
	Name     string    `json:"name"`
	Email    string    `json:"email"`
	Role     string    `json:"role"`
	Active   bool      `json:"active"`
}

// AvailabilityRule mirrors booking.availability_rules.
type AvailabilityRule struct {
	ID            uuid.UUID  `json:"id"`
	TenantID      uuid.UUID  `json:"tenant_id"`
	TeamMemberID  uuid.UUID  `json:"team_member_id"`
	Weekday       int        `json:"weekday"` // 0=Sunday .. 6=Saturday
	StartMin      int        `json:"start_min"`
	EndMin        int        `json:"end_min"`
	EffectiveFrom *time.Time `json:"effective_from,omitempty"`
	EffectiveTo   *time.Time `json:"effective_to,omitempty"`
}

// CreateTeamMember inserts a team member.
func (s *Store) CreateTeamMember(ctx context.Context, m *TeamMember) error {
	if m.ID == uuid.Nil {
		m.ID = uuid.New()
	}
	const q = `INSERT INTO team_members (id, tenant_id, name, email, role, active) VALUES ($1,$2,$3,$4,$5,$6)`
	return s.withTenant(ctx, m.TenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, q, m.ID, m.TenantID, m.Name, m.Email, m.Role, m.Active); err != nil {
			return fmt.Errorf("insert team member: %w", err)
		}
		return nil
	})
}

// ListTeamMembers returns team members of a tenant.
func (s *Store) ListTeamMembers(ctx context.Context, tenantID uuid.UUID) ([]TeamMember, error) {
	var out []TeamMember
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, name, email, role, active FROM team_members WHERE tenant_id=$1 ORDER BY name`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m TeamMember
			if err := rows.Scan(&m.ID, &m.TenantID, &m.Name, &m.Email, &m.Role, &m.Active); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

// GetTeamMemberByEmail fetches the (active or not) team member whose email
// matches case-insensitively — used by GET /v1/bookings?mine=true to map
// the caller's identity to their team-member row.
func (s *Store) GetTeamMemberByEmail(ctx context.Context, tenantID uuid.UUID, email string) (TeamMember, error) {
	var m TeamMember
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT id, tenant_id, name, email, role, active FROM team_members
			 WHERE tenant_id=$1 AND lower(email)=lower($2) ORDER BY name LIMIT 1`,
			tenantID, email).Scan(&m.ID, &m.TenantID, &m.Name, &m.Email, &m.Role, &m.Active)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrNotFound
	}
	return m, err
}

// GetTeamMember fetches one team member scoped to a tenant.
func (s *Store) GetTeamMember(ctx context.Context, tenantID, id uuid.UUID) (TeamMember, error) {
	var m TeamMember
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT id, tenant_id, name, email, role, active FROM team_members WHERE tenant_id=$1 AND id=$2`,
			tenantID, id).Scan(&m.ID, &m.TenantID, &m.Name, &m.Email, &m.Role, &m.Active)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrNotFound
	}
	return m, err
}

// UpdateTeamMember replaces mutable team member fields.
func (s *Store) UpdateTeamMember(ctx context.Context, m *TeamMember) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE team_members SET name=$3, email=$4, role=$5, active=$6 WHERE tenant_id=$1 AND id=$2`,
			m.TenantID, m.ID, m.Name, m.Email, m.Role, m.Active)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// DeleteTeamMember removes a team member.
func (s *Store) DeleteTeamMember(ctx context.Context, tenantID, id uuid.UUID) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM team_members WHERE tenant_id=$1 AND id=$2`, tenantID, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// SetAvailability atomically replaces all weekly rules of a team member.
func (s *Store) SetAvailability(ctx context.Context, tenantID, teamMemberID uuid.UUID, rules []AvailabilityRule) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM availability_rules WHERE tenant_id=$1 AND team_member_id=$2`, tenantID, teamMemberID)
		if err != nil {
			return fmt.Errorf("clear availability rules: %w", err)
		}
		const ins = `INSERT INTO availability_rules (id, tenant_id, team_member_id, weekday, start_min, end_min, effective_from, effective_to)
		             VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`
		for _, r := range rules {
			if r.ID == uuid.Nil {
				r.ID = uuid.New()
			}
			if _, err := tx.Exec(ctx, ins, r.ID, tenantID, teamMemberID, r.Weekday, r.StartMin, r.EndMin, r.EffectiveFrom, r.EffectiveTo); err != nil {
				return fmt.Errorf("insert availability rule: %w", err)
			}
		}
		return nil
	})
}

// ListAvailabilityRules returns rules for a team member.
func (s *Store) ListAvailabilityRules(ctx context.Context, tenantID, teamMemberID uuid.UUID) ([]AvailabilityRule, error) {
	var out []AvailabilityRule
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, team_member_id, weekday, start_min, end_min, effective_from, effective_to
			 FROM availability_rules WHERE tenant_id=$1 AND team_member_id=$2 ORDER BY weekday, start_min`,
			tenantID, teamMemberID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r AvailabilityRule
			if err := rows.Scan(&r.ID, &r.TenantID, &r.TeamMemberID, &r.Weekday, &r.StartMin, &r.EndMin, &r.EffectiveFrom, &r.EffectiveTo); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Contacts
// ---------------------------------------------------------------------------

// Contact mirrors booking.contacts. Source/ExternalID are set only for
// contacts created by the reverse CRM sync (e.g. source='twenty'); they are
// NULL for regular contacts.
type Contact struct {
	ID         uuid.UUID `json:"id"`
	TenantID   uuid.UUID `json:"tenant_id"`
	Name       string    `json:"name"`
	Phone      string    `json:"phone"`
	Email      string    `json:"email"`
	Notes      string    `json:"notes"`
	Source     string    `json:"source,omitempty"`
	ExternalID string    `json:"external_id,omitempty"`
}

// CreateContact inserts a contact. Empty Source/ExternalID are stored as
// NULL (regular contact).
func (s *Store) CreateContact(ctx context.Context, c *Contact) error {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	const q = `INSERT INTO contacts (id, tenant_id, name, phone, email, notes, source, external_id)
	           VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),NULLIF($8,''))`
	return s.withTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, q, c.ID, c.TenantID, c.Name, c.Phone, c.Email, c.Notes, c.Source, c.ExternalID); err != nil {
			return fmt.Errorf("insert contact: %w", err)
		}
		return nil
	})
}

// ListContacts returns contacts of a tenant.
func (s *Store) ListContacts(ctx context.Context, tenantID uuid.UUID) ([]Contact, error) {
	var out []Contact
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, name, phone, email, notes FROM contacts WHERE tenant_id=$1 ORDER BY name`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c Contact
			if err := rows.Scan(&c.ID, &c.TenantID, &c.Name, &c.Phone, &c.Email, &c.Notes); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// GetContact fetches one contact scoped to a tenant.
func (s *Store) GetContact(ctx context.Context, tenantID, id uuid.UUID) (Contact, error) {
	var c Contact
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT id, tenant_id, name, phone, email, notes FROM contacts WHERE tenant_id=$1 AND id=$2`,
			tenantID, id).Scan(&c.ID, &c.TenantID, &c.Name, &c.Phone, &c.Email, &c.Notes)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// UpdateContact replaces mutable contact fields.
func (s *Store) UpdateContact(ctx context.Context, c *Contact) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE contacts SET name=$3, phone=$4, email=$5, notes=$6 WHERE tenant_id=$1 AND id=$2`,
			c.TenantID, c.ID, c.Name, c.Phone, c.Email, c.Notes)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// DeleteContact removes a contact.
func (s *Store) DeleteContact(ctx context.Context, tenantID, id uuid.UUID) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM contacts WHERE tenant_id=$1 AND id=$2`, tenantID, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}
