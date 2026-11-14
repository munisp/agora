"""SPEC-W65 A (G3): KYC golden corpus — OFFLINE-DETERMINISTIC ONLY.

Corpus: tests/corpus/kyc/<kind>/<case>.json with
{kind, doc, model_answer, expected_fields, expected_mrz_valid}. All values
are SYNTHETIC (existing ADAEZE/JANE fixture style) — no real PII.

What this suite pins (deterministic regression, no live inference):
  (a) parse_model_answer replay per corpus case (whitelist, null fallback,
      code-computed mrz_valid);
  (b) stubbed-sidecar e2e per case via conftest make_chat_completion_stub
      through POST /v1/ocr/extract (extraction fields, GATE R1 kyc event
      redaction extracted_fields={}, transcript suppression, expires_at TTL);
  (c) sha256 pins per prompt_for_kind(kyc kind) — prompt edits are
      deliberate diffs;
  (d) corpus schema validation: expected keys subset of KYC_FIELD_WHITELISTS
      (plus code-computed mrz_valid for kyc_passport).

What it does NOT pin (G3 honesty): live-model accuracy on Nigerian document
layouts — environment-blocked (no model in CI); see docs/ocr.md.
"""

from __future__ import annotations

import hashlib
import json
import re
from pathlib import Path

import pytest

from app.config import settings
from app.llm import (
    KYC_FIELD_WHITELISTS,
    OcrClient,
    parse_model_answer,
    prompt_for_kind,
)
from conftest import INTERNAL_TOKEN, TENANT_A, make_chat_completion_stub, make_png

CORPUS_DIR = Path(__file__).parent / "corpus" / "kyc"
EXTRACT = "/v1/ocr/extract"


def _load_cases() -> list[tuple[str, dict]]:
    cases = []
    for path in sorted(CORPUS_DIR.glob("*/*.json")):
        cases.append((f"{path.parent.name}/{path.stem}", json.loads(path.read_text())))
    return cases


CASES = _load_cases()
CASE_IDS = [cid for cid, _ in CASES]


# (c) sha256 pins for prompt_for_kind(kind) — a prompt edit must update these
# deliberately (same commit as the prompt change).
PROMPT_SHA256 = {
    "kyc_nin_slip": "3dc8b8325cc98accc108ee940bf42c0ceb039c97536a5f1da1ab9c4a572c2504",
    "kyc_drivers_license": "ac85509d5631c3f89970846abd1c8c1da3556dc5fc6463af3495a573a4c0ac35",
    "kyc_passport": "a739df53601bdbe2b803ec4d92669c5b9ef4309665f1ef7a12744c0efc3fd414",
    "kyc_voters_card": "35e05d17381c8c4205506889a633aef67b84b41d4d7a3cf237d59d8ba3d36883",
    "kyc_cac_document": "5c263fb4faec885871efb0b91370c04abca247fbc6b9e5cf3ec285a9683708b1",
}


_JSON_TAIL_RE = re.compile(r"\{.*\}", re.DOTALL)
_FENCE_RE = re.compile(r"```(?:json)?\s*(\{.*?\})\s*```", re.DOTALL)


def _model_json(case: dict) -> dict:
    """The raw JSON object the model answer carries (pre-whitelist)."""
    m = _FENCE_RE.search(case["model_answer"]) or _JSON_TAIL_RE.search(
        case["model_answer"]
    )
    assert m, "corpus model_answer carries no JSON object"
    raw = m.group(1) if m.re is _FENCE_RE else m.group(0)
    parsed = json.loads(raw)
    assert isinstance(parsed, dict)
    return parsed


def _canaries(case: dict) -> list[str]:
    """Every synthetic PII string in the case (whitelisted + dropped extras)."""
    out = []
    for value in _model_json(case).values():
        if isinstance(value, str) and value.strip():
            out.append(value)
    return out


# ---------------------------------------------------------------------------
# (d) corpus schema + structural validation
# ---------------------------------------------------------------------------
def test_corpus_schema_and_structure():
    assert CASES, "corpus is empty"
    per_kind: dict[str, list[dict]] = {}
    for cid, case in CASES:
        per_kind.setdefault(case["kind"], []).append(case)
        # required keys, exact case shape
        assert set(case) == {
            "kind",
            "doc",
            "model_answer",
            "expected_fields",
            "expected_mrz_valid",
        }, cid
        # path layout matches the declared kind
        assert cid.split("/")[0] == case["kind"], cid
        assert isinstance(case["model_answer"], str) and case["model_answer"].strip()
        assert isinstance(case["expected_fields"], dict)
        # synthetic-only marker present
        assert case["doc"].get("synthetic"), cid
        # (d) expected keys subset of the whitelist (+ code-computed mrz_valid)
        allowed = set(KYC_FIELD_WHITELISTS[case["kind"]])
        if case["kind"] == "kyc_passport":
            allowed |= {"mrz_valid"}
            assert case["expected_mrz_valid"] in (True, False, None), cid
            assert case["expected_fields"].get("mrz_valid") == case["expected_mrz_valid"], cid
        else:
            assert case["expected_mrz_valid"] is None, cid
            assert "mrz_valid" not in case["expected_fields"], cid
        assert set(case["expected_fields"]) <= allowed, cid
        # adversarial cases must actually over-emit (extras present pre-whitelist)
        if case["doc"].get("adversarial"):
            extras = set(_model_json(case)) - set(KYC_FIELD_WHITELISTS[case["kind"]]) - {"mrz"}
            assert extras, f"{cid}: adversarial case emits no extra keys"
    # all 5 kyc_* kinds covered, >=2 cases each incl. one adversarial
    assert set(per_kind) == set(KYC_FIELD_WHITELISTS), sorted(per_kind)
    for kind, cases in per_kind.items():
        assert len(cases) >= 2, kind
        assert any(c["doc"].get("adversarial") for c in cases), kind
    # passport carries an MRZ-failure case
    assert any(
        c["expected_mrz_valid"] is False for c in per_kind["kyc_passport"]
    ), "kyc_passport corpus lacks an MRZ-failure case"


# ---------------------------------------------------------------------------
# (c) prompt sha256 pins
# ---------------------------------------------------------------------------
@pytest.mark.parametrize("kind", sorted(PROMPT_SHA256))
def test_prompt_sha256_pin(kind):
    digest = hashlib.sha256(prompt_for_kind(kind).encode("utf-8")).hexdigest()
    assert digest == PROMPT_SHA256[kind], (
        f"prompt_for_kind({kind!r}) changed — update the pin deliberately"
    )


# ---------------------------------------------------------------------------
# (a) parse replay per corpus case
# ---------------------------------------------------------------------------
@pytest.mark.parametrize("cid,case", CASES, ids=CASE_IDS)
def test_golden_parse_replay(cid, case):
    text, fields = parse_model_answer(case["model_answer"], case["kind"])
    assert fields == case["expected_fields"]
    # transcript is retained by the parser, JSON stripped (suppressed later
    # at persistence for kyc — asserted in the e2e below)
    assert text.strip() and "```" not in text and "{" not in text
    if case["kind"] == "kyc_passport":
        assert fields["mrz_valid"] == case["expected_mrz_valid"]
        assert "mrz" not in fields  # raw MRZ never retained (P7/P9)
    # every whitelisted key is present (null fallback), nothing else
    expected_keys = set(KYC_FIELD_WHITELISTS[case["kind"]])
    if case["kind"] == "kyc_passport":
        expected_keys |= {"mrz_valid"}
    assert set(fields) == expected_keys


# ---------------------------------------------------------------------------
# (b) stubbed-sidecar e2e per corpus case (OCR_MOCK off, local stub llama)
# ---------------------------------------------------------------------------
@pytest.mark.parametrize("cid,case", CASES, ids=CASE_IDS)
def test_golden_stubbed_sidecar_e2e(cid, case, client, deps, monkeypatch):
    stub, captured = make_chat_completion_stub(case["model_answer"])
    try:
        monkeypatch.setattr(settings, "ocr_mock", False)
        deps.ocr = OcrClient(base_url=f"{stub.url}/v1", model="minicpm-ocr", timeout_s=10.0)
        resp = client.post(
            EXTRACT,
            files={"file": ("doc.png", make_png(), "image/png")},
            data={"kind": case["kind"], "tenant": TENANT_A},
            headers={"X-Internal-Token": INTERNAL_TOKEN},
        )
    finally:
        stub.close()
    assert resp.status_code == 200, (cid, resp.text)
    body = resp.json()

    # extraction fields: exactly the corpus expectation (whitelist + code MRZ)
    assert body["status"] == "extracted"
    assert body["extracted_fields"] == case["expected_fields"], cid

    # the stub saw exactly ONE call carrying this kind's prompt
    assert len(captured) == 1
    [msg] = captured[0]["json"]["messages"]
    assert msg["content"][0] == {"type": "text", "text": prompt_for_kind(case["kind"])}

    # A5 transcript suppression: not echoed, not persisted
    assert body["extracted_text"] == ""
    row = deps.db.rows[body["document_id"]]
    assert row["extracted_text"] is None
    assert row["extracted_fields"] == case["expected_fields"]

    # P4 / GATE R3: kyc TTL set on insert
    assert row["expires_at"] is not None

    # GATE R1: DocumentExtracted redacted — extracted_fields == {} and no
    # synthetic PII canary (whitelisted OR dropped-extra) on the bus.
    doc_events = [
        p for _, topic, p in deps.dapr.published if topic == "opendesk.ocr.events"
    ]
    assert len(doc_events) == 1
    evt = doc_events[0]
    assert evt["type"] == "com.opendesk.ocr.DocumentExtracted"
    assert evt["data"]["kind"] == case["kind"]
    assert evt["data"]["extracted_fields"] == {}
    blob = json.dumps(deps.dapr.published)
    for canary in _canaries(case):
        assert canary not in blob, f"{cid}: canary {canary!r} leaked onto the bus"
