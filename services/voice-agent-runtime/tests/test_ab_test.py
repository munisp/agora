"""A/B prompt testing tests (Wave 5 #8): score aggregation math, win-rate
computation, promotion recommendation and promoted-file writing — all with a
fake judge and a fake chat endpoint (no live stack)."""

from __future__ import annotations

import importlib.util
from pathlib import Path

import pytest

EVAL_DIR = Path(__file__).resolve().parents[1] / "eval"


def _load_ab():
    spec = importlib.util.spec_from_file_location("ab_test", EVAL_DIR / "ab_test.py")
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


ab = _load_ab()

SCENARIOS = [
    {"id": "s1", "site_slug": "acme", "turns": [{"say": "hi", "judge": "c"}, {"say": "book", "judge": "c"}]},
    {"id": "s2", "site_slug": "acme", "turns": [{"say": "cancel", "judge": "c"}]},
]


# --------------------------------------------------------------------------
# Aggregation math
# --------------------------------------------------------------------------
class TestAggregate:
    def test_means_and_scenario_winners(self):
        results = [
            {"variant": "A", "scenario": "s1", "score": 2, "error": None},
            {"variant": "A", "scenario": "s1", "score": 4, "error": None},
            {"variant": "A", "scenario": "s2", "score": 1, "error": None},
            {"variant": "B", "scenario": "s1", "score": 5, "error": None},
            {"variant": "B", "scenario": "s1", "score": 3, "error": None},
            {"variant": "B", "scenario": "s2", "score": 4, "error": None},
        ]
        summary = ab.aggregate(results)
        assert summary["variants"]["A"]["mean"] == pytest.approx(7 / 3, abs=1e-3)
        assert summary["variants"]["B"]["mean"] == 4.0
        # s1: A=3.0 vs B=4.0 -> B; s2: A=1 vs B=4 -> B
        assert summary["scenarios"]["s1"]["winner"] == "B"
        assert summary["scenarios"]["s2"]["winner"] == "B"
        assert summary["variants"]["B"]["scenarios_won"] == 2.0
        assert summary["variants"]["B"]["win_rate"] == 1.0
        assert summary["variants"]["A"]["win_rate"] == 0.0

    def test_tie_counts_half(self):
        results = [
            {"variant": "A", "scenario": "s1", "score": 3, "error": None},
            {"variant": "B", "scenario": "s1", "score": 3, "error": None},
            {"variant": "A", "scenario": "s2", "score": 5, "error": None},
            {"variant": "B", "scenario": "s2", "score": 1, "error": None},
        ]
        summary = ab.aggregate(results)
        assert summary["scenarios"]["s1"]["winner"] == "tie"
        assert summary["variants"]["A"]["scenarios_won"] == 1.5
        assert summary["variants"]["B"]["scenarios_won"] == 0.5
        assert summary["variants"]["A"]["win_rate"] == 0.75

    def test_unscored_turns_excluded_from_mean(self):
        results = [
            {"variant": "A", "scenario": "s1", "score": 4, "error": None},
            {"variant": "A", "scenario": "s1", "score": None, "error": "boom"},
        ]
        summary = ab.aggregate(results)
        va = summary["variants"]["A"]
        assert va["mean"] == 4.0
        assert va["turns"] == 2
        assert va["scored_turns"] == 1
        assert va["errors"] == 1


# --------------------------------------------------------------------------
# Recommendation + promote
# --------------------------------------------------------------------------
class TestRecommend:
    def test_b_promoted_on_clear_win(self):
        results = [
            {"variant": "A", "scenario": "s1", "score": 2, "error": None},
            {"variant": "A", "scenario": "s2", "score": 2, "error": None},
            {"variant": "B", "scenario": "s1", "score": 5, "error": None},
            {"variant": "B", "scenario": "s2", "score": 4, "error": None},
        ]
        decision = ab.recommend(ab.aggregate(results))
        assert decision["promote"] is True
        assert decision["winner"] == "B"
        assert decision["gap"] == pytest.approx(2.5, abs=1e-3)

    def test_small_gap_keeps_incumbent(self):
        results = [
            {"variant": "A", "scenario": "s1", "score": 4, "error": None},
            {"variant": "A", "scenario": "s2", "score": 4, "error": None},
            {"variant": "B", "scenario": "s1", "score": 4, "error": None},
            {"variant": "B", "scenario": "s2", "score": 4.2, "error": None},
        ]
        decision = ab.recommend(ab.aggregate(results))
        # Gap 0.1 < MIN_SCORE_GAP -> conservative: keep A even though B's
        # mean is higher.
        assert decision["promote"] is False
        assert decision["winner"] == "A"

    def test_higher_mean_but_fewer_scenarios_keeps_incumbent(self):
        results = [
            {"variant": "A", "scenario": "s1", "score": 1, "error": None},
            {"variant": "A", "scenario": "s2", "score": 5, "error": None},
            {"variant": "A", "scenario": "s3", "score": 5, "error": None},
            {"variant": "B", "scenario": "s1", "score": 5, "error": None},
            {"variant": "B", "scenario": "s2", "score": 4, "error": None},
            {"variant": "B", "scenario": "s3", "score": 4, "error": None},
        ]
        summary = ab.aggregate(results)
        # B mean 4.33 > A mean 3.67, but B wins only 1 of 3 scenarios.
        assert summary["variants"]["B"]["mean"] > summary["variants"]["A"]["mean"]
        decision = ab.recommend(summary)
        assert decision["promote"] is False

    def test_promote_writes_tenant_file(self, tmp_path):
        path = ab.promote_persona("acme", "You are the B persona.", out_dir=tmp_path)
        assert path.name == "acme.md"
        text = path.read_text()
        assert "tenant `acme`" in text
        assert "You are the B persona." in text


# --------------------------------------------------------------------------
# run_variant plumbing with fake judge
# --------------------------------------------------------------------------
class TestRunVariant:
    def test_runs_all_scenarios_and_scores(self):
        chat_calls = []

        def chat(site_slug, message, conversation_id, persona_override):
            chat_calls.append((site_slug, message, persona_override))
            return f"reply to {message}", ["get_business_info"]

        judged = []

        def judge(turn, reply, tools):
            judged.append(reply)
            return {"score": 5, "rationale": "great"}

        results = ab.run_variant(
            SCENARIOS, variant="B", persona="PERSONA-B", chat_fn=chat, judge_fn=judge
        )
        # 3 turns across 2 scenarios, all scored 5.
        assert len(results) == 3
        assert all(r["score"] == 5 for r in results)
        assert all(r["variant"] == "B" for r in results)
        # The candidate persona is injected on every request.
        assert all(p == "PERSONA-B" for _, _, p in chat_calls)
        summary = ab.aggregate(results)
        assert summary["variants"]["B"]["mean"] == 5.0

    def test_chat_error_recorded_not_raised(self):
        def chat(*_a, **_k):
            raise ConnectionError("runtime down")

        results = ab.run_variant(
            SCENARIOS[:1], variant="A", persona=None, chat_fn=chat, judge_fn=None
        )
        assert all(r["error"] for r in results)
        summary = ab.aggregate(results)
        assert summary["variants"]["A"]["errors"] == 2
        assert summary["variants"]["A"]["scored_turns"] == 0
