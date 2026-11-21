# RAG reranker + retrieval evaluation (W113)

Hybrid retrieval (BM25 + kNN + RRF, SPEC §10) fuses **ranks** but never
re-scores content against the query. W113 adds a cross-encoder rerank stage
that lifts the retrieval-quality ceiling, plus the harness that measures it.

## Pipeline

```
query ──► BM25 leg (fetch max(k*2,10)) ─┐
        ► kNN leg  (fetch max(k*2,10)) ─┴► RRF fuse to candidate pool
                                          (max(k, RERANK_CANDIDATES)=20)
                                        ► cross-encoder rerank pool
                                        ► top k returned
```

- Model: `cross-encoder/ms-marco-MiniLM-L-6-v2` (22M params) — CPU-feasible
  by design; Agora targets CPU-only deployments in developing markets. The
  Docker image pre-downloads it alongside all-MiniLM-L6-v2, so runtime
  never needs HF Hub.
- Both `/v1/search` and `/v1/context` rerank, so chat/voice/USSD grounding
  (W108–W110) gets the better ordering transparently — and the W112
  grounding metrics (`miss`/`ungrounded` rates) will show it.
- `/v1/search` responses gain `"reranked": true|false` and a per-hit
  `rerank_score` (present only when reranked). `rrf_score` is unchanged.

## Failure posture (best-effort, like grounding itself)

The reranker runs under `RERANK_TIMEOUT_S` (default 1.0s). **Any** error or
timeout — model load failure, OOM, slow CPU — logs a warning and returns the
RRF order. Search never fails because of reranking; a broken reranker
degrades the platform to W112 behavior, honestly.

| Env | Default | Meaning |
| --- | --- | --- |
| `RERANK_ENABLED` | `on` | Master switch; `off` = byte-identical W112 behavior |
| `RERANK_MODEL` | `cross-encoder/ms-marco-MiniLM-L-6-v2` | Any sentence-transformers CrossEncoder |
| `RERANK_CANDIDATES` | `20` | Pool size the reranker re-scores |
| `RERANK_TIMEOUT_S` | `1.0` | Hard budget per query; exceeded → RRF fallback |
| `RERANK_MAX_CHARS` | `2000` | Per-chunk content ceiling sent to the model |

## Evaluation harness

`services/knowledge-service/eval/rerank_eval.py` measures the actual uplift
on a golden set of paraphrase-heavy small-business queries (hours, refunds,
delivery, pricing, bookings — the Agora tenant corpus profile): bi-encoder
retrieval order vs reranked order, **Recall@k and MRR@k**, JSON report.

```bash
# in the knowledge-service container (models pre-cached)
python3 eval/rerank_eval.py --out eval/rerank_report.json

# as a release gate (fails when models are unavailable instead of skipping)
python3 eval/rerank_eval.py --require
```

The harness SKIPs honestly (exit 0) when models are absent — it never
invents an uplift number. Metric functions are pure and unit-tested
hermetically in `tests/test_rerank.py`.

## Honest limits

- The cross-encoder improves *ranking* of what the legs retrieved; it cannot
  surface a chunk neither BM25 nor kNN returned. Corpus coverage still
  matters most (see `channel-grounding.md` honest limits).
- A domain/fine-tuned bi-encoder would shrink the candidate-pool miss rate;
  that needs tenant data and training budget — deferred.
- Latency: reranking adds up to `RERANK_TIMEOUT_S` per search/context call
  (typically ~50–150ms for 20 pairs on a modern CPU core).
