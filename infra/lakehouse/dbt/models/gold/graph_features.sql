-- gold.graph_features — passthrough view over the Iceberg table written by
-- the Spark graph_export job (infra/lakehouse/spark/jobs/graph_export.py,
-- SPEC-W28 §1/§4, GNN seam). One row per graph node per nightly snapshot;
-- the Phase-3 GNN trainer reads these feature rows directly from Iceberg
-- (this view keeps the dbt docs/lineage and schema tests in one place).
-- Prediction write-back lands as Person.propensity_* properties via the
-- graph_enrichment.py path — see docs/graph.md.
{{ config(materialized='view') }}

select
    snapshot_date,
    tenant_id,
    label,
    node_id,
    in_degree,
    out_degree,
    consent_marketing,
    quarantine,
    bookings_total,
    ltv_cents,
    no_show_rate,
    propensity_show,
    propensity_convert,
    channel_of_first_touch,
    last_active_at
from iceberg.gold.graph_node_features
