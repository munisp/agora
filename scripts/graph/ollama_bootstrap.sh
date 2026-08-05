#!/bin/sh
# ollama_bootstrap.sh — idempotent model bootstrap for the W28 graph stack
# (SPEC-W28 §1, WS-D). Pulls the models graph-service / graph-sync need:
#   * chat model  (default qwen2.5:7b-instruct) — NL->Cypher GraphRAG
#   * embed model (default nomic-embed-text)    — entity-resolution embeddings
#
# Idempotent: `ollama list` is checked first; models already present are
# skipped, so re-runs and container restarts are cheap. Safe to run against
# either the CPU service (default) or the GPU profile service — point
# OLLAMA_HOST at whichever one is up.
#
# Usage:
#   scripts/graph/ollama_bootstrap.sh                 # local ollama
#   OLLAMA_HOST=http://ollama:11434 scripts/graph/ollama_bootstrap.sh
#   docker compose -f docker-compose.yml -f infra/docker-compose.graph.yml \
#     up ollama-models                                # same script, in-cluster
#
# Env:
#   OLLAMA_HOST        (default http://127.0.0.1:11434)
#   OLLAMA_CHAT_MODEL  (default qwen2.5:7b-instruct)
#   OLLAMA_EMBED_MODEL (default nomic-embed-text)
#   BOOTSTRAP_RETRIES  (default 30) — server-readiness polls, 2s apart
set -eu

OLLAMA_HOST="${OLLAMA_HOST:-http://127.0.0.1:11434}"
CHAT_MODEL="${OLLAMA_CHAT_MODEL:-qwen2.5:7b-instruct}"
EMBED_MODEL="${OLLAMA_EMBED_MODEL:-nomic-embed-text}"
RETRIES="${BOOTSTRAP_RETRIES:-30}"

export OLLAMA_HOST

log() { printf '%s %s\n' "[ollama-bootstrap]" "$*"; }

# Wait for the server (compose also gates on the healthcheck; this makes the
# script robust when run standalone).
i=0
until ollama list >/dev/null 2>&1; do
    i=$((i + 1))
    if [ "$i" -ge "$RETRIES" ]; then
        log "ERROR: ollama server at $OLLAMA_HOST not ready after $((RETRIES * 2))s"
        exit 1
    fi
    log "waiting for ollama server at $OLLAMA_HOST ($i/$RETRIES)"
    sleep 2
done

pull_if_missing() {
    model="$1"
    # `ollama list` prints "NAME ID SIZE MODIFIED"; match the model name in
    # column 1 exactly (with or without an explicit :tag suffix match).
    if ollama list | awk 'NR>1 {print $1}' | grep -qx "$model"; then
        log "present: $model (skip)"
    else
        log "pulling: $model"
        ollama pull "$model"
        log "pulled:  $model"
    fi
}

pull_if_missing "$CHAT_MODEL"
pull_if_missing "$EMBED_MODEL"

log "done. Models available at $OLLAMA_HOST:"
ollama list
