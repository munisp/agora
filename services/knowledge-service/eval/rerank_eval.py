"""Reranker evaluation harness (SPEC-W113).

Measures what the cross-encoder rerank stage actually buys on a golden set
of small-business-domain queries (hours, refunds, delivery, pricing,
bookings — the Agora tenant corpus profile):

    bi-encoder retrieval order (before)  vs  reranked order (after)
    -> Recall@k and MRR@k per query, then means; JSON report.

Runs OFFLINE against the local HF cache (the Docker image pre-downloads
both models; runtime never needs network). When the models are unavailable
the harness prints SKIP with the reason and exits 0 — it never fakes a
number. `--require` exits 1 on SKIP so CI can gate on a real run.

    python3 eval/rerank_eval.py [--k 5] [--out report.json] [--require]

The metric functions (recall_at_k, mrr_at_k) and the golden set are pure
module data — unit-tested hermetically without models.
"""

from __future__ import annotations

import argparse
import json
import math
import sys

# ---------------------------------------------------------------------------
# Golden set: (chunk_id, text) corpus + queries with relevant chunk ids.
# Deliberately adversarial for BM25/bi-encoders: relevant chunks paraphrase
# the query instead of repeating its terms; distractors share vocabulary.
# ---------------------------------------------------------------------------
CORPUS: list[tuple[str, str]] = [
    ("c-hours-1", "Our workshop is open Monday to Saturday, 8am to 6pm."),
    ("c-hours-2", "Sunday closures apply to all branches; public holidays too."),
    ("c-hours-3", "You can reach the front desk during working hours."),
    ("c-refund-1", "Customers may return goods within 14 days for a full refund."),
    ("c-refund-2", "Refunds are processed to the original payment method."),
    ("c-refund-3", "Sale items can be exchanged but not returned for cash."),
    ("c-delivery-1", "We dispatch orders within Lagos in 24 hours."),
    ("c-delivery-2", "Interstate delivery takes 3 to 5 working days by road."),
    ("c-delivery-3", "A tracking number is sent by SMS once the rider picks up."),
    ("c-pricing-1", "The standard service call-out fee is five thousand naira."),
    ("c-pricing-2", "Bulk orders above fifty units attract a ten percent discount."),
    ("c-pricing-3", "Invoices are payable within thirty days of issue."),
    ("c-booking-1", "Appointments can be rescheduled up to four hours ahead."),
    ("c-booking-2", "Walk-in clients are served after booked appointments."),
    ("c-booking-3", "A deposit of twenty percent confirms your reservation."),
    ("c-staff-1", "All technicians are certified and carry company ID cards."),
    ("c-staff-2", "Our support team speaks English, Yoruba and Igbo."),
    ("c-pay-1", "We accept bank transfer, card, and USSD payment."),
    ("c-pay-2", "Cash on delivery is available within Lagos only."),
    ("c-warranty-1", "Repairs carry a ninety-day workmanship warranty."),
]

QUERIES: list[tuple[str, set[str]]] = [
    # (query, relevant chunk ids) — paraphrase-heavy on purpose
    ("what time do you open", {"c-hours-1"}),
    ("are you open on Sundays", {"c-hours-2"}),
    ("can I get my money back", {"c-refund-1", "c-refund-2"}),
    ("how long does shipping take outside Lagos", {"c-delivery-2"}),
    ("how will I know when my order is on the way", {"c-delivery-3"}),
    ("how much does a visit cost", {"c-pricing-1"}),
    ("do you give discounts for large orders", {"c-pricing-2"}),
    ("can I move my appointment", {"c-booking-1"}),
    ("what payment methods do you take", {"c-pay-1", "c-pay-2"}),
    ("is your repair work guaranteed", {"c-warranty-1"}),
]


# ---------------------------------------------------------------------------
# Pure metric functions (unit-tested)
# ---------------------------------------------------------------------------
def recall_at_k(ranked_ids: list[str], relevant: set[str], k: int) -> float:
    """Fraction of relevant ids appearing in the top-k of ranked_ids."""
    if not relevant:
        return 0.0
    top = ranked_ids[:k]
    return sum(1 for r in relevant if r in top) / len(relevant)


def mrr_at_k(ranked_ids: list[str], relevant: set[str], k: int) -> float:
    """Reciprocal rank of the FIRST relevant hit within top-k (0 if none)."""
    for i, rid in enumerate(ranked_ids[:k], start=1):
        if rid in relevant:
            return 1.0 / i
    return 0.0


def _cosine(a: list[float], b: list[float]) -> float:
    dot = sum(x * y for x, y in zip(a, b))
    na = math.sqrt(sum(x * x for x in a)) or 1.0
    nb = math.sqrt(sum(x * x for x in b)) or 1.0
    return dot / (na * nb)


def evaluate(before: dict[str, list[str]], after: dict[str, list[str]],
             k: int) -> dict:
    """Aggregate metrics for two rankings keyed by query string."""
    out = {"k": k, "queries": len(QUERIES), "before": {"recall": 0.0, "mrr": 0.0},
           "after": {"recall": 0.0, "mrr": 0.0}, "per_query": []}
    for q, relevant in QUERIES:
        b_ids, a_ids = before[q], after[q]
        row = {
            "query": q,
            "before": {"recall": recall_at_k(b_ids, relevant, k),
                       "mrr": mrr_at_k(b_ids, relevant, k)},
            "after": {"recall": recall_at_k(a_ids, relevant, k),
                      "mrr": mrr_at_k(a_ids, relevant, k)},
        }
        out["per_query"].append(row)
        out["before"]["recall"] += row["before"]["recall"]
        out["before"]["mrr"] += row["before"]["mrr"]
        out["after"]["recall"] += row["after"]["recall"]
        out["after"]["mrr"] += row["after"]["mrr"]
    n = len(QUERIES)
    for side in ("before", "after"):
        out[side]["recall"] = round(out[side]["recall"] / n, 4)
        out[side]["mrr"] = round(out[side]["mrr"] / n, 4)
    out["uplift"] = {
        "recall": round(out["after"]["recall"] - out["before"]["recall"], 4),
        "mrr": round(out["after"]["mrr"] - out["before"]["mrr"], 4),
    }
    return out


def run(k: int, embed_model: str, rerank_model: str,
        candidates: int) -> dict:
    """Real-model run against the local HF cache. Raises on missing models —
    main() converts that into an honest SKIP."""
    from sentence_transformers import CrossEncoder, SentenceTransformer

    bi = SentenceTransformer(embed_model, device="cpu")
    ce = CrossEncoder(rerank_model, device="cpu")

    ids = [cid for cid, _ in CORPUS]
    texts = [t for _, t in CORPUS]
    emb = bi.encode(texts, normalize_embeddings=True, show_progress_bar=False)

    before: dict[str, list[str]] = {}
    after: dict[str, list[str]] = {}
    for q, _rel in QUERIES:
        [qv] = bi.encode([q], normalize_embeddings=True,
                         show_progress_bar=False)
        scored = sorted(
            zip(ids, (_cosine(qv, list(v)) for v in emb)),
            key=lambda kv: -kv[1],
        )
        pool = scored[:candidates]
        before[q] = [cid for cid, _ in pool]
        pool_texts = [texts[ids.index(cid)] for cid, _ in pool]
        ce_scores = ce.predict([(q, t) for t in pool_texts],
                               show_progress_bar=False)
        reranked = sorted(zip((cid for cid, _ in pool), ce_scores),
                          key=lambda kv: -float(kv[1]))
        # candidates not in the pool keep retrieval order after the pool
        after[q] = [cid for cid, _ in reranked] + [
            cid for cid, _ in scored[candidates:]
        ]
    report = evaluate(before, after, k)
    report["models"] = {"bi_encoder": embed_model, "cross_encoder": rerank_model}
    report["candidates"] = candidates
    return report


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--k", type=int, default=5)
    ap.add_argument("--out", default="")
    ap.add_argument("--require", action="store_true",
                    help="exit 1 when models are unavailable (CI gate)")
    ap.add_argument("--embed-model", default="all-MiniLM-L6-v2")
    ap.add_argument("--rerank-model",
                    default="cross-encoder/ms-marco-MiniLM-L-6-v2")
    ap.add_argument("--candidates", type=int, default=10)
    args = ap.parse_args()

    try:
        report = run(args.k, args.embed_model, args.rerank_model,
                     args.candidates)
    except Exception as exc:  # noqa: BLE001 - honest SKIP, never fake
        print(f"SKIP: rerank eval could not run: {exc}")
        return 1 if args.require else 0

    payload = json.dumps(report, indent=2)
    print(payload)
    if args.out:
        with open(args.out, "w") as f:
            f.write(payload + "\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
