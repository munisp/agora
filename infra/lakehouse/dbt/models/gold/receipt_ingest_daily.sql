-- gold.receipt_ingest_daily — passthrough view over the Iceberg table written
-- by the Spark receipt_recon_gold job (infra/lakehouse/spark/jobs/receipt_recon_gold.py,
-- SPEC-W62). One row per tenant/day with OCR receipt ingest volume from
-- bronze.receipt_documents (hash-only + whitelisted scalars, no free text —
-- W62 G1). reco_pricing precedent: Spark writes, dbt exposes the view + tests.
{{ config(materialized='view') }}

select
    tenant_id,
    day,
    documents,
    pages,
    receipts,
    receipts_with_amount
from iceberg.gold.receipt_ingest_daily
