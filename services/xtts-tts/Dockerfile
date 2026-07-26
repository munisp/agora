# XTTS-TTS sidecar (SPEC-W10 Part B) — voice-cloning TTS.
# Default build = MOCK (XTTS_MOCK=1): deterministic voice-keyed sine wav, no
# GPU, no model download. Real Coqui XTTS-v2 inference (XTTS_MOCK=0) needs a
# CUDA GPU with >=6GB VRAM — CPU inference is not viable (minutes per short
# utterance). See the CUDA variant block below and docs/voices-sidecars.md.
#
# Run:     docker build -t opendesk/xtts-tts:dev services/xtts-tts
# Compose: infra/compose/voices.compose.yml

FROM python:3.11-slim

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    XTTS_MOCK=1 \
    VOICES_DIR=/data \
    PORT=5810

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY app/ /app/app/

EXPOSE 5810
VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD python -c "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://localhost:5810/healthz', timeout=3).status==200 else 1)"

CMD ["python", "-m", "app.main"]

# ---------------------------------------------------------------------------
# CUDA variant (real XTTS-v2 inference) — ready-to-build alternative.
#
#   1. Swap the base image (torch+CUDA preinstalled):
#        FROM pytorch/pytorch:2.5.1-cuda12.4-cudnn9-runtime
#   2. After requirements.txt, install the GPU extras:
#        COPY requirements-gpu.txt .
#        RUN pip install --no-cache-dir -r requirements-gpu.txt
#   3. Optionally pre-download the checkpoint (~1.8GB) at build time so cold
#      starts are fast (otherwise it downloads on first synthesis into the
#      container layer — mount TTS_HOME on a volume to persist it):
#        ENV TTS_HOME=/models
#        RUN python -c "from TTS.api import TTS; TTS('tts_models/multilingual/multi-dataset/xtts_v2')"
#   4. Run with GPU access: `docker run --gpus all ...` or uncomment the
#      deploy.resources.reservations.devices block in
#      infra/compose/voices.compose.yml, and set XTTS_MOCK=0.
# ---------------------------------------------------------------------------
