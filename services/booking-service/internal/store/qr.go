package store

// QR scan ingest (SPEC-W45 CODER-A item 9): printed QR codes land on the
// admin-web /l/{slug} redirect (SPEC-W13 Agent E), which pings the scan
// ingest endpoint so scans are countable even when the visitor bounces
// before the widget loads. Scans are recorded per tenant (resolved from the
// site slug server-side) and read back through the tenant analytics path
// (GET /v1/qr/scans, view_analytics) alongside the QR-attributed leads
// (channel 'qr', SPEC-W13 §3).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// QRScan mirrors booking.qr_scans — one recorded QR code scan.
type QRScan struct {
	ID        uuid.UUID `json:"id"`
	TenantID  uuid.UUID `json:"tenant_id"`
	SiteSlug  string    `json:"site_slug"`
	Ref       string    `json:"ref,omitempty"` // printed-code ref / campaign hint (optional)
	UserAgent string    `json:"user_agent,omitempty"`
	ScannedAt time.Time `json:"scanned_at"`
}

// QRScanSummary is the tenant analytics read: total scans and the distinct
// slugs scanned, newest activity last.
type QRScanSummary struct {
	TotalScans int64      `json:"total_scans"`
	Scans      []QRScan   `json:"scans"`
	From       *time.Time `json:"from,omitempty"`
	To         *time.Time `json:"to,omitempty"`
}

// ensureQRScansTable bootstraps booking.qr_scans idempotently (same
// pattern as ensureReferralTables). RLS: enabled + forced with the
// tenant_isolation policy.
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) ensureQRScansTable(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS qr_scans (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID NOT NULL,
    site_slug  TEXT NOT NULL,
    ref        TEXT NOT NULL DEFAULT '',
    user_agent TEXT NOT NULL DEFAULT '',
    scanned_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_qr_scans_tenant_time ON qr_scans (tenant_id, scanned_at);
ALTER TABLE qr_scans ENABLE ROW LEVEL SECURITY;
ALTER TABLE qr_scans FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'qr_scans' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON qr_scans
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure qr_scans table: %w", err)
	}
	return nil
}

// InsertQRScan records one scan against the tenant resolved from the site
// slug (public ingest path — the caller already validated the slug).
func (s *Store) InsertQRScan(ctx context.Context, scan *QRScan) error {
	if scan.ID == uuid.Nil {
		scan.ID = uuid.New()
	}
	return s.withTenant(ctx, scan.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO qr_scans (id, tenant_id, site_slug, ref, user_agent)
			 VALUES ($1,$2,$3,$4,$5) RETURNING scanned_at`,
			scan.ID, scan.TenantID, scan.SiteSlug, scan.Ref, scan.UserAgent).Scan(&scan.ScannedAt)
	})
}

// ListQRScans returns the tenant's scans in [from,to] (nil = unbounded),
// newest first, capped at 1000 — the QR analytics read path.
func (s *Store) ListQRScans(ctx context.Context, tenantID uuid.UUID, from, to *time.Time) (QRScanSummary, error) {
	q := `SELECT id, tenant_id, site_slug, ref, user_agent, scanned_at FROM qr_scans WHERE tenant_id=$1`
	args := []any{tenantID}
	n := 1
	if from != nil {
		n++
		q += fmt.Sprintf(` AND scanned_at >= $%d`, n)
		args = append(args, *from)
	}
	if to != nil {
		n++
		q += fmt.Sprintf(` AND scanned_at <= $%d`, n)
		args = append(args, *to)
	}
	sum := QRScanSummary{Scans: []QRScan{}, From: from, To: to}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM qr_scans WHERE tenant_id=$1`, tenantID).Scan(&sum.TotalScans); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, q+` ORDER BY scanned_at DESC LIMIT 1000`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sc QRScan
			if err := rows.Scan(&sc.ID, &sc.TenantID, &sc.SiteSlug, &sc.Ref, &sc.UserAgent, &sc.ScannedAt); err != nil {
				return err
			}
			sum.Scans = append(sum.Scans, sc)
		}
		return rows.Err()
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return sum, nil
	}
	return sum, err
}
