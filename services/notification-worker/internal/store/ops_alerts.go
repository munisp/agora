package store

// SPEC-W44 K3 / F15-04: ops-alert persistence. The ops-alerts consumer
// (internal/opsalerts) consumes opendesk.ops.alerts CloudEvents (produced by
// activities.EmitOpsAlert and peers) and lands them here for the
// role-gated GET /v1/ops-alerts read-back. Idempotent on the CloudEvent id
// (UNIQUE event_id + ON CONFLICT DO NOTHING) so redelivery never
// double-records.
//
// RLS (N-08): fail-closed NULLIF policy on the app.tenant_id GUC with the
// role-gated internal escape (app_notifications_internal), matching the
// billing 0002 idiom — alerts span tenants (platform-wide alerts carry an
// empty tenant_id), so the read path uses the internal pool.

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// OpsAlert mirrors one ops_alerts row.
type OpsAlert struct {
	ID         uuid.UUID `json:"id"`
	EventID    string    `json:"event_id"`
	TenantID   string    `json:"tenant_id"`
	Source     string    `json:"source"`
	Type       string    `json:"type"`
	Severity   string    `json:"severity"`
	Payload    []byte    `json:"payload"` // raw CloudEvent JSON
	ReceivedAt time.Time `json:"received_at"`
}

// ensureOpsAlertsSchema bootstraps the ops_alerts table idempotently
// (called from New, after ensureCivicLedgerSchema).
func (s *Store) ensureOpsAlertsSchema(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS ops_alerts (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id    TEXT NOT NULL,
    tenant_id   TEXT NOT NULL DEFAULT '',
    source      TEXT NOT NULL DEFAULT '',
    type        TEXT NOT NULL DEFAULT '',
    severity    TEXT NOT NULL DEFAULT 'info',
    payload     JSONB NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_ops_alerts_event ON ops_alerts (event_id);
CREATE INDEX IF NOT EXISTS idx_ops_alerts_received ON ops_alerts (received_at DESC);
ALTER TABLE ops_alerts ENABLE ROW LEVEL SECURITY;
ALTER TABLE ops_alerts FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON ops_alerts;
CREATE POLICY tenant_isolation ON ops_alerts
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
           OR pg_has_role(current_user, 'app_notifications_internal', 'member'))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
           OR pg_has_role(current_user, 'app_notifications_internal', 'member'));`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure ops_alerts table: %w", err)
	}
	return nil
}

// InsertOpsAlert persists one consumed alert. Idempotent on event_id: a
// redelivered CloudEvent is a no-op reporting inserted=false.
func (s *Store) InsertOpsAlert(ctx context.Context, a *OpsAlert) (bool, error) {
	if a.EventID == "" {
		return false, fmt.Errorf("insert ops alert: event_id is required")
	}
	const q = `INSERT INTO ops_alerts (event_id, tenant_id, source, type, severity, payload)
	           VALUES ($1,$2,$3,$4,$5,$6)
	           ON CONFLICT (event_id) DO NOTHING
	           RETURNING id, received_at`
	err := s.internal().QueryRow(ctx, q,
		a.EventID, a.TenantID, a.Source, a.Type, a.Severity, a.Payload).
		Scan(&a.ID, &a.ReceivedAt)
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("insert ops alert: %w", err)
	}
	return true, nil
}

// ListOpsAlerts returns alerts newest-first; tenantID empty lists across
// tenants (the HTTP layer role-gates that to platform admins). The read
// uses the internal pool (cross-tenant by design).
func (s *Store) ListOpsAlerts(ctx context.Context, tenantID string, limit int) ([]OpsAlert, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT id, event_id, tenant_id, source, type, severity, payload, received_at
	      FROM ops_alerts`
	args := []any{}
	if tenantID != "" {
		q += ` WHERE tenant_id = $1`
		args = append(args, tenantID)
		q += ` ORDER BY received_at DESC LIMIT $2`
		args = append(args, limit)
	} else {
		q += ` ORDER BY received_at DESC LIMIT $1`
		args = append(args, limit)
	}
	var out []OpsAlert
	err := func() error {
		rows, err := s.internal().Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a OpsAlert
			if err := rows.Scan(&a.ID, &a.EventID, &a.TenantID, &a.Source, &a.Type, &a.Severity, &a.Payload, &a.ReceivedAt); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	}()
	if err != nil {
		return nil, fmt.Errorf("list ops alerts: %w", err)
	}
	return out, nil
}
