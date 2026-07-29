"""Voice-quality conformance gates (SPEC-W11 Part E).

Consumes eval-harness artifacts and SessionMetrics JSON exports (the
`quality` object that app/metrics.py SessionMetrics.quality_payload attaches
to the SessionEnded CloudEvent) and enforces four gates:

- LATENCY:  mouth-to-ear p95 <= QUALITY_P95_MS (default 2500). Mouth-to-ear
            is derived from existing latency metric fields, in order of
            precedence per session export:
              1. explicit `mouth_to_ear_ms` per-turn sample list,
              2. per-stage sample lists (`stt_latencies_ms` +
                 `llm_latencies_ms` + `tts_latencies_ms`, elementwise),
              3. aggregate `avg_{stt,llm,tts}_latency_ms` fields,
              4. today's quality_payload shape (`avg_llm_latency_ms` only)
                 plus configurable STT/TTS allowances — an explicit
                 lower-bound estimate until per-stage exports land.
- STT WER:  word-error rate per language <= QUALITY_MAX_WER (default 0.25)
            over eval/corpora/corpus.yaml samples. Uses jiwer when
            importable, otherwise the minimal word-Levenshtein fallback in
            this file (no hard dependency). Hypotheses come from
            --stt-results JSON ({sample_id: hypothesis}) or the manifest's
            own `hypothesis` field (committed fixtures for self-test).
- TTS:      automated sanity on synthesized wav files (duration > 0,
            silence ratio < 0.8 — pure stdlib `wave`), plus the manual
            intelligibility protocol documented in
            docs/eval-quality-gates.md.
- MOS:      MOS-proxy (1-5) from session metrics (latency, interruptions,
            WER); gate >= QUALITY_MIN_MOS (default 3.5). Formula:
                lat_pen = 1.0 * clamp((p95_ms - 1000) / 2000, 0, 1)
                int_pen = 0.5 * clamp(interruption_rate / 0.15, 0, 1)
                wer_pen = 2.0 * clamp(wer / 0.40, 0, 1)
                mos     = clamp(5 - lat_pen - int_pen - wer_pen, 1, 5)
            interruption_rate = interruptions / turns across sessions
            (0 when the export predates interruption tracking).

Each gate reports PASS / FAIL / SKIP with evidence lines; the process exit
code is non-zero when any gate FAILs (SKIP never fails the run — it marks
inputs that were not provided, e.g. no synthesized wav in CI).

Usage:
    python3 eval/quality_gates.py [--metrics session_metrics.json]
        [--stt-results stt_hypotheses.json]
        [--corpus eval/corpora/corpus.yaml]
        [--tts-wav preview.wav [more.wav ...]]
        [--report eval/quality_report.json]

Env:    QUALITY_P95_MS (2500), QUALITY_MAX_WER (0.25), QUALITY_MIN_MOS (3.5),
        QUALITY_STT_ALLOWANCE_MS (350), QUALITY_TTS_ALLOWANCE_MS (450)
"""
from __future__ import annotations

import argparse, json, math, os, string, sys, time, wave
from pathlib import Path

import yaml

HERE = Path(__file__).resolve().parent

DEFAULTS = {
    "p95_ms": float(os.environ.get("QUALITY_P95_MS", "2500")),
    "max_wer": float(os.environ.get("QUALITY_MAX_WER", "0.25")),
    "min_mos": float(os.environ.get("QUALITY_MIN_MOS", "3.5")),
    # Estimated per-turn STT/TTS contributions used only when the export
    # carries no per-stage latency fields (case 4 above).
    "stt_allowance_ms": float(os.environ.get("QUALITY_STT_ALLOWANCE_MS", "350")),
    "tts_allowance_ms": float(os.environ.get("QUALITY_TTS_ALLOWANCE_MS", "450")),
}

SILENCE_RATIO_LIMIT = 0.8
SILENCE_PEAK_FRACTION = 0.02  # chunk is silent when peak < 2% of full scale
CHUNK_MS = 100

# ---------------------------------------------------------------------------
# WER: jiwer when available, else a minimal documented word-Levenshtein.
# ---------------------------------------------------------------------------
_PUNCT = str.maketrans("", "", string.punctuation)


def normalize_text(text: str) -> list[str]:
    """Lowercase, strip punctuation, collapse whitespace, split to words."""
    return text.lower().translate(_PUNCT).split()


def _levenshtein(a: list[str], b: list[str]) -> int:
    """Classic DP edit distance over word lists (O(len(a)*len(b)) — corpora
    samples are sentence-scale, so this is fine)."""
    prev = list(range(len(b) + 1))
    for i, wa in enumerate(a, 1):
        cur = [i]
        for j, wb in enumerate(b, 1):
            cur.append(min(prev[j] + 1, cur[j - 1] + 1, prev[j - 1] + (wa != wb)))
        prev = cur
    return prev[-1]


try:  # pragma: no cover - depends on optional install
    import jiwer as _jiwer

    _WER_BACKEND = "jiwer"

    def word_error_rate(ref: str, hyp: str) -> float:
        return float(_jiwer.wer(" ".join(normalize_text(ref)),
                                " ".join(normalize_text(hyp))))

except ImportError:
    _WER_BACKEND = "builtin-levenshtein (jiwer not installed)"

    def word_error_rate(ref: str, hyp: str) -> float:
        ref_words, hyp_words = normalize_text(ref), normalize_text(hyp)
        if not ref_words:
            return 0.0 if not hyp_words else 1.0
        return _levenshtein(ref_words, hyp_words) / len(ref_words)


# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------
def _p95(samples: list[float]) -> float:
    """Nearest-rank p95."""
    if not samples:
        return 0.0
    ordered = sorted(samples)
    return ordered[max(0, math.ceil(0.95 * len(ordered)) - 1)]


def _clamp(x: float, lo: float, hi: float) -> float:
    return max(lo, min(hi, x))


def load_session_metrics(path: str) -> tuple[list[dict], list[str]]:
    """Load a SessionMetrics JSON export. Accepted shapes:
    - a list of quality_payload dicts,
    - {"sessions": [...]},
    - a single quality_payload dict (one session)."""
    evidence = []
    data = json.loads(Path(path).read_text())
    if isinstance(data, dict) and isinstance(data.get("sessions"), list):
        sessions = data["sessions"]
    elif isinstance(data, list):
        sessions = data
    elif isinstance(data, dict):
        sessions = [data]
    else:
        raise ValueError(f"unrecognized session-metrics shape in {path}")
    sessions = [s for s in sessions if isinstance(s, dict)]
    evidence.append(f"loaded {len(sessions)} session(s) from {path}")
    return sessions, evidence


def mouth_to_ear_samples(sessions: list[dict], cfg: dict) -> tuple[list[float], list[str]]:
    """Per-session mouth-to-ear latency samples (ms); see module docstring
    for the precedence order of the derivation."""
    samples: list[float] = []
    evidence: list[str] = []
    estimated = 0
    for i, s in enumerate(sessions):
        label = s.get("conversation_id") or f"session[{i}]"
        if isinstance(s.get("mouth_to_ear_ms"), list) and s["mouth_to_ear_ms"]:
            vals = [float(v) for v in s["mouth_to_ear_ms"]]
            samples.extend(vals)
            evidence.append(f"{label}: {len(vals)} explicit mouth_to_ear_ms sample(s)")
            continue
        lists = [s.get(k) for k in ("stt_latencies_ms", "llm_latencies_ms", "tts_latencies_ms")]
        if all(isinstance(v, list) and v for v in lists):
            n = min(len(v) for v in lists)
            vals = [sum(float(v[j]) for v in lists) for j in range(n)]
            samples.extend(vals)
            evidence.append(f"{label}: {n} sample(s) from per-stage latency lists")
            continue
        agg = [s.get("avg_stt_latency_ms"), s.get("avg_llm_latency_ms"), s.get("avg_tts_latency_ms")]
        if any(v is not None for v in agg):
            stt = float(agg[0]) if agg[0] is not None else cfg["stt_allowance_ms"]
            llm = float(agg[1]) if agg[1] is not None else 0.0
            tts = float(agg[2]) if agg[2] is not None else cfg["tts_allowance_ms"]
            samples.append(stt + llm + tts)
            if agg[0] is None or agg[2] is None:
                estimated += 1
                evidence.append(
                    f"{label}: mouth-to-ear estimated as avg_llm({llm:.0f})"
                    f" + stt_allowance({stt:.0f}) + tts_allowance({tts:.0f})"
                    " — lower-bound estimate; per-stage exports not present")
            else:
                evidence.append(f"{label}: mouth-to-ear from aggregate stage averages")
    if estimated:
        evidence.append(
            f"{estimated}/{len(sessions)} session(s) used STT/TTS allowances"
            " (QUALITY_STT_ALLOWANCE_MS/QUALITY_TTS_ALLOWANCE_MS)")
    return samples, evidence


def mos_proxy(p95_ms: float, interruption_rate: float, wer: float) -> tuple[float, list[str]]:
    lat_pen = 1.0 * _clamp((p95_ms - 1000.0) / 2000.0, 0.0, 1.0)
    int_pen = 0.5 * _clamp(interruption_rate / 0.15, 0.0, 1.0)
    wer_pen = 2.0 * _clamp(wer / 0.40, 0.0, 1.0)
    mos = _clamp(5.0 - lat_pen - int_pen - wer_pen, 1.0, 5.0)
    evidence = [
        f"lat_pen={lat_pen:.3f} (p95={p95_ms:.0f}ms; 0 at <=1000ms, 1.0 at >=3000ms)",
        f"int_pen={int_pen:.3f} (interruption_rate={interruption_rate:.3f}; 0.5 at >=0.15)",
        f"wer_pen={wer_pen:.3f} (wer={wer:.3f}; 2.0 at >=0.40)",
    ]
    return round(mos, 2), evidence


# ---------------------------------------------------------------------------
# gates
# ---------------------------------------------------------------------------
def gate_latency(sessions: list[dict] | None, cfg: dict) -> dict:
    if not sessions:
        return {"gate": "LATENCY", "status": "SKIP", "value": None,
                "threshold_ms": cfg["p95_ms"],
                "evidence": ["no session-metrics export provided (--metrics)"]}
    samples, evidence = mouth_to_ear_samples(sessions, cfg)
    if not samples:
        return {"gate": "LATENCY", "status": "SKIP", "value": None,
                "threshold_ms": cfg["p95_ms"],
                "evidence": evidence + ["no latency samples derivable from export"]}
    p95 = _p95(samples)
    ok = p95 <= cfg["p95_ms"]
    evidence.insert(0, f"samples n={len(samples)} min={min(samples):.0f}ms "
                       f"max={max(samples):.0f}ms p95={p95:.0f}ms")
    return {"gate": "LATENCY", "status": "PASS" if ok else "FAIL",
            "value": round(p95, 1), "threshold_ms": cfg["p95_ms"],
            "samples": len(samples), "evidence": evidence,
            "_samples": samples}


def gate_stt(corpus_path: str, stt_results_path: str | None, cfg: dict) -> dict:
    evidence = [f"WER backend: {_WER_BACKEND}"]
    path = Path(corpus_path)
    if not path.is_file():
        return {"gate": "STT_WER", "status": "SKIP", "value": None,
                "threshold_wer": cfg["max_wer"],
                "evidence": evidence + [f"corpus manifest not found: {corpus_path}"]}
    manifest = yaml.safe_load(path.read_text()) or {}
    samples = manifest.get("samples") or []
    hypotheses: dict[str, str] = {}
    if stt_results_path:
        hypotheses = {str(k): str(v) for k, v in
                      json.loads(Path(stt_results_path).read_text()).items()}
        evidence.append(f"hypotheses loaded from {stt_results_path} "
                        f"({len(hypotheses)} entrie(s))")
    per_lang: dict[str, list[float]] = {}
    skipped = 0
    for s in samples:
        sid, lang = str(s.get("id")), str(s.get("lang", "und"))
        ref = str(s.get("transcript", ""))
        hyp = hypotheses.get(sid, s.get("hypothesis"))
        if hyp is None:
            skipped += 1
            evidence.append(f"sample {sid} ({lang}): SKIP — no hypothesis "
                            "(run STT over the audio and pass --stt-results; "
                            "see eval/corpora/README.md)")
            continue
        wer = word_error_rate(ref, str(hyp))
        per_lang.setdefault(lang, []).append(wer)
        evidence.append(f"sample {sid} ({lang}): WER={wer:.3f}")
    if skipped:
        evidence.append(f"{skipped} sample(s) skipped for lack of hypotheses")
    if not per_lang:
        return {"gate": "STT_WER", "status": "SKIP", "value": None,
                "threshold_wer": cfg["max_wer"],
                "evidence": evidence + ["no evaluable samples in corpus"]}
    lang_wer = {lang: sum(v) / len(v) for lang, v in sorted(per_lang.items())}
    worst = max(lang_wer.values())
    ok = all(w <= cfg["max_wer"] for w in lang_wer.values())
    evidence.insert(1, "per-language WER: " + ", ".join(
        f"{lang}={w:.3f} ({len(per_lang[lang])} sample(s))" for lang, w in lang_wer.items()))
    overall = sum(w for v in per_lang.values() for w in v) / sum(len(v) for v in per_lang.values())
    return {"gate": "STT_WER", "status": "PASS" if ok else "FAIL",
            "value": round(overall, 3), "threshold_wer": cfg["max_wer"],
            "per_language": {k: round(v, 3) for k, v in lang_wer.items()},
            "evidence": evidence, "_wer": overall}


def _wav_peak(chunk: bytes, width: int) -> int:
    """Peak absolute amplitude of a raw PCM chunk (stdlib only)."""
    if width == 1:  # unsigned 8-bit, centered at 128
        return max((abs(b - 128) for b in chunk), default=0)
    import array
    typecode = {2: "h", 4: "i"}.get(width)
    if typecode is None:
        raise ValueError(f"unsupported sample width: {width} byte(s)")
    vals = array.array(typecode)
    vals.frombytes(chunk[: len(chunk) - len(chunk) % width])
    return max((abs(v) for v in vals), default=0)


def analyze_wav(path: str) -> dict:
    """Duration + silence ratio of a PCM wav, using only the stdlib `wave`
    module. Silence = 100ms chunk whose peak amplitude is below 2% of full
    scale."""
    with wave.open(path, "rb") as w:
        frames, rate = w.getnframes(), w.getframerate()
        width, channels = w.getsampwidth(), w.getnchannels()
        chunk_frames = max(1, rate * CHUNK_MS // 1000)
        total = silent = 0
        while True:
            data = w.readframes(chunk_frames)
            if not data:
                break
            if _wav_peak(data, width) < SILENCE_PEAK_FRACTION * (2 ** (8 * width - 1)):
                silent += 1
            total += 1
    duration = frames / float(rate) if rate else 0.0
    return {"path": path, "duration_s": round(duration, 2),
            "silence_ratio": round(silent / total, 3) if total else 1.0,
            "rate_hz": rate, "sample_width": width, "channels": channels}


def gate_tts(wav_paths: list[str], cfg: dict) -> dict:
    evidence = []
    if not wav_paths:
        return {"gate": "TTS", "status": "SKIP", "value": None,
                "evidence": ["no synthesized wav provided (--tts-wav); apply the "
                             "manual intelligibility protocol in "
                             "docs/eval-quality-gates.md before release"]}
    ok_all = True
    for p in wav_paths:
        try:
            info = analyze_wav(p)
        except Exception as exc:
            ok_all = False
            evidence.append(f"{p}: FAIL — unreadable wav ({exc})")
            continue
        ok = info["duration_s"] > 0 and info["silence_ratio"] < SILENCE_RATIO_LIMIT
        ok_all = ok_all and ok
        evidence.append(
            f"{p}: {'ok' if ok else 'FAIL'} — duration={info['duration_s']}s "
            f"silence_ratio={info['silence_ratio']} (limit <{SILENCE_RATIO_LIMIT}) "
            f"[{info['rate_hz']}Hz {info['sample_width'] * 8}-bit {info['channels']}ch]")
    evidence.append("manual intelligibility spot-check still required per "
                    "docs/eval-quality-gates.md (automated gate covers sanity only)")
    return {"gate": "TTS", "status": "PASS" if ok_all else "FAIL",
            "value": None, "wavs": len(wav_paths), "evidence": evidence}


def gate_mos(sessions: list[dict] | None, latency_gate: dict, stt_gate: dict, cfg: dict) -> dict:
    evidence = []
    if not sessions:
        return {"gate": "MOS_PROXY", "status": "SKIP", "value": None,
                "threshold_mos": cfg["min_mos"],
                "evidence": ["no session-metrics export provided (--metrics)"]}
    p95 = float(latency_gate.get("value") or 0.0)
    wer = float(stt_gate.get("_wer") or 0.0)
    if stt_gate.get("status") == "SKIP":
        evidence.append("STT gate skipped — wer_pen computed with wer=0.0")
    interruptions = sum(float(s.get("interruptions", 0) or 0) for s in sessions)
    turns = sum(float(s.get("turn_count", 0) or 0) for s in sessions)
    rate = interruptions / turns if turns else 0.0
    if not any("interruptions" in s for s in sessions):
        evidence.append("export carries no interruption counts "
                        "(pre-Wave-11 exporter) — interruption_rate treated as 0")
    mos, mos_evidence = mos_proxy(p95, rate, wer)
    evidence = mos_evidence + evidence
    ok = mos >= cfg["min_mos"]
    return {"gate": "MOS_PROXY", "status": "PASS" if ok else "FAIL",
            "value": mos, "threshold_mos": cfg["min_mos"], "evidence": evidence}


# ---------------------------------------------------------------------------
def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--metrics", default=os.environ.get("QUALITY_METRICS_JSON", ""),
                    help="SessionMetrics JSON export (SessionEnded quality objects)")
    ap.add_argument("--stt-results", default=os.environ.get("QUALITY_STT_RESULTS", ""),
                    help="JSON {sample_id: hypothesis} from an STT run over the corpus")
    ap.add_argument("--corpus", default=str(HERE / "corpora" / "corpus.yaml"))
    ap.add_argument("--tts-wav", nargs="*", default=[], metavar="WAV",
                    help="synthesized wav file(s) for the TTS sanity gate")
    ap.add_argument("--report", default=str(HERE / "quality_report.json"))
    args = ap.parse_args()
    cfg = DEFAULTS

    sessions = None
    load_evidence: list[str] = []
    if args.metrics:
        sessions, load_evidence = load_session_metrics(args.metrics)

    gates = []
    gates.append(gate_latency(sessions, cfg))
    gates.append(gate_stt(args.corpus, args.stt_results or None, cfg))
    gates.append(gate_tts(args.tts_wav, cfg))
    gates.append(gate_mos(sessions, gates[0], gates[1], cfg))

    failed = [g for g in gates if g["status"] == "FAIL"]
    overall = "FAIL" if failed else "PASS"

    print(f"[gates] quality gates @ {time.strftime('%Y-%m-%d %H:%M:%S UTC', time.gmtime())}"
          f" — thresholds: p95<={cfg['p95_ms']:.0f}ms wer<={cfg['max_wer']} mos>={cfg['min_mos']}")
    for line in load_evidence:
        print(f"  [input] {line}")
    for g in gates:
        value = g.get("value")
        verdict = g["status"]
        print(f"  [{verdict}] {g['gate']}" + (f" — value={value}" if value is not None else ""))
        for line in g["evidence"]:
            print(f"      {line}")

    report = {"generated": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
              "overall": overall,
              "thresholds": {"p95_ms": cfg["p95_ms"], "max_wer": cfg["max_wer"],
                             "min_mos": cfg["min_mos"]},
              "gates": [{k: v for k, v in g.items() if not k.startswith("_")}
                        for g in gates]}
    Path(args.report).write_text(json.dumps(report, indent=2) + "\n")
    print(f"[gates] wrote {args.report}; result: {overall}")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
