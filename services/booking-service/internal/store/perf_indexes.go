// perf_indexes.go — SPEC-W46 (W46-A item 1): performance index migration.
//
// booking-service has no standalone migration runner: the established
// migration convention is the idempotent bootstrap ensure* DDL executed by
// store.New (see ensureSitesTable / ensureIdempotencyIndex / the satellite
// stores' ensureSchema). Fresh installs receive the same indexes from the
// infra init scripts (W46-I); this bootstrap migrates EXISTING databases
// idempotently. Every statement runs CONCURRENTLY and therefore must NOT
// execute inside a transaction block or a multi-statement Exec — each index
// is built by its own single-statement Exec on a dedicated connection with
// statement_timeout disabled (the pool's AfterConnect pins 15s, which a
// concurrent build on a large table can exceed).
//
// Audit refs: P-GO PERF-01/03/04/12/13/17 + P-DATA DDL #1,#2,#3,#6,#7,#10,#11.
//
// NOTE (RLS): superuser bootstrap path, intentionally outside withTenant.
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// perfIndex is one CONCURRENTLY-built index. table guards the build: when
// the owning table does not exist yet (embedded-postgres test harnesses
// bootstrap only the tables they exercise), the index is skipped — the
// owner store's own ensureSchema creates it on first use.
type perfIndex struct {
	table string
	ddl   string
}

// perfIndexDDL mirrors the P-DATA audit DDL pack for booking-DB tables.
var perfIndexDDL = []perfIndex{
	// DDL #1 (PERF-01): availability engine + slot-overlap recheck — the
	// hottest booking query (runs per availability read AND per booking
	// create/reschedule under the per-member advisory lock).
	{"bookings", `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_bookings_tenant_member_starts
		ON bookings (tenant_id, team_member_id, starts_at) WHERE status <> 'cancelled'`},
	// DDL #2 (PERF-03/04): CRM-360 per-contact history + lending signals;
	// contact_id was an unindexed FK.
	{"bookings", `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_bookings_tenant_contact_starts
		ON bookings (tenant_id, contact_id, starts_at DESC)`},
	// DDL #3: the stale-pending sweeper is cross-tenant, so the
	// tenant-leading indexes cannot serve it.
	{"bookings", `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_bookings_pending_created
		ON bookings (created_at) WHERE status = 'pending'`},
	// DDL #6: the nightly recon cron is cross-tenant.
	{"commission_payouts", `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_commission_payouts_processing
		ON commission_payouts (created_at) WHERE status = 'processing'`},
	// DDL #7: matured-payout nightly scan GROUP BY account/beneficiary.
	{"commission_ledger", `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_commission_ledger_acct_benef
		ON commission_ledger (account_code, beneficiary_id, created_at)`},
	// DDL #10: the PUBLIC unauthenticated redeem endpoint resolves a code
	// without tenant context; the PK is (tenant_id, code).
	{"promo_codes", `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_promo_codes_code
		ON promo_codes (code)`},
	// DDL #11 (PERF-17 + PERF-13 family): leading-wildcard ILIKE search.
	// tickets/workorders GIN trgm indexes live in the helpdesk/workorders
	// stores (their ensureSchema owns those tables and they may not exist
	// at store.New time).
	{"contacts", `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_contacts_name_trgm
		ON contacts USING gin (name gin_trgm_ops)`},
}

// trgmTables are the pg_trgm GIN targets bootstrapped here (contacts only);
// they require the pg_trgm extension, which is best-effort (see below).
var trgmTables = map[string]bool{"contacts": true}

// ensurePerfIndexes builds the W46 performance indexes idempotently. The
// B-tree indexes are boot-blocking on error (they guard the write hot
// path); the pg_trgm extension + GIN trigram index are best-effort — a
// server without the contrib module degrades to the previous seq-scan
// search behavior instead of failing the boot (same posture as the
// PostGIS-dependent geo tables).
func (s *Store) ensurePerfIndexes(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire perf-index connection: %w", err)
	}
	defer conn.Release()
	// Concurrent index builds on large tables can take minutes; the pool's
	// 15s statement_timeout must not kill them. Session-local, released
	// back to the pool AFTER this connection is discarded (pgx resets
	// session state on Release? no — so reset explicitly at the end).
	if _, err := conn.Exec(ctx, `SET statement_timeout = 0`); err != nil {
		return fmt.Errorf("relax statement_timeout for index builds: %w", err)
	}
	defer func() {
		// Restore the pool-wide posture before the conn is reused.
		_, _ = conn.Exec(context.Background(), `SET statement_timeout = '15s'`)
	}()

	trgmOK := false
	for _, ix := range perfIndexDDL {
		exists, err := tableExistsOnConn(ctx, conn, ix.table)
		if err != nil {
			return fmt.Errorf("probe table %s: %w", ix.table, err)
		}
		if !exists {
			continue
		}
		if trgmTables[ix.table] && !trgmOK {
			// Best-effort extension: without it the GIN trgm index cannot
			// build — skip ONLY that index, never fail the boot.
			if _, err := conn.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_trgm`); err != nil {
				continue
			}
			trgmOK = true
		}
		if _, err := conn.Exec(ctx, ix.ddl); err != nil {
			if trgmTables[ix.table] {
				// Trigram index build failure degrades search performance;
				// it must not take the service down (geo-table posture).
				continue
			}
			return fmt.Errorf("build perf index on %s: %w", ix.table, err)
		}
	}
	return nil
}

// tableExistsOnConn reports whether name is a table in the public schema
// (to_regclass returns NULL for missing tables).
func tableExistsOnConn(ctx context.Context, conn interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, name string) (bool, error) {
	var reg *string
	if err := conn.QueryRow(ctx, `SELECT to_regclass($1)::text`, "public."+name).Scan(&reg); err != nil {
		return false, err
	}
	return reg != nil, nil
}
