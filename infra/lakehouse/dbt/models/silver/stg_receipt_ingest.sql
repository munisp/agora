-- stg_receipt_ingest: standardized view over bronze.receipt_ingest (W64 G-B).
-- ReceiptIngested events are routed to their OWN bronze table by
-- analytics-pipeline — they must NEVER land in bronze.booking_events (the
-- stg_booking_events accepted_values test is the tripwire). Producers:
-- fieldcapture AttachReceipt (channel 'field_pwa') and media_inbox
-- IngestMediaInbox fresh-insert branch (channel 'whatsapp'; replay never
-- emits). PII posture: sender_phone is never emitted by the producers and
-- never read by the mapper, so it cannot appear here. channel is a
-- lowercased closed enum; booking_id stays nullable (an ingest may precede
-- booking linkage) and comes from data only — never the tenant-slug
-- CloudEvent subject (the W62 G2 lesson).
with source as (
    select * from {{ source('bronze', 'receipt_ingest') }}
),

standardized as (
    select
        event_id,
        tenant_id,
        lower(channel) as channel,              -- field_pwa|whatsapp
        document_id,
        capture_client_id,
        media_inbox_id,
        booking_id,
        mime,
        bytes,
        sha256_prefix,
        state,
        occurred_at,
        cast(date_trunc('day', occurred_at) as date) as event_day
    from source
    where event_id is not null
      and tenant_id is not null
      and document_id is not null
      and occurred_at is not null
)

select * from standardized
