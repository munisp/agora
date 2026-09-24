// Package poolx builds pgx connection pools with the SPEC-W46 (PERF-08)
// connection budgets: pgx defaults (MaxConns=max(4,NumCPU), MinConns=0, no
// lifetime) queue under modest concurrency and pay cold-connect latency on
// first queries. The budgets below keep the whole fleet inside the
// compose-level max_connections=300 (SPEC-W46 I-01):
//
//	identity-service  (this process): store 10 + store-internal 4
//	    + consent 10 + consent-internal 4 + apps 10        = 38 conns/replica
//	notification-worker: main 10 + internal 4              = 14
//	kyc-service: 10 · crm-sync-service: 10 · messaging-gateway: 0
//	booking-service (W46-A): ~40 · conversation 10 · billing 10
//	payments 8 · knowledge 8 · permify 20 · temporal ~40 · misc ~30
//	=> fleet worst case ~250 < 300 with headroom.
//
// Budgets are DEPLOY-OVERRIDABLE via the pgx stdlib conn-string params
// pool_max_conns / pool_min_conns / pool_max_conn_lifetime on DATABASE_URL
// (and INTERNAL_DATABASE_URL) — values present in the URL always win.
package poolx

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SPEC-W46 W46-B satellite defaults (booking-service, the "main" service,
// uses 20 — see its own pool helper).
const (
	// MaxConns is the per-pool default for a satellite service pool.
	MaxConns int32 = 10
	// InternalMaxConns is the default for the low-traffic internal/escape
	// role pools (tenant-table ops, outbox sweeps, suppression checks).
	InternalMaxConns int32 = 4
	// MinConns keeps two connections warm so first queries after idle do
	// not pay the ~5-15ms cold-connect latency.
	MinConns int32 = 2
	// InternalMinConns keeps one warm connection on the internal pools.
	InternalMinConns int32 = 1
	// MaxConnLifetime recycles connections so stale/server-side-killed
	// conns are re-established predictably.
	MaxConnLifetime = 30 * time.Minute
)

// New parses databaseURL exactly like pgxpool.New (pgxpool.ParseConfig
// natively honors the pool_max_conns / pool_min_conns /
// pool_max_conn_lifetime conn-string params), fills in the W46 satellite
// defaults for every pool parameter the URL leaves unset, and connects.
// maxConns/minConns are the caller's per-pool budgets (MaxConns /
// InternalMaxConns above).
func New(ctx context.Context, databaseURL string, maxConns, minConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	// The URL wins wherever it sets a pool param explicitly (substring
	// covers both the postgres://?pool_max_conns= query form and the
	// keyword DSN form).
	if !strings.Contains(databaseURL, "pool_max_conns") {
		cfg.MaxConns = maxConns
	}
	if !strings.Contains(databaseURL, "pool_min_conns") {
		cfg.MinConns = minConns
	}
	if !strings.Contains(databaseURL, "pool_max_conn_lifetime") {
		cfg.MaxConnLifetime = MaxConnLifetime
	}
	if cfg.MinConns > cfg.MaxConns { // a URL-set max below the default min
		cfg.MinConns = cfg.MaxConns
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}
