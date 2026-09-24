package store

// SPEC-W46 PERF-08 unit tests: pool defaults + DATABASE_URL pool_* param
// precedence. pgxpool.NewWithConfig connects lazily, so no live Postgres is
// needed to assert the parsed config.

import (
	"context"
	"testing"
	"time"
)

func TestNewPoolDefaults(t *testing.T) {
	p, err := newPool(context.Background(),
		"postgres://u:p@localhost:1/db?sslmode=disable", poolMaxConns, poolMinConns)
	if err != nil {
		t.Fatalf("newPool: %v", err)
	}
	defer p.Close()
	cfg := p.Config()
	if cfg.MaxConns != poolMaxConns {
		t.Fatalf("MaxConns default = %d, want %d", cfg.MaxConns, poolMaxConns)
	}
	if cfg.MinConns != poolMinConns {
		t.Fatalf("MinConns default = %d, want %d", cfg.MinConns, poolMinConns)
	}
	if cfg.MaxConnLifetime != poolMaxConnLifetime {
		t.Fatalf("MaxConnLifetime default = %v, want %v", cfg.MaxConnLifetime, poolMaxConnLifetime)
	}
}

func TestNewPoolURLParamsWin(t *testing.T) {
	p, err := newPool(context.Background(),
		"postgres://u:p@localhost:1/db?sslmode=disable&pool_max_conns=25&pool_min_conns=3&pool_max_conn_lifetime=5m",
		poolMaxConns, poolMinConns)
	if err != nil {
		t.Fatalf("newPool: %v", err)
	}
	defer p.Close()
	cfg := p.Config()
	if cfg.MaxConns != 25 || cfg.MinConns != 3 || cfg.MaxConnLifetime != 5*time.Minute {
		t.Fatalf("URL pool params must win: max=%d min=%d lifetime=%v",
			cfg.MaxConns, cfg.MinConns, cfg.MaxConnLifetime)
	}
}

func TestNewPoolMinClampedBelowURLMax(t *testing.T) {
	p, err := newPool(context.Background(),
		"postgres://u:p@localhost:1/db?sslmode=disable&pool_max_conns=1",
		poolMaxConns, poolMinConns)
	if err != nil {
		t.Fatalf("newPool: %v", err)
	}
	defer p.Close()
	if cfg := p.Config(); cfg.MinConns > cfg.MaxConns {
		t.Fatalf("MinConns %d must be clamped to URL-set MaxConns %d", cfg.MinConns, cfg.MaxConns)
	}
}
