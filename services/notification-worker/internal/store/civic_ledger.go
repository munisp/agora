package store

// SPEC-W32 WS-B: civic delivery ledger. Every citizen status-notification
// attempt and every SLA-breach escalation lands here (one row per Temporal
// activity attempt), giving the e2e "citizen notification ledger entry"
// gate and the ops runbook a queryable trail. Citizen phones are stored
// normalized (store.NormalizePhone), exactly like the DND registry; the
// case ref stored is the CANONICAL ref after a merge (SPEC-W32 §4.3).

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CivicNotification mirrors one civic_notifications ledger row.
type CivicNotification struct {
	ID         uuid.UUID `json:"id"`
	TenantID   string    `json:"tenant_id"`
	TenantSlug string    `json:"tenant_slug"`
	Ref        string    `json:"ref"` // canonical ref after a merge
	Status     string    `json:"status"`
	Channel    string    `json:"channel"`
	Phone      string    `json:"phone"` // normalized E.164 ("" for escalations)
	Outcome    string    `json:"outcome"`
	Attempt    int       `json:"attempt"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// ensureCivicLedgerSchema bootstraps the civic_notifications table
// idempotently (called from New, after ensureDNDSchema).
func (s *Store) ensureCivicLedgerSchema(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS civic_notifications (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   TEXT NOT NULL DEFAULT '',
    tenant_slug TEXT NOT NULL DEFAULT '',
    ref         TEXT NOT NULL,
    status      TEXT NOT NULL,
    channel     TEXT NOT NULL DEFAULT 'sms',
    phone_e164  TEXT NOT NULL DEFAULT '',
    outcome     TEXT NOT NULL
                CHECK (outcome IN ('sent','failed','escalated')),
    attempt     INTEGER NOT NULL DEFAULT 1,
    error       TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_civic_notifications_ref
    ON civic_notifications (tenant_slug, ref, created_at DESC);
ALTER TABLE civic_notifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE civic_notifications FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_policies
                   WHERE schemaname = current_schema()
                     AND tablename = 'civic_notifications'
                     AND policyname = 'tenant_isolation') THEN
        -- tenant_id is TEXT here (civic refs are not UUID-keyed), so the
        -- policy compares the GUC without the ::uuid cast.
        CREATE POLICY tenant_isolation ON civic_notifications
            USING (CASE
                WHEN coalesce(current_setting('app.tenant_id', true), '') = '' THEN true
                ELSE tenant_id = current_setting('app.tenant_id', true)
            END);
    END IF;
END
$$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure civic_notifications table: %w", err)
	}
	return nil
}

// RecordCivicNotification appends one ledger row (id generated). Outcome
// must be sent|failed|escalated (CHECK constraint). Runs inside withTenant
// so the RLS policy hard-enforces the tenant scope of the write.
func (s *Store) RecordCivicNotification(ctx context.Context, tenantID, tenantSlug, ref, status, channel, phone, outcome string, attempt int, errText string) error {
	const q = `INSERT INTO civic_notifications
	    (tenant_id, tenant_slug, ref, status, channel, phone_e164, outcome, attempt, error)
	    VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, q, tenantID, tenantSlug, ref, status, channel, NormalizePhone(phone), outcome, attempt, errText)
		return err
	})
	if err != nil {
		return fmt.Errorf("record civic notification: %w", err)
	}
	return nil
}
