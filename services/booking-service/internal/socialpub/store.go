package socialpub

// Store: social_accounts / social_creatives / social_posts / social_ads,
// all FORCE-RLS tenant_isolation (the devices/store.go idiom: idempotent
// ensureSchema, pg_policies-guarded policy, SET LOCAL app.tenant_id
// inside withTenant). Packaging mirrors internal/helpdesk: NewStore wraps
// an existing pool (tests), DialStore opens a small dedicated pool (main
// wiring path). maxConns 4: social publishing is a low-QPS operator path.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a row does not exist (mirrors
// store.ErrNotFound so httpapi can map both to 404).
var ErrNotFound = errors.New("not found")

// Store persists the social-publisher tables.
type Store struct {
	pool    *pgxpool.Pool
	ownPool bool // true when opened via DialStore
}

// NewStore wraps an existing pool and ensures the schema.
func NewStore(ctx context.Context, pool *pgxpool.Pool) (*Store, error) {
	s := &Store{pool: pool}
	if err := s.ensureSchema(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// DialStore opens a small dedicated pool and ensures the schema.
func DialStore(ctx context.Context, databaseURL string) (*Store, error) {
	poolCfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse postgres config: %w", err)
	}
	poolCfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	s, err := NewStore(ctx, pool)
	if err != nil {
		pool.Close()
		return nil, err
	}
	s.ownPool = true
	return s, nil
}

// Close releases the pool when this store opened it.
func (s *Store) Close() {
	if s.ownPool {
		s.pool.Close()
	}
}

// ensureSchema bootstraps the four social-publisher tables idempotently:
// RLS enabled + forced with the tenant_isolation policy, guarded by a
// pg_policies existence check (the devices/store.go pattern).
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) ensureSchema(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS social_accounts (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID NOT NULL,
    provider                  TEXT NOT NULL
                              CHECK (provider IN ('meta','tiktok','x')),
    account_ref               TEXT NOT NULL,
    display_name              TEXT NOT NULL,
    status                    TEXT NOT NULL DEFAULT 'connected'
                              CHECK (status IN ('connected','expired','revoked')),
    political_ads_authorized  BOOLEAN NOT NULL DEFAULT FALSE,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_social_accounts_tenant ON social_accounts (tenant_id, provider, status);
ALTER TABLE social_accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE social_accounts FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'social_accounts' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON social_accounts
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS social_creatives (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL,
    name            TEXT NOT NULL,
    kind            TEXT NOT NULL
                    CHECK (kind IN ('text','image','video')),
    body            TEXT NOT NULL,
    media_url       TEXT,
    disclaimer_text TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_social_creatives_tenant ON social_creatives (tenant_id, kind);
ALTER TABLE social_creatives ENABLE ROW LEVEL SECURITY;
ALTER TABLE social_creatives FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'social_creatives' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON social_creatives
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS social_posts (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        UUID NOT NULL,
    account_id       UUID NOT NULL REFERENCES social_accounts (id),
    creative_id      UUID NOT NULL REFERENCES social_creatives (id),
    status           TEXT NOT NULL DEFAULT 'draft'
                     CHECK (status IN ('draft','queued','publishing','published','failed')),
    provider_post_id TEXT,
    error            TEXT,
    published_at     TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_social_posts_tenant ON social_posts (tenant_id, status, created_at DESC);
ALTER TABLE social_posts ENABLE ROW LEVEL SECURITY;
ALTER TABLE social_posts FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'social_posts' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON social_posts
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS social_ads (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NOT NULL,
    account_id         UUID NOT NULL REFERENCES social_accounts (id),
    creative_id        UUID NOT NULL REFERENCES social_creatives (id),
    name               TEXT NOT NULL,
    objective          TEXT NOT NULL
                       CHECK (objective IN ('awareness','traffic','engagement')),
    budget_kobo        BIGINT NOT NULL CHECK (budget_kobo > 0),
    daily_budget_kobo  BIGINT NOT NULL CHECK (daily_budget_kobo > 0),
    targeting          JSONB NOT NULL DEFAULT '{}',
    political          BOOLEAN NOT NULL DEFAULT FALSE,
    disclaimer_text    TEXT,
    status             TEXT NOT NULL DEFAULT 'draft'
                       CHECK (status IN ('draft','review','active','paused','rejected')),
    provider_ad_id     TEXT,
    error              TEXT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_social_ads_tenant ON social_ads (tenant_id, status, created_at DESC);
ALTER TABLE social_ads ENABLE ROW LEVEL SECURITY;
ALTER TABLE social_ads FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'social_ads' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON social_ads
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure social-publisher tables: %w", err)
	}
	return nil
}

// withTenant runs fn inside a transaction with `SET LOCAL app.tenant_id`
// applied (mirrors store.Store.withTenant — same parameter-binding-safe
// set_config call) so the RLS tenant_isolation policy scopes every
// statement of fn to the given tenant.
func (s *Store) withTenant(ctx context.Context, tenantID uuid.UUID, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID.String()); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// EnqueueOutbox appends one row to the shared transactional outbox
// (drained to Kafka by the W5 outbox dispatcher via Dapr; mirrors
// helpdesk.Store.EnqueueOutbox).
//
// NOTE (RLS): the outbox table is not tenant-scoped (no RLS policy — the
// dispatcher drains it cross-tenant by design).
func (s *Store) EnqueueOutbox(ctx context.Context, aggregateID uuid.UUID, topic string, payload []byte) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO outbox (aggregate_id, topic, payload) VALUES ($1,$2,$3)`,
		aggregateID, topic, payload); err != nil {
		return fmt.Errorf("enqueue outbox: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Accounts
// ---------------------------------------------------------------------------

const accountCols = `id, tenant_id, provider, account_ref, display_name, status, political_ads_authorized, created_at, updated_at`

func scanAccount(row pgx.Row) (Account, error) {
	var a Account
	err := row.Scan(&a.ID, &a.TenantID, &a.Provider, &a.AccountRef, &a.DisplayName,
		&a.Status, &a.PoliticalAuth, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

// CreateAccount inserts one account (Validate first), stamping id and
// timestamps.
func (s *Store) CreateAccount(ctx context.Context, a *Account) error {
	if err := a.Validate(); err != nil {
		return err
	}
	return s.withTenant(ctx, a.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO social_accounts (tenant_id, provider, account_ref, display_name, status, political_ads_authorized)
			 VALUES ($1,$2,$3,$4,$5,$6)
			 RETURNING `+accountCols,
			a.TenantID, a.Provider, a.AccountRef, a.DisplayName, a.Status, a.PoliticalAuth).
			Scan(&a.ID, &a.TenantID, &a.Provider, &a.AccountRef, &a.DisplayName,
				&a.Status, &a.PoliticalAuth, &a.CreatedAt, &a.UpdatedAt)
	})
}

// GetAccount fetches one account by id (tenant-scoped).
func (s *Store) GetAccount(ctx context.Context, tenantID, id uuid.UUID) (Account, error) {
	var a Account
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		a, err = scanAccount(tx.QueryRow(ctx,
			`SELECT `+accountCols+` FROM social_accounts WHERE id=$1`, id))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	return a, err
}

// ListAccounts lists accounts, optionally filtered by provider/status.
func (s *Store) ListAccounts(ctx context.Context, tenantID uuid.UUID, provider, status string) ([]Account, error) {
	out := []Account{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+accountCols+` FROM social_accounts
			 WHERE ($1='' OR provider=$1) AND ($2='' OR status=$2)
			 ORDER BY created_at DESC`, provider, status)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			a, err := scanAccount(rows)
			if err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

// UpdateAccount replaces the mutable fields of one account (status,
// display_name, account_ref, political_ads_authorized).
func (s *Store) UpdateAccount(ctx context.Context, a *Account) error {
	if err := a.Validate(); err != nil {
		return err
	}
	return s.withTenant(ctx, a.TenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`UPDATE social_accounts
			 SET account_ref=$2, display_name=$3, status=$4, political_ads_authorized=$5, updated_at=now()
			 WHERE id=$1
			 RETURNING `+accountCols,
			a.ID, a.AccountRef, a.DisplayName, a.Status, a.PoliticalAuth).
			Scan(&a.ID, &a.TenantID, &a.Provider, &a.AccountRef, &a.DisplayName,
				&a.Status, &a.PoliticalAuth, &a.CreatedAt, &a.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
}

// ---------------------------------------------------------------------------
// Creatives
// ---------------------------------------------------------------------------

const creativeCols = `id, tenant_id, name, kind, body, media_url, disclaimer_text, created_at, updated_at`

func scanCreative(row pgx.Row) (Creative, error) {
	var c Creative
	err := row.Scan(&c.ID, &c.TenantID, &c.Name, &c.Kind, &c.Body,
		&c.MediaURL, &c.DisclaimerText, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

// CreateCreative inserts one creative (Validate first).
func (s *Store) CreateCreative(ctx context.Context, c *Creative) error {
	if err := c.Validate(); err != nil {
		return err
	}
	return s.withTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO social_creatives (tenant_id, name, kind, body, media_url, disclaimer_text)
			 VALUES ($1,$2,$3,$4,$5,$6)
			 RETURNING `+creativeCols,
			c.TenantID, c.Name, c.Kind, c.Body, c.MediaURL, c.DisclaimerText).
			Scan(&c.ID, &c.TenantID, &c.Name, &c.Kind, &c.Body,
				&c.MediaURL, &c.DisclaimerText, &c.CreatedAt, &c.UpdatedAt)
	})
}

// GetCreative fetches one creative by id (tenant-scoped).
func (s *Store) GetCreative(ctx context.Context, tenantID, id uuid.UUID) (Creative, error) {
	var c Creative
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		c, err = scanCreative(tx.QueryRow(ctx,
			`SELECT `+creativeCols+` FROM social_creatives WHERE id=$1`, id))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Creative{}, ErrNotFound
	}
	return c, err
}

// ListCreatives lists creatives, optionally filtered by kind.
func (s *Store) ListCreatives(ctx context.Context, tenantID uuid.UUID, kind string) ([]Creative, error) {
	out := []Creative{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+creativeCols+` FROM social_creatives
			 WHERE ($1='' OR kind=$1)
			 ORDER BY created_at DESC`, kind)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanCreative(rows)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// UpdateCreative replaces the mutable fields of one creative.
func (s *Store) UpdateCreative(ctx context.Context, c *Creative) error {
	if err := c.Validate(); err != nil {
		return err
	}
	return s.withTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`UPDATE social_creatives
			 SET name=$2, kind=$3, body=$4, media_url=$5, disclaimer_text=$6, updated_at=now()
			 WHERE id=$1
			 RETURNING `+creativeCols,
			c.ID, c.Name, c.Kind, c.Body, c.MediaURL, c.DisclaimerText).
			Scan(&c.ID, &c.TenantID, &c.Name, &c.Kind, &c.Body,
				&c.MediaURL, &c.DisclaimerText, &c.CreatedAt, &c.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
}

// ---------------------------------------------------------------------------
// Posts
// ---------------------------------------------------------------------------

const postCols = `id, tenant_id, account_id, creative_id, status, provider_post_id, error, published_at, created_at`

func scanPost(row pgx.Row) (Post, error) {
	var p Post
	err := row.Scan(&p.ID, &p.TenantID, &p.AccountID, &p.CreativeID, &p.Status,
		&p.ProviderPostID, &p.Error, &p.PublishedAt, &p.CreatedAt)
	return p, err
}

// CreatePost inserts one post row (status draft|queued by the caller).
func (s *Store) CreatePost(ctx context.Context, p *Post) error {
	if p.TenantID == uuid.Nil || p.AccountID == uuid.Nil || p.CreativeID == uuid.Nil {
		return fmt.Errorf("%w: tenant_id, account_id and creative_id are required", ErrInvalidInput)
	}
	if p.Status != PostDraft && p.Status != PostQueued {
		return fmt.Errorf("%w: new post status must be draft|queued", ErrInvalidInput)
	}
	return s.withTenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO social_posts (tenant_id, account_id, creative_id, status)
			 VALUES ($1,$2,$3,$4)
			 RETURNING `+postCols,
			p.TenantID, p.AccountID, p.CreativeID, p.Status).
			Scan(&p.ID, &p.TenantID, &p.AccountID, &p.CreativeID, &p.Status,
				&p.ProviderPostID, &p.Error, &p.PublishedAt, &p.CreatedAt)
	})
}

// GetPost fetches one post by id (tenant-scoped).
func (s *Store) GetPost(ctx context.Context, tenantID, id uuid.UUID) (Post, error) {
	var p Post
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		p, err = scanPost(tx.QueryRow(ctx,
			`SELECT `+postCols+` FROM social_posts WHERE id=$1`, id))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Post{}, ErrNotFound
	}
	return p, err
}

// ListPosts lists posts, optionally filtered by status/account.
func (s *Store) ListPosts(ctx context.Context, tenantID uuid.UUID, status string, accountID uuid.UUID) ([]Post, error) {
	out := []Post{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+postCols+` FROM social_posts
			 WHERE ($1='' OR status=$1) AND ($2::uuid IS NULL OR account_id=$2)
			 ORDER BY created_at DESC`, status, nullableUUID(accountID))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanPost(rows)
			if err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// CompletePublish marks a post published|failed after the provider call:
// success stamps provider_post_id + published_at and clears error;
// failure records the (truncated) error.
func (s *Store) CompletePublish(ctx context.Context, tenantID, id uuid.UUID, providerPostID, publishErr string) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var tag pgconn.CommandTag
		var err error
		if publishErr == "" {
			tag, err = tx.Exec(ctx,
				`UPDATE social_posts
				 SET status='published', provider_post_id=$2, error=NULL, published_at=now()
				 WHERE id=$1`, id, providerPostID)
		} else {
			tag, err = tx.Exec(ctx,
				`UPDATE social_posts
				 SET status='failed', error=$2
				 WHERE id=$1`, id, truncate(publishErr, maxErrorLen))
		}
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// SetPostQueued flips a draft post to queued.
func (s *Store) SetPostQueued(ctx context.Context, tenantID, id uuid.UUID) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE social_posts SET status='queued' WHERE id=$1 AND status='draft'`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: post status draft → queued", ErrInvalidTransition)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Ads
// ---------------------------------------------------------------------------

const adCols = `id, tenant_id, account_id, creative_id, name, objective, budget_kobo, daily_budget_kobo, targeting, political, disclaimer_text, status, provider_ad_id, error, created_at, updated_at`

func scanAd(row pgx.Row) (Ad, error) {
	var a Ad
	var raw []byte
	err := row.Scan(&a.ID, &a.TenantID, &a.AccountID, &a.CreativeID, &a.Name,
		&a.Objective, &a.BudgetKobo, &a.DailyBudgetKobo, &raw, &a.Political,
		&a.DisclaimerText, &a.Status, &a.ProviderAdID, &a.Error, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return Ad{}, err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a.Targeting); err != nil {
			return Ad{}, fmt.Errorf("decode targeting: %w", err)
		}
	}
	return a, nil
}

// CreateAd inserts one ad (Validate first; status draft).
func (s *Store) CreateAd(ctx context.Context, a *Ad) error {
	a.Status = AdDraft
	if err := a.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(a.Targeting)
	if err != nil {
		return fmt.Errorf("%w: targeting marshal: %v", ErrInvalidInput, err)
	}
	return s.withTenant(ctx, a.TenantID, func(tx pgx.Tx) error {
		var retRaw []byte
		return tx.QueryRow(ctx,
			`INSERT INTO social_ads (tenant_id, account_id, creative_id, name, objective,
			     budget_kobo, daily_budget_kobo, targeting, political, disclaimer_text, status)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			 RETURNING `+adCols,
			a.TenantID, a.AccountID, a.CreativeID, a.Name, a.Objective,
			a.BudgetKobo, a.DailyBudgetKobo, raw, a.Political, a.DisclaimerText, a.Status).
			Scan(&a.ID, &a.TenantID, &a.AccountID, &a.CreativeID, &a.Name,
				&a.Objective, &a.BudgetKobo, &a.DailyBudgetKobo, &retRaw, &a.Political,
				&a.DisclaimerText, &a.Status, &a.ProviderAdID, &a.Error, &a.CreatedAt, &a.UpdatedAt)
	})
}

// GetAd fetches one ad by id (tenant-scoped).
func (s *Store) GetAd(ctx context.Context, tenantID, id uuid.UUID) (Ad, error) {
	var a Ad
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		a, err = scanAd(tx.QueryRow(ctx,
			`SELECT `+adCols+` FROM social_ads WHERE id=$1`, id))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Ad{}, ErrNotFound
	}
	return a, err
}

// ListAds lists ads, optionally filtered by status/account.
func (s *Store) ListAds(ctx context.Context, tenantID uuid.UUID, status string, accountID uuid.UUID) ([]Ad, error) {
	out := []Ad{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+adCols+` FROM social_ads
			 WHERE ($1='' OR status=$1) AND ($2::uuid IS NULL OR account_id=$2)
			 ORDER BY created_at DESC`, status, nullableUUID(accountID))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			a, err := scanAd(rows)
			if err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

// UpdateAd replaces the mutable fields of one ad (name, objective,
// budgets, targeting, political, disclaimer) — allowed while the ad is
// draft|review|rejected (an active|paused ad's budgets are provider-side
// already; edit via pause → follow-up).
func (s *Store) UpdateAd(ctx context.Context, a *Ad) error {
	if err := a.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(a.Targeting)
	if err != nil {
		return fmt.Errorf("%w: targeting marshal: %v", ErrInvalidInput, err)
	}
	return s.withTenant(ctx, a.TenantID, func(tx pgx.Tx) error {
		var retRaw []byte
		err := tx.QueryRow(ctx,
			`UPDATE social_ads
			 SET name=$2, objective=$3, budget_kobo=$4, daily_budget_kobo=$5,
			     targeting=$6, political=$7, disclaimer_text=$8, updated_at=now()
			 WHERE id=$1 AND status IN ('draft','review','rejected')
			 RETURNING `+adCols,
			a.ID, a.Name, a.Objective, a.BudgetKobo, a.DailyBudgetKobo,
			raw, a.Political, a.DisclaimerText).
			Scan(&a.ID, &a.TenantID, &a.AccountID, &a.CreativeID, &a.Name,
				&a.Objective, &a.BudgetKobo, &a.DailyBudgetKobo, &retRaw, &a.Political,
				&a.DisclaimerText, &a.Status, &a.ProviderAdID, &a.Error, &a.CreatedAt, &a.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
}

// SetAdStatus moves an ad through the operator state machine (PATCH), or
// records the launch outcome: launch success stamps provider_ad_id and
// lands in review; rejection lands in rejected with the reason in error.
func (s *Store) SetAdStatus(ctx context.Context, tenantID, id uuid.UUID, to, providerAdID, reason string) (Ad, error) {
	var a Ad
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := scanAd(tx.QueryRow(ctx,
			`SELECT `+adCols+` FROM social_ads WHERE id=$1 FOR UPDATE`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := ValidateAdTransition(cur.Status, to); err != nil {
			return err
		}
		var retRaw []byte
		err = tx.QueryRow(ctx,
			`UPDATE social_ads
			 SET status=$2,
			     provider_ad_id = COALESCE($3, provider_ad_id),
			     error = CASE WHEN $4='' THEN error ELSE $4 END,
			     updated_at=now()
			 WHERE id=$1
			 RETURNING `+adCols,
			id, to, nullableString(providerAdID), truncate(reason, maxErrorLen)).
			Scan(&a.ID, &a.TenantID, &a.AccountID, &a.CreativeID, &a.Name,
				&a.Objective, &a.BudgetKobo, &a.DailyBudgetKobo, &retRaw, &a.Political,
				&a.DisclaimerText, &a.Status, &a.ProviderAdID, &a.Error, &a.CreatedAt, &a.UpdatedAt)
		if err != nil {
			return err
		}
		if len(retRaw) > 0 {
			if err := json.Unmarshal(retRaw, &a.Targeting); err != nil {
				return fmt.Errorf("decode targeting: %w", err)
			}
		}
		return nil
	})
	return a, err
}

func nullableUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
}
