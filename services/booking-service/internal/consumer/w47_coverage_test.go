package consumer

// SPEC-W47 (W47-A item 6, A-DB DB-02) cascade-coverage test: boots the FULL
// booking-DB schema (core store + every app package bootstrap) and fails
// when a tenant-scoped table exists that is neither purged/anonymized by
// the TenantDeleted cascade (store.TenantCascadeTables) nor on the
// documented keep-list below. Adding a tenant table without classifying it
// breaks this test — that is the point.

import (
	"context"
	"sort"
	"testing"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/opendesk/booking-service/internal/campaignstudio"
	"github.com/opendesk/booking-service/internal/crm360"
	"github.com/opendesk/booking-service/internal/devices"
	"github.com/opendesk/booking-service/internal/fieldcapture"
	"github.com/opendesk/booking-service/internal/helpdesk"
	"github.com/opendesk/booking-service/internal/lending"
	"github.com/opendesk/booking-service/internal/loyalty"
	"github.com/opendesk/booking-service/internal/socialpub"
	"github.com/opendesk/booking-service/internal/store"
	"github.com/opendesk/booking-service/internal/surveys"
	"github.com/opendesk/booking-service/internal/workforce"
	"github.com/opendesk/booking-service/internal/workorders"
)

// tenantTableKeepList documents the tenant-scoped tables the cascade
// deliberately KEEPS, with the reason. Everything else with a tenant_id
// column must appear in store.TenantCascadeTables().
var tenantTableKeepList = map[string]string{
	// Catalog / config — no customer PII.
	"offerings":          "service catalog config",
	"team_members":       "staff records; staff identity lifecycle is identity-service's (K9 cascade)",
	"availability_rules": "staff schedule config",
	"civic_categories":   "civic config",
	"civic_routing_rules": "civic config",
	"civic_ref_seqs":     "public-ref sequence state",
	"sla_policies":       "helpdesk config",
	"surveys":            "survey definitions (config)",
	"loyalty_programs":   "loyalty config",
	"commission_rules":   "commission config",
	"loan_products":      "lending config",
	"geo_campaigns":      "campaign config (targeting geometry is public geo)",
	"service_areas":      "service-area config",
	"sites":              "public slug registry tombstone (documented RLS exemption)",
	"crm_tags":           "contact→tag links; the contact row is anonymized, tags are labels",
	// Financial / audit spine — retained per A-DB DB-02 ("keep
	// financial/audit rows"), with PII links anonymized via contacts.
	"bookings":             "open bookings cancelled; terminal rows are the audit/financial spine",
	// SPEC-W49 B3: payment-attempt audit (tx_ref reconciliation chain) —
	// financial audit rows keyed by booking/tx_ref, no customer PII; kept
	// with the bookings spine they annotate.
	"booking_payment_attempts": "financial payment-attempt audit (tx_ref reconciliation); no customer PII",
	"campaigns":            "campaign spine; spend ledger audit (W47 lifecycle adds terminal cancel)",
	"campaign_spend":       "financial spend ledger",
	"promo_codes":          "campaign config + redemption-count audit",
	"commission_ledger":    "financial ledger",
	"commission_payouts":   "financial payouts",
	"loyalty_wallets":      "points balances (financial); contact link anonymized",
	"loyalty_ledger":       "financial ledger",
	"loan_applications":    "financial application record (AML retention)",
	"loan_accounts":        "financial accounts (AML retention)",
	"repayments":           "financial ledger",
	"lending_ledger":       "financial ledger",
	"ticket_events":        "ticket audit timeline (structured columns only)",
	// Ops/machine tables — no structured customer PII.
	"incidents":                    "IoT incident ops audit (machine payloads)",
	"incident_deliveries":          "webhook delivery audit",
	"incident_dispatch_endpoints":  "operator-configured webhook endpoints",
	"work_orders":                  "field-service ops record",
	"shifts":                       "workforce ops record (staff)",
	"time_entries":                 "workforce ops record (staff)",
	"leave_requests":               "workforce ops record (staff)",
	"social_accounts":              "operator-connected social accounts (no access tokens stored)",
	"social_posts":                 "published-content ops record",
	"social_ads":                   "ad ops record",
	"social_creatives":             "ad creative ops record",
	"studio_segments":              "campaign-studio config",
	"studio_journeys":              "campaign-studio config",
	"studio_enrollments":           "journey enrollment ledger; contact link anonymized",
	"studio_step_events":           "journey step audit; contact link anonymized",
}

func TestTenantCascadeCoverage_FullSchema(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping embedded-postgres coverage test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_w47_cov").
		Port(5590).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	dsn := "postgres://postgres:postgres@localhost:5590/booking_w47_cov?sslmode=disable"
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx connect: %v", err)
	}
	// Base schema the core store expects (mirrors the httpapi W45 test
	// schema): init-scripts create these in production.
	if _, err := conn.Exec(ctx, `
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE TABLE IF NOT EXISTS offerings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL, name TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
    duration_min INTEGER NOT NULL, buffer_min INTEGER NOT NULL DEFAULT 0,
    price_cents INTEGER NOT NULL DEFAULT 0, currency CHAR(3) NOT NULL DEFAULT 'USD',
    capacity INTEGER NOT NULL DEFAULT 1, bookable BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS contacts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL, name TEXT NOT NULL, phone TEXT, email TEXT, notes TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS bookings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL, offering_id UUID NOT NULL, team_member_id UUID, contact_id UUID,
    starts_at TIMESTAMPTZ NOT NULL, ends_at TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending', source TEXT NOT NULL DEFAULT 'api',
    idempotency_key TEXT, created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS outbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(), aggregate_id UUID NOT NULL,
    topic TEXT NOT NULL, payload JSONB NOT NULL, sent_at TIMESTAMPTZ
);`); err != nil {
		t.Fatalf("base schema: %v", err)
	}
	conn.Close(ctx) //nolint:errcheck

	// Bootstrap the FULL schema: core store + every app package.
	st, err := store.New(ctx, dsn, 0)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(st.Close)
	closers := []interface{ Close() }{}
	dial := func(name string, fn func() error) {
		t.Helper()
		if err := fn(); err != nil {
			t.Fatalf("%s bootstrap: %v", name, err)
		}
	}
	dial("helpdesk", func() error { s, e := helpdesk.DialStore(ctx, dsn, 0); if e == nil { closers = append(closers, s) }; return e })
	dial("surveys", func() error { s, e := surveys.DialStore(ctx, dsn); if e == nil { closers = append(closers, s) }; return e })
	dial("devices", func() error { s, e := devices.DialStore(ctx, dsn); if e == nil { closers = append(closers, s) }; return e })
	dial("fieldcapture", func() error { s, e := fieldcapture.DialStore(ctx, dsn); if e == nil { closers = append(closers, s) }; return e })
	dial("crm360", func() error { s, e := crm360.DialStore(ctx, dsn); if e == nil { closers = append(closers, s) }; return e })
	dial("workorders", func() error { s, e := workorders.DialStore(ctx, dsn); if e == nil { closers = append(closers, s) }; return e })
	dial("loyalty", func() error { s, e := loyalty.DialStore(ctx, dsn); if e == nil { closers = append(closers, s) }; return e })
	dial("lending", func() error { s, e := lending.DialStore(ctx, dsn); if e == nil { closers = append(closers, s) }; return e })
	dial("workforce", func() error { s, e := workforce.DialStore(ctx, dsn); if e == nil { closers = append(closers, s) }; return e })
	dial("socialpub", func() error { s, e := socialpub.DialStore(ctx, dsn); if e == nil { closers = append(closers, s) }; return e })
	dial("campaignstudio", func() error { s, e := campaignstudio.DialStore(ctx, dsn); if e == nil { closers = append(closers, s) }; return e })
	defer func() {
		for _, c := range closers {
			c.Close()
		}
	}()

	// Inventory every tenant-scoped table.
	conn, err = pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(ctx,
		`SELECT DISTINCT table_name FROM information_schema.columns
		 WHERE table_schema='public' AND column_name='tenant_id' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var inventory []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		inventory = append(inventory, name)
	}
	rows.Close()
	conn.Close(ctx) //nolint:errcheck

	covered := store.TenantCascadeTables()
	var unclassified []string
	for _, tbl := range inventory {
		if _, ok := covered[tbl]; ok {
			continue
		}
		if _, ok := tenantTableKeepList[tbl]; ok {
			continue
		}
		unclassified = append(unclassified, tbl)
	}
	if len(unclassified) > 0 {
		sort.Strings(unclassified)
		t.Fatalf("tenant-scoped tables without a TenantDeleted cascade path or a documented keep-list entry (A-DB DB-02): %v", unclassified)
	}

	// The extended cascade must run green against the FULL schema (no
	// skip-missing shortcuts) and report the package tables it touched.
	sum, err := st.DeleteTenantData(ctx, uuid.New())
	if err != nil {
		t.Fatalf("DeleteTenantData on full schema: %v", err)
	}
	for _, tbl := range []string{"tickets", "survey_invites", "survey_responses", "device_tokens",
		"portal_tokens", "csat_tokens", "field_captures", "field_checkins", "crm_notes", "outbox"} {
		if _, ok := sum.Details[tbl]; !ok {
			t.Fatalf("full-schema cascade skipped existing table %s (details=%+v)", tbl, sum.Details)
		}
	}
}
