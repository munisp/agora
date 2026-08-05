FROM python:3.12-slim

WORKDIR /srv
COPY pyproject.toml README.md ./
COPY app ./app
RUN pip install --no-cache-dir .

ENV PORT=7014 HOST=0.0.0.0
EXPOSE 7014
CMD ["python", "-m", "app.main"]
