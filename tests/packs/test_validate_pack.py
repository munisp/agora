"""Tests for scripts/validate_pack.py (Wave 5 #6): schema validation on
fixtures, the four shipped packs, and the registry index round-trip."""

from __future__ import annotations

import importlib.util
import json
import sys
from pathlib import Path

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[2]
FIXTURES = Path(__file__).parent / "fixtures"

spec = importlib.util.spec_from_file_location("validate_pack", ROOT / "scripts" / "validate_pack.py")
vp = importlib.util.module_from_spec(spec)
sys.modules["validate_pack"] = vp
spec.loader.exec_module(vp)


def load(name: str):
    return yaml.safe_load((FIXTURES / name).read_text())


# --------------------------------------------------------------- valid packs
def test_valid_full_fixture_passes():
    assert vp.validate_pack(load("valid_full.yaml")) == []


def test_valid_minimal_fixture_passes():
    assert vp.validate_pack(load("valid_minimal.yaml")) == []


@pytest.mark.parametrize("pack", sorted((ROOT / "industries").glob("*.yaml")))
def test_shipped_packs_pass(pack):
    doc, errs = vp.load_pack(pack)
    assert errs == []
    assert vp.validate_pack(doc, source=str(pack)) == []


# ------------------------------------------------------------- invalid packs
def test_unknown_temporal_workflow_rejected():
    errs = vp.validate_pack(load("invalid_workflow.yaml"))
    assert any("temporalWorkflow" in e and "NotAWorkflow" in e for e in errs)


def test_schema_violations_all_reported():
    errs = vp.validate_pack(load("invalid_schema.yaml"))
    joined = "\n".join(errs)
    for expected in (
        "displayName",
        "terminology.contact",
        "agentPersona",
        "depositPercent",
        "noShowFeeCents",
        "phoneConfirmation",
        "offerings[0]",
        "knowledgeSeed[0]",
        "dashboardLabels.bookingPlural",
        "agents[0]",
        "duplicate agent id",
        "customTools[0]",
    ):
        assert expected in joined, f"missing error about {expected!r}:\n{joined}"


def test_non_mapping_document_rejected():
    assert vp.validate_pack(["not", "a", "mapping"]) != []


# -------------------------------------------------------------------- index
def test_shipped_index_valid():
    assert vp.validate_index(ROOT / "industries" / "index.json") == []


def test_index_detects_sha_mismatch(tmp_path):
    pack = tmp_path / "p1.yaml"
    pack.write_text(yaml.safe_dump(load("valid_minimal.yaml")))
    index = tmp_path / "index.json"
    index.write_text(json.dumps({
        "schemaVersion": 1,
        "packs": [{
            "id": "fixture-min", "version": "1.0.0",
            "sha256": "0" * 64, "author": "tester", "signature": None,
            "path": "p1.yaml",
        }],
    }))
    errs = vp.validate_index(index)
    assert any("sha256 mismatch" in e for e in errs)


def test_index_detects_missing_file_and_bad_entry(tmp_path):
    index = tmp_path / "index.json"
    index.write_text(json.dumps({
        "schemaVersion": 1,
        "packs": [
            {"id": "ghost", "version": "1.0.0", "sha256": "a" * 64,
             "author": "tester", "signature": None, "path": "ghost.yaml"},
            {"version": "1.0.0"},  # no id
        ],
    }))
    errs = vp.validate_index(index)
    assert any("not found" in e for e in errs)
    assert any(".id is required" in e for e in errs)


def test_upsert_index_round_trip(tmp_path):
    pack = tmp_path / "rt.yaml"
    pack.write_text((FIXTURES / "valid_minimal.yaml").read_text())
    index = tmp_path / "index.json"
    assert vp.upsert_index(index, pack, version="2.0.0", author="tester") == []
    data = json.loads(index.read_text())
    (entry,) = data["packs"]
    assert entry["id"] == "fixture-min"
    assert entry["version"] == "2.0.0"
    assert entry["author"] == "tester"
    assert entry["signature"] is None
    assert entry["sha256"] == vp.sha256_file(pack)
    # and the written index validates against the pack file
    pack2 = tmp_path / "fixture-min.yaml"
    pack2.write_text(pack.read_text())
    assert vp.validate_index(index) == []


def test_upsert_index_rejects_invalid_pack(tmp_path):
    pack = tmp_path / "bad.yaml"
    pack.write_text((FIXTURES / "invalid_workflow.yaml").read_text())
    index = tmp_path / "index.json"
    errs = vp.upsert_index(index, pack, version="1.0.0", author="tester")
    assert errs
    assert not index.exists()  # nothing written on validation failure


# ---------------------------------------------------------------------- cli
def test_cli_validate_ok_and_fail(capsys):
    assert vp.main(["validate", str(FIXTURES / "valid_full.yaml")]) == 0
    assert vp.main(["validate", str(FIXTURES / "invalid_workflow.yaml")]) == 1


# ------------------------------------------------------- mcpServers (SPEC-W9)
def _with_mcp(servers):
    doc = load("valid_minimal.yaml")
    doc["mcpServers"] = servers
    return doc


def test_mcp_servers_valid_block_accepted():
    doc = _with_mcp([
        {"name": "n8n", "url": "https://n8n.example.com/mcp/front-desk/sse"},
        {"name": "crm-2", "url": "https://mcp.crm.example.com/"},
    ])
    assert vp.validate_pack(doc) == []


def test_mcp_servers_absent_or_empty_accepted():
    assert vp.validate_pack(load("valid_minimal.yaml")) == []
    assert vp.validate_pack(_with_mcp([])) == []


def test_mcp_servers_rejects_bad_shapes():
    cases = [
        ("not-a-list", "mcpServers must be a list"),
        (["junk"], "must be a mapping"),
        ([{"name": "Bad Name", "url": "https://x.example.com/"}], "must match"),
        ([{"name": "n8n", "url": "http://x.example.com/sse"}], "absolute https"),
        ([{"name": "n8n", "url": "not-a-url"}], "absolute https"),
        ([{"name": "n8n", "url": "https://x.example.com/"},
          {"name": "n8n", "url": "https://y.example.com/"}], "duplicate server name"),
        ([{"name": "n8n", "url": "https://x.example.com/",
           "headers": {"authorization": "Bearer x"}}], "headers are not allowed"),
    ]
    for servers, expected in cases:
        errs = vp.validate_pack(_with_mcp(servers))
        assert any(expected in e for e in errs), f"{expected!r} missing in {errs}"


# -------------------------------------------------------------- ussd (SPEC-W12)
def _with_ussd(ussd):
    doc = load("valid_minimal.yaml")
    doc["ussd"] = ussd
    return doc


def test_ussd_valid_block_accepted():
    doc = _with_ussd({"menu": [
        {"key": "1", "label": "Book appointment", "action": "book"},
        {"key": "2", "label": "Talk to an agent", "action": "handoff"},
        {"key": "3", "label": "Opening hours"},  # action optional
    ]})
    assert vp.validate_pack(doc) == []


def test_ussd_absent_accepted():
    assert vp.validate_pack(load("valid_minimal.yaml")) == []


def test_ussd_rejects_bad_shapes():
    cases = [
        ("not-a-mapping", "ussd must be a mapping"),
        ({}, "ussd.menu must be a non-empty list"),
        ({"menu": []}, "ussd.menu must be a non-empty list"),
        ({"menu": [{"label": "No key", "action": "book"}]}, "key is required"),
        ({"menu": [{"key": "123456789", "label": "Long key"}]}, "key must be <= 8 chars"),
        ({"menu": [{"key": "1", "action": "book"}]}, "label is required"),
        ({"menu": [{"key": "1", "label": "x" * 81}]}, "label must be <= 80 chars"),
        ({"menu": [{"key": "1", "label": "A"}, {"key": "1", "label": "B"}]},
         "duplicate menu key"),
        ({"menu": [{"key": "1", "label": "A", "action": "teleport"}]},
         "must be one of book, handoff, info, sos, status"),
    ]
    for ussd, expected in cases:
        errs = vp.validate_pack(_with_ussd(ussd))
        assert any(expected in e for e in errs), f"{expected!r} missing in {errs}"


# ------------------------------------------------------------ growth (SPEC-W15)
def _with_growth(growth):
    doc = load("valid_minimal.yaml")
    doc["growth"] = growth
    return doc


def test_growth_valid_block_accepted():
    doc = _with_growth({
        "referral_bounty_ngn": 2000,
        "primary_channels": ["whatsapp", "ussd"],
        "cac_target_ngn": 4000,
    })
    assert vp.validate_pack(doc) == []


def test_growth_absent_and_zero_bounty_accepted():
    assert vp.validate_pack(load("valid_minimal.yaml")) == []
    # a zero bounty is valid (no referral programme)
    assert vp.validate_pack(_with_growth({
        "referral_bounty_ngn": 0,
        "primary_channels": ["whatsapp"],
        "cac_target_ngn": 1,
    })) == []


def test_growth_rejects_bad_shapes():
    cases = [
        ("not-a-mapping", "growth must be a mapping"),
        ({}, "growth.referral_bounty_ngn must be an int >= 0"),
        ({"referral_bounty_ngn": -1, "primary_channels": ["ussd"],
          "cac_target_ngn": 100}, "referral_bounty_ngn must be an int >= 0"),
        ({"referral_bounty_ngn": "2000", "primary_channels": ["ussd"],
          "cac_target_ngn": 100}, "referral_bounty_ngn must be an int >= 0"),
        ({"referral_bounty_ngn": True, "primary_channels": ["ussd"],
          "cac_target_ngn": 100}, "referral_bounty_ngn must be an int >= 0"),
        ({"referral_bounty_ngn": 0, "cac_target_ngn": 100},
         "primary_channels must be a non-empty list of strings"),
        ({"referral_bounty_ngn": 0, "primary_channels": [],
          "cac_target_ngn": 100}, "primary_channels must be a non-empty list"),
        ({"referral_bounty_ngn": 0, "primary_channels": "whatsapp",
          "cac_target_ngn": 100}, "primary_channels must be a non-empty list"),
        ({"referral_bounty_ngn": 0, "primary_channels": ["whatsapp", ""],
          "cac_target_ngn": 100}, "primary_channels[1] must be a non-empty string"),
        ({"referral_bounty_ngn": 0, "primary_channels": ["whatsapp", 7],
          "cac_target_ngn": 100}, "primary_channels[1] must be a non-empty string"),
        ({"referral_bounty_ngn": 0, "primary_channels": ["ussd"]},
         "cac_target_ngn must be an int > 0"),
        ({"referral_bounty_ngn": 0, "primary_channels": ["ussd"],
          "cac_target_ngn": 0}, "cac_target_ngn must be an int > 0"),
        ({"referral_bounty_ngn": 0, "primary_channels": ["ussd"],
          "cac_target_ngn": -500}, "cac_target_ngn must be an int > 0"),
    ]
    for growth, expected in cases:
        errs = vp.validate_pack(_with_growth(growth))
        assert any(expected in e for e in errs), f"{expected!r} missing in {errs}"


# -------------------------------------------------------------- i18n (SPEC-W15)
def _with_i18n(i18n):
    doc = load("valid_minimal.yaml")
    doc["i18n"] = i18n
    return doc


def test_i18n_valid_block_accepted():
    doc = _with_i18n({
        "pcm": {"greeting": "Welcome! How we fit help you today?"},
        "en": {"greeting": "Welcome! How can we help you today?"},
        "ha": {"greeting": "Sannu! Yaya za mu taimake ku yau?"},
        "yo": {"greeting": "Kaabo! Bawo ni a se le ran wo lowo?"},
        "ig": {"greeting": "Nnoo! Kedu ka anyi ga-esi nyere gi aka?"},
    })
    assert vp.validate_pack(doc) == []


def test_i18n_absent_accepted():
    assert vp.validate_pack(load("valid_minimal.yaml")) == []


def test_i18n_rejects_bad_shapes():
    cases = [
        ("not-a-mapping", "i18n must be a mapping of locale -> strings"),
        ({"fr": {"greeting": "Bonjour"}}, "locale 'fr' must be one of en, pcm, ha, yo, ig"),
        ({"pcm": "just a string"}, "i18n['pcm'] must be a mapping of key -> text"),
        ({"pcm": {"greeting": ""}}, "i18n['pcm']['greeting'] must be a non-empty string"),
        ({"pcm": {"greeting": "   "}}, "must be a non-empty string"),
        ({"pcm": {"greeting": 42}}, "must be a non-empty string"),
    ]
    for i18n, expected in cases:
        errs = vp.validate_pack(_with_i18n(i18n))
        assert any(expected in e for e in errs), f"{expected!r} missing in {errs}"
