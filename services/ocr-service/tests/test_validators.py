"""SPEC-W115 C2: deterministic content-validation verdicts (pure module).

Every rule, verdict precedence, unknown-kind honesty, the duplicate flag,
and non-numeric coercion — no service, no I/O (GATE R1/R4).
"""

from __future__ import annotations

import sys

sys.path.insert(0, ".")

from app.validators import (  # noqa: E402
    NEEDS_REVIEW,
    REJECTED,
    VALIDATED,
    validate_document,
)

GOOD_STUB = {
    "employer_name": "ACME STORES LTD",
    "employee_name": "ADA LOVELACE",
    "pay_period_start": "2026-10-01",
    "pay_period_end": "2026-10-31",
    "pay_date": "2026-10-31",
    "gross_pay": 250000.0,
    "net_pay": 210000.0,
    "deductions_total": 40000.0,
    "currency": "NGN",
}


def test_consistent_stub_validates():
    verdict, reasons = validate_document("pay_stub", dict(GOOD_STUB))
    assert verdict == VALIDATED
    assert reasons == []


def test_string_numbers_coerce():
    fields = dict(GOOD_STUB, gross_pay="250,000.00", net_pay="210000",
                  deductions_total="40,000")
    verdict, reasons = validate_document("pay_stub", fields)
    assert verdict == VALIDATED
    assert reasons == []


def test_missing_each_required_field_rejects():
    for name in ("employer_name", "employee_name", "gross_pay", "net_pay"):
        fields = dict(GOOD_STUB)
        fields[name] = None
        verdict, reasons = validate_document("pay_stub", fields)
        assert verdict == REJECTED
        assert f"missing_required_field:{name}" in reasons


def test_empty_string_required_field_rejects():
    fields = dict(GOOD_STUB, employer_name="   ")
    verdict, reasons = validate_document("pay_stub", fields)
    assert verdict == REJECTED
    assert "missing_required_field:employer_name" in reasons


def test_net_exceeds_gross_rejects():
    fields = dict(GOOD_STUB, net_pay=300000.0)
    verdict, reasons = validate_document("pay_stub", fields)
    assert verdict == REJECTED
    assert "net_exceeds_gross" in reasons


def test_arithmetic_inconsistent_needs_review():
    # gross - deductions - net = 250000 - 40000 - 100000 = 110000 off
    fields = dict(GOOD_STUB, net_pay=100000.0)
    verdict, reasons = validate_document("pay_stub", fields)
    assert verdict == NEEDS_REVIEW
    assert "arithmetic_inconsistent" in reasons


def test_arithmetic_within_one_percent_passes():
    fields = dict(GOOD_STUB, net_pay=211000.0)  # 1000 off = 0.4% of gross
    verdict, _ = validate_document("pay_stub", fields)
    assert verdict == VALIDATED


def test_arithmetic_skipped_without_deductions():
    fields = dict(GOOD_STUB)
    del fields["deductions_total"]
    fields["net_pay"] = 100000.0
    verdict, reasons = validate_document("pay_stub", fields)
    assert "arithmetic_inconsistent" not in reasons


def test_period_inverted_rejects():
    fields = dict(GOOD_STUB, pay_period_start="2026-10-31",
                  pay_period_end="2026-10-01")
    verdict, reasons = validate_document("pay_stub", fields)
    assert verdict == REJECTED
    assert "period_inverted" in reasons


def test_period_unparseable_needs_review():
    fields = dict(GOOD_STUB, pay_period_start="31/10/2026")
    verdict, reasons = validate_document("pay_stub", fields)
    assert verdict == NEEDS_REVIEW
    assert "period_unparseable" in reasons


def test_missing_currency_needs_review():
    for bad in (None, "", "Naira", "NG"):
        fields = dict(GOOD_STUB, currency=bad)
        verdict, reasons = validate_document("pay_stub", fields)
        assert verdict == NEEDS_REVIEW
        assert "currency_missing" in reasons


def test_worst_verdict_wins():
    # rejected (net>gross) + needs_review (currency) => rejected
    fields = dict(GOOD_STUB, net_pay=999999.0, currency=None)
    verdict, reasons = validate_document("pay_stub", fields)
    assert verdict == REJECTED
    assert "net_exceeds_gross" in reasons
    assert "currency_missing" in reasons


def test_unknown_kind_is_never_claimed_validated():
    for kind in ("receipt", "document", "knowledge_scan", "kyc_passport"):
        verdict, reasons = validate_document(kind, {"vendor": "x"})
        assert verdict == NEEDS_REVIEW
        assert reasons == ["no_validator_for_kind"]


def test_none_fields_tolerated():
    verdict, reasons = validate_document("pay_stub", None)
    assert verdict == REJECTED
    assert any(r.startswith("missing_required_field:") for r in reasons)


def test_registry_duplicate_flag_adds_reason():
    verdict, reasons = validate_document(
        "pay_stub", dict(GOOD_STUB), registry_duplicate=True
    )
    assert verdict == NEEDS_REVIEW
    assert reasons == ["duplicate_seen_cross_tenant"]


def test_registry_duplicate_never_downgrades_rejected():
    fields = dict(GOOD_STUB, net_pay=999999.0)
    verdict, reasons = validate_document(
        "pay_stub", fields, registry_duplicate=True
    )
    assert verdict == REJECTED
    assert "duplicate_seen_cross_tenant" in reasons
