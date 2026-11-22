"""Cross-encoder rerank stage (SPEC-W113).

The hybrid pipeline (BM25 + kNN + RRF) fuses RANKS but never re-scores
content against the query. A cross-encoder sees (query, chunk) TOGETHER and
produces a real relevance score, fixing paraphrase/terminology misses that
both legs get wrong. Model default: cross-encoder/ms-marco-MiniLM-L-6-v2
(22M params, CPU-feasible — matches the CPU-only deployment posture).

Mirrors the Embedder idiom: lazy load behind asyncio.Lock + to_thread,
device="cpu", HF cache dir from settings. No model load at import/startup.
Callers must treat reranking as best-effort: on any failure they keep the
RRF order (see main.py:_hybrid).
"""

from __future__ import annotations

import asyncio
import os
from typing import Any, Sequence

from .logging import get_logger

log = get_logger(__name__)


class CrossEncoderReranker:
    def __init__(
        self, model_name: str, cache_dir: str | None = None, max_chars: int = 2000
    ):
        self._model_name = model_name
        self._cache_dir = cache_dir
        self._max_chars = max_chars
        self._model = None
        self._lock = asyncio.Lock()

    def _load(self):  # blocking — run in a thread
        from sentence_transformers import CrossEncoder

        if self._cache_dir:
            os.environ.setdefault("SENTENCE_TRANSFORMERS_HOME", self._cache_dir)
        log.info("reranker_model.loading", model=self._model_name)
        return CrossEncoder(self._model_name, device="cpu")

    async def ensure_loaded(self) -> None:
        if self._model is not None:
            return
        async with self._lock:
            if self._model is None:
                self._model = await asyncio.to_thread(self._load)
                log.info("reranker_model.loaded", model=self._model_name)

    async def score(self, query: str, contents: Sequence[str]) -> list[float]:
        """One batched cross-encoder predict over (query, content) pairs."""
        await self.ensure_loaded()
        pairs = [(query, c[: self._max_chars]) for c in contents]
        scores = await asyncio.to_thread(
            self._model.predict, pairs, show_progress_bar=False
        )
        return [float(s) for s in scores]

    async def rerank(
        self, query: str, hits: list[dict[str, Any]]
    ) -> list[dict[str, Any]]:
        """Re-order hits by cross-encoder score, annotated `_rerank_score`.

        Stable: score ties keep the incoming (RRF) order. Input is not
        mutated; returned hits are copies.
        """
        if not hits:
            return []
        contents = [str(h["_source"].get("content", "")) for h in hits]
        scores = await self.score(query, contents)
        order = sorted(range(len(hits)), key=lambda i: -scores[i])
        out = []
        for i in order:
            hit = dict(hits[i])
            hit["_rerank_score"] = scores[i]
            out.append(hit)
        return out
