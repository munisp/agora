-- gold.receipt_ingest_daily — passthrough view over the Iceberg table written
-- by the Spark receipt_recon_gold job (infra/lakehouse/spark/jobs/receipt_recon_gold.py,
-- SPEC-W62/W64). One row per tenant/day/channel with OCR receipt ingest volume
-- from bronze.receipt_documents (hash-only + whitelisted scalars, no free text —
-- W62 G1). The W64 channel column is attributed from bronze.receipt_ingest
-- (ReceiptIngested events, document_id join) — closing the W62
-- channel-attribution gap; NULL when no ingest event exists for the document
-- (e.g. direct /v1/ocr/extract uploads or pre-W64 rows). reco_pricing
-- precedent: Spark writes, dbt exposes the view + tests.
{{ config(materialized='view') }}

select
    tenant_id,
    day,
    channel,
    documents,
    pages,
    receipts,
    receipts_with_amount
from iceberg.gold.receipt_ingest_daily
