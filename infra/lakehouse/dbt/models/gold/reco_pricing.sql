-- gold.reco_pricing — passthrough view over the Iceberg table written by the
-- Spark revenue_intelligence job (infra/lakehouse/spark/jobs/revenue_intelligence.py,
-- SPEC-W3 §3 innovation 9). One row per tenant/offering per job run;
-- analytics-pipeline's GET /v1/recommendations reads the latest row per
-- offering directly from Iceberg (this view keeps the dbt docs/lineage and
-- schema tests in one place).
{{ config(materialized='view') }}

select
    tenant_id,
    offering_id,
    computed_at,
    bookings_30d,
    net_revenue_cents_30d,
    peak_hour,
    peak_share,
    no_show_rate,
    suggested_peak_multiplier,
    suggested_deposit_pct
from iceberg.gold.reco_pricing
