"""A/B prompt testing (Wave 5 #8, STRATEGY §3).

Runs two persona variants across ALL eval scenarios against the live
`POST /voice/chat` endpoint and lets the LLM judge pick a winner:

- **A** = the tenant's current persona (resolved server-side from the
  industry pack, exactly as production serves it).
- **B** = a candidate persona from `eval/personas/*.md`, injected per request
  via the `persona_override` chat field. The runtime only honors that field
  when started with `EVAL_PERSONA_OVERRIDE=true` (default off — it is a
  prompt-injection surface on a public endpoint).

Scoring: every turn of every scenario is scored 1-5 by the same LLM judge
as eval.py. Summary = mean score per variant + per-scenario win rate
(a scenario is "won" by the variant with the higher mean; ties count ½).
`--promote` writes the winner to `eval/promoted/{tenant}.md` (drop-in
replacement for the pack persona) and prints the recommendation.

Usage:
    EVAL_PERSONA_OVERRIDE=true on the voice runtime, then:
    python3 eval/ab_test.py --tenant acme --persona-b eval/personas/salon_warm_concise.md
    python3 eval/ab_test.py --tenant acme --persona-b ... --promote
    make ab-test AB_ARGS="--tenant acme --persona-b eval/personas/salon_warm_concise.md"

Env: VOICE_BASE_URL, LLM_BASE_URL, LLM_MODEL, LLM_API_KEY (same family as eval.py).
"""
from __future__ import annotations

import argparse, json, os, sys, time, uuid
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

import eval as base_eval  # noqa: E402  (scenarios + judge shared with eval.py)

MIN_SCORE_GAP = 0.25  # minimum mean-score advantage for a promote recommendation


def run_variant(
    scenarios: list[dict],
    *,
    variant: str,
    persona: str | None,
    chat_fn,
    judge_fn=None,
) -> list[dict]:
    """Replay every scenario turn for one persona variant.

    ``chat_fn(site_slug, message, conversation_id, persona_override)`` ->
    (reply, tools_called). ``judge_fn(turn, reply, tools_called)`` ->
    {"score": 1..5|None, "rationale": str}. Returns one record per turn."""
    results = []
    for sc in scenarios:
        conv_id = str(uuid.uuid4())
        for turn in sc["turns"]:
            error = None
            try:
                reply, tools = chat_fn(
                    sc["site_slug"], turn["say"], conv_id, persona
                )
            except Exception as exc:  # noqa: BLE001 - record and continue
                reply, tools, error = "", [], str(exc)
            verdict = (
                judge_fn(turn, reply, tools)
                if judge_fn is not None and error is None
                else {"score": None, "rationale": "judge off" if judge_fn is None else error}
            )
            results.append(
                {
                    "variant": variant,
                    "scenario": sc["id"],
                    "say": turn["say"],
                    "reply": reply,
                    "tools_called": tools,
                    "score": verdict.get("score"),
                    "rationale": verdict.get("rationale", ""),
                    "error": error,
                }
            )
    return results


def _mean(values: list[float]) -> float:
    return sum(values) / len(values) if values else 0.0


def aggregate(results: list[dict]) -> dict:
    """Statistical summary: mean score per variant (overall + per scenario),
    per-scenario winners and win rates. Turns without a judge score are
    excluded from the means (counted separately)."""
    variants = sorted({r["variant"] for r in results})
    summary: dict = {"variants": {}, "scenarios": {}}
    for v in variants:
        vrs = [r for r in results if r["variant"] == v]
        scored = [r["score"] for r in vrs if isinstance(r["score"], (int, float))]
        errors = sum(1 for r in vrs if r["error"])
        summary["variants"][v] = {
            "turns": len(vrs),
            "scored_turns": len(scored),
            "errors": errors,
            "mean": round(_mean(scored), 3),
        }
    for sid in sorted({r["scenario"] for r in results}):
        per_v = {}
        for v in variants:
            scored = [
                r["score"]
                for r in results
                if r["scenario"] == sid
                and r["variant"] == v
                and isinstance(r["score"], (int, float))
            ]
            per_v[v] = _mean(scored) if scored else None
        ranked = [m for m in per_v.values() if m is not None]
        if not ranked:
            winner = None
        else:
            best = max(ranked)
            leaders = [v for v, m in per_v.items() if m == best]
            winner = leaders[0] if len(leaders) == 1 else "tie"
        summary["scenarios"][sid] = {"means": per_v, "winner": winner}
    # Win rate: scenarios won per variant; a tie awards ½ to each leader.
    n_sc = len(summary["scenarios"])
    for v in variants:
        wins = 0.0
        for sc in summary["scenarios"].values():
            w = sc["winner"]
            if w == v:
                wins += 1.0
            elif w == "tie":
                wins += 0.5
        summary["variants"][v]["scenarios_won"] = wins
        summary["variants"][v]["win_rate"] = round(wins / n_sc, 3) if n_sc else 0.0
    return summary


def recommend(summary: dict, *, a: str = "A", b: str = "B") -> dict:
    """Promotion decision: B is recommended when its overall mean beats A's
    by at least MIN_SCORE_GAP AND it wins strictly more scenarios than A.
    Anything else keeps the incumbent (conservative default)."""
    va, vb = summary["variants"].get(a, {}), summary["variants"].get(b, {})
    gap = round(vb.get("mean", 0.0) - va.get("mean", 0.0), 3)
    won_more = vb.get("scenarios_won", 0.0) > va.get("scenarios_won", 0.0)
    promote = gap >= MIN_SCORE_GAP and won_more
    rationale = (
        f"B mean {vb.get('mean', 0):.2f} vs A mean {va.get('mean', 0):.2f} "
        f"(gap {gap:+.2f}, need ≥ +{MIN_SCORE_GAP}); scenarios "
        f"{vb.get('scenarios_won', 0):.1f} vs {va.get('scenarios_won', 0):.1f}"
    )
    return {
        "promote": promote,
        "winner": b if promote else a,
        "gap": gap,
        "rationale": rationale,
    }


def promote_persona(tenant: str, persona: str, out_dir: Path | None = None) -> Path:
    """Write the winning persona to eval/promoted/{tenant}.md — the drop-in
    replacement for the tenant pack's agentPersona."""
    out = (out_dir or HERE / "promoted")
    out.mkdir(parents=True, exist_ok=True)
    path = out / f"{tenant}.md"
    header = (
        f"# Promoted persona — tenant `{tenant}`\n"
        f"<!-- generated by eval/ab_test.py "
        f"{time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())}; "
        "copy the body into the industry pack `agentPersona` to apply -->\n\n"
    )
    path.write_text(header + persona.strip() + "\n")
    return path


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--tenant", required=True, help="tenant slug (names the promoted file)")
    ap.add_argument("--persona-b", required=True, help="candidate persona markdown (eval/personas/*.md)")
    ap.add_argument("--base-url", default=os.environ.get("VOICE_BASE_URL", "http://localhost:7006"))
    ap.add_argument("--no-judge", action="store_true")
    ap.add_argument("--promote", action="store_true", help="write the winner to eval/promoted/{tenant}.md")
    args = ap.parse_args()

    persona_b_path = Path(args.persona_b)
    persona_b = persona_b_path.read_text().strip()
    scenarios = base_eval.load_scenarios()
    if not scenarios:
        print("[ab] no scenarios found", file=sys.stderr)
        return 2

    import httpx

    llm_base = os.environ.get("LLM_BASE_URL", "http://localhost:11434/v1")
    llm_model = os.environ.get("LLM_MODEL", "qwen3:8b")
    llm_key = os.environ.get("LLM_API_KEY", "ollama")

    with httpx.Client(timeout=60.0) as client:
        # K15(b): resume requires the session_secret issued at session
        # creation; round-trip it per conversation_id.
        secrets: dict[str, str] = {}

        def chat_fn(site_slug, message, conversation_id, persona_override):
            body = {
                "site_slug": site_slug,
                "message": message,
                "conversation_id": conversation_id,
            }
            if conversation_id in secrets:
                body["session_secret"] = secrets[conversation_id]
            if persona_override:
                body["persona_override"] = persona_override
            r = client.post(f"{args.base_url.rstrip('/')}/voice/chat", json=body)
            r.raise_for_status()
            payload = r.json()
            if payload.get("session_secret"):
                secrets[conversation_id] = payload["session_secret"]
            return payload.get("reply", ""), [
                t.get("tool") for t in payload.get("tool_calls", [])
            ]

        judge_fn = None
        if not args.no_judge:
            def judge_fn(turn, reply, tools):  # noqa: F811
                return base_eval.judge(
                    client, llm_base, llm_model, llm_key,
                    turn.get("judge", ""), turn["say"], reply, tools,
                )

        results = run_variant(
            scenarios, variant="A", persona=None, chat_fn=chat_fn, judge_fn=judge_fn
        ) + run_variant(
            scenarios, variant="B", persona=persona_b, chat_fn=chat_fn, judge_fn=judge_fn
        )

    summary = aggregate(results)
    decision = recommend(summary)

    lines = [
        "# A/B persona eval report", "",
        f"Generated: {time.strftime('%Y-%m-%d %H:%M:%S UTC', time.gmtime())}",
        f"Tenant: {args.tenant} | B candidate: {persona_b_path.name} | "
        f"Judge: {'off' if args.no_judge else llm_model}", "",
        f"| Variant | Mean | Scenarios won | Win rate | Errors |",
        f"|---|---|---|---|---|",
    ]
    for v, stats in summary["variants"].items():
        lines.append(
            f"| {v} | {stats['mean']:.2f} | {stats['scenarios_won']:.1f} | "
            f"{stats['win_rate']:.0%} | {stats['errors']} |"
        )
    lines += ["", "## Per-scenario means", ""]
    for sid, sc in summary["scenarios"].items():
        means = ", ".join(
            f"{v}={m:.2f}" if m is not None else f"{v}=n/a"
            for v, m in sc["means"].items()
        )
        lines.append(f"- **{sid}**: {means} → winner: {sc['winner']}")
    lines += ["", f"**Recommendation: {'PROMOTE B' if decision['promote'] else 'KEEP A'}** "
              f"({decision['rationale']})", ""]
    (HERE / "ab_report.md").write_text("\n".join(lines))
    print(f"[ab] wrote {HERE / 'ab_report.md'}")
    print(f"[ab] recommendation: {'PROMOTE B' if decision['promote'] else 'KEEP A'} — {decision['rationale']}")

    if args.promote:
        winner_persona = persona_b if decision["promote"] else ""
        if not winner_persona:
            print("[ab] not promoting: A keeps the incumbent persona; "
                  "eval/promoted not written", file=sys.stderr)
            return 1
        path = promote_persona(args.tenant, winner_persona)
        print(f"[ab] promoted B persona -> {path}")
    return 0 if decision["promote"] or not args.promote else 1


if __name__ == "__main__":
    sys.exit(main())
