# graph-ml — per-tenant propensity scoring & recommendations (SPEC-W29 §3 WS-A).
# Heuristic-only base image: NO torch/torch-geometric (those are the optional
# requirements-gnn.txt overlay for a GPU variant).
FROM python:3.11-slim

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1

WORKDIR /app

COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt

COPY graph_ml ./graph_ml

# Non-root runtime.
RUN useradd --create-home --uid 10001 graphml && mkdir -p /app/models && chown -R graphml /app
USER graphml

ENV PORT=7016 \
    GRAPH_ML_BACKEND=heuristic \
    GRAPH_ML_MODEL_DIR=/app/models

EXPOSE 7016

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD python -c "import urllib.request,os;urllib.request.urlopen(f\"http://127.0.0.1:{os.getenv('PORT','7016')}/healthz\",timeout=4)"

# HTTP API + interval scheduler. Cron-style batch alternative:
#   docker run --rm graph-ml python -m graph_ml.score
CMD ["python", "-m", "uvicorn", "graph_ml.main:app", "--host", "0.0.0.0", "--port", "7016"]
