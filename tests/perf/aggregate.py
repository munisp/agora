#!/usr/bin/env python3
"""tests/perf/aggregate.py — performance budget aggregator (latency + throughput).

Reads:
  * funds-e2e HTTP timings JSON (tests/funds-e2e/harness.py output:
    {"calls": [{"call": ..., "ms": ..., "status": ...}, ...]}) — pass one or
    more via --timings (e.g. the sim-mode AND the TB-mode real-ledger run);
  * OPTIONAL Go benchmark output (`go test -bench=. -benchmem` text,
    `BenchmarkX-N  iters  ns/op  ...` lines) via --bench.

Budgets B1..B7 come from docs/performance-budgets.md (markdown table rows
`| B<n> | <operation> | <endpoint> | <= <p50> | <= <p99> | >= <rps> rps |`,
latency cells in ms or s). When the file or a row is absent/unparseable the
SPEC-W42 fallback budgets below apply (identical values) and the source is
labeled in the output.

Per budget line the aggregator computes, from the harness HTTP timings:
  * p50 / p99 latency (nearest-rank, deterministic) vs the latency budgets;
  * sustained throughput rps = N / wall, where wall is the summed measured
    duration of the N timed calls (sequential single-client throughput —
    the harness issues calls serially) vs the throughput budget;
and emits PASS/FAIL PER DIMENSION plus one line verdict (PASS only if every
measured dimension passes; NOT-MEASURED when the harness produced no samples
for the call — listed, never excused, with a pointer to why). Booking create
(B1/B2) becomes MEASURED-at-HTTP exactly when booking.create_authed /
booking.public_create_booking timings exist (harness v2, SPEC-W42: booking
write path runs via IDENTITY_BASE_URL direct-GET tenant resolution).

Usage:
  python3 tests/perf/aggregate.py \
      --timings /tmp/funds-e2e/timings/funds-e2e-timings.json \
      [--timings /tmp/funds-e2e-tb/timings/funds-e2e-timings.json] \
      [--bench /tmp/booking-bench.txt] [--out tests/perf/RESULTS.md]
Exit code: 0 when no measured line exceeds any budget dimension
(NOT-MEASURED lines do not fail the run but are listed); 1 otherwise.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from datetime import datetime, timezone
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]

# SPEC-W42 fallback budgets (identical to docs/performance-budgets.md
# B1..B7; used when the doc or a row is absent). Latencies in ms, rps is a
# minimum sustained throughput.
FALLBACK_BUDGETS = {
    "B1": {"p50": 300.0, "p99": 1500.0, "rps": 25.0},
    "B2": {"p50": 300.0, "p99": 1500.0, "rps": 25.0},
    "B3": {"p50": 300.0, "p99": 1500.0, "rps": 25.0},
    "B4": {"p50": 300.0, "p99": 1500.0, "rps": 25.0},
    "B5": {"p50": 150.0, "p99": 500.0, "rps": 100.0},
    "B6": {"p50": 100.0, "p99": 300.0, "rps": 100.0},
    "B7": {"p50": 100.0, "p99": 300.0, "rps": 100.0},
}

# Human labels for RESULTS.md (operation column of the budget table).
METRIC_LABELS = {
    "B1": "booking create, authenticated (POST /v1/bookings)",
    "B2": "public booking create (POST /public/sites/{slug}/bookings)",
    "B3": "invoice generate (POST /v1/invoices/generate)",
    "B4": "invoice issue (POST /v1/invoices/{id}/issue)",
    "B5": "paystack webhook (POST /webhooks/paystack)",
    "B6": "deposit hold (POST /v1/deposits)",
    "B7": "deposit capture (POST /v1/deposits/{id}/capture)",
}

# Budget id -> timings call names (harness) and/or go-benchmark name.
METRIC_SOURCES = {
    "B1": {"timings": ["booking.create_authed"], "bench": []},
    "B2": {"timings": ["booking.public_create_booking"], "bench": ["BenchmarkCreateBookingTx"]},
    "B3": {"timings": ["billing.invoice_generate"], "bench": []},
    "B4": {"timings": ["billing.invoice_issue"], "bench": []},
    "B5": {"timings": ["billing.webhook_paystack", "billing.webhook_replay"], "bench": []},
    "B6": {"timings": ["payments.hold", "payments.hold_replay"], "bench": []},
    "B7": {"timings": ["payments.capture", "payments.capture_replay"], "bench": []},
}

# Call names in the harness that exist only as correctness replays (not part
# of any budget line) — surfaced for transparency in the output footer.
NON_BUDGETED_PREFIXES = ("booking.create_authed_replay", "booking.public_create_booking_replay")


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


LAT_RE = re.compile(r"[≤<=]+\s*([\d.]+)\s*(ms|s)\b", re.I)
RPS_RE = re.compile(r"[≥>=]+\s*([\d.]+)\s*rps", re.I)


def parse_latency_ms(cell: str) -> float | None:
    m = LAT_RE.search(cell)
    if not m:
        return None
    v = float(m.group(1))
    return v * 1000.0 if m.group(2).lower() == "s" else v


def load_budgets(path: Path) -> tuple[dict[str, dict[str, float]], str]:
    """Parse the B1..B7 table rows of docs/performance-budgets.md:
    | B1 | <operation> | <endpoint> | <= p50 | <= p99 | >= rps rps | ... |
    Rows that do not parse fall back to FALLBACK_BUDGETS (merged)."""
    budgets = {k: dict(v) for k, v in FALLBACK_BUDGETS.items()}
    if not path.is_file():
        return budgets, "SPEC-W42 fallback budgets (docs/performance-budgets.md absent)"
    parsed = 0
    for line in path.read_text().splitlines():
        if not line.strip().startswith("|"):
            continue
        cells = [c.strip() for c in line.strip().strip("|").split("|")]
        if len(cells) < 6 or not re.fullmatch(r"B[1-7]", cells[0]):
            continue
        bid = cells[0]
        p50 = parse_latency_ms(cells[3])
        p99 = parse_latency_ms(cells[4])
        rps_m = RPS_RE.search(cells[5])
        if p50 is not None:
            budgets[bid]["p50"] = p50
        if p99 is not None:
            budgets[bid]["p99"] = p99
        if rps_m:
            budgets[bid]["rps"] = float(rps_m.group(1))
        parsed += 1
    if parsed:
        return budgets, f"{path} ({parsed}/7 budget rows parsed; merged over SPEC-W42 fallbacks)"
    return budgets, f"SPEC-W42 fallback budgets ({path} had no parseable B1..B7 rows)"


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--timings", action="append", default=[],
                    help="funds-e2e timings JSON (repeatable — e.g. sim + real-ledger runs)")
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
    for bid in sorted(METRIC_LABELS):
        label = METRIC_LABELS[bid]
        budget = budgets[bid]
        samples = [c["ms"] for c in calls if c.get("call") in METRIC_SOURCES[bid]["timings"]]
        samples.sort()
        bench_note = ""
        for bname in METRIC_SOURCES[bid]["bench"]:
            if bname in bench:
                bench_note = f"; store bench {bname} mean {bench[bname]:.2f} ms/op"
        budget_cell = f"p50 <= {budget['p50']:.0f} ms; p99 <= {budget['p99']:.0f} ms; >= {budget['rps']:.0f} rps"
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
            wall_s = sum(samples) / 1000.0
            # Sustained throughput: the harness issues these calls serially,
            # so rps = N / summed measured wall of the N calls (sequential
            # single-client throughput at the HTTP boundary).
            rps = len(samples) / wall_s if wall_s > 0 else float("inf")
            dims = []
            dims.append(("p50", p50 <= budget["p50"], f"p50={p50:.1f} ms <= {budget['p50']:.0f}"))
            dims.append(("p99", p99 <= budget["p99"], f"p99={p99:.1f} ms <= {budget['p99']:.0f}"))
            dims.append(("rps", rps >= budget["rps"], f"rps={rps:.1f} >= {budget['rps']:.0f}"))
            dim_str = "; ".join(f"{txt} {'PASS' if ok else 'FAIL'}" for _d, ok, txt in dims)
            stats = (f"n={len(samples)} min={samples[0]:.1f} ms max={samples[-1]:.1f} ms "
                     f"| {dim_str}" + bench_note)
            line_fail = any(not ok for _d, ok, _t in dims)
            verdict = "FAIL" if line_fail else "PASS"
            any_fail = any_fail or line_fail
        lines.append(f"| {bid} | {label} | {budget_cell} | {stats} | **{verdict}** |")

    overall = "FAIL" if any_fail else "PASS"
    n_measured = sum(1 for l in lines if "**PASS**" in l or "**FAIL**" in l)
    doc = f"""# tests/perf — RESULTS (measured baseline vs budgets)

Generated: {datetime.now(timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')} by
`tests/perf/aggregate.py` — do not hand-edit; re-run the aggregator.

Budget source: {budget_src}
Timings inputs: {', '.join(args.timings) or '(none)'}
Bench inputs: {', '.join(args.bench) or '(none)'}

Method: p50/p99 nearest-rank percentiles plus sustained throughput
(rps = N / summed measured wall of the N serially-issued timed calls),
each compared against its budget dimension. A line is PASS only when every
measured dimension passes. NOT-MEASURED lines are listed, not excused —
each names where the gap is documented. Booking-create lines (B1/B2) are
MEASURED-at-HTTP only when the harness actually exercised the booking write
path (SPEC-W42: IDENTITY_BASE_URL direct-GET tenant resolution, degraded
no-Dapr/no-Temporal posture — see tests/funds-e2e/README.md).

| # | metric | budgets | measured | verdict |
|---|---|---|---|---|
{chr(10).join(lines)}

**ATOMIC overall verdict: {overall}** ({n_measured}/{len(METRIC_LABELS)}
budget lines had measurements).

## Environment caveat

These numbers come from a constrained sandbox (2 CPU / 4 GB, embedded
PostgreSQL on a local unix socket, services built in debug mode for the
Rust binaries, loopback HTTP, no TLS, no API gateway; the throughput figure
is SEQUENTIAL single-client throughput, not a concurrency load test — see
tests/load/ for that). They are an INDICATIVE baseline that the flow works
and where the time goes — they are not production-representative. Re-run on
production-shaped hardware before using them for capacity decisions (see
docs/runbooks/capacity-planning.md).
"""
    out = Path(args.out)
    out.write_text(doc)
    print(f"wrote {out} ({len(doc)} bytes)")
    print(f"ATOMIC: {overall}; measured={n_measured}/{len(METRIC_LABELS)}")
    return 1 if any_fail else 0


if __name__ == "__main__":
    sys.exit(main())
