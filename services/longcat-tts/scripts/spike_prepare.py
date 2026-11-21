"""spike_prepare.py — LongCat pcm/yo SPIKE data prep + tokenizer coverage
(SPEC-W105 C1).

This is EXPERIMENT PREPARATION ONLY. It trains nothing and claims no
Nigerian-language capability: it converts a voice-corpus export (W103/W104)
into a LongCat-style training manifest and produces the tokenizer coverage
report that gate G3 of docs/longcat-pcm-yo-spike.md requires before any
training run.

Input: voice-corpus export jsonl. BOTH export shapes are accepted and
detected per line (never assumed):
  - generic export:  {"audio_path", "text", "speaker_id", "language", ...}
  - cosyvoice export: {"utt", "wav", "text", "spk", "split"}

Coverage analysis, two modes:
  - "unicode-heuristic" (no deps, always available): buckets every
    character as ascii (printable Latin-1 ASCII) / cjk (CJK unified
    ideographs — what LongCat's zh training covers) / oov (everything
    else). LongCat's tokenizer is en/zh-trained, so oov_pct measures the
    scale of tokenizer work the spike needs.
  - "tokenizer" (optional, never faked): when LONGCAT_TOKENIZER names a
    HuggingFace model/dir and transformers is installed, the CLI loads the
    real tokenizer and reports token FERTILITY (tokens per character) for
    the corpus against an English control — BPE tokenizers never go OOV,
    but a tokenizer that never saw Yoruba text shreds it into many more
    tokens than English, which is the honest coverage signal. When the
    tokenizer is unavailable the report says so verbatim.

CLI:
    python -m scripts.spike_prepare --input corpus.jsonl --out manifest.jsonl \
        --coverage coverage.json [--language yo] [--limit 500]
"""

from __future__ import annotations

import argparse
import json
import os
import unicodedata
from typing import Callable, Iterable, Optional

#: Fixed English control sentence for the fertility comparison (echoed in
#: the report so reviews can reproduce the number).
ENGLISH_CONTROL = (
    "The quick brown fox jumps over the lazy dog while the customer waits."
)

_CJK_RANGES = (
    (0x4E00, 0x9FFF),    # CJK Unified Ideographs
    (0x3400, 0x4DBF),    # Extension A
    (0x20000, 0x2A6DF),  # Extension B
)


def _is_cjk(ch: str) -> bool:
    cp = ord(ch)
    return any(lo <= cp <= hi for lo, hi in _CJK_RANGES)


def _is_ascii(ch: str) -> bool:
    return 0x20 <= ord(ch) <= 0x7E or ch in "\n\t"


def iter_records(path: str) -> Iterable[dict]:
    """Yield parsed json lines (blank lines skipped)."""
    with open(path, encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if line:
                yield json.loads(line)


def build_manifest(records: Iterable[dict], language: Optional[str] = None) -> list[dict]:
    """voice-corpus export (either shape) -> LongCat-style manifest rows.

    Language filtering is exact-match, case-folded; records without a
    language key pass the filter (generic-export rows always carry one,
    cosyvoice-export rows never do — documented in the spike doc).
    """
    want = (language or "").strip().lower()
    out: list[dict] = []
    for rec in records:
        lang = str(rec.get("language", "") or "").strip().lower()
        if want and lang and lang != want:
            continue
        if "audio_path" in rec:  # generic voice-corpus export shape
            utt = rec.get("utterance_id") or rec.get("audio_path")
            wav = rec["audio_path"]
            spk = rec["speaker_id"]
        else:  # cosyvoice fine-tune export shape
            utt = rec["utt"]
            wav = rec["wav"]
            spk = rec["spk"]
        out.append({"utt": utt, "wav": wav, "text": rec["text"], "spk": spk})
    return out


def char_coverage(texts: Iterable[str]) -> dict:
    """Unicode-heuristic coverage report (no deps, always honest)."""
    counts = {"ascii": 0, "cjk": 0, "oov": 0}
    oov_chars: dict[str, int] = {}
    total = 0
    for text in texts:
        for ch in text:
            if ch in ("\n", "\t"):
                continue  # structural whitespace is not tokenizer content
            total += 1
            if _is_ascii(ch):
                counts["ascii"] += 1
            elif _is_cjk(ch):
                counts["cjk"] += 1
            else:
                counts["oov"] += 1
                oov_chars[ch] = oov_chars.get(ch, 0) + 1
    top = sorted(oov_chars.items(), key=lambda kv: (-kv[1], kv[0]))[:20]
    return {
        "mode": "unicode-heuristic",
        "chars": total,
        "ascii_pct": round(100.0 * counts["ascii"] / total, 2) if total else 0.0,
        "cjk_pct": round(100.0 * counts["cjk"] / total, 2) if total else 0.0,
        "oov_pct": round(100.0 * counts["oov"] / total, 2) if total else 0.0,
        "top_oov": [
            {
                "char": ch,
                "name": unicodedata.name(ch, "UNKNOWN"),
                "count": n,
            }
            for ch, n in top
        ],
    }


def token_fertility(
    texts: list[str], tokenizer_encode: Callable[[str], list]
) -> dict:
    """Tokens-per-character for the corpus vs the English control.

    BPE tokenizers never go OOV — a tokenizer that never saw Yoruba/pcm
    text shreds it into MORE tokens per character instead, so fertility is
    the honest coverage signal. ``tokenizer_encode`` is injected (the CLI
    binds the real HF tokenizer; tests bind a stub).
    """
    def tpc(samples: list[str]) -> float:
        tokens = 0
        chars = 0
        for text in samples:
            tokens += len(tokenizer_encode(text))
            chars += sum(1 for ch in text if ch not in ("\n", "\t"))
        return round(tokens / chars, 4) if chars else 0.0

    corpus_tpc = tpc(texts)
    control_tpc = tpc([ENGLISH_CONTROL])
    return {
        "mode": "tokenizer",
        "english_control": ENGLISH_CONTROL,
        "tokens_per_char_corpus": corpus_tpc,
        "tokens_per_char_english_control": control_tpc,
        "fertility_ratio": (
            round(corpus_tpc / control_tpc, 3) if control_tpc else None
        ),
    }


def _load_hf_tokenizer():
    """Lazy real-tokenizer loader. Returns (encode, None) or (None, reason)."""
    name = os.environ.get("LONGCAT_TOKENIZER", "").strip()
    if not name:
        return None, "LONGCAT_TOKENIZER unset (heuristic mode)"
    try:
        from transformers import AutoTokenizer  # type: ignore
    except ImportError:
        return None, "transformers not installed (heuristic mode)"
    try:
        tok = AutoTokenizer.from_pretrained(name)
    except Exception as exc:  # noqa: BLE001 - report, never crash the report
        return None, f"tokenizer load failed: {exc}"
    return (lambda text: tok.encode(text)), None


def main(argv: Optional[list[str]] = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--input", required=True, help="voice-corpus export jsonl")
    parser.add_argument("--out", required=True, help="LongCat manifest jsonl out")
    parser.add_argument("--coverage", required=True, help="coverage report json out")
    parser.add_argument("--language", default="", help="exact language filter")
    parser.add_argument("--limit", type=int, default=0, help="max records (0 = all)")
    args = parser.parse_args(argv)

    records = list(iter_records(args.input))
    if args.limit > 0:
        records = records[: args.limit]
    manifest = build_manifest(records, language=args.language or None)
    with open(args.out, "w", encoding="utf-8") as fh:
        for row in manifest:
            fh.write(json.dumps(row, ensure_ascii=False) + "\n")

    texts = [row["text"] for row in manifest]
    report = char_coverage(texts)
    encode, reason = _load_hf_tokenizer()
    if encode is not None:
        report["tokenizer"] = token_fertility(texts, encode)
    else:
        report["tokenizer_mode"] = f"unavailable: {reason}"
    report["records"] = len(records)
    report["manifest_rows"] = len(manifest)
    with open(args.coverage, "w", encoding="utf-8") as fh:
        json.dump(report, fh, indent=1, ensure_ascii=False)
    return 0


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
