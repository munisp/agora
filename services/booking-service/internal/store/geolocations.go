package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Geospatial location model (SPEC-W8 Part A): PostGIS-backed
// contact_locations / service_areas / geo_campaigns plus the
// geo_campaign_sends ledger the GeoCampaignWorkflow uses for idempotent
// replay (skip contacts already sent for a campaign).
//
// RLS mirrors the sibling tables: enabled + forced with the
// tenant_isolation policy, and every query runs through withTenant. All
// geometry enters the database parameterized ($1…): GeoJSON text goes to
// ST_GeomFromGeoJSON, lat/lng pairs to ST_MakePoint — no string
// interpolation of user input.
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.

// ErrPostGISUnavailable is returned by geo store methods when the database
// has no PostGIS extension (e.g. the embedded Postgres used by store tests,
// or a non-postgis image). Handlers map it to 503.
var ErrPostGISUnavailable = errors.New("postgis extension unavailable")

// ContactLocation sources (contact_locations.source CHECK).
const (
	LocationSourceBookingAddress = "booking_address"
	LocationSourceChannelShare   = "channel_share"
	LocationSourceManual         = "manual"
	LocationSourceGeocode        = "geocode"
)

// Geo campaign statuses (geo_campaigns.status CHECK).
const (
	GeoCampaignDraft     = "draft"
	GeoCampaignRunning   = "running"
	GeoCampaignCompleted = "completed"
	GeoCampaignFailed    = "failed"
)

// ContactLocation mirrors booking.contact_locations: the last known
// position of one contact (upserted per source).
type ContactLocation struct {
	TenantID  uuid.UUID `json:"tenant_id"`
	ContactID uuid.UUID `json:"contact_id"`
	Lat       float64   `json:"lat"`
	Lng       float64   `json:"lng"`
	Source    string    `json:"source"`
	UpdatedAt time.Time `json:"updated_at"`
}

// LocationPoint is one booking-joined contact position for the summary map.
type LocationPoint struct {
	Lat        float64   `json:"lat"`
	Lng        float64   `json:"lng"`
	BookingID  uuid.UUID `json:"booking_id"`
	OfferingID uuid.UUID `json:"offering_id"`
	StartsAt   time.Time `json:"starts_at"`
}

// LocationCluster is a server-side ST_SnapToGrid aggregate cell.
type LocationCluster struct {
	Lat   float64 `json:"lat"`
	Lng   float64 `json:"lng"`
	Count int     `json:"count"`
}

// ServiceArea mirrors booking.service_areas; GeoJSON carries the
// MultiPolygon back to clients (ST_AsGeoJSON on read).
type ServiceArea struct {
	ID        uuid.UUID `json:"id"`
	TenantID  uuid.UUID `json:"tenant_id"`
	Name      string    `json:"name"`
	GeoJSON   string    `json:"geojson"`
	Meta      []byte    `json:"meta"`
	CreatedAt time.Time `json:"created_at"`
}

// GeoCampaign mirrors booking.geo_campaigns.
type GeoCampaign struct {
	ID            uuid.UUID  `json:"id"`
	TenantID      uuid.UUID  `json:"tenant_id"`
	Name          string     `json:"name"`
	Channel       string     `json:"channel"`
	Message       string     `json:"message"`
	TargetGeoJSON string     `json:"target"`
	AudienceCount int        `json:"audience_count"`
	Status        string     `json:"status"`
	CreatedAt     time.Time  `json:"created_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
}

// CampaignRecipient is one audience contact of a geo campaign (joined with
// the contacts table for name + phone).
type CampaignRecipient struct {
	ContactID uuid.UUID `json:"contact_id"`
	Name      string    `json:"name"`
	Phone     string    `json:"phone"`
}

// ensureGeoTables bootstraps the PostGIS tables idempotently (like
// ensureWaitlistTable). Returns ErrPostGISUnavailable without touching
// anything when the server cannot provide the postgis extension, so
// non-postgis deployments (and the embedded-Postgres store tests) keep
// working with geo features disabled.
func (s *Store) ensureGeoTables(ctx context.Context) error {
	var available bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'postgis')`).Scan(&available); err != nil {
		return fmt.Errorf("check postgis availability: %w", err)
	}
	if !available {
		return ErrPostGISUnavailable
	}
	const ddl = `
CREATE EXTENSION IF NOT EXISTS postgis;

CREATE TABLE IF NOT EXISTS contact_locations (
    tenant_id  UUID NOT NULL,
    contact_id UUID NOT NULL,
    geom       geography(Point,4326) NOT NULL,
    source     TEXT NOT NULL DEFAULT 'manual'
               CHECK (source IN ('booking_address','channel_share','manual','geocode')),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, contact_id)
);
CREATE INDEX IF NOT EXISTS idx_contact_locations_geom ON contact_locations USING GIST (geom);
ALTER TABLE contact_locations ENABLE ROW LEVEL SECURITY;
ALTER TABLE contact_locations FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'contact_locations' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON contact_locations
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS service_areas (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID NOT NULL,
    name       TEXT NOT NULL DEFAULT '',
    geom       geography(MultiPolygon,4326) NOT NULL,
    meta       JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_service_areas_geom ON service_areas USING GIST (geom);
ALTER TABLE service_areas ENABLE ROW LEVEL SECURITY;
ALTER TABLE service_areas FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'service_areas' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON service_areas
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS geo_campaigns (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL,
    name           TEXT NOT NULL DEFAULT '',
    channel        TEXT NOT NULL DEFAULT 'sms',
    message        TEXT NOT NULL DEFAULT '',
    target         geography(MultiPolygon,4326) NOT NULL,
    audience_count INT NOT NULL DEFAULT 0,
    status         TEXT NOT NULL DEFAULT 'draft'
                   CHECK (status IN ('draft','running','completed','failed')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at     TIMESTAMPTZ,
    completed_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_geo_campaigns_target ON geo_campaigns USING GIST (target);
ALTER TABLE geo_campaigns ENABLE ROW LEVEL SECURITY;
ALTER TABLE geo_campaigns FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'geo_campaigns' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON geo_campaigns
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

-- Idempotent-replay ledger: one row per (campaign, contact) actually sent.
-- The GeoCampaignWorkflow checks it before each batch so a restarted
-- campaign never double-sends.
CREATE TABLE IF NOT EXISTS geo_campaign_sends (
    campaign_id UUID NOT NULL,
    tenant_id   UUID NOT NULL,
    contact_id  UUID NOT NULL,
    sent_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (campaign_id, contact_id)
);
ALTER TABLE geo_campaign_sends ENABLE ROW LEVEL SECURITY;
ALTER TABLE geo_campaign_sends FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'geo_campaign_sends' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON geo_campaign_sends
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure geo tables: %w", err)
	}
	return nil
}

// GeoEnabled reports whether the PostGIS tables were bootstrapped.
func (s *Store) GeoEnabled() bool { return s.geoEnabled }

func (s *Store) requireGeo() error {
	if !s.geoEnabled {
		return ErrPostGISUnavailable
	}
	return nil
}

// ---------------------------------------------------------------------------
// Contact locations
// ---------------------------------------------------------------------------

// UpsertContactLocation inserts or replaces the contact's position
// (pk conflict on (tenant_id, contact_id)). ST_MakePoint takes (x=lng,
// y=lat); both arrive as bound parameters.
func (s *Store) UpsertContactLocation(ctx context.Context, loc *ContactLocation) error {
	if err := s.requireGeo(); err != nil {
		return err
	}
	const q = `INSERT INTO contact_locations (tenant_id, contact_id, geom, source, updated_at)
	           VALUES ($1, $2, ST_SetSRID(ST_MakePoint($3, $4), 4326)::geography, $5, now())
	           ON CONFLICT (tenant_id, contact_id)
	           DO UPDATE SET geom = EXCLUDED.geom, source = EXCLUDED.source, updated_at = now()
	           RETURNING updated_at`
	return s.withTenant(ctx, loc.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, loc.TenantID, loc.ContactID, loc.Lng, loc.Lat, loc.Source).
			Scan(&loc.UpdatedAt)
	})
}

// SummaryFilter narrows the booking-joined location summary. Query text and
// args are produced by the pure builders in internal/geo (unit-tested
// without a database); the store only executes them.
type SummaryFilter struct {
	From       *time.Time
	To         *time.Time
	OfferingID *uuid.UUID
	Limit      int
}

// ListLocationPoints runs the built points query (capped by the builder's
// LIMIT, contract max 5000).
func (s *Store) ListLocationPoints(ctx context.Context, tenantID uuid.UUID, q string, args []any) ([]LocationPoint, error) {
	if err := s.requireGeo(); err != nil {
		return nil, err
	}
	var out []LocationPoint
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p LocationPoint
			if err := rows.Scan(&p.Lat, &p.Lng, &p.BookingID, &p.OfferingID, &p.StartsAt); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// ListLocationClusters runs the built ST_SnapToGrid cluster query.
func (s *Store) ListLocationClusters(ctx context.Context, tenantID uuid.UUID, q string, args []any) ([]LocationCluster, error) {
	if err := s.requireGeo(); err != nil {
		return nil, err
	}
	var out []LocationCluster
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c LocationCluster
			if err := rows.Scan(&c.Lat, &c.Lng, &c.Count); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Service areas
// ---------------------------------------------------------------------------

// CreateServiceArea stores a GeoJSON Polygon/MultiPolygon as
// geography(MultiPolygon,4326); ST_Multi promotes Polygon → MultiPolygon.
func (s *Store) CreateServiceArea(ctx context.Context, a *ServiceArea) error {
	if err := s.requireGeo(); err != nil {
		return err
	}
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	meta := a.Meta
	if len(meta) == 0 {
		meta = []byte(`{}`)
	}
	const q = `INSERT INTO service_areas (id, tenant_id, name, geom, meta, created_at)
	           VALUES ($1, $2, $3, ST_Multi(ST_GeomFromGeoJSON($4))::geography, $5, now())
	           RETURNING created_at`
	return s.withTenant(ctx, a.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, a.ID, a.TenantID, a.Name, a.GeoJSON, meta).Scan(&a.CreatedAt)
	})
}

const serviceAreaCols = `id, tenant_id, name, ST_AsGeoJSON(geom), meta, created_at`

func scanServiceArea(row pgx.Row) (ServiceArea, error) {
	var a ServiceArea
	err := row.Scan(&a.ID, &a.TenantID, &a.Name, &a.GeoJSON, &a.Meta, &a.CreatedAt)
	return a, err
}

// ListServiceAreas returns all service areas of a tenant (newest first).
func (s *Store) ListServiceAreas(ctx context.Context, tenantID uuid.UUID) ([]ServiceArea, error) {
	if err := s.requireGeo(); err != nil {
		return nil, err
	}
	const q = `SELECT ` + serviceAreaCols + ` FROM service_areas WHERE tenant_id=$1 ORDER BY created_at DESC`
	var out []ServiceArea
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			a, err := scanServiceArea(rows)
			if err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

// DeleteServiceArea removes one service area scoped to a tenant.
func (s *Store) DeleteServiceArea(ctx context.Context, tenantID, id uuid.UUID) error {
	if err := s.requireGeo(); err != nil {
		return err
	}
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM service_areas WHERE tenant_id=$1 AND id=$2`, tenantID, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Audience preview (queries built in internal/geo)
// ---------------------------------------------------------------------------

// AudienceCount runs the built ST_Within/ST_DWithin count query.
func (s *Store) AudienceCount(ctx context.Context, tenantID uuid.UUID, q string, args []any) (int, error) {
	if err := s.requireGeo(); err != nil {
		return 0, err
	}
	var n int
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, args...).Scan(&n)
	})
	return n, err
}

// AudienceContact pairs a contact id with its phone for the masked sample.
type AudienceContact struct {
	ContactID uuid.UUID `json:"contact_id"`
	Phone     string    `json:"-"`
}

// AudienceSample runs the built sample query.
func (s *Store) AudienceSample(ctx context.Context, tenantID uuid.UUID, q string, args []any) ([]AudienceContact, error) {
	if err := s.requireGeo(); err != nil {
		return nil, err
	}
	var out []AudienceContact
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c AudienceContact
			if err := rows.Scan(&c.ContactID, &c.Phone); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Geo campaigns
// ---------------------------------------------------------------------------

// CircleTarget is a center+radius campaign/audience selector. The target
// column is geography(MultiPolygon), so circles are stored as a geodesic
// ST_Buffer (meters) of the center point — consistent with the ST_DWithin
// audience preview for the same selector.
type CircleTarget struct {
	Lat     float64
	Lng     float64
	RadiusM float64
}

// CreateGeoCampaign inserts a campaign row in `running` status (started_at
// = now()): the API starts the GeoCampaignWorkflow right after, so a
// created campaign is by definition launched. Exactly one target shape is
// used: c.TargetGeoJSON (validated GeoJSON Polygon/MultiPolygon, promoted
// via ST_Multi) or circle (ST_Buffer in meters).
func (s *Store) CreateGeoCampaign(ctx context.Context, c *GeoCampaign, circle *CircleTarget) error {
	if err := s.requireGeo(); err != nil {
		return err
	}
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	return s.withTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		if circle != nil {
			const q = `INSERT INTO geo_campaigns (id, tenant_id, name, channel, message, target, status, created_at, started_at)
			           VALUES ($1, $2, $3, $4, $5,
			                   ST_Multi(ST_Buffer(ST_SetSRID(ST_MakePoint($6, $7), 4326)::geography, $8)::geometry)::geography,
			                   $9, now(), now())
			           RETURNING created_at, started_at`
			return tx.QueryRow(ctx, q, c.ID, c.TenantID, c.Name, c.Channel, c.Message,
				circle.Lng, circle.Lat, circle.RadiusM, c.Status).Scan(&c.CreatedAt, &c.StartedAt)
		}
		const q = `INSERT INTO geo_campaigns (id, tenant_id, name, channel, message, target, status, created_at, started_at)
		           VALUES ($1, $2, $3, $4, $5, ST_Multi(ST_GeomFromGeoJSON($6))::geography, $7, now(), now())
		           RETURNING created_at, started_at`
		return tx.QueryRow(ctx, q, c.ID, c.TenantID, c.Name, c.Channel, c.Message, c.TargetGeoJSON, c.Status).
			Scan(&c.CreatedAt, &c.StartedAt)
	})
}

const geoCampaignCols = `id, tenant_id, name, channel, message, ST_AsGeoJSON(target), audience_count, status, created_at, started_at, completed_at`

func scanGeoCampaign(row pgx.Row) (GeoCampaign, error) {
	var c GeoCampaign
	err := row.Scan(&c.ID, &c.TenantID, &c.Name, &c.Channel, &c.Message, &c.TargetGeoJSON,
		&c.AudienceCount, &c.Status, &c.CreatedAt, &c.StartedAt, &c.CompletedAt)
	return c, err
}

// ListGeoCampaigns returns the tenant's campaigns, newest first.
func (s *Store) ListGeoCampaigns(ctx context.Context, tenantID uuid.UUID) ([]GeoCampaign, error) {
	if err := s.requireGeo(); err != nil {
		return nil, err
	}
	const q = `SELECT ` + geoCampaignCols + ` FROM geo_campaigns WHERE tenant_id=$1 ORDER BY created_at DESC LIMIT 200`
	var out []GeoCampaign
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanGeoCampaign(rows)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// GetGeoCampaign fetches one campaign scoped to a tenant.
func (s *Store) GetGeoCampaign(ctx context.Context, tenantID, id uuid.UUID) (GeoCampaign, error) {
	if err := s.requireGeo(); err != nil {
		return GeoCampaign{}, err
	}
	const q = `SELECT ` + geoCampaignCols + ` FROM geo_campaigns WHERE tenant_id=$1 AND id=$2`
	var c GeoCampaign
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		c, err = scanGeoCampaign(tx.QueryRow(ctx, q, tenantID, id))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// GeoCampaignAudienceBatch returns the next keyset page (contact_id >
// after, ascending) of campaign audience contacts that carry a phone,
// matched by ST_Within against the campaign's stored target.
func (s *Store) GeoCampaignAudienceBatch(ctx context.Context, tenantID, campaignID, after uuid.UUID, limit int) ([]CampaignRecipient, error) {
	if err := s.requireGeo(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	const q = `SELECT c.id, c.name, c.phone
	           FROM contact_locations cl
	           JOIN contacts c ON c.tenant_id = cl.tenant_id AND c.id = cl.contact_id
	           JOIN geo_campaigns g ON g.tenant_id = cl.tenant_id AND g.id = $2
	           WHERE cl.tenant_id = $1 AND cl.contact_id > $3
	             AND ST_Within(cl.geom, g.target)
	             AND COALESCE(c.phone, '') <> ''
	           ORDER BY cl.contact_id
	           LIMIT $4`
	var out []CampaignRecipient
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, tenantID, campaignID, after, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r CampaignRecipient
			if err := rows.Scan(&r.ContactID, &r.Name, &r.Phone); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// GeoCampaignSentContacts returns the subset of contactIDs already present
// in the send ledger (idempotent replay skip).
func (s *Store) GeoCampaignSentContacts(ctx context.Context, tenantID, campaignID uuid.UUID, contactIDs []uuid.UUID) (map[uuid.UUID]bool, error) {
	if err := s.requireGeo(); err != nil {
		return nil, err
	}
	sent := map[uuid.UUID]bool{}
	if len(contactIDs) == 0 {
		return sent, nil
	}
	const q = `SELECT contact_id FROM geo_campaign_sends
	           WHERE tenant_id=$1 AND campaign_id=$2 AND contact_id = ANY($3)`
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, tenantID, campaignID, contactIDs)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			sent[id] = true
		}
		return rows.Err()
	})
	return sent, err
}

// RecordGeoCampaignSends atomically (one tenant tx):
//  1. appends ledger rows for the just-sent recipients (ON CONFLICT DO
//     NOTHING — re-recording after an activity retry is a no-op);
//  2. writes one usage-metering outbox row per newly recorded send (the
//     UsageExtra pattern: metering cannot drift from the send ledger);
//  3. bumps geo_campaigns.audience_count by the number of NEW ledger rows.
//
// Returns the number of newly recorded sends.
func (s *Store) RecordGeoCampaignSends(ctx context.Context, tenantID, campaignID uuid.UUID, contactIDs []uuid.UUID, usageExtra func(contactID uuid.UUID) []ExtraOutbox) (int, error) {
	if err := s.requireGeo(); err != nil {
		return 0, err
	}
	recorded := 0
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		for _, cid := range contactIDs {
			tag, err := tx.Exec(ctx,
				`INSERT INTO geo_campaign_sends (campaign_id, tenant_id, contact_id)
				 VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, campaignID, tenantID, cid)
			if err != nil {
				return fmt.Errorf("record campaign send: %w", err)
			}
			if tag.RowsAffected() == 0 {
				continue // already sent (idempotent replay)
			}
			recorded++
			if usageExtra != nil {
				if err := insertExtraOutbox(ctx, tx, campaignID, usageExtra(cid)); err != nil {
					return err
				}
			}
		}
		if recorded > 0 {
			if _, err := tx.Exec(ctx,
				`UPDATE geo_campaigns SET audience_count = audience_count + $3
				 WHERE tenant_id=$1 AND id=$2`, tenantID, campaignID, recorded); err != nil {
				return fmt.Errorf("bump audience_count: %w", err)
			}
		}
		return nil
	})
	return recorded, err
}

// SetGeoCampaignStatus transitions a campaign to completed/failed (terminal
// states stamp completed_at).
func (s *Store) SetGeoCampaignStatus(ctx context.Context, tenantID, id uuid.UUID, status string) error {
	if err := s.requireGeo(); err != nil {
		return err
	}
	const q = `UPDATE geo_campaigns SET status=$3, completed_at=now()
	           WHERE tenant_id=$1 AND id=$2`
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, q, tenantID, id, status)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}
