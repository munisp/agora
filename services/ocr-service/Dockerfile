# syntax=docker/dockerfile:1

# ---- build stage: create venv, install pinned deps (no model weights are
# ---- baked in — MiniCPM-OCR GGUFs live on the ocr-models volume attached
# ---- to the llama-ocr sidecar, NOT in this image; see README runbook).
FROM python:3.12-slim AS builder

ENV PIP_NO_CACHE_DIR=1 \
    PYTHONDONTWRITEBYTECODE=1

RUN python -m venv /opt/venv
ENV PATH="/opt/venv/bin:$PATH"

COPY requirements.txt /src/requirements.txt
RUN pip install --upgrade pip==24.3.1 && pip install -r /src/requirements.txt

# ---- runtime stage
FROM python:3.12-slim

# poppler-utils: pdftoppm rasterizes PDF pages for the vision sidecar.
# Slim-image impact (I5): poppler-utils --no-install-recommends adds ~35 MB
# (poppler itself + libpng/libjpeg/fontconfig deps) on top of the ~45 MB
# python:3.12-slim base — acceptable; a full OCR stack in-image (torch/
# transformers, several GB) is exactly what the sidecar topology avoids.
RUN apt-get update \
    && apt-get install -y --no-install-recommends poppler-utils \
    && rm -rf /var/lib/apt/lists/*

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PATH="/opt/venv/bin:$PATH" \
    PORT=7020

COPY --from=builder /opt/venv /opt/venv
COPY app /app/app

RUN useradd --create-home --uid 10001 opendesk && chown -R opendesk /app
USER opendesk
WORKDIR /app

EXPOSE 7020
HEALTHCHECK --interval=15s --timeout=3s --retries=5 \
  CMD python -c "import urllib.request,sys;sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:7020/healthz').status<500 else 1)"

CMD ["python", "-m", "uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "7020"]
