"""model-registry — model version registry, drift monitoring, A/B testing,
and continuous-training scheduler (SPEC-W33 §4 C1/C2/C3/C5).

W33 invariants bound here:
  I1 honest degradation — Kafka down / registry empty → log-and-continue,
     consumers fall back; the scheduler never crashes on job errors.
  I2 honest provenance — every version row carries seed/dataset_hash/git_sha
     where knowable.
  I3 honest metrics — metrics jsonb states its data basis (synthetic labeled
     synthetic) at the producer; this service stores, never fabricates.
  I4 tenant isolation — Postgres RLS (FORCE) + single write path through
     store.RegistryStore.
  I5 CPU-first slim image — no torch, no sklearn, no pandas; FastAPI +
     psycopg + APScheduler + prometheus-client only; the Kafka producer is
     import-guarded (service runs fine without kafka-python installed).
"""
