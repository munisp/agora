# MMS-TTS sidecar (SPEC-W10 Part B).
# Default runtime = MOCK (MMS_MOCK=1): deterministic sine wav, zero model
# downloads. Real inference (MMS_MOCK=0) lazy-loads facebook/mms-tts-{lang}
# per language, gated by MMS_LANGS (default eng,pcm), cached on the
# mms-models volume at HF_HOME=/models (~100-250MB per checkpoint).
#
# Run:     docker build -t opendesk/mms-tts:dev services/mms-tts
# Compose: infra/compose/voices.compose.yml

FROM python:3.11-slim

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    MMS_MOCK=1 \
    MMS_LANGS=eng,pcm \
    HF_HOME=/models \
    PORT=5800

WORKDIR /app

# CPU-only torch first (avoids the multi-GB CUDA wheels).
RUN pip install --no-cache-dir torch==2.5.1 --index-url https://download.pytorch.org/whl/cpu

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY app/ /app/app/

EXPOSE 5800
VOLUME ["/models"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD python -c "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://localhost:5800/healthz', timeout=3).status==200 else 1)"

CMD ["python", "-m", "app.main"]
