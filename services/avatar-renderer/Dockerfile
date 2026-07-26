# Avatar renderer sidecar (SPEC-W9 Part A / A3).
# Default build = MOCK renderer: no GPU, publishes a warm test pattern so the
# discovery -> join -> publish -> browser-tile pipeline is e2e-testable.
#
# Run:  docker build -t opendesk/avatar-renderer:dev services/avatar-renderer
# Compose override: infra/compose/avatar.compose.yml

FROM python:3.11-slim

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    AVATAR_RENDERER_MODE=mock

WORKDIR /app

# LiveKit room client + server API (room discovery). Version set mirrors the
# voice-agent-runtime constraint family (livekit-api 0.6.x).
RUN pip install --no-cache-dir "livekit-api>=0.6,<0.7" "livekit>=0.17,<1"

# ---------------------------------------------------------------------------
# MuseTalk enablement (AVATAR_RENDERER_MODE=musetalk) — GPU REQUIRED.
# Real-time MuseTalk lip-sync inference needs a CUDA GPU (see docs/avatar.md
# "GPU honesty"; CPU inference is far below the 15fps floor). Ready-to-uncomment
# steps, per the MuseTalk repo (https://github.com/TMElyralab/MuseTalk):
#
#   1. Swap the base image for a CUDA runtime, e.g.:
#        FROM pytorch/pytorch:2.3.1-cuda12.1-cudnn8-runtime
#   2. Clone MuseTalk + install its requirements (pin a commit for builds):
#        RUN apt-get update && apt-get install -y --no-install-recommends git ffmpeg \
#            && rm -rf /var/lib/apt/lists/*
#        RUN git clone --depth 1 https://github.com/TMElyralab/MuseTalk /opt/musetalk
#        RUN pip install --no-cache-dir -r /opt/musetalk/requirements.txt
#   3. Download model weights per the MuseTalk README (models/ tree) to /models
#      ( bake into the image or mount a volume):
#        VOLUME /models
#   4. Implement MuseTalkRenderer.next_frame in app/main.py (audio buffer ->
#      MuseTalk inference -> RGB24 frames) and set AVATAR_RENDERER_MODE=musetalk.
#   5. Run the container with GPU access: `docker run --gpus all ...` or the
#      compose `deploy.resources.reservations.devices` block (commented in
#      infra/compose/avatar.compose.yml).
# ---------------------------------------------------------------------------

COPY app/ /app/app/

CMD ["python", "-m", "app.main"]

