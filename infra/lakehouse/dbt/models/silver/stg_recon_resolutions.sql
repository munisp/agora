-- stg_recon_resolutions: standardized view over bronze.recon_resolutions
-- (W64). One row per ReceiptReconResolved — a finding resolved via
-- POST /v1/recon/receipts/{id}/resolve (manage_bookings), emitted by
-- booking-service Store.ResolveFinding in the same tx as the resolved_at
-- update. finding_id joins to stg_receipt_recon_findings (the gold
-- receipt_recon_daily.resolved_count join); resolved_day is derived from
-- resolved_at (falling back to occurred_at) so resolutions group by the
-- day the resolution happened.
with source as (
    select * from {{ source('bronze', 'recon_resolutions') }}
),

standardized as (
    select
        event_id,
        tenant_id,
        finding_id,
        run_id,
        lower(kind) as kind,                    -- same enum as finding_kind
        resolved_at,
        resolved_by,
        occurred_at,
        cast(date_trunc('day', coalesce(resolved_at, occurred_at)) as date) as resolved_day
    from source
    where event_id is not null
      and tenant_id is not null
      and finding_id is not null
      and coalesce(resolved_at, occurred_at) is not null
)

select * from standardized
