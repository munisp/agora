"""Eval harness (SPEC-W3 §4, innovation 5).

Replays eval/scenarios/*.yaml against POST /voice/chat, asserts expected tool
calls, scores each turn 1-5 with an LLM judge (OpenAI-compatible env — the
same LLM_BASE_URL/LLM_MODEL/LLM_API_KEY family, so Ollama qwen3:8b works out
of the box), writes eval/report.md and best-effort indexes results into
OpenSearch `evals` when OPENSEARCH_ADDR is reachable.

Usage:  python3 eval/eval.py [--base-url http://localhost:7006] [--no-judge]
Env:    VOICE_BASE_URL, LLM_BASE_URL, LLM_MODEL, LLM_API_KEY, OPENSEARCH_ADDR
"""
from __future__ import annotations

import argparse, json, os, sys, time, uuid
from pathlib import Path

import httpx, yaml

HERE = Path(__file__).resolve().parent
JUDGE_PROMPT = """You are judging an AI receptionist turn. Score 1-5 (5 = fully meets the criterion).
Criterion: {criteria}

Caller said: {user}
Agent replied: {reply}
Tools called: {tools}

Answer with ONLY JSON: {{"score": <1-5>, "rationale": "<one sentence>"}}"""


def load_scenarios() -> list[dict]:
    out = []
    for f in sorted((HERE / "scenarios").glob("*.yaml")):
        out.append(yaml.safe_load(f.read_text()))
    return out


def judge(client, base_url, model, api_key, criteria, user, reply, tools) -> dict:
    try:
        r = client.post(f"{base_url.rstrip('/')}/chat/completions",
            headers={"authorization": f"Bearer {api_key}"},
            json={"model": model, "temperature": 0.0, "messages": [
                {"role": "user", "content": JUDGE_PROMPT.format(
                    criteria=criteria, user=user, reply=reply,
                    tools=", ".join(tools) or "none")}]}, timeout=30.0)
        content = r.json()["choices"][0]["message"]["content"]
        start, end = content.find("{"), content.rfind("}")
        parsed = json.loads(content[start:end + 1])
        return {"score": max(1, min(5, int(parsed.get("score", 1)))),
                "rationale": str(parsed.get("rationale", ""))}
    except Exception as exc:
        return {"score": None, "rationale": f"judge unavailable: {exc}"}


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--base-url", default=os.environ.get("VOICE_BASE_URL", "http://localhost:7006"))
    ap.add_argument("--no-judge", action="store_true")
    args = ap.parse_args()
    llm_base = os.environ.get("LLM_BASE_URL", "http://localhost:11434/v1")
    llm_model = os.environ.get("LLM_MODEL", "qwen3:8b")
    llm_key = os.environ.get("LLM_API_KEY", "ollama")
    os_addr = os.environ.get("OPENSEARCH_ADDR", "")

    results, failures = [], 0
    with httpx.Client(timeout=60.0) as client:
        for sc in load_scenarios():
            conv_id = str(uuid.uuid4())
            # K15(b): resume requires the session_secret issued on the first
            # turn; round-trip it for the rest of the scenario.
            secret: str | None = None
            record = {"scenario": sc["id"], "site_slug": sc["site_slug"],
                      "ts": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "turns": []}
            for turn in sc["turns"]:
                tools_called, reply, error = [], "", None
                try:
                    payload = {
                        "site_slug": sc["site_slug"], "message": turn["say"],
                        "conversation_id": conv_id}
                    if secret:
                        payload["session_secret"] = secret
                    r = client.post(f"{args.base_url}/voice/chat", json=payload)
                    r.raise_for_status()
                    body = r.json()
                    reply = body.get("reply", "")
                    secret = body.get("session_secret") or secret
                    tools_called = [t.get("tool") for t in body.get("tool_calls", [])]
                except Exception as exc:
                    error = str(exc)
                expected = turn.get("expect_tools") or []
                missing = [t for t in expected if t not in tools_called]
                verdict = None
                if not args.no_judge and error is None:
                    verdict = judge(client, llm_base, llm_model, llm_key,
                                    turn.get("judge", ""), turn["say"], reply, tools_called)
                ok = error is None and not missing and (verdict is None or (verdict["score"] or 0) >= 3)
                failures += 0 if ok else 1
                record["turns"].append({"say": turn["say"], "reply": reply,
                    "tools_called": tools_called, "expected_tools": expected,
                    "missing_tools": missing, "judge": verdict, "error": error, "ok": ok})
            results.append(record)

        # Optional OpenSearch indexing (best-effort).
        if os_addr:
            for rec in results:
                try:
                    client.put(f"{os_addr.rstrip('/')}/evals/_doc/{uuid.uuid4()}",
                               json=rec, timeout=5.0)
                except Exception as exc:
                    print(f"[eval] opensearch indexing skipped: {exc}", file=sys.stderr)
                    break

    lines = ["# Voice agent eval report", "",
             f"Generated: {time.strftime('%Y-%m-%d %H:%M:%S UTC', time.gmtime())}",
             f"Base URL: {args.base_url} | Judge: {'off' if args.no_judge else llm_model}", ""]
    for rec in results:
        passed = sum(1 for t in rec["turns"] if t["ok"])
        lines += [f"## {rec['scenario']} — {passed}/{len(rec['turns'])} turns ok", ""]
        for t in rec["turns"]:
            mark = "PASS" if t["ok"] else "FAIL"
            lines.append(f"- [{mark}] **{t['say']}**")
            lines.append(f"  - reply: {t['reply'][:300] or t['error']}")
            lines.append(f"  - tools: {t['tools_called']} (expected {t['expected_tools']})"
                         + (f" MISSING {t['missing_tools']}" if t["missing_tools"] else ""))
            if t["judge"]:
                lines.append(f"  - judge: {t['judge']['score']}/5 — {t['judge']['rationale']}")
        lines.append("")
    (HERE / "report.md").write_text("\n".join(lines))
    print(f"[eval] wrote {HERE / 'report.md'}; turn failures: {failures}")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
