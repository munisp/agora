#!/bin/bash
# One pass of the lakehouse batch jobs in Spark local mode (same order as ../run-scheduled.sh).
# BATCH_JOBS: space-separated job files; a failed job is logged and the pass continues (exit 1 at the end).
set -u
JOBS="${BATCH_JOBS:-silver_clean_bookings.py revenue_intelligence.py}"
rc=0
for job in $JOBS; do
  echo "[lakehouse-batch $(date -u +%FT%TZ)] running $job"
  if spark-submit --master "local[${SPARK_CORES:-2}]" --driver-memory "${SPARK_DRIVER_MEMORY:-900m}" \
       --conf spark.ui.enabled=false --conf spark.jars.ivy=/tmp/.ivy \
       --conf spark.sql.extensions=org.apache.iceberg.spark.extensions.IcebergSparkSessionExtensions \
       "/opt/spark-jobs/$job"; then
    echo "[lakehouse-batch] $job OK"
  else
    echo "[lakehouse-batch] $job FAILED"; rc=1
  fi
done
exit $rc
