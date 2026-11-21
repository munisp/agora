# syntax=docker/dockerfile:1

# ---- build stage: create venv, install pinned deps, pre-download the
# ---- embedding model so runtime never needs network access to HF Hub.
FROM python:3.12-slim AS builder

ENV PIP_NO_CACHE_DIR=1 \
    PYTHONDONTWRITEBYTECODE=1

RUN apt-get update && apt-get install -y --no-install-recommends build-essential \
    && rm -rf /var/lib/apt/lists/*

RUN python -m venv /opt/venv
ENV PATH="/opt/venv/bin:$PATH"

COPY pyproject.toml /src/pyproject.toml
RUN pip install --upgrade pip==24.3.1 && pip install /src

# Pre-download all-MiniLM-L6-v2 into a cache dir copied into the runtime image.
ENV SENTENCE_TRANSFORMERS_HOME=/models/hf
RUN python -c "from sentence_transformers import SentenceTransformer; SentenceTransformer('all-MiniLM-L6-v2', device='cpu')"
# SPEC-W113: pre-download the cross-encoder reranker (22M params, CPU) into
# the same cache — runtime reranking never needs HF Hub either.
RUN python -c "from sentence_transformers import CrossEncoder; CrossEncoder('cross-encoder/ms-marco-MiniLM-L-6-v2', device='cpu')"

# ---- runtime stage
FROM python:3.12-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PATH="/opt/venv/bin:$PATH" \
    SENTENCE_TRANSFORMERS_HOME=/models/hf \
    PORT=7008

COPY --from=builder /opt/venv /opt/venv
COPY --from=builder /models /models
COPY app /app/app
# SPEC-W113: reranker evaluation harness runnable in-container
# (python3 eval/rerank_eval.py --require as a release gate).
COPY eval /app/eval

RUN useradd --create-home --uid 10001 opendesk && chown -R opendesk /app /models
USER opendesk
WORKDIR /app

EXPOSE 7008
HEALTHCHECK --interval=15s --timeout=3s --retries=5 \
  CMD python -c "import urllib.request,sys;sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:7008/healthz').status<500 else 1)"

CMD ["python", "-m", "uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "7008"]
