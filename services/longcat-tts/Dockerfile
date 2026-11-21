# LongCat-TTS sidecar (SPEC-W102 C1) — consent-gated voice-cloning TTS.
# Default build = MOCK (LONGCAT_MOCK=1): deterministic voice-keyed sine wav,
# no GPU, no model download. Real LongCat-AudioDiT zero-shot inference
# (LONGCAT_MOCK=0) needs a CUDA GPU with >=8GB VRAM (1B variant) — CPU
# inference is not viable. See the CUDA variant block below and
# docs/voices-sidecars.md.
#
# Run:     docker build -t opendesk/longcat-tts:dev services/longcat-tts
# Compose: services/voice-agent-runtime/docker-compose.fragment.yml

FROM python:3.11-slim

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    LONGCAT_MOCK=1 \
    VOICES_DIR=/data \
    PORT=5830

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY app/ /app/app/

EXPOSE 5830
VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD python -c "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://localhost:5830/healthz', timeout=3).status==200 else 1)"

CMD ["python", "-m", "app.main"]

# ---------------------------------------------------------------------------
# CUDA variant (real LongCat-AudioDiT inference) — ready-to-build alternative.
# LongCat-AudioDiT (meituan-longcat) is MIT-licensed for BOTH code and
# weights — commercial-safe like CosyVoice-2 (docs/voice-licensing.md).
#
#   1. Swap the base image (torch+CUDA preinstalled):
#        FROM pytorch/pytorch:2.5.1-cuda12.4-cudnn9-runtime
#   2. After requirements.txt, install the GPU extras:
#        COPY requirements-gpu.txt .
#        RUN pip install --no-cache-dir -r requirements-gpu.txt
#   3. Pre-pull the 1B checkpoint (~2.5GB) at build time for fast cold starts
#      (or mount LONGCAT_MODEL_DIR on a volume):
#        ENV LONGCAT_MODEL_DIR=/models/LongCat-AudioDiT-1B
#        RUN python -c "from huggingface_hub import snapshot_download; snapshot_download('meituan-longcat/LongCat-AudioDiT-1B', local_dir='/models/LongCat-AudioDiT-1B')"
#   4. Run with GPU access: `docker run --gpus all ...` and set
#      LONGCAT_MOCK=0.
# ---------------------------------------------------------------------------
