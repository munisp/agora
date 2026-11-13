-- stg_receipt_recon_findings: standardized view over bronze.receipt_recon_findings
-- (W62 G2). ReceiptReconFinding events are routed to their OWN bronze table by
-- analytics-pipeline — previously they landed sparsely in bronze.booking_events,
-- breaking the stg_booking_events accepted_values test.
-- finding_kind/confidence are lowercased closed enums so gold models and the
-- schema tests can rely on them; booking_id stays nullable
-- (payment_without_receipt findings may have no booking).
with source as (
    select * from {{ source('bronze', 'receipt_recon_findings') }}
),

standardized as (
    select
        event_id,
        tenant_id,
        run_id,                                 -- NULL until booking-service enriches findingEventPayload (follow-up)
        lower(finding_kind) as finding_kind,    -- receipt_without_payment|payment_without_receipt|amount_mismatch|currency_unknown
        lower(confidence) as confidence,        -- exact|heuristic|none
        capture_client_id,
        booking_id,
        attempt_tx_ref,
        expected_amount_minor,
        captured_amount_minor,
        receipt_amount_major,
        upper(receipt_currency) as receipt_currency,
        occurred_at,
        cast(date_trunc('day', occurred_at) as date) as finding_day
    from source
    where event_id is not null
      and tenant_id is not null
      and occurred_at is not null
)

select * from standardized
