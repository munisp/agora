-- 0006_plan_presets.sql — plan source of truth + VAT-ready tax foundation
-- (SPEC-W45 K18-billing-side; ORPH O5). Applied idempotently by the service
-- at startup after 0005 (same sqlx::raw_sql bootstrap pattern; no psql
-- backslash commands).
--
--   1. tenant_plans: billing's plan source. identity-service pushes
--      PUT /v1/tenants/{id}/plan on plan change (K18); invoice generation
--      reads the plan here (default 'free') and copies plan_presets ->
--      rate_cards for a tenant that has NO rate cards yet (see
--      src/invoices.rs::generate_invoice). RLS'd exactly like rate_cards
--      (0002 tenant_isolation policy: fail-closed on the request-scoped
--      app.tenant_id GUC, role-gated internal escape hatch).
--   2. tax_bps on rate_cards + plan_presets: VAT/tax basis points
--      (0..=10000, default 0). Stamped onto each invoice line at generation
--      (VAT-ready foundation only — amounts are unchanged).
--   3. invoices.billing_email (nullable): the tenant billing contact used
--      for invoice delivery; carried into the InvoicePaid/InvoiceVoided
--      event payloads when present.

-- ---------------------------------------------------------------------------
-- 1) tenant_plans (tenant-scoped; RLS idiom copied from 0002 rate_cards)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS tenant_plans (
    tenant_id  UUID PRIMARY KEY,
    plan       TEXT NOT NULL DEFAULT 'free',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE tenant_plans ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_plans FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON tenant_plans;
CREATE POLICY tenant_isolation ON tenant_plans
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           OR pg_has_role(current_user, 'app_billing_internal', 'member'))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           OR pg_has_role(current_user, 'app_billing_internal', 'member'));

-- ---------------------------------------------------------------------------
-- 2) tax_bps (integer 0..=10000, default 0) on rate_cards + plan_presets.
--    ADD COLUMN IF NOT EXISTS keeps re-application safe; the CHECKs follow
--    the 0005 idiom (DROP + NOT VALID + VALIDATE) so historical rows are
--    verified once at apply time without a rewrite.
-- ---------------------------------------------------------------------------
ALTER TABLE rate_cards ADD COLUMN IF NOT EXISTS tax_bps INTEGER NOT NULL DEFAULT 0;
ALTER TABLE rate_cards DROP CONSTRAINT IF EXISTS rate_cards_tax_bps_range;
ALTER TABLE rate_cards ADD CONSTRAINT rate_cards_tax_bps_range
    CHECK (tax_bps >= 0 AND tax_bps <= 10000) NOT VALID;
ALTER TABLE rate_cards VALIDATE CONSTRAINT rate_cards_tax_bps_range;

ALTER TABLE plan_presets ADD COLUMN IF NOT EXISTS tax_bps INTEGER NOT NULL DEFAULT 0;
ALTER TABLE plan_presets DROP CONSTRAINT IF EXISTS plan_presets_tax_bps_range;
ALTER TABLE plan_presets ADD CONSTRAINT plan_presets_tax_bps_range
    CHECK (tax_bps >= 0 AND tax_bps <= 10000) NOT VALID;
ALTER TABLE plan_presets VALIDATE CONSTRAINT plan_presets_tax_bps_range;

-- ---------------------------------------------------------------------------
-- 3) invoices.billing_email (nullable; no CHECK — light format validation
--    lives at the API layer, PATCH /v1/invoices/{id}/billing-email)
-- ---------------------------------------------------------------------------
ALTER TABLE invoices ADD COLUMN IF NOT EXISTS billing_email TEXT;
