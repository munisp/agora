package store

// SPEC-W119 §3.1: persistence for the OpenSearch projection syncer
// (internal/search.Syncer).
//
// TENANT SAFETY — DUAL LAYER. The syncer is a background system process,
// but every fetch below carries BOTH:
//  1. an explicit tenant_id = $1 predicate (application level), and
//  2. the withTenant transaction wrapper (SELECT set_config('app.tenant_id',
//     ...)) so the FORCE-RLS tenant_isolation policy is a second
//     enforcement layer.
// Relying on RLS alone would silently leak rows across tenants anywhere the
// policy is absent (e.g. a mis-provisioned database); relying on the
// predicate alone would leave no defense in depth. Cross-tenant leakage is
// a CRITICAL bug class (SPEC-W119 §2) — keep both layers.
//
// The two bookkeeping tables (search_sync_state, search_contact_hashes)
// deliberately have NO RLS: they hold only UUIDs, timestamps and content
// hashes — no tenant payload — and are read/written by the service role
// without a tenant GUC (the syncer iterates ALL tenants). Tenant
// enumeration goes through the sites registry, the one intentionally
// non-RLS tenant-keyed table (public-site registry).

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// SyncBooking is one bookings row projected into the bookings index
// (contact_name/contact_phone denormalized via LEFT JOIN contacts).
type SyncBooking struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	OfferingID   uuid.UUID
	TeamMemberID *uuid.UUID
	PublicRef    string
	Status       string
	Source       string
	ContactName  string
	ContactPhone string
	StartsAt     time.Time
	EndsAt       time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// SyncLead is one leads row projected into the leads index.
type SyncLead struct {
	ID                  uuid.UUID
	TenantID            uuid.UUID
	PhoneE164           string
	ChannelOfFirstTouch string
	CampaignID          *uuid.UUID
	PromoCode           *string
	Status              string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// SyncContact is one contacts row projected into the contacts index.
type SyncContact struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	Name       string
	Phone      string
	Email      string
	Notes      string
	Source     string
	ExternalID string
}

// ContactHash is one search_contact_hashes row.
type ContactHash struct {
	ContactID uuid.UUID
	TenantID  uuid.UUID
	Hash      string
}

// EnsureSearchSyncSchema bootstraps the syncer bookkeeping tables
// idempotently (W51-C idiom: runtime ensure-DDL, runs as the service role).
// NO RLS by design — see the file header.
func (s *Store) EnsureSearchSyncSchema(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS search_sync_state (
    tenant_id UUID NOT NULL,
    entity    TEXT NOT NULL,
    watermark TIMESTAMPTZ NOT NULL DEFAULT '-infinity',
    last_id   UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    PRIMARY KEY (tenant_id, entity)
);
CREATE TABLE IF NOT EXISTS search_contact_hashes (
    contact_id UUID PRIMARY KEY,
    tenant_id  UUID NOT NULL,
    hash       TEXT NOT NULL
);`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure search sync schema: %w", err)
	}
	return nil
}

// ListSyncTenantIDs enumerates tenants to sweep from the sites registry
// (intentionally non-RLS — see ensureSitesTable).
func (s *Store) ListSyncTenantIDs(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT tenant_id FROM sites ORDER BY tenant_id`)
	if err != nil {
		return nil, fmt.Errorf("list sync tenants: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list sync tenants: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// GetSyncCursor reads the (watermark, last_id) cursor for one
// (tenant, entity); the zero value ('-infinity', uuid.Nil) means "from the
// beginning".
func (s *Store) GetSyncCursor(ctx context.Context, tenantID uuid.UUID, entity string) (time.Time, uuid.UUID, error) {
	// pgtype.Timestamptz: the DDL default watermark is '-infinity', which
	// cannot scan into a bare time.Time — treat any non-finite value as
	// the zero cursor ("from the beginning").
	var wm pgtype.Timestamptz
	var lastID uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT watermark, last_id FROM search_sync_state WHERE tenant_id=$1 AND entity=$2`,
		tenantID, entity).Scan(&wm, &lastID)
	if err == pgx.ErrNoRows {
		return time.Time{}, uuid.Nil, nil
	}
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("get sync cursor: %w", err)
	}
	if !wm.Valid || wm.InfinityModifier != pgtype.Finite {
		return time.Time{}, uuid.Nil, nil
	}
	return wm.Time, lastID, nil
}

// SetSyncCursor upserts the cursor for one (tenant, entity).
func (s *Store) SetSyncCursor(ctx context.Context, tenantID uuid.UUID, entity string, watermark time.Time, lastID uuid.UUID) error {
	if watermark.IsZero() {
		// '-infinity' — before every real timestamp.
		watermark = time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO search_sync_state (tenant_id, entity, watermark, last_id)
		 VALUES ($1,$2,$3,$4)
		 ON CONFLICT (tenant_id, entity)
		 DO UPDATE SET watermark=EXCLUDED.watermark, last_id=EXCLUDED.last_id`,
		tenantID, entity, watermark, lastID); err != nil {
		return fmt.Errorf("set sync cursor: %w", err)
	}
	return nil
}

// FetchBookingsForSync pages bookings changed after the (updated_at, id)
// cursor, oldest first, with the contact denormalization. Explicit
// tenant_id predicate AND withTenant — see the file header.
func (s *Store) FetchBookingsForSync(ctx context.Context, tenantID uuid.UUID, after time.Time, afterID uuid.UUID, limit int) ([]SyncBooking, error) {
	var out []SyncBooking
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT b.id, b.tenant_id, b.offering_id, b.team_member_id,
			       COALESCE(b.public_ref, ''), b.status, b.source,
			       COALESCE(c.name, ''), COALESCE(c.phone, ''),
			       b.starts_at, b.ends_at, b.created_at, b.updated_at
			FROM bookings b
			LEFT JOIN contacts c ON c.id = b.contact_id
			WHERE b.tenant_id = $1 AND (b.updated_at, b.id) > ($2, $3)
			ORDER BY b.updated_at, b.id
			LIMIT $4`, tenantID, after, afterID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b SyncBooking
			if err := rows.Scan(&b.ID, &b.TenantID, &b.OfferingID, &b.TeamMemberID,
				&b.PublicRef, &b.Status, &b.Source, &b.ContactName, &b.ContactPhone,
				&b.StartsAt, &b.EndsAt, &b.CreatedAt, &b.UpdatedAt); err != nil {
				return err
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("fetch bookings for sync: %w", err)
	}
	return out, nil
}

// FetchLeadsForSync pages leads changed after the (updated_at, id) cursor.
// Explicit tenant_id predicate AND withTenant — see the file header.
func (s *Store) FetchLeadsForSync(ctx context.Context, tenantID uuid.UUID, after time.Time, afterID uuid.UUID, limit int) ([]SyncLead, error) {
	var out []SyncLead
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, tenant_id, phone_e164, channel_of_first_touch,
			       campaign_id, promo_code, status, created_at, updated_at
			FROM leads
			WHERE tenant_id = $1 AND (updated_at, id) > ($2, $3)
			ORDER BY updated_at, id
			LIMIT $4`, tenantID, after, afterID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l SyncLead
			if err := rows.Scan(&l.ID, &l.TenantID, &l.PhoneE164, &l.ChannelOfFirstTouch,
				&l.CampaignID, &l.PromoCode, &l.Status, &l.CreatedAt, &l.UpdatedAt); err != nil {
				return err
			}
			out = append(out, l)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("fetch leads for sync: %w", err)
	}
	return out, nil
}

// ScanContactsForSync pages the tenant's contacts in id order (contacts
// have no timestamp columns — the syncer hash-diffs a full scan).
// Explicit tenant_id predicate AND withTenant — see the file header.
func (s *Store) ScanContactsForSync(ctx context.Context, tenantID uuid.UUID, afterID uuid.UUID, limit int) ([]SyncContact, error) {
	var out []SyncContact
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, tenant_id, name, COALESCE(phone, ''), COALESCE(email, ''),
			       notes, COALESCE(source, ''), COALESCE(external_id, '')
			FROM contacts
			WHERE tenant_id = $1 AND id > $2
			ORDER BY id
			LIMIT $3`, tenantID, afterID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c SyncContact
			if err := rows.Scan(&c.ID, &c.TenantID, &c.Name, &c.Phone, &c.Email,
				&c.Notes, &c.Source, &c.ExternalID); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("scan contacts for sync: %w", err)
	}
	return out, nil
}

// ContactHashMap loads the tenant's recorded content hashes (bookkeeping
// table — no RLS, no GUC needed).
func (s *Store) ContactHashMap(ctx context.Context, tenantID uuid.UUID) (map[uuid.UUID]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT contact_id, hash FROM search_contact_hashes WHERE tenant_id=$1`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("contact hash map: %w", err)
	}
	defer rows.Close()
	out := make(map[uuid.UUID]string)
	for rows.Next() {
		var id uuid.UUID
		var h string
		if err := rows.Scan(&id, &h); err != nil {
			return nil, fmt.Errorf("contact hash map: %w", err)
		}
		out[id] = h
	}
	return out, rows.Err()
}

// UpsertContactHash records the content hash for one contact.
func (s *Store) UpsertContactHash(ctx context.Context, tenantID, contactID uuid.UUID, hash string) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO search_contact_hashes (contact_id, tenant_id, hash)
		 VALUES ($1,$2,$3)
		 ON CONFLICT (contact_id) DO UPDATE SET hash=EXCLUDED.hash, tenant_id=EXCLUDED.tenant_id`,
		contactID, tenantID, hash); err != nil {
		return fmt.Errorf("upsert contact hash: %w", err)
	}
	return nil
}

// DeleteContactHash drops one hash row (after the index doc is purged).
func (s *Store) DeleteContactHash(ctx context.Context, contactID uuid.UUID) error {
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM search_contact_hashes WHERE contact_id=$1`, contactID); err != nil {
		return fmt.Errorf("delete contact hash: %w", err)
	}
	return nil
}

// DeletedContactIDs returns hash rows whose contact row no longer exists
// (hard delete) — the anti-join probe runs inside withTenant because
// contacts is RLS'd; the probe itself is tenant-scoped on BOTH sides.
func (s *Store) DeletedContactIDs(ctx context.Context, tenantID uuid.UUID) ([]uuid.UUID, error) {
	var out []uuid.UUID
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT h.contact_id
			FROM search_contact_hashes h
			WHERE h.tenant_id = $1
			  AND NOT EXISTS (
			      SELECT 1 FROM contacts c
			      WHERE c.id = h.contact_id AND c.tenant_id = $1
			  )`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out = append(out, id)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("deleted contact ids: %w", err)
	}
	return out, nil
}
