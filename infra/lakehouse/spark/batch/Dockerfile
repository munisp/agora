# Lakehouse batch image (2026-11-21): the scheduled Spark jobs in LOCAL mode, for clusters without a Spark master
# (k8s CronJob). Python 3.11 (the jobs use datetime.UTC) + Java 17 + PySpark 3.5.3 (bundles Spark); the Iceberg runtime
# and AWS bundle are baked into PySpark's jars, so a run never downloads --packages from Maven.
# Build context: infra/lakehouse/spark
FROM python:3.11-slim-bookworm
RUN apt-get update && apt-get install -y --no-install-recommends openjdk-17-jre-headless procps \
    && rm -rf /var/lib/apt/lists/* \
    && pip install --no-cache-dir pyspark==3.5.3 \
    && useradd -u 185 -m spark
ARG JARS=/usr/local/lib/python3.11/site-packages/pyspark/jars
ADD https://repo1.maven.org/maven2/org/apache/iceberg/iceberg-spark-runtime-3.5_2.12/1.6.1/iceberg-spark-runtime-3.5_2.12-1.6.1.jar ${JARS}/
ADD https://repo1.maven.org/maven2/org/apache/iceberg/iceberg-aws-bundle/1.6.1/iceberg-aws-bundle-1.6.1.jar ${JARS}/
RUN chmod 0644 ${JARS}/iceberg-*.jar
COPY jobs/ /opt/spark-jobs/
COPY batch/run-batch.sh /opt/run-batch.sh
RUN chmod 0755 /opt/run-batch.sh
ENV PYSPARK_PYTHON=python3.11 PYSPARK_DRIVER_PYTHON=python3.11 JAVA_HOME=/usr/lib/jvm/java-17-openjdk-amd64
USER 185
ENTRYPOINT ["/opt/run-batch.sh"]
