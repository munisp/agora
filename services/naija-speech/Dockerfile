# Agora naija-speech on ghana (GB10, ARM, CUDA 13). Built FROM the agora-xtts image: it already carries NVIDIA's
# PyTorch + vLLM stack and a torchaudio built against it (pip wheels would replace the CUDA torch).
# Packages are added with --no-deps for the same reason; versions are the ones verified on ghana 2026-11-16.
# Model licences: YarnGPT2 Apache-2.0; WavTokenizer MIT; MMS (ASR + pcm/ful TTS) CC-BY-NC 4.0.
FROM agora-xtts:5
RUN pip install --no-cache-dir --no-deps "outetts==0.2.3" "uroman==1.3.1.1" "inflect==7.6.0" "loguru==0.8.0" \
        "mecab==0.996.15" "whisper==1.1.10" "sounddevice==0.5.6" \
    && git clone -q https://github.com/saheedniyi02/yarngpt.git /opt/yarngpt-src \
    && git -C /opt/yarngpt-src checkout -q 8bb0eb27d307f4b5903149e24ecc1bd3caa4af2d \
    && mkdir -p /opt/pkg && ln -s /opt/yarngpt-src /opt/pkg/yarngpt
WORKDIR /srv
COPY app/ /srv/app/
ENV PYTHONPATH=/opt/pkg PYTHONUNBUFFERED=1 PORT=5820 HF_HOME=/root/.cache/huggingface \
    WAVTOKENIZER_CKPT=/models/wavtokenizer_large_speech_320_24k.ckpt
EXPOSE 5820
CMD ["python", "-m", "app.main"]
