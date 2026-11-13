#!/usr/bin/env bash
# run-scheduled.sh — lakehouse batch scheduler loop (SPEC-W47 / A-MW "Apache
# Sedona DEPLOYED-BUT-ORPHAN (unscheduled)").
#
# The lakehouse ships real batch jobs (silver clean, CAC, revenue, and the
# Apache Sedona geo_analytics gold job) but nothing ran them on a schedule —
# they only ran via manual spark-submit or the seed bootstrap hook. This
# sidecar loop closes that gap. It runs inside the `spark-scheduler` compose
# service (profile `batch-scheduler`, infra/docker-compose.lakehouse.yml) on
# the bitnami/spark:3.5 image, submitting to the compose spark-master.
#
# Every job is idempotent (dynamic partition overwrite / CTAS-on-first-run)
# and degrades gracefully on missing inputs (see infra/lakehouse/spark/
# README.md "Job disposition"), so a fixed-interval re-run loop is safe.
# Seed tooling (seed_*) stays with scripts/seeds/bootstrap.sh — NOT here.
#
# Env (all optional):
#   SPARK_MASTER_URL        spark://spark-master:7077
#   SPARK_JOBS_DIR          /opt/spark-jobs      (compose mount point)
#   BATCH_INTERVAL_SECONDS  3600                 (sleep between full passes)
#   BATCH_JOBS              space-separated job filenames (default below)
#   SPARK_PACKAGES          Ivy packages for --packages (default: the pinned
#                           Iceberg runtime + aws-bundle the silver/revenue
#                           jobs need; sedona_common-based jobs MERGE and
#                           dedupe this with their own pinned sets, so one
#                           flag serves every job)
set -u

SPARK_SUBMIT="${SPARK_SUBMIT:-/opt/bitnami/spark/bin/spark-submit}"
MASTER="${SPARK_MASTER_URL:-spark://spark-master:7077}"
JOBS_DIR="${SPARK_JOBS_DIR:-/opt/spark-jobs}"
INTERVAL="${BATCH_INTERVAL_SECONDS:-3600}"
SPARK_PACKAGES="${SPARK_PACKAGES:-org.apache.iceberg:iceberg-spark-runtime-3.5_2.12:1.6.1,org.apache.iceberg:iceberg-aws-bundle:1.6.1}"
# Order matters: silver first, then the gold/CAC/Sedona consumers of silver.
# receipt_recon_gold.py (SPEC-W62) reads bronze directly; it degrades
# gracefully (log + skip, exit 0) until the receipt bronze producers land.
BATCH_JOBS="${BATCH_JOBS:-silver_clean_bookings.py silver_clean_transcripts.py cac_analytics.py revenue_intelligence.py geo_analytics.py receipt_recon_gold.py}"

log() { echo "[spark-scheduler $(date -u +%FT%TZ)] $*"; }

run_pass() {
    local rc=0 job
    for job in $BATCH_JOBS; do
        if [ ! -f "$JOBS_DIR/$job" ]; then
            log "WARN: $JOBS_DIR/$job missing — skipped"
            rc=1
            continue
        fi
        log "submitting $job"
        if "$SPARK_SUBMIT" --master "$MASTER" --packages "$SPARK_PACKAGES" "$JOBS_DIR/$job"; then
            log "$job OK"
        else
            # Never abort the loop on a failed job: the next interval retries
            # (jobs are idempotent) and one broken job must not starve others.
            log "WARN: $job FAILED (exit $?) — will retry next pass"
            rc=1
        fi
    done
    return $rc
}

log "starting: master=$MASTER jobs_dir=$JOBS_DIR interval=${INTERVAL}s jobs: $BATCH_JOBS"
while true; do
    run_pass || true
    log "pass complete; sleeping ${INTERVAL}s"
    sleep "$INTERVAL"
done
