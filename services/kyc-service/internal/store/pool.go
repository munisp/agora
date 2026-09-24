package store

// SPEC-W46 PERF-08: budgeted pgx pool. pgx defaults (MaxConns=
// max(4,NumCPU), MinConns=0, no lifetime) queue under modest concurrency
// and pay cold-connect latency on first queries. Budgets keep the fleet
// inside the compose-level max_connections=300 (SPEC-W46 I-01):
//
//	kyc-service (this process): 10 conns/replica
//	identity-service: 38 · notification-worker 14 · crm-sync 10
//	messaging-gateway: 0 · booking-service (W46-A): ~40 · conversation 10
//	billing 10 · payments 8 · knowledge 8 · permify 20 · temporal ~40 · misc ~30
//	=> fleet worst case ~250 < 300 with headroom.
//
// Budgets are DEPLOY-OVERRIDABLE via the pgx stdlib conn-string params
// pool_max_conns / pool_min_conns / pool_max_conn_lifetime on DATABASE_URL
// — values present in the URL always win.

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// W46-B satellite pool budgets (booking-service, the "main" service, uses
// 20 — see its own helper).
const (
	// poolMaxConns is the per-pool default for a satellite service pool.
	poolMaxConns int32 = 10
	// poolMinConns keeps two warm connections (no cold-connect latency on
	// first queries after idle).
	poolMinConns int32 = 2
	// poolMaxConnLifetime recycles connections predictably.
	poolMaxConnLifetime = 30 * time.Minute
)

// newPool parses databaseURL exactly like pgxpool.New (pgxpool.ParseConfig
// natively honors the pool_max_conns / pool_min_conns /
// pool_max_conn_lifetime conn-string params), fills in the W46 satellite
// defaults for every pool parameter the URL leaves unset, and connects.
func newPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	// The URL wins wherever it sets a pool param explicitly (substring
	// covers both the postgres://?pool_max_conns= query form and the
	// keyword DSN form).
	if !strings.Contains(databaseURL, "pool_max_conns") {
		cfg.MaxConns = poolMaxConns
	}
	if !strings.Contains(databaseURL, "pool_min_conns") {
		cfg.MinConns = poolMinConns
	}
	if !strings.Contains(databaseURL, "pool_max_conn_lifetime") {
		cfg.MaxConnLifetime = poolMaxConnLifetime
	}
	if cfg.MinConns > cfg.MaxConns { // a URL-set max below the default min
		cfg.MinConns = cfg.MaxConns
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}
