FROM python:3.11-slim

WORKDIR /srv
COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt
COPY fraud_engine ./fraud_engine

# Internal-only service (SPEC-W30 §1: :7017, no APISIX route).
ENV PORT=7017 HOST=0.0.0.0 PYTHONUNBUFFERED=1
EXPOSE 7017
CMD ["python", "-m", "fraud_engine.main"]
