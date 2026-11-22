"""SPEC-W105 C3 tests: spike_prepare — manifest build (both export shapes),
language filter, coverage buckets, token fertility (stub tokenizer), and the
CLI end-to-end in heuristic mode."""

from __future__ import annotations

import json

import pytest

from scripts.spike_prepare import (
    ENGLISH_CONTROL,
    build_manifest,
    char_coverage,
    iter_records,
    main,
    token_fertility,
)


def _write_jsonl(path, rows):
    path.write_text(
        "\n".join(json.dumps(r, ensure_ascii=False) for r in rows) + "\n",
        encoding="utf-8",
    )


GENERIC_ROWS = [
    {
        "audio_path": "audio/spk-1/utt-1.wav",
        "text": "Ẹ káàrọ̀",
        "speaker_id": "spk-1",
        "language": "yo",
        "duration_s": 1.2,
        "sample_rate": 16000,
    },
    {
        "audio_path": "audio/spk-2/utt-2.wav",
        "text": "How body",
        "speaker_id": "spk-2",
        "language": "pcm",
        "duration_s": 1.0,
        "sample_rate": 16000,
    },
]

COSYVOICE_ROWS = [
    {"utt": "utt-1", "wav": "audio/spk-1/utt-1.wav", "text": "Ẹ káàrọ̀",
     "spk": "spk-1", "split": "train"},
    {"utt": "utt-2", "wav": "audio/spk-2/utt-2.wav", "text": "How body",
     "spk": "spk-2", "split": "valid"},
]


class TestManifest:
    def test_generic_export_shape(self):
        rows = build_manifest(GENERIC_ROWS)
        assert rows == [
            {"utt": "audio/spk-1/utt-1.wav", "wav": "audio/spk-1/utt-1.wav",
             "text": "Ẹ káàrọ̀", "spk": "spk-1"},
            {"utt": "audio/spk-2/utt-2.wav", "wav": "audio/spk-2/utt-2.wav",
             "text": "How body", "spk": "spk-2"},
        ]

    def test_cosyvoice_export_shape(self):
        rows = build_manifest(COSYVOICE_ROWS)
        assert [r["utt"] for r in rows] == ["utt-1", "utt-2"]
        assert rows[0]["spk"] == "spk-1"
        assert rows[1]["text"] == "How body"

    def test_language_filter_exact_casefolded(self):
        rows = build_manifest(GENERIC_ROWS, language="YO")
        assert len(rows) == 1
        assert rows[0]["text"] == "Ẹ káàrọ̀"
        assert build_manifest(GENERIC_ROWS, language="pcm")[0]["spk"] == "spk-2"
        assert build_manifest(GENERIC_ROWS, language="ha") == []

    def test_empty_input(self):
        assert build_manifest([]) == []
        assert build_manifest([], language="yo") == []


class TestCoverage:
    def test_ascii_only(self):
        report = char_coverage(["How body, you dey?"])
        assert report["mode"] == "unicode-heuristic"
        assert report["ascii_pct"] == 100.0
        assert report["oov_pct"] == 0.0
        assert report["top_oov"] == []

    def test_tone_marked_yoruba_counts_oov(self):
        report = char_coverage(["Ẹ káàrọ̀"])
        assert report["oov_pct"] > 0
        chars = {entry["char"] for entry in report["top_oov"]}
        assert "Ẹ" in chars
        names = {entry["char"]: entry["name"] for entry in report["top_oov"]}
        assert "LATIN" in names["Ẹ"]

    def test_cjk_bucket(self):
        report = char_coverage(["你好世界"])
        assert report["cjk_pct"] == 100.0
        assert report["oov_pct"] == 0.0

    def test_empty_texts_zero_not_nan(self):
        report = char_coverage([])
        assert report["chars"] == 0
        assert report["ascii_pct"] == 0.0
        assert report["oov_pct"] == 0.0


class TestTokenFertility:
    @staticmethod
    def _stub_encode(text: str) -> list[str]:
        """Stub tokenizer: ASCII words -> 1 token/word; non-ASCII chars ->
        one token EACH (simulates an en/zh tokenizer shredding Yoruba)."""
        tokens: list[str] = []
        for word in text.split():
            current = ""
            for ch in word:
                if ord(ch) < 128:
                    current += ch
                else:
                    if current:
                        tokens.append(current)
                        current = ""
                    tokens.append(ch)
            if current:
                tokens.append(current)
        return tokens

    def test_ratio_math(self):
        report = token_fertility(["Ẹ káàrọ̀ ẹ ẹ ẹ"], self._stub_encode)
        assert report["mode"] == "tokenizer"
        assert report["english_control"] == ENGLISH_CONTROL
        assert report["tokens_per_char_corpus"] > 0
        assert report["tokens_per_char_english_control"] > 0
        expected = round(
            report["tokens_per_char_corpus"]
            / report["tokens_per_char_english_control"],
            3,
        )
        assert report["fertility_ratio"] == expected

    def test_shredded_corpus_ratio_above_one(self):
        report = token_fertility(["Ẹ́ ń lò ó pọ̀"], self._stub_encode)
        assert report["fertility_ratio"] > 1.0


class TestCli:
    def test_end_to_end_heuristic(self, tmp_path, monkeypatch):
        monkeypatch.delenv("LONGCAT_TOKENIZER", raising=False)
        src = tmp_path / "corpus.jsonl"
        _write_jsonl(src, GENERIC_ROWS)
        out = tmp_path / "manifest.jsonl"
        cov = tmp_path / "coverage.json"
        rc = main([
            "--input", str(src),
            "--out", str(out),
            "--coverage", str(cov),
            "--language", "yo",
        ])
        assert rc == 0
        rows = [json.loads(l) for l in out.read_text(encoding="utf-8").splitlines()]
        assert len(rows) == 1
        assert rows[0]["text"] == "Ẹ káàrọ̀"
        report = json.loads(cov.read_text(encoding="utf-8"))
        assert report["mode"] == "unicode-heuristic"
        assert report["manifest_rows"] == 1
        assert report["records"] == 2
        assert report["tokenizer_mode"].startswith("unavailable:")

    def test_iter_records_skips_blanks(self, tmp_path):
        p = tmp_path / "x.jsonl"
        p.write_text('{"a": 1}\n\n{"b": 2}\n', encoding="utf-8")
        assert list(iter_records(str(p))) == [{"a": 1}, {"b": 2}]
