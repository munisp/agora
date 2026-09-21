"""A/B prompt testing harness (Wave 5 #8).

Pairs two persona variants (A = control, B = candidate) against the eval
scenario set via POST /voice/chat `persona_override`, scores every turn with
the LLM judge and prints a per-scenario/per-turn comparison table plus the
mean judge score per variant (accuracy proxy: 5.0 = fully meets the turn
criterion, matching the eval harness scale).

Requires the runtime started with EVAL_PERSONA_OVERRIDE=true (off by
default — the override is an eval-only backdoor).

Usage:
    python eval/ab_test.py --base-url http://localhost:7006 \
        --control prompts/persona_default.txt --variant prompts/persona_v2.txt

Env: VOICE_BASE_URL, LLM_BASE_URL, LLM_MODEL, LLM_API_KEY (same family as
     eval/eval.py — Ollama qwen3:8b works out of the box).
"""
from __future__ import annotations

import argparse
import json
import os
import sys
import time
import uuid
from pathlib import Path

import httpx
import yaml

HERE = Path(__file__).resolve().parent
JUDGE_PROMPT = """You are judging an AI receptionist turn. Score 1-5 (5 = fully meets the criterion).
Criterion: {criteria}

Caller said: {user}
Agent replied: {reply}

Answer with ONLY JSON: {{"score": <1-5>, "rationale": "<one sentence>"}}"""


def load_scenarios() -> list[dict]:
    return [yaml.safe_load(f.read_text())
            for f in sorted((HERE / "scenarios").glob("*.yaml"))]


def load_persona(ref: str) -> str:
    """Persona text from a file path or a raw inline string."""
    p = Path(ref)
    if p.is_file():
        return p.read_text().strip()
    return ref


def judge(client, base_url, model, api_key, criteria, user, reply) -> dict:
    try:
        r = client.post(
            f"{base_url.rstrip('/')}/chat/completions",
            headers={"authorization": f"Bearer {api_key}"},
            json={"model": model, "temperature": 0.0, "messages": [{
                "role": "user",
                "content": JUDGE_PROMPT.format(criteria=criteria, user=user, reply=reply)}]},
            timeout=30.0)
        content = r.json()["choices"][0]["message"]["content"]
        start, end = content.find("{"), content.rfind("}")
        parsed = json.loads(content[start:end + 1])
        return {"score": max(1, min(5, int(parsed.get("score", 1)))),
                "rationale": str(parsed.get("rationale", ""))}
    except Exception as exc:
        return {"score": None, "rationale": f"judge unavailable: {exc}"}


def run_variant(client, base_url, scenarios, persona, judge_args, label) -> dict:
    """Replay the scenario set with a persona override; return turn results."""
    out = {"label": label, "persona_chars": len(persona), "scenarios": {}}
    for sc in scenarios:
        conv_id = str(uuid.uuid4())
        secret = None  # K15(b): round-trip the resume credential.
        turns = []
        for turn in sc["turns"]:
            reply, error = "", None
            try:
                payload = {
                    "site_slug": sc["site_slug"],
                    "message": turn["say"],
                    "conversation_id": conv_id,
                    "persona_override": persona,
                }
                if secret:
                    payload["session_secret"] = secret
                r = client.post(f"{base_url}/voice/chat", json=payload)
                r.raise_for_status()
                body = r.json()
                reply = body.get("reply", "")
                secret = body.get("session_secret") or secret
            except Exception as exc:
                error = str(exc)
            verdict = None
            if error is None:
                verdict = judge(client, *judge_args, turn.get("judge", ""),
                                turn["say"], reply)
            turns.append({"say": turn["say"], "reply": reply, "error": error,
                          "judge": verdict})
        out["scenarios"][sc["id"]] = turns
    return out


def mean_score(result: dict) -> float | None:
    scores = [t["judge"]["score"] for turns in result["scenarios"].values()
              for t in turns if t["judge"] and t["judge"]["score"] is not None]
    return round(sum(scores) / len(scores), 2) if scores else None


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--base-url", default=os.environ.get("VOICE_BASE_URL", "http://localhost:7006"))
    ap.add_argument("--control", required=True, help="persona A (file path or text)")
    ap.add_argument("--variant", required=True, help="persona B (file path or text)")
    ap.add_argument("--no-judge", action="store_true")
    args = ap.parse_args()

    judge_args = (os.environ.get("LLM_BASE_URL", "http://localhost:11434/v1"),
                  os.environ.get("LLM_MODEL", "qwen3:8b"),
                  os.environ.get("LLM_API_KEY", "ollama"))
    persona_a, persona_b = load_persona(args.control), load_persona(args.variant)
    scenarios = load_scenarios()
    if not scenarios:
        print("[ab] no scenarios found under eval/scenarios/", file=sys.stderr)
        return 1

    with httpx.Client(timeout=60.0) as client:
        res_a = run_variant(client, args.base_url, scenarios, persona_a,
                            judge_args, "A(control)")
        res_b = run_variant(client, args.base_url, scenarios, persona_b,
                            judge_args, "B(variant)")

    # Comparison table.
    print(f"\n# A/B prompt comparison — {time.strftime('%Y-%m-%d %H:%M UTC', time.gmtime())}")
    print(f"base: {args.base_url}  judge: {'off' if args.no_judge else judge_args[1]}\n")
    print(f"{'scenario':<28} {'turn':<38} {'A':>3} {'B':>3}  winner")
    print("-" * 84)
    for sid in res_a["scenarios"]:
        for i, (ta, tb) in enumerate(zip(res_a["scenarios"][sid],
                                         res_b["scenarios"][sid])):
            sa = ta["judge"]["score"] if ta["judge"] else None
            sb = tb["judge"]["score"] if tb["judge"] else None
            if sa is None or sb is None:
                winner = "?"
            elif sb > sa:
                winner = "B"
            elif sa > sb:
                winner = "A"
            else:
                winner = "="
            say = ta["say"][:36]
            print(f"{sid:<28} {say:<38} {str(sa):>3} {str(sb):>3}  {winner}")
    ma, mb = mean_score(res_a), mean_score(res_b)
    print("-" * 84)
    print(f"mean judge score:  A = {ma}   B = {mb}   "
          + ("B wins" if (mb or 0) > (ma or 0)
             else "A wins" if (ma or 0) > (mb or 0) else "tie"))

    report = {"ts": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
              "mean": {"A": ma, "B": mb}, "A": res_a, "B": res_b}
    out_path = HERE / "ab_report.json"
    out_path.write_text(json.dumps(report, indent=2, ensure_ascii=False))
    print(f"[ab] wrote {out_path}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
