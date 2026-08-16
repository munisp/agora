package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Leads, promo codes, campaigns + spend (SPEC-W13 Agent A, CAC app):
// leads / promo_codes / promo_redemptions / campaigns / campaign_spend,
// all tenant-scoped with RLS (same bootstrap + pg_policies pattern as
// incidents.go).
// ---------------------------------------------------------------------------

// Lead mirrors booking.leads (contract SPEC-W13 §1). Attribution fields
// (campaign_id / promo_code / utm / channel_of_first_touch) are first-touch:
// they are written once at insert and never overwritten.
type Lead struct {
	ID                  uuid.UUID      `json:"lead_id"`
	TenantID            uuid.UUID      `json:"tenant_id"`
	PhoneE164           string         `json:"phone_e164"`
	ChannelOfFirstTouch string         `json:"channel_of_first_touch"`
	CampaignID          *uuid.UUID     `json:"campaign_id"`
	PromoCode           *string        `json:"promo_code"`
	UTM                 map[string]any `json:"utm,omitempty"`
	LgaID               *int           `json:"lga_id"`
	Status              string         `json:"status"`
	ConsentID           *uuid.UUID     `json:"consent_id"`
	DedupeKey           string         `json:"dedupe_key"`
	CreatedAt           time.Time      `json:"created_at"`
	UpdatedAt           time.Time      `json:"updated_at"`
}

// PromoCode mirrors booking.promo_codes (contract SPEC-W13 §6).
// MaxRedemptions 0 = unlimited.
type PromoCode struct {
	TenantID       uuid.UUID  `json:"tenant_id"`
	Code           string     `json:"code"`
	CampaignID     *uuid.UUID `json:"campaign_id"`
	DiscountNGN    *float64   `json:"discount_ngn"`
	MaxRedemptions int        `json:"max_redemptions"`
	RedeemedCount  int        `json:"redeemed_count"`
	CreatedAt      time.Time  `json:"created_at"`
}

// PromoRedemption mirrors booking.promo_redemptions: one row per
// (tenant, code, phone) — the idempotency anchor of POST /v1/promo/redeem.
type PromoRedemption struct {
	TenantID   uuid.UUID `json:"tenant_id"`
	Code       string    `json:"code"`
	PhoneE164  string    `json:"phone_e164"`
	LeadID     uuid.UUID `json:"lead_id"`
	RedeemedAt time.Time `json:"redeemed_at"`
}

// Campaign mirrors booking.campaigns: the minimal general marketing-campaign
// entity the CAC spend endpoint attaches to (geo_campaigns stays the Wave-8
// PostGIS-coupled sibling; leads.campaign_id may reference either — no FK by
// design).
type Campaign struct {
	ID        uuid.UUID  `json:"id"`
	TenantID  uuid.UUID  `json:"tenant_id"`
	Name      string     `json:"name"`
	Channel   string     `json:"channel"`
	StartsAt  *time.Time `json:"start_ts"`
	EndsAt    *time.Time `json:"end_ts"`
	CreatedAt time.Time  `json:"created_at"`
}

// CampaignView is a Campaign plus its lifetime spend sum (GET /v1/campaigns).
type CampaignView struct {
	Campaign
	SpendNGN float64 `json:"spend_ngn"`
}

// CampaignSpend mirrors booking.campaign_spend: one row per
// (tenant, campaign, day, channel). Writes are SET semantics (reposting the
// same day/channel replaces the amount) so a retried POST never
// double-counts.
type CampaignSpend struct {
	TenantID   uuid.UUID `json:"tenant_id"`
	CampaignID uuid.UUID `json:"campaign_id"`
	Day        time.Time `json:"day"` // UTC date (time-of-day truncated)
	Channel    string    `json:"channel"`
	AmountNGN  float64   `json:"amount_ngn"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ChannelSpend is one (channel, sum) pair of the spend-sum endpoint.
type ChannelSpend struct {
	Channel  string  `json:"channel"`
	SpendNGN float64 `json:"spend_ngn"`
}

// ensureLeadTables bootstraps the SPEC-W13 tables idempotently (same pattern
// as ensureIncidentTables). RLS: enabled + forced with the tenant_isolation
// policy, guarded by a pg_policies existence check.
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) ensureLeadTables(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS leads (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id              UUID NOT NULL,
    phone_e164             TEXT NOT NULL,
    channel_of_first_touch TEXT NOT NULL
                           CHECK (channel_of_first_touch IN
                                 ('voice','whatsapp','telegram','web','sms','webhook','ussd','qr','promo','field')),
    campaign_id            UUID,
    promo_code             TEXT,
    utm                    JSONB,
    lga_id                 INTEGER,
    status                 TEXT NOT NULL DEFAULT 'new'
                           CHECK (status IN ('new','contacted','qualified','converted','lost')),
    consent_id             UUID,
    dedupe_key             TEXT NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, dedupe_key)
);
CREATE INDEX IF NOT EXISTS idx_leads_tenant_status ON leads (tenant_id, status, created_at);
CREATE INDEX IF NOT EXISTS idx_leads_tenant_campaign ON leads (tenant_id, campaign_id);
CREATE INDEX IF NOT EXISTS idx_leads_tenant_channel ON leads (tenant_id, channel_of_first_touch, created_at);
ALTER TABLE leads ENABLE ROW LEVEL SECURITY;
ALTER TABLE leads FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'leads' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON leads
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS promo_codes (
    tenant_id       UUID NOT NULL,
    code            TEXT NOT NULL,
    campaign_id     UUID,
    discount_ngn    NUMERIC(14,2),
    max_redemptions INTEGER NOT NULL DEFAULT 0,
    redeemed_count  INTEGER NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, code)
);
ALTER TABLE promo_codes ENABLE ROW LEVEL SECURITY;
ALTER TABLE promo_codes FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'promo_codes' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON promo_codes
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS promo_redemptions (
    tenant_id   UUID NOT NULL,
    code        TEXT NOT NULL,
    phone_e164  TEXT NOT NULL,
    lead_id     UUID NOT NULL,
    redeemed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, code, phone_e164)
);
ALTER TABLE promo_redemptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE promo_redemptions FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'promo_redemptions' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON promo_redemptions
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS campaigns (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID NOT NULL,
    name       TEXT NOT NULL,
    channel    TEXT NOT NULL DEFAULT '',
    starts_at  TIMESTAMPTZ,
    ends_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_campaigns_tenant ON campaigns (tenant_id, created_at);
ALTER TABLE campaigns ENABLE ROW LEVEL SECURITY;
ALTER TABLE campaigns FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'campaigns' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON campaigns
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS campaign_spend (
    tenant_id   UUID NOT NULL,
    campaign_id UUID NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    day         DATE NOT NULL,
    channel     TEXT NOT NULL DEFAULT '',
    amount_ngn  NUMERIC(14,2) NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, campaign_id, day, channel)
);
CREATE INDEX IF NOT EXISTS idx_campaign_spend_campaign ON campaign_spend (tenant_id, campaign_id, day);
ALTER TABLE campaign_spend ENABLE ROW LEVEL SECURITY;
ALTER TABLE campaign_spend FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'campaign_spend' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON campaign_spend
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure lead tables: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Leads
// ---------------------------------------------------------------------------

const leadCols = `id, tenant_id, phone_e164, channel_of_first_touch, campaign_id, promo_code, utm, lga_id, status, consent_id, dedupe_key, created_at, updated_at`

func scanLead(row pgx.Row) (Lead, error) {
	var l Lead
	err := row.Scan(&l.ID, &l.TenantID, &l.PhoneE164, &l.ChannelOfFirstTouch,
		&l.CampaignID, &l.PromoCode, &l.UTM, &l.LgaID, &l.Status, &l.ConsentID,
		&l.DedupeKey, &l.CreatedAt, &l.UpdatedAt)
	return l, err
}

// InsertLead persists one lead. Idempotent on (tenant_id, dedupe_key)
// (contract §1: dedupe_key = sha256(tenant|lower(phone)|channel|YYYY-MM-DD),
// 24h dedup): a duplicate insert is a no-op and the EXISTING row is loaded
// into in (created=false). First-touch attribution is thus never overwritten.
func (s *Store) InsertLead(ctx context.Context, in *Lead) (created bool, err error) {
	if in.ID == uuid.Nil {
		in.ID = uuid.New()
	}
	const q = `INSERT INTO leads (` + leadCols + `)
		           VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'new',$9,$10,now(),now())
		           ON CONFLICT (tenant_id, dedupe_key) DO NOTHING
		           RETURNING created_at, updated_at`
	err = s.withTenant(ctx, in.TenantID, func(tx pgx.Tx) error {
		scanErr := tx.QueryRow(ctx, q, in.ID, in.TenantID, in.PhoneE164, in.ChannelOfFirstTouch,
			in.CampaignID, in.PromoCode, in.UTM, in.LgaID, in.ConsentID, in.DedupeKey).
			Scan(&in.CreatedAt, &in.UpdatedAt)
		if errors.Is(scanErr, pgx.ErrNoRows) {
			// Dedupe hit: return the existing first-touch row unchanged.
			existing, getErr := scanLead(tx.QueryRow(ctx,
				`SELECT `+leadCols+` FROM leads WHERE tenant_id=$1 AND dedupe_key=$2`,
				in.TenantID, in.DedupeKey))
			if getErr != nil {
				return fmt.Errorf("load existing lead: %w", getErr)
			}
			*in = existing
			created = false
			return nil
		}
		if scanErr != nil {
			return fmt.Errorf("insert lead: %w", scanErr)
		}
		in.Status = "new"
		created = true
		return nil
	})
	return created, err
}

// GetLead fetches one lead scoped to a tenant.
func (s *Store) GetLead(ctx context.Context, tenantID, id uuid.UUID) (Lead, error) {
	const q = `SELECT ` + leadCols + ` FROM leads WHERE tenant_id=$1 AND id=$2`
	var l Lead
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		l, err = scanLead(tx.QueryRow(ctx, q, tenantID, id))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return l, ErrNotFound
	}
	return l, err
}

// ListLeads returns leads of a tenant, newest first, with optional
// status / channel / campaign_id / created_at [from,to] filters (zero
// values disable a filter).
func (s *Store) ListLeads(ctx context.Context, tenantID uuid.UUID, status, channel string, campaignID *uuid.UUID, from, to *time.Time) ([]Lead, error) {
	q := `SELECT ` + leadCols + ` FROM leads WHERE tenant_id=$1`
	args := []any{tenantID}
	n := 1
	if status != "" {
		n++
		q += fmt.Sprintf(` AND status=$%d`, n)
		args = append(args, status)
	}
	if channel != "" {
		n++
		q += fmt.Sprintf(` AND channel_of_first_touch=$%d`, n)
		args = append(args, channel)
	}
	if campaignID != nil {
		n++
		q += fmt.Sprintf(` AND campaign_id=$%d`, n)
		args = append(args, *campaignID)
	}
	if from != nil {
		n++
		q += fmt.Sprintf(` AND created_at >= $%d`, n)
		args = append(args, *from)
	}
	if to != nil {
		n++
		q += fmt.Sprintf(` AND created_at <= $%d`, n)
		args = append(args, *to)
	}
	q += ` ORDER BY created_at DESC LIMIT 500`
	var out []Lead
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			l, err := scanLead(rows)
			if err != nil {
				return err
			}
			out = append(out, l)
		}
		return rows.Err()
	})
	return out, err
}

// UpdateLeadStatus sets a lead's status (and updated_at). Transition
// legality is enforced by the leads service; the store only guarantees
// tenant scoping. Attribution fields are NOT touched (first-touch wins).
// Returns the updated row.
func (s *Store) UpdateLeadStatus(ctx context.Context, tenantID, id uuid.UUID, status string) (Lead, error) {
	const q = `UPDATE leads SET status=$3, updated_at=now()
		           WHERE tenant_id=$1 AND id=$2
		           RETURNING ` + leadCols
	var l Lead
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		l, err = scanLead(tx.QueryRow(ctx, q, tenantID, id, status))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return l, ErrNotFound
	}
	return l, err
}

// ---------------------------------------------------------------------------
// Promo codes + redemptions
// ---------------------------------------------------------------------------

const promoCols = `tenant_id, code, campaign_id, discount_ngn, max_redemptions, redeemed_count, created_at`

func scanPromoCode(row pgx.Row) (PromoCode, error) {
	var p PromoCode
	err := row.Scan(&p.TenantID, &p.Code, &p.CampaignID, &p.DiscountNGN,
		&p.MaxRedemptions, &p.RedeemedCount, &p.CreatedAt)
	return p, err
}

// UpsertPromoCode creates or replaces a promo code (PK tenant_id+code;
// redeemed_count is preserved on replace).
func (s *Store) UpsertPromoCode(ctx context.Context, p *PromoCode) error {
	const q = `INSERT INTO promo_codes (` + promoCols + `)
		           VALUES ($1,$2,$3,$4,$5,0,now())
		           ON CONFLICT (tenant_id, code) DO UPDATE
		             SET campaign_id=EXCLUDED.campaign_id,
		                 discount_ngn=EXCLUDED.discount_ngn,
		                 max_redemptions=EXCLUDED.max_redemptions
		           RETURNING redeemed_count, created_at`
	return s.withTenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, p.TenantID, p.Code, p.CampaignID, p.DiscountNGN,
			p.MaxRedemptions).Scan(&p.RedeemedCount, &p.CreatedAt)
	})
}

// GetPromoCode fetches one promo code scoped to a tenant.
func (s *Store) GetPromoCode(ctx context.Context, tenantID uuid.UUID, code string) (PromoCode, error) {
	const q = `SELECT ` + promoCols + ` FROM promo_codes WHERE tenant_id=$1 AND code=$2`
	var p PromoCode
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		p, err = scanPromoCode(tx.QueryRow(ctx, q, tenantID, code))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// LookupPromoCode resolves a promo code to its owning tenant + row for the
// PUBLIC redeem endpoint (POST /v1/promo/redeem carries no tenant context).
//
// NOTE (RLS): public code resolution path — like public site-slug
// resolution — it intentionally runs outside withTenant and returns only
// the row needed to enter the correct tenant scope. Codes are expected to
// be unguessable; on ambiguous duplicates the newest wins.
func (s *Store) LookupPromoCode(ctx context.Context, code string) (PromoCode, error) {
	const q = `SELECT ` + promoCols + ` FROM promo_codes WHERE code=$1 ORDER BY created_at DESC LIMIT 1`
	p, err := scanPromoCode(s.pool.QueryRow(ctx, q, code))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// ListPromoCodes returns all promo codes of a tenant (newest first).
func (s *Store) ListPromoCodes(ctx context.Context, tenantID uuid.UUID) ([]PromoCode, error) {
	const q = `SELECT ` + promoCols + ` FROM promo_codes WHERE tenant_id=$1 ORDER BY created_at DESC LIMIT 500`
	var out []PromoCode
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanPromoCode(rows)
			if err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// RedeemPromoTx atomically redeems a promo code for a phone number
// (contract §6: idempotent per code+phone):
//  1. lock the promo code row (FOR UPDATE) and enforce max_redemptions;
//  2. insert the (tenant, code, phone) redemption anchor — a replay short-
//     circuits here and returns the original outcome (alreadyRedeemed=true);
//  3. insert the lead built by the caller with promo attribution (dedupe
//     conflict → the existing first-touch lead is returned untouched);
//  4. bump redeemed_count.
//
// leadCreated reports whether the funnel `lead_created` event must be
// emitted (false on dedupe hit AND on redemption replay).
func (s *Store) RedeemPromoTx(ctx context.Context, tenantID uuid.UUID, code, phone string, lead *Lead) (out Lead, leadCreated, alreadyRedeemed bool, err error) {
	err = s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		promo, err := scanPromoCode(tx.QueryRow(ctx,
			`SELECT `+promoCols+` FROM promo_codes WHERE tenant_id=$1 AND code=$2 FOR UPDATE`,
			tenantID, code))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if lead.ID == uuid.Nil {
			lead.ID = uuid.New()
		}
		var redemptionLeadID uuid.UUID
		scanErr := tx.QueryRow(ctx,
			`INSERT INTO promo_redemptions (tenant_id, code, phone_e164, lead_id, redeemed_at)
			 VALUES ($1,$2,$3,$4,now())
			 ON CONFLICT (tenant_id, code, phone_e164) DO NOTHING
			 RETURNING lead_id`,
			tenantID, code, phone, lead.ID).Scan(&redemptionLeadID)
		if errors.Is(scanErr, pgx.ErrNoRows) {
			// Replay: same code+phone — return the original lead, no count bump.
			alreadyRedeemed = true
			out, err = scanLead(tx.QueryRow(ctx,
				`SELECT `+leadCols+` FROM leads WHERE tenant_id=$1 AND id = (
				   SELECT lead_id FROM promo_redemptions
				   WHERE tenant_id=$1 AND code=$2 AND phone_e164=$3)`,
				tenantID, code, phone))
			if err != nil {
				return fmt.Errorf("load original redemption lead: %w", err)
			}
			return nil
		}
		if scanErr != nil {
			return fmt.Errorf("insert promo redemption: %w", scanErr)
		}
		if promo.MaxRedemptions > 0 && promo.RedeemedCount >= promo.MaxRedemptions {
			return ErrPromoExhausted
		}
		// First-touch lead insert (same ON CONFLICT semantics as InsertLead).
		scanErr = tx.QueryRow(ctx,
			`INSERT INTO leads (`+leadCols+`)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'new',$9,$10,now(),now())
			 ON CONFLICT (tenant_id, dedupe_key) DO NOTHING
			 RETURNING id`,
			lead.ID, tenantID, lead.PhoneE164, lead.ChannelOfFirstTouch,
			lead.CampaignID, lead.PromoCode, lead.UTM, lead.LgaID,
			lead.ConsentID, lead.DedupeKey).Scan(&out.ID)
		if errors.Is(scanErr, pgx.ErrNoRows) {
			// Same-day dedupe hit: keep the existing lead; redemption stands.
			out, err = scanLead(tx.QueryRow(ctx,
				`SELECT `+leadCols+` FROM leads WHERE tenant_id=$1 AND dedupe_key=$2`,
				tenantID, lead.DedupeKey))
			if err != nil {
				return fmt.Errorf("load deduped lead: %w", err)
			}
			leadCreated = false
		} else if scanErr != nil {
			return fmt.Errorf("insert promo lead: %w", scanErr)
		} else {
			leadCreated = true
		}
		// Point the redemption at the lead that actually won (new or existing).
		if _, err := tx.Exec(ctx,
			`UPDATE promo_redemptions SET lead_id=$4 WHERE tenant_id=$1 AND code=$2 AND phone_e164=$3`,
			tenantID, code, phone, out.ID); err != nil {
			return fmt.Errorf("link redemption lead: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE promo_codes SET redeemed_count = redeemed_count + 1
			 WHERE tenant_id=$1 AND code=$2`, tenantID, code); err != nil {
			return fmt.Errorf("bump redeemed_count: %w", err)
		}
		out, err = scanLead(tx.QueryRow(ctx,
			`SELECT `+leadCols+` FROM leads WHERE tenant_id=$1 AND id=$2`, tenantID, out.ID))
		return err
	})
	return out, leadCreated, alreadyRedeemed, err
}

// ErrPromoExhausted marks a promo code whose max_redemptions is reached.
var ErrPromoExhausted = errors.New("promo code exhausted")

// ---------------------------------------------------------------------------
// Campaigns + spend
// ---------------------------------------------------------------------------

// CreateCampaign inserts a campaign.
func (s *Store) CreateCampaign(ctx context.Context, c *Campaign) error {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	const q = `INSERT INTO campaigns (id, tenant_id, name, channel, starts_at, ends_at, created_at)
		           VALUES ($1,$2,$3,$4,$5,$6,now()) RETURNING created_at`
	return s.withTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, c.ID, c.TenantID, c.Name, c.Channel, c.StartsAt, c.EndsAt).
			Scan(&c.CreatedAt)
	})
}

// GetCampaign fetches one campaign scoped to a tenant.
func (s *Store) GetCampaign(ctx context.Context, tenantID, id uuid.UUID) (Campaign, error) {
	const q = `SELECT id, tenant_id, name, channel, starts_at, ends_at, created_at
		           FROM campaigns WHERE tenant_id=$1 AND id=$2`
	var c Campaign
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, tenantID, id).Scan(&c.ID, &c.TenantID, &c.Name,
			&c.Channel, &c.StartsAt, &c.EndsAt, &c.CreatedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// ListCampaignsWithSpend returns all campaigns of a tenant with their
// lifetime spend sums (GET /v1/campaigns — analytics dashboard read).
func (s *Store) ListCampaignsWithSpend(ctx context.Context, tenantID uuid.UUID) ([]CampaignView, error) {
	const q = `SELECT c.id, c.tenant_id, c.name, c.channel, c.starts_at, c.ends_at, c.created_at,
		                  COALESCE(SUM(s.amount_ngn), 0)::float8 AS spend_ngn
		           FROM campaigns c
		           LEFT JOIN campaign_spend s ON s.tenant_id = c.tenant_id AND s.campaign_id = c.id
		           WHERE c.tenant_id = $1
		           GROUP BY c.id
		           ORDER BY c.created_at DESC LIMIT 500`
	var out []CampaignView
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v CampaignView
			if err := rows.Scan(&v.ID, &v.TenantID, &v.Name, &v.Channel, &v.StartsAt,
				&v.EndsAt, &v.CreatedAt, &v.SpendNGN); err != nil {
				return err
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	return out, err
}

// UpsertCampaignSpend records spend for one (campaign, day, channel) with
// SET semantics: reposting the same key REPLACES the amount, so a retried
// POST /v1/campaigns/{id}/spend never double-counts. The campaign must
// exist in the tenant (FK + explicit check → ErrNotFound).
func (s *Store) UpsertCampaignSpend(ctx context.Context, sp *CampaignSpend) error {
	return s.withTenant(ctx, sp.TenantID, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM campaigns WHERE tenant_id=$1 AND id=$2)`,
			sp.TenantID, sp.CampaignID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		const q = `INSERT INTO campaign_spend (tenant_id, campaign_id, day, channel, amount_ngn, created_at, updated_at)
			           VALUES ($1,$2,$3::date,$4,$5,now(),now())
			           ON CONFLICT (tenant_id, campaign_id, day, channel) DO UPDATE
			             SET amount_ngn=EXCLUDED.amount_ngn, updated_at=now()
			           RETURNING day, created_at, updated_at`
		return tx.QueryRow(ctx, q, sp.TenantID, sp.CampaignID, sp.Day, sp.Channel, sp.AmountNGN).
			Scan(&sp.Day, &sp.CreatedAt, &sp.UpdatedAt)
	})
}

// CampaignSpendSum sums spend of one campaign in [from,to] (day bounds,
// inclusive; nil = unbounded), broken down by channel. Backs the internal
// GET /internal/campaigns/{id}/spend-sum endpoint consumed by
// analytics-service (SPEC-W13 §4/§5).
func (s *Store) CampaignSpendSum(ctx context.Context, tenantID, campaignID uuid.UUID, from, to *time.Time) (total float64, byChannel []ChannelSpend, err error) {
	q := `SELECT channel, COALESCE(SUM(amount_ngn),0)::float8
		           FROM campaign_spend WHERE tenant_id=$1 AND campaign_id=$2`
	args := []any{tenantID, campaignID}
	n := 2
	if from != nil {
		n++
		q += fmt.Sprintf(` AND day >= $%d::date`, n)
		args = append(args, *from)
	}
	if to != nil {
		n++
		q += fmt.Sprintf(` AND day <= $%d::date`, n)
		args = append(args, *to)
	}
	q += ` GROUP BY channel ORDER BY channel`
	byChannel = []ChannelSpend{}
	err = s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var cs ChannelSpend
			if err := rows.Scan(&cs.Channel, &cs.SpendNGN); err != nil {
				return err
			}
			total += cs.SpendNGN
			byChannel = append(byChannel, cs)
		}
		return rows.Err()
	})
	return total, byChannel, err
}
