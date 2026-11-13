-- stg_receipt_documents: standardized view over bronze.receipt_documents (W62).
-- OCR receipt facts only: the analytics-pipeline mapper lands kind='receipt'
-- documents and DROPS kyc_* kinds entirely (G1); only whitelisted scalars
-- (amount_minor/currency/receipt_date) plus hash-only identifiers (sha256/phash)
-- ever reach the lake — no free text (vendor/note/items).
with source as (
    select * from {{ source('bronze', 'receipt_documents') }}
),

standardized as (
    select
        event_id,
        tenant_id,
        document_id,
        lower(kind) as kind,                    -- receipt
        lower(status) as status,
        page_count,
        sha256,
        phash,
        model_version,
        amount_minor,
        upper(currency) as currency,            -- ISO-3
        receipt_date,                           -- YYYY-MM-DD string (nullable)
        occurred_at,
        cast(date_trunc('day', occurred_at) as date) as event_day
    from source
    where event_id is not null
      and tenant_id is not null
      and document_id is not null
      and occurred_at is not null
)

select * from standardized
