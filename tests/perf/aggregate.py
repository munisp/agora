#!/usr/bin/env python3
"""tests/perf/aggregate.py — performance budget aggregator.

Reads:
  * funds-e2e HTTP timings JSON (tests/funds-e2e/harness.py output:
    {"calls": [{"call": ..., "ms": ..., "status": ...}, ...]}) — pass one or
    more via --timings;
  * OPTIONAL Go benchmark output (`go test -bench=. -benchmem` text,
    `BenchmarkX-N  iters  ns/op  ...` lines) via --bench.

Computes p50/p99/min/max per budgeted call and writes tests/perf/RESULTS.md
with a per-line verdict: PASS / FAIL / NOT-MEASURED, plus one ATOMIC
overall verdict (FAIL if any measured line fails).

Budgets come from docs/performance-budgets.md when present (markdown table
rows: `| <metric> | ... | p99 <= <ms> ms |` — first number after "p99" in a
row wins); when the file or a metric row is absent the SPEC-W41 fallback
budgets below apply (and the source is labeled in the output).

Budgeted calls (SPEC-W41-5):
  booking create        p99 <= 1500 ms
  invoice generate      p99 <= 1500 ms
  paystack webhook      p99 <=  500 ms
  deposit hold/capture  p99 <=  300 ms

Usage:
  python3 tests/perf/aggregate.py \
      --timings /tmp/funds-e2e/timings/funds-e2e-timings.json \
      [--bench /tmp/booking-bench.txt] [--out tests/perf/RESULTS.md]
Exit code: 0 when no measured line exceeds budget (NOT-MEASURED lines do
not fail the run but are listed); 1 otherwise.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from datetime import datetime, timezone
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]

# SPEC-W41-5 fallback budgets (used when docs/performance-budgets.md is
# absent or lacks the metric row). ms.
FALLBACK_BUDGETS_MS = {
    "booking_create": 1500.0,
    "invoice_generate": 1500.0,
    "paystack_webhook": 500.0,
    "deposit_hold": 300.0,
    "deposit_capture": 300.0,
}

# Budget metric -> timings call names (harness) and/or go-benchmark name.
METRIC_SOURCES = {
    "booking_create": {
        "timings": ["booking.public_create_booking"],
        "bench": ["BenchmarkCreateBookingTx"],
    },
    "invoice_generate": {
        "timings": ["billing.invoice_generate"],
        "bench": [],
    },
    "paystack_webhook": {
        "timings": ["billing.webhook_paystack", "billing.webhook_replay"],
        "bench": [],
    },
    "deposit_hold": {
        "timings": ["payments.hold", "payments.hold_replay"],
        "bench": [],
    },
    "deposit_capture": {
        "timings": ["payments.capture", "payments.capture_replay"],
        "bench": [],
    },
}

# Human labels for RESULTS.md.
METRIC_LABELS = {
    "booking_create": "booking create (HTTP public create, else store bench)",
    "invoice_generate": "invoice generate (POST /v1/invoices/generate)",
    "paystack_webhook": "paystack webhook (POST /webhooks/paystack)",
    "deposit_hold": "deposit hold (POST /v1/deposits)",
    "deposit_capture": "deposit capture (POST /v1/deposits/{id}/capture)",
}


def percentile(sorted_vals: list[float], pct: float) -> float:
    """Nearest-rank percentile (deterministic, no interpolation)."""
    if not sorted_vals:
        raise ValueError("empty")
    k = max(1, int(round((pct / 100.0) * len(sorted_vals) + 0.4999)))
    return sorted_vals[min(k, len(sorted_vals)) - 1]


def load_timings(paths: list[str]) -> list[dict]:
    calls: list[dict] = []
    for p in paths:
        data = json.loads(Path(p).read_text())
        entries = data["calls"] if isinstance(data, dict) else data
        for e in entries:
            if isinstance(e.get("ms"), (int, float)) and e.get("status", 200) < 500:
                calls.append(e)
    return calls


BENCH_RE = re.compile(r"^(Benchmark\w+)-\d+\s+\d+\s+([\d.]+)\s+ns/op", re.M)


def load_bench_ms(paths: list[str]) -> dict[str, float]:
    """Benchmark name -> mean ms/op (go bench reports a mean; labeled as
    such — it is not a percentile)."""
    out: dict[str, float] = {}
    for p in paths:
        for name, ns in BENCH_RE.findall(Path(p).read_text()):
            out[name] = float(ns) / 1e6
    return out


def load_budgets(path: Path) -> tuple[dict[str, float], str]:
    if path.is_file():
        budgets: dict[str, float] = {}
        for line in path.read_text().splitlines():
            if not line.strip().startswith("|"):
                continue
            m = re.search(r"p99\s*(?:<=?|≤)?\s*([\d.]+)\s*ms", line, re.I)
            if not m:
                continue
            cells = [c.strip() for c in line.strip("|").split("|")]
            key = cells[0].lower().replace(" ", "_").replace("/", "_")
            for metric in FALLBACK_BUDGETS_MS:
                if metric in key or metric.replace("_", " ") in cells[0].lower():
                    budgets[metric] = float(m.group(1))
        merged = dict(FALLBACK_BUDGETS_MS)
        merged.update(budgets)
        return merged, f"{path} (merged over SPEC fallbacks)"
    return dict(FALLBACK_BUDGETS_MS), "SPEC-W41 fallback budgets (docs/performance-budgets.md absent)"


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--timings", action="append", default=[],
                    help="funds-e2e timings JSON (repeatable)")
    ap.add_argument("--bench", action="append", default=[],
                    help="go test -bench output file (repeatable)")
    ap.add_argument("--budgets", default=str(REPO_ROOT / "docs/performance-budgets.md"))
    ap.add_argument("--out", default=str(REPO_ROOT / "tests/perf/RESULTS.md"))
    args = ap.parse_args()

    calls = load_timings(args.timings) if args.timings else []
    bench = load_bench_ms(args.bench) if args.bench else {}
    budgets, budget_src = load_budgets(Path(args.budgets))

    lines = []
    any_fail = False
    for metric, label in METRIC_LABELS.items():
        budget = budgets[metric]
        samples = [c["ms"] for c in calls if c.get("call") in METRIC_SOURCES[metric]["timings"]]
        samples.sort()
        bench_note = ""
        for bname in METRIC_SOURCES[metric]["bench"]:
            if bname in bench:
                bench_note = f"; store bench {bname} mean {bench[bname]:.2f} ms/op"
        if not samples:
            verdict = "NOT-MEASURED"
            stats = "n=0"
            if bench_note:
                stats += bench_note + " (go-bench mean only; HTTP path not exercised)"
            else:
                stats += " (no samples — see funds-e2e RESULTS for why)"
        else:
            p50 = percentile(samples, 50)
            p99 = percentile(samples, 99)
            stats = (f"n={len(samples)} p50={p50:.1f} ms p99={p99:.1f} ms "
                     f"min={samples[0]:.1f} ms max={samples[-1]:.1f} ms"
                     + bench_note)
            if p99 <= budget:
                verdict = "PASS"
            else:
                verdict = "FAIL"
                any_fail = True
        lines.append(f"| {label} | {budget:.0f} ms | {stats} | **{verdict}** |")

    overall = "FAIL" if any_fail else "PASS"
    n_measured = sum(1 for l in lines if "**PASS**" in l or "**FAIL**" in l)
    doc = f"""# tests/perf — RESULTS (measured baseline vs budgets)

Generated: {datetime.now(timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')} by
`tests/perf/aggregate.py` — do not hand-edit; re-run the aggregator.

Budget source: {budget_src}
Timings inputs: {', '.join(args.timings) or '(none)'}
Bench inputs: {', '.join(args.bench) or '(none)'}

| metric | budget (p99) | measured | verdict |
|---|---|---|---|
{chr(10).join(lines)}

**ATOMIC overall verdict: {overall}** ({n_measured}/{len(METRIC_LABELS)}
budget lines had measurements; NOT-MEASURED lines are listed, not excused —
each names where the gap is documented).

## Environment caveat

These numbers come from a constrained sandbox (2 CPU / 4 GB, embedded
PostgreSQL on a local unix socket, services built in debug mode for the
Rust binaries, loopback HTTP, no TLS, no API gateway). They are an
INDICATIVE baseline that the flow works and where the time goes — they are
not production-representative. Re-run on production-shaped hardware before
using them for capacity decisions (see docs/runbooks/capacity-planning.md).
"""
    out = Path(args.out)
    out.write_text(doc)
    print(f"wrote {out} ({len(doc)} bytes)")
    print(f"ATOMIC: {overall}; measured={n_measured}/{len(METRIC_LABELS)}")
    return 1 if any_fail else 0


if __name__ == "__main__":
    sys.exit(main())
