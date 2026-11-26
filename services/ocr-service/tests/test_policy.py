"""2026-11-26: policy rules -- the model's draft is filtered to the fixed vocabulary, rules check documents
deterministically, staff turn them on, and active rules change a caller document's verdict."""

from __future__ import annotations

import time
from datetime import date

import pytest

import app.main as main_module
from app import policy
from app.config import settings
from conftest import INTERNAL_TOKEN, TENANT_A, make_png

TODAY = date(2026, 11, 26)
STUB = {
    "employer_name": "Dangote Cement Plc", "employee_name": "Chika Obi", "pay_period_start": "2026-10-01",
    "pay_period_end": "2026-10-31", "pay_date": "2026-10-31", "gross_pay": 350000.0, "net_pay": 280000.0,
    "deductions_total": 70000.0, "currency": "NGN",
}
MODEL_DRAFT = {"rules": [
    {"kind": "pay_stub", "field": "pay_date", "check": "max_age_days", "value": 92, "severity": "reject",
     "text": "Pay stubs must be from the last 3 months.", "source": "Payslips older than three (3) months will not be accepted."},
    {"kind": "pay_stub", "field": "gross_pay", "check": "min", "value": "70,000", "severity": "review",
     "text": "Gross monthly pay must be at least ₦70,000.", "source": "…not less than the national minimum wage of ₦70,000…"},
    {"document": "pay_stub", "field": "currency", "check": "equals", "value": "NGN", "text": "Amounts must be in Naira."},
    {"kind": "pay_stub", "field": "pay_period_end", "check": "period_max_days", "value": 31,
     "text": "A pay stub covers one month."},
    # dropped: unknown field, unknown check, wrong type, unknown kind
    {"kind": "pay_stub", "field": "pension_pin", "check": "required"},
    {"kind": "pay_stub", "field": "gross_pay", "check": "regex", "value": ".*"},
    {"kind": "pay_stub", "field": "employer_name", "check": "min", "value": 3},
    {"kind": "passport", "field": "expiry", "check": "required"},
]}


def test_normalize_keeps_only_the_vocabulary():
    rules = policy.normalize_rules(MODEL_DRAFT)
    assert [r["id"] for r in rules] == ["r1", "r2", "r3", "r4"]
    assert rules[0]["severity"] == "reject" and rules[1]["severity"] == "review"
    assert rules[1]["value"] == 70000.0
    assert rules[0]["source"].startswith("Payslips older")
    assert rules[2]["text"] == "Amounts must be in Naira."
    assert policy.normalize_rules("nonsense") == [] and policy.normalize_rules({"rules": None}) == []


def test_rules_check_a_document():
    rules = policy.normalize_rules(MODEL_DRAFT)
    assert policy.apply_rules("pay_stub", STUB, rules, today=TODAY) == ("validated", [])
    stale = dict(STUB, pay_date="2026-06-30", pay_period_start="2026-06-01", pay_period_end="2026-06-30")
    verdict, reasons = policy.apply_rules("pay_stub", stale, rules, today=TODAY)
    assert verdict == "rejected" and reasons == ["policy:Pay stubs must be from the last 3 months."]
    low = dict(STUB, gross_pay=55000.0, net_pay=50000.0, deductions_total=5000.0)
    assert policy.apply_rules("pay_stub", low, rules, today=TODAY) == (
        "needs_review", ["policy:Gross monthly pay must be at least ₦70,000."])
    unreadable = dict(STUB, pay_date=None)
    verdict, reasons = policy.apply_rules("pay_stub", unreadable, rules, today=TODAY)
    assert verdict == "needs_review" and reasons == ["policy_unchecked:Pay stubs must be from the last 3 months."]
    quarter = dict(STUB, pay_period_start="2026-08-01")
    assert policy.apply_rules("pay_stub", quarter, rules, today=TODAY)[1] == ["policy:A pay stub covers one month."]
    off = [dict(r, enabled=False) for r in rules]
    assert policy.apply_rules("pay_stub", stale, off, today=TODAY) == ("validated", [])
    assert policy.apply_rules("receipt", stale, rules, today=TODAY) == ("validated", [])


def test_combine_takes_the_worst_and_puts_policy_first():
    assert policy.combine(("needs_review", ["currency_missing"]), ("rejected", ["policy:x"])) == (
        "rejected", ["policy:x", "currency_missing"])


def test_model_json_with_fences_and_prose():
    assert policy.parse_model_json('```json\n{"rules": []}\n```') == {"rules": []}
    assert policy.parse_model_json('Here you go: {"rules": [1]} hope it helps') == {"rules": [1]}
    assert policy.parse_model_json("no json") is None


@pytest.fixture
def policy_client(client, monkeypatch):
    monkeypatch.setattr(settings, "policy_llm_base_url", "http://llm.test/v1")
    monkeypatch.setattr(settings, "policy_llm_model", "gemma")

    async def fake_draft(text):
        assert "three (3) months" in text
        return policy.normalize_rules(MODEL_DRAFT)

    monkeypatch.setattr(main_module, "_draft_rules", fake_draft)
    return client


H = {"X-Internal-Token": INTERNAL_TOKEN}
POLICY_TXT = (b"Contribution evidence policy.\nPayslips older than three (3) months will not be accepted.\n"
              b"Gross pay must not be less than the national minimum wage of N70,000.")


def _wait_draft(c, pid):
    for _ in range(50):
        body = c.get(f"/v1/ocr/policies/{pid}", params={"tenant": TENANT_A}, headers=H).json()
        if body["status"] != "reading":
            return body
        time.sleep(0.05)
    raise AssertionError("policy never read")


def test_upload_review_turn_on_and_it_applies(policy_client, deps, monkeypatch):
    c = policy_client
    r = c.post("/v1/ocr/policies", files={"file": ("policy.txt", POLICY_TXT, "text/plain")},
               data={"tenant": TENANT_A, "title": "Contribution evidence policy"}, headers=H)
    assert r.status_code == 202, r.text
    assert r.json()["status"] == "reading"
    body = _wait_draft(c, r.json()["id"])
    assert body["status"] == "draft" and len(body["rules"]) == 4

    # a draft does nothing yet
    stale = dict(STUB, pay_date="2026-06-30", pay_period_start="2026-06-01", pay_period_end="2026-06-30")
    monkeypatch.setattr(main_module, "mock_extraction", _mock_with(stale))
    up = lambda: c.post("/v1/ocr/extract", files={"file": ("stub.png", make_png(), "image/png")},  # noqa: E731
                        data={"kind": "pay_stub", "tenant": TENANT_A}, headers=H).json()
    assert not any(x.startswith("policy") for x in up()["validation_reasons"])

    # nothing chosen -> refused; then turn on all but the currency rule
    assert c.put(f"/v1/ocr/policies/{body['id']}", params={"tenant": TENANT_A}, headers=H,
                 json={"status": "active", "enabled": []}).status_code == 400
    on = c.put(f"/v1/ocr/policies/{body['id']}", params={"tenant": TENANT_A}, headers=H,
               json={"status": "active", "enabled": ["r1", "r2", "r4"]})
    assert on.status_code == 200 and on.json()["status"] == "active"
    assert [r["enabled"] for r in on.json()["rules"]] == [True, True, False, True]

    res = up()
    assert res["validation_verdict"] == "rejected"
    assert res["validation_reasons"][0] == "policy:Pay stubs must be from the last 3 months."

    # off again -> back to the built-in checks only
    c.put(f"/v1/ocr/policies/{body['id']}", params={"tenant": TENANT_A}, headers=H, json={"status": "off"})
    assert not any(x.startswith("policy") for x in up()["validation_reasons"])
    assert c.get("/v1/ocr/policies", params={"tenant": TENANT_A}, headers=H).json()["items"][0]["status"] == "off"
    assert c.delete(f"/v1/ocr/policies/{body['id']}", params={"tenant": TENANT_A}, headers=H).status_code == 204


def test_other_tenant_cannot_see_or_change_it(policy_client):
    c = policy_client
    r = c.post("/v1/ocr/policies", files={"file": ("policy.txt", POLICY_TXT, "text/plain")},
               data={"tenant": TENANT_A}, headers=H)
    pid = _wait_draft(c, r.json()["id"])["id"]
    other = "99999999-2222-3333-4444-555555555555"
    assert c.get(f"/v1/ocr/policies/{pid}", params={"tenant": other}, headers=H).status_code == 404
    assert c.put(f"/v1/ocr/policies/{pid}", params={"tenant": other}, headers=H,
                 json={"status": "active"}).status_code == 404


def test_unreadable_upload_and_unconfigured(policy_client, monkeypatch):
    c = policy_client
    r = c.post("/v1/ocr/policies", files={"file": ("policy.exe", b"MZ....", "application/octet-stream")},
               data={"tenant": TENANT_A}, headers=H)
    assert r.status_code == 400
    monkeypatch.setattr(settings, "policy_llm_base_url", "")
    r = c.post("/v1/ocr/policies", files={"file": ("policy.txt", POLICY_TXT, "text/plain")},
               data={"tenant": TENANT_A}, headers=H)
    assert r.status_code == 503


def _mock_with(fields):
    real = main_module.mock_extraction

    def fake(kind, data, mime, model):
        ex = real(kind, data, mime, model)
        if kind == "pay_stub":
            ex.fields = dict(fields)
        return ex

    return fake
