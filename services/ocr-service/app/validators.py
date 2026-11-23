"""SPEC-W115 C2: deterministic content-validation verdicts.

The extraction model TRANSCRIBES; validation is deterministic code (the
SPEC-W58 P7 MRZ ruling generalized): a verdict is computed from the
whitelisted extracted fields, never claimed by the model.

Verdict semantics (honest, heuristic — NOT forgery-proof authentication):

  - "validated":    every applicable consistency check passed.
  - "needs_review": extraction succeeded but something is off or
                    unverifiable; a human should look. Also the verdict
                    for kinds with no dedicated validator.
  - "rejected":     the document contradicts itself or is unusable for
                    its declared purpose (missing required fields,
                    impossible arithmetic, inverted period).

Pure functions, no I/O — unit-testable without the service (GATE R1).
"""

from __future__ import annotations

import re
from typing import Any

Verdict = str  # "validated" | "needs_review" | "rejected"

VALIDATED: Verdict = "validated"
NEEDS_REVIEW: Verdict = "needs_review"
REJECTED: Verdict = "rejected"

#: Worst-of precedence for combining rule outcomes.
_SEVERITY = {VALIDATED: 0, NEEDS_REVIEW: 1, REJECTED: 2}

#: pay_stub required fields (SPEC-W115 C2). A missing one is a REJECT —
#: the caller declared the document's purpose, and the purpose cannot be
#: served without these.
PAY_STUB_REQUIRED = ("employer_name", "employee_name", "gross_pay", "net_pay")

_ISO_DATE_RE = re.compile(r"^(\d{4})-(\d{2})-(\d{2})")
_CURRENCY_RE = re.compile(r"^[A-Za-z]{3}$")


def _worst(verdicts: list[Verdict]) -> Verdict:
    return max(verdicts, key=lambda v: _SEVERITY[v], default=VALIDATED)


def _as_number(value: Any) -> float | None:
    """llm._as_number idiom: tolerant coercion, bool excluded."""
    if isinstance(value, (int, float)) and not isinstance(value, bool):
        return float(value)
    if isinstance(value, str):
        try:
            return float(value.replace(",", "").strip())
        except ValueError:
            return None
    return None


def _iso_date_tuple(value: Any) -> tuple[int, int, int] | None:
    """Parse a 'YYYY-MM-DD' (or ISO datetime prefix) into a comparable
    tuple; None when absent/unparseable/impossible."""
    if not isinstance(value, str):
        return None
    m = _ISO_DATE_RE.match(value.strip())
    if not m:
        return None
    y, mo, d = int(m.group(1)), int(m.group(2)), int(m.group(3))
    if not (1 <= mo <= 12 and 1 <= d <= 31):
        return None
    return (y, mo, d)


def _validate_pay_stub(fields: dict[str, Any]) -> tuple[Verdict, list[str]]:
    """Pay-stub consistency rules (SPEC-W115 C2). Each rule contributes a
    (verdict, reason) pair; the document verdict is the worst."""
    verdicts: list[Verdict] = []
    reasons: list[str] = []

    # 1) required fields present and non-empty
    for name in PAY_STUB_REQUIRED:
        value = fields.get(name)
        if value is None or (isinstance(value, str) and not value.strip()):
            verdicts.append(REJECTED)
            reasons.append(f"missing_required_field:{name}")

    # 2) net <= gross (both numeric)
    gross = _as_number(fields.get("gross_pay"))
    net = _as_number(fields.get("net_pay"))
    if gross is not None and net is not None and net > gross:
        verdicts.append(REJECTED)
        reasons.append("net_exceeds_gross")

    # 3) arithmetic consistency when deductions_total is present:
    #    |gross - deductions - net| <= 1% of gross
    deductions = _as_number(fields.get("deductions_total"))
    if gross is not None and net is not None and deductions is not None and gross > 0:
        if abs(gross - deductions - net) > 0.01 * gross:
            verdicts.append(NEEDS_REVIEW)
            reasons.append("arithmetic_inconsistent")

    # 4) pay-period sanity
    start = _iso_date_tuple(fields.get("pay_period_start"))
    end = _iso_date_tuple(fields.get("pay_period_end"))
    raw_start = fields.get("pay_period_start")
    raw_end = fields.get("pay_period_end")
    if (raw_start and start is None) or (raw_end and end is None):
        verdicts.append(NEEDS_REVIEW)
        reasons.append("period_unparseable")
    elif start is not None and end is not None and end < start:
        verdicts.append(REJECTED)
        reasons.append("period_inverted")

    # 5) currency present and ISO-4217-shaped
    currency = fields.get("currency")
    if not isinstance(currency, str) or not _CURRENCY_RE.match(currency.strip()):
        verdicts.append(NEEDS_REVIEW)
        reasons.append("currency_missing")

    return _worst(verdicts), reasons


def validate_document(
    kind: str,
    extracted_fields: dict[str, Any] | None,
    *,
    registry_duplicate: bool = False,
) -> tuple[Verdict, list[str]]:
    """Compute the (verdict, reasons) pair for one extracted document.

    `registry_duplicate` marks a cross-tenant fingerprint match (SPEC-W67).
    NOTE (disclosure gate): the LIVE extract path never wires this input —
    SPEC-W67 G5 keeps cross-tenant match knowledge out of caller-visible
    responses; the staff alert channel carries it. The input exists for
    staff-side re-validation tooling and is unit-tested here.
    """
    fields = extracted_fields if isinstance(extracted_fields, dict) else {}
    if kind == "pay_stub":
        verdict, reasons = _validate_pay_stub(fields)
    else:
        # Kinds without a dedicated validator are honestly unverifiable —
        # never claim "validated" for something nobody checked.
        verdict, reasons = NEEDS_REVIEW, ["no_validator_for_kind"]

    if registry_duplicate and "duplicate_seen_cross_tenant" not in reasons:
        reasons.append("duplicate_seen_cross_tenant")
        verdict = _worst([verdict, NEEDS_REVIEW])

    return verdict, reasons
