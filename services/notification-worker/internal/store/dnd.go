package store

// SPEC-W12 Agent B: DND registry (NCC 2442 global list + per-tenant
// opt-outs) backing the marketing-send suppression guard
// (internal/pacer/guards.go) and the /v1/dnd HTTP API
// (internal/httpapi/dnd.go).
//
// The dnd_numbers table lives in the `notifications` database alongside the
// webhook tables and is bootstrapped idempotently like them. Tenant
// isolation is application-level (every tenant-scoped query filters
// tenant_id/tenant_slug) PLUS defense-in-depth RLS (SQL-003): the
// tenant_isolation policy keeps the global NCC 2442 rows (tenant_id NULL)
// visible to every tenant context while scoping per-tenant opt-out rows to
// the session's app.tenant_id (unset GUC = legacy application-level
// posture, see the package header).

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// isNoRows reports whether err is pgx's empty-result error.
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// DND row sources.
const (
	// DNDSourceNCC2442 is a number from the Nigerian Communications
	// Commission 2442 do-not-disturb registry (global rows).
	DNDSourceNCC2442 = "ncc2442"
	// DNDSourceTenantOptOut is a per-tenant opt-out (the customer asked one
	// tenant to stop marketing messages).
	DNDSourceTenantOptOut = "tenant_optout"
)

// Suppression reasons returned by IsSuppressed (consumed by the pacer
// guard as the {reason} label of notifications_suppressed_total).
const (
	DNDReasonTenantOptOut = "tenant_optout"
	DNDReasonGlobalDND    = "global_dnd"
)

// ensureDNDSchema bootstraps the dnd_numbers table idempotently (called
// from New, after ensureSchema).
//
// Uniqueness is enforced by two partial unique indexes because NULL
// tenant_id (global rows) never conflicts under a plain UNIQUE constraint.
func (s *Store) ensureDNDSchema(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS dnd_numbers (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID,                       -- NULL = global NCC 2442 list entry
    tenant_slug TEXT NOT NULL DEFAULT '',
    phone_e164  TEXT NOT NULL,
    source      TEXT NOT NULL DEFAULT 'ncc2442',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_dnd_global_phone
    ON dnd_numbers (phone_e164) WHERE tenant_id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_dnd_tenant_phone
    ON dnd_numbers (tenant_id, phone_e164) WHERE tenant_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_dnd_tenant_slug_phone
    ON dnd_numbers (tenant_slug, phone_e164) WHERE tenant_id IS NOT NULL;
ALTER TABLE dnd_numbers ENABLE ROW LEVEL SECURITY;
ALTER TABLE dnd_numbers FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_policies
                   WHERE schemaname = current_schema()
                     AND tablename = 'dnd_numbers'
                     AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON dnd_numbers
            USING (tenant_id IS NULL OR CASE
                WHEN coalesce(current_setting('app.tenant_id', true), '') = '' THEN true
                ELSE tenant_id = current_setting('app.tenant_id', true)::uuid
            END);
    END IF;
END
$$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure dnd_numbers table: %w", err)
	}
	return nil
}

// NormalizePhone canonicalizes a phone number for DND matching: all
// formatting (spaces, dashes, parentheses, dots) is stripped and a leading
// "00" international prefix is folded to "+". Numbers WITHOUT a country
// code are kept as national digits — importers should load E.164; matching
// is exact-equality on the normalized form (documented in
// docs/dnd-quiet-hours.md).
func NormalizePhone(phone string) string {
	var b strings.Builder
	b.Grow(len(phone))
	for i, r := range phone {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' && i == 0:
			b.WriteRune('+')
		}
	}
	n := b.String()
	if strings.HasPrefix(n, "00") {
		n = "+" + n[2:]
	}
	return n
}

// ImportGlobalDND bulk-loads numbers into the GLOBAL NCC 2442 list
// (tenant_id NULL). Idempotent: existing numbers are skipped
// (ON CONFLICT DO NOTHING), so re-importing an updated registry snapshot is
// safe. source defaults to ncc2442. Returns the number of NEW rows.
func (s *Store) ImportGlobalDND(ctx context.Context, phones []string, source string) (int, error) {
	if source == "" {
		source = DNDSourceNCC2442
	}
	inserted := 0
	for _, p := range phones {
		p = NormalizePhone(p)
		if p == "" {
			continue
		}
		tag, err := s.pool.Exec(ctx,
			`INSERT INTO dnd_numbers (tenant_id, tenant_slug, phone_e164, source)
			 VALUES (NULL, '', $1, $2) ON CONFLICT DO NOTHING`, p, source)
		if err != nil {
			return inserted, fmt.Errorf("import dnd number: %w", err)
		}
		inserted += int(tag.RowsAffected())
	}
	return inserted, nil
}

// AddTenantOptOut records a per-tenant marketing opt-out. Idempotent on
// (tenant_id, phone_e164). Runs inside withTenant so the RLS policy
// hard-enforces the tenant scope of the write.
func (s *Store) AddTenantOptOut(ctx context.Context, tenantID uuid.UUID, tenantSlug, phone string) error {
	phone = NormalizePhone(phone)
	if phone == "" {
		return fmt.Errorf("tenant opt-out: phone is required")
	}
	err := s.withTenant(ctx, tenantID.String(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO dnd_numbers (tenant_id, tenant_slug, phone_e164, source)
			 VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`,
			tenantID, tenantSlug, phone, DNDSourceTenantOptOut)
		return err
	})
	if err != nil {
		return fmt.Errorf("add tenant opt-out: %w", err)
	}
	return nil
}

// IsSuppressed checks the DND registry for phone in the required order
// (SPEC-W12): per-tenant opt-out first (when tenantSlug is known), then the
// global NCC 2442 list. The returned reason is DNDReasonTenantOptOut or
// DNDReasonGlobalDND.
func (s *Store) IsSuppressed(ctx context.Context, tenantSlug, phone string) (bool, string, error) {
	phone = NormalizePhone(phone)
	if phone == "" {
		return false, "", nil
	}
	if tenantSlug != "" {
		var id uuid.UUID
		err := s.pool.QueryRow(ctx,
			`SELECT id FROM dnd_numbers
			 WHERE tenant_id IS NOT NULL AND tenant_slug = $1 AND phone_e164 = $2
			 LIMIT 1`, tenantSlug, phone).Scan(&id)
		if err == nil {
			return true, DNDReasonTenantOptOut, nil
		}
		if !isNoRows(err) {
			return false, "", fmt.Errorf("dnd tenant lookup: %w", err)
		}
	}
	var id uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM dnd_numbers WHERE tenant_id IS NULL AND phone_e164 = $1 LIMIT 1`, phone).Scan(&id)
	if err == nil {
		return true, DNDReasonGlobalDND, nil
	}
	if !isNoRows(err) {
		return false, "", fmt.Errorf("dnd global lookup: %w", err)
	}
	return false, "", nil
}

// RemoveDND honors an opt-out removal: with tenantSlug empty it deletes the
// phone from the global list AND every tenant list (a full re-consent);
// with a tenantSlug it deletes only that tenant's opt-out row. Returns the
// number of rows removed.
func (s *Store) RemoveDND(ctx context.Context, phone, tenantSlug string) (int64, error) {
	phone = NormalizePhone(phone)
	if phone == "" {
		return 0, fmt.Errorf("remove dnd: phone is required")
	}
	if tenantSlug == "" {
		tag, err := s.pool.Exec(ctx, `DELETE FROM dnd_numbers WHERE phone_e164 = $1`, phone)
		if err != nil {
			return 0, fmt.Errorf("remove dnd number: %w", err)
		}
		return tag.RowsAffected(), nil
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM dnd_numbers WHERE tenant_id IS NOT NULL AND tenant_slug = $1 AND phone_e164 = $2`,
		tenantSlug, phone)
	if err != nil {
		return 0, fmt.Errorf("remove tenant opt-out: %w", err)
	}
	return tag.RowsAffected(), nil
}
