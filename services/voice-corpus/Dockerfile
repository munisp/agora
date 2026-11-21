# voice-corpus (SPEC-W103) — commissioned native-speaker corpus registry.
# Pure data infrastructure: FastAPI + stdlib only, NO model code, NO GPU.
FROM python:3.11-slim

WORKDIR /srv

COPY requirements.txt /srv/requirements.txt
RUN pip install --no-cache-dir -r /srv/requirements.txt

COPY app /srv/app

ENV CORPUS_DIR=/data \
    CORPUS_LANGUAGES=pcm,yo,ha,ig,en \
    PORT=5840

VOLUME ["/data"]
EXPOSE 5840

HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
  CMD python -c "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://localhost:5840/healthz', timeout=3).status==200 else 1)"

CMD ["python", "-m", "app.main"]
