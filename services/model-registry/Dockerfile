# model-registry — model version registry + drift + A/B + continuous training
# (SPEC-W33 §4 C1/C2/C3/C5). I5 slim-image discipline: python:3.12-slim,
# NO torch / sklearn / pandas / MLflow; kafka-python deliberately excluded
# (producer is import-guarded — see model_registry/alerts.py).
FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1

WORKDIR /app

COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt

COPY model_registry ./model_registry

# Non-root runtime.
RUN useradd --create-home --uid 10001 modelreg && mkdir -p /data/manifests && chown -R modelreg /app /data
USER modelreg

ENV PORT=7019 \
    DRIFT_MANIFEST_DIR=/data/manifests

EXPOSE 7019

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD python -c "import urllib.request,os;urllib.request.urlopen(f\"http://127.0.0.1:{os.getenv('PORT','7019')}/healthz\",timeout=4)"

CMD ["python", "-m", "uvicorn", "model_registry.main:app", "--host", "0.0.0.0", "--port", "7019"]
