# Piper HTTP sidecar for voice-agent-runtime (TTS).
FROM python:3.12-slim AS base

RUN pip install --no-cache-dir piper-tts==1.3.* fastapi==0.115.* uvicorn==0.30.*

# Voice models are downloaded at container start (see compose fragment /
# README), not baked into the image:
#   python -m piper.download_voices en_US-lessac-medium --download-dir /voices
RUN mkdir -p /voices
VOLUME ["/voices"]

WORKDIR /opt/sidecar
COPY piper_server.py /opt/sidecar/piper_server.py

EXPOSE 5500
ENV PIPER_MODEL_DIR=/voices \
    PIPER_VOICE=en_US-lessac-medium \
    PORT=5500

ENTRYPOINT ["python", "/opt/sidecar/piper_server.py"]
