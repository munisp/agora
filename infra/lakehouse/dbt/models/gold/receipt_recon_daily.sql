-- gold.receipt_recon_daily — passthrough view over the Iceberg table written
-- by the Spark receipt_recon_gold job (infra/lakehouse/spark/jobs/receipt_recon_gold.py,
-- SPEC-W62). One row per tenant/day/finding_kind/confidence, aggregated from
-- bronze.receipt_recon_findings (routed to its own bronze table by the W62 G2
-- fix). reco_pricing precedent: Spark writes, dbt exposes the view + tests.
{{ config(materialized='view') }}

select
    tenant_id,
    day,
    finding_kind,
    confidence,
    findings_count,
    resolved_count,
    expected_sum_minor,
    captured_sum_minor,
    mismatch_abs_sum_minor
from iceberg.gold.receipt_recon_daily
