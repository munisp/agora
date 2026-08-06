# credit-bureau — learned + rule credit scoring (SPEC-W33 §3 B2).
# Rules-only base image: NO torch (that is the requirements-ml.txt
# overlay, mirroring services/graph-ml; invariant I5).
FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1

WORKDIR /app

COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt

COPY credit_bureau ./credit_bureau

# Non-root runtime.
RUN useradd --create-home --uid 10002 creditbureau && mkdir -p /app/models && chown -R creditbureau /app
USER creditbureau

ENV PORT=7017 \
    CREDIT_ML_REGISTRY_DIR=

EXPOSE 7017

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD python -c "import urllib.request,os;urllib.request.urlopen(f\"http://127.0.0.1:{os.getenv('PORT','7017')}/healthz\",timeout=4)"

CMD ["python", "-m", "uvicorn", "credit_bureau.main:app", "--host", "0.0.0.0", "--port", "7017"]
