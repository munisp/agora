"""Tests for eval/latency_bench.py (SPEC-W73 WS-B B6 a-c).

(a) default mode runs with zero network (socket-use guard), deterministic
    structure, exit 0;
(b) percentile math correct on a known fixture;
(c) --json/--md files written with the pinned schema.
"""

from __future__ import annotations

import json
import socket
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "eval"))

import latency_bench  # noqa: E402

STAGE_KEYS = {"p50_ms", "p95_ms", "mean_ms", "min_ms", "max_ms", "n"}


def _assert_pinned_schema(report: dict, iterations: int, mode: str) -> None:
    assert set(report) == {"stt", "llm", "tts", "e2e", "meta"}
    for stage in ("stt", "llm", "tts", "e2e"):
        assert set(report[stage]) == STAGE_KEYS, stage
        assert report[stage]["n"] == iterations
        s = report[stage]
        assert 0.0 <= s["min_ms"] <= s["p50_ms"] <= s["p95_ms"] <= s["max_ms"]
        assert s["min_ms"] <= s["mean_ms"] <= s["max_ms"]
    assert set(report["meta"]) == {"iterations", "mode", "ts"}
    assert report["meta"]["iterations"] == iterations
    assert report["meta"]["mode"] == mode


@pytest.fixture()
def no_network(monkeypatch):
    """Hard socket guard: any network use in offline mode fails the test."""

    def _boom(*_args, **_kwargs):
        raise AssertionError("offline bench attempted network access")

    monkeypatch.setattr(socket.socket, "connect", _boom)
    monkeypatch.setattr(socket.socket, "connect_ex", _boom)
    monkeypatch.setattr(socket, "create_connection", _boom)
    monkeypatch.setattr(socket, "getaddrinfo", _boom)


async def test_a_offline_mode_zero_network_and_structure(no_network):
    report = await latency_bench.run_bench(iterations=3, mode="offline")
    _assert_pinned_schema(report, 3, "offline")


def test_a_main_exit_zero_offline(no_network, capsys):
    rc = latency_bench.main(["--iterations", "3"])
    assert rc == 0
    out = capsys.readouterr().out
    assert "mode=offline" in out
    for stage in ("stt", "llm", "tts", "e2e"):
        assert stage in out


def test_b_percentile_math_known_fixture():
    # Linear interpolation between closest ranks (inclusive convention).
    fixture = [10.0, 20.0, 30.0, 40.0, 50.0]
    assert latency_bench.percentile(fixture, 50) == 30.0
    assert latency_bench.percentile(fixture, 95) == 48.0  # rank 3.8 -> 40+0.8*10
    assert latency_bench.percentile(fixture, 0) == 10.0
    assert latency_bench.percentile(fixture, 100) == 50.0
    # Order-independence, even-count midpoint, degenerate inputs.
    assert latency_bench.percentile([50.0, 10.0, 30.0, 20.0, 40.0], 50) == 30.0
    assert latency_bench.percentile([1.0, 2.0, 3.0, 4.0], 50) == 2.5
    assert latency_bench.percentile([7.0], 95) == 7.0
    assert latency_bench.percentile([], 50) == 0.0
    s = latency_bench.summarize(fixture)
    assert s["p50_ms"] == 30.0 and s["p95_ms"] == 48.0 and s["mean_ms"] == 30.0
    assert s["min_ms"] == 10.0 and s["max_ms"] == 50.0 and s["n"] == 5


def test_c_json_and_md_outputs(tmp_path, no_network):
    json_path = tmp_path / "bench.json"
    md_path = tmp_path / "bench.md"
    rc = latency_bench.main(
        ["--iterations", "3", "--json", str(json_path), "--md", str(md_path)]
    )
    assert rc == 0
    report = json.loads(json_path.read_text())
    _assert_pinned_schema(report, 3, "offline")
    md = md_path.read_text()
    assert "| Stage | p50 (ms) | p95 (ms) | mean (ms) |" in md
    for stage in ("stt", "llm", "tts", "e2e"):
        assert f"| {stage} |" in md


def test_harness_error_is_nonzero():
    # iterations < 1 is a harness error -> exit 1, not a traceback.
    assert latency_bench.main(["--iterations", "0"]) == 1
