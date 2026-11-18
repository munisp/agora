# CosyVoice-TTS sidecar (SPEC-W72 A1) — consent-gated voice-cloning TTS.
# Default build = MOCK (COSYVOICE_MOCK=1): deterministic voice-keyed sine wav,
# no GPU, no model download. Real CosyVoice-2 zero-shot inference
# (COSYVOICE_MOCK=0) needs a CUDA GPU with >=8GB VRAM — CPU inference is not
# viable. See the CUDA variant block below and docs/voices-sidecars.md.
#
# Run:     docker build -t opendesk/cosyvoice-tts:dev services/cosyvoice-tts
# Compose: services/voice-agent-runtime/docker-compose.fragment.yml

FROM python:3.11-slim

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    COSYVOICE_MOCK=1 \
    VOICES_DIR=/data \
    PORT=5820

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY app/ /app/app/

EXPOSE 5820
VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD python -c "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://localhost:5820/healthz', timeout=3).status==200 else 1)"

CMD ["python", "-m", "app.main"]

# ---------------------------------------------------------------------------
# CUDA variant (real CosyVoice-2 inference) — ready-to-build alternative.
# CosyVoice-2 (FunAudioLLM/CosyVoice) is Apache-2.0 — commercial-safe, unlike
# the Coqui XTTS-v2 weights (see docs/voice-licensing.md).
#
#   1. Swap the base image (torch+CUDA preinstalled):
#        FROM pytorch/pytorch:2.5.1-cuda12.4-cudnn9-runtime
#   2. After requirements.txt, install the GPU extras:
#        COPY requirements-gpu.txt .
#        RUN pip install --no-cache-dir -r requirements-gpu.txt
#   3. Fetch the CosyVoice2-0.5B checkpoint (~2GB) at build time so cold
#      starts are fast (otherwise it must be mounted/downloaded into the
#      container — mount COSYVOICE_MODEL_DIR on a volume to persist it):
#        ENV COSYVOICE_MODEL_DIR=/models/CosyVoice2-0.5B
#        RUN python -c "from modelscope import snapshot_download; snapshot_download('iic/CosyVoice2-0.5B', local_dir='/models/CosyVoice2-0.5B')"
#   4. Run with GPU access: `docker run --gpus all ...` and set
#      COSYVOICE_MOCK=0.
# ---------------------------------------------------------------------------
