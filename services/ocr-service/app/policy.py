"""Policy rules (2026-11-26): a business's own document checks, read from its policy document.

A business uploads a policy (PDF / Word / text). A language model READS it and proposes rules in a small fixed
vocabulary -- a field the extractor already reads (pay_date, gross_pay, currency...) and a check (min, max,
max_age_days...) -- each with the policy sentence it came from. Staff review the rules and turn them on. From then on
every caller document of that kind is checked against them here, deterministically: the model never judges a caller's
document, it only drafted the rules a person approved (the SPEC-W115 "the model transcribes, code decides" ruling).

Pure functions, no I/O.
"""

from __future__ import annotations

import json
import re
from datetime import date
from typing import Any

from .validators import NEEDS_REVIEW, REJECTED, VALIDATED, _as_number, _iso_date_tuple, _worst

#: What a rule may look at: the fields the extractor reads per kind, and their type.
POLICY_FIELDS: dict[str, dict[str, str]] = {
    "pay_stub": {
        "employer_name": "text",
        "employee_name": "text",
        "pay_period_start": "date",
        "pay_period_end": "date",
        "pay_date": "date",
        "gross_pay": "number",
        "net_pay": "number",
        "deductions_total": "number",
        "currency": "text",
    },
    "receipt": {"vendor": "text", "amount": "number", "date": "date", "currency": "text"},
}

KIND_LABELS = {"pay_stub": "pay stub", "receipt": "receipt"}

#: check -> field types it applies to, and whether it needs a value.
CHECKS: dict[str, tuple[tuple[str, ...], bool]] = {
    "required": (("text", "date", "number"), False),
    "min": (("number",), True),
    "max": (("number",), True),
    "equals": (("text",), True),
    "one_of": (("text",), True),
    "max_age_days": (("date",), True),
    "not_in_future": (("date",), False),
    "period_max_days": (("date",), True),  # pay_stub: pay_period_end - pay_period_start
}

SEVERITIES = {"reject": REJECTED, "review": NEEDS_REVIEW}
MAX_RULES = 20
MAX_TEXT = 200


def _clip(s: Any, n: int = MAX_TEXT) -> str:
    return re.sub(r"\s+", " ", str(s or "")).strip()[:n]


def normalize_rules(raw: Any) -> list[dict[str, Any]]:
    """Keep only well-formed rules in the vocabulary; give each a stable id. Anything else the model wrote is dropped
    (never executed): unknown kinds, fields, checks, values of the wrong type."""
    items = raw.get("rules") if isinstance(raw, dict) else raw
    if not isinstance(items, list):
        return []
    out: list[dict[str, Any]] = []
    seen: set[tuple] = set()
    for item in items:
        if not isinstance(item, dict):
            continue
        kind = str(item.get("document") or item.get("kind") or "")
        field, check = str(item.get("field") or ""), str(item.get("check") or "")
        ftype = POLICY_FIELDS.get(kind, {}).get(field)
        spec = CHECKS.get(check)
        if ftype is None or spec is None or ftype not in spec[0]:
            continue
        if check == "period_max_days" and (kind != "pay_stub" or field != "pay_period_end"):
            continue
        value: Any = None
        if spec[1]:
            v = item.get("value")
            if check in ("min", "max", "max_age_days", "period_max_days"):
                value = _as_number(v)
                if value is None or value < 0 or (check != "min" and check != "max" and value > 3650):
                    continue
                if check in ("max_age_days", "period_max_days"):
                    value = int(value)
            elif check == "equals":
                value = _clip(v, 80)
                if not value:
                    continue
            elif check == "one_of":
                vals = v if isinstance(v, list) else [x for x in re.split(r"[,;]", str(v or ""))]
                value = [_clip(x, 80) for x in vals if _clip(x, 80)][:20]
                if not value:
                    continue
        key = (kind, field, check, json.dumps(value, sort_keys=True))
        if key in seen:
            continue
        seen.add(key)
        severity = "reject" if str(item.get("severity") or "").lower().startswith("rej") else "review"
        text = _clip(item.get("text")) or describe(kind, field, check, value)
        out.append({
            "id": f"r{len(out) + 1}",
            "kind": kind,
            "field": field,
            "check": check,
            "value": value,
            "severity": severity,
            "text": text,
            "source": _clip(item.get("source"), 300),
            "enabled": item.get("enabled", True) is not False,
        })
        if len(out) >= MAX_RULES:
            break
    return out


def describe(kind: str, field: str, check: str, value: Any) -> str:
    """A plain sentence for a rule, when the model gave none."""
    label = KIND_LABELS.get(kind, kind)
    f = field.replace("_", " ")
    if check == "required":
        return f"The {label} must show the {f}."
    if check == "min":
        return f"The {f} on the {label} must be at least {value:,.0f}."
    if check == "max":
        return f"The {f} on the {label} must be at most {value:,.0f}."
    if check == "equals":
        return f"The {f} on the {label} must be {value}."
    if check == "one_of":
        return f"The {f} on the {label} must be one of: {', '.join(value)}."
    if check == "max_age_days":
        return f"The {label}'s {f} must be within the last {value} days."
    if check == "not_in_future":
        return f"The {label}'s {f} cannot be in the future."
    if check == "period_max_days":
        return f"The {label} must cover a pay period of at most {value} days."
    return f"{label}: {f} {check} {value}"


def _days(t: tuple[int, int, int]) -> int:
    return date(*t).toordinal()


def apply_rules(
    kind: str, fields: dict[str, Any] | None, rules: list[dict[str, Any]], *, today: date | None = None
) -> tuple[str, list[str]]:
    """Check one extracted document against the business's active rules. Returns (verdict, reasons); a broken rule
    gives reason "policy:<rule text>", a rule that cannot be checked (field not readable) gives
    "policy_unchecked:<rule text>" and needs review -- never a silent pass."""
    fields = fields if isinstance(fields, dict) else {}
    today = today or date.today()
    verdicts: list[str] = []
    reasons: list[str] = []
    for r in rules:
        if r.get("kind") != kind or not r.get("enabled", True):
            continue
        field, check, value = r.get("field"), r.get("check"), r.get("value")
        raw = fields.get(field)
        present = raw is not None and not (isinstance(raw, str) and not raw.strip())
        broken: bool | None  # None = could not check
        if check == "required":
            broken = not present
        elif not present:
            broken = None
        elif check in ("min", "max"):
            n = _as_number(raw)
            broken = None if n is None else (n < value if check == "min" else n > value)
        elif check == "equals":
            broken = _clip(raw, 80).lower() != str(value).lower()
        elif check == "one_of":
            broken = _clip(raw, 80).lower() not in {str(v).lower() for v in value}
        elif check in ("max_age_days", "not_in_future"):
            d = _iso_date_tuple(raw)
            if d is None:
                broken = None
            else:
                age = today.toordinal() - _days(d)
                broken = age < 0 if check == "not_in_future" else (age > value or age < 0)
        elif check == "period_max_days":
            s, e = _iso_date_tuple(fields.get("pay_period_start")), _iso_date_tuple(raw)
            broken = None if s is None or e is None else (_days(e) - _days(s) + 1) > value
        else:
            continue
        if broken is None:
            verdicts.append(NEEDS_REVIEW)
            reasons.append(f"policy_unchecked:{r.get('text')}")
        elif broken:
            verdicts.append(SEVERITIES.get(r.get("severity"), NEEDS_REVIEW))
            reasons.append(f"policy:{r.get('text')}")
    return (_worst(verdicts) if verdicts else VALIDATED), reasons


def combine(base: tuple[str, list[str]], policy: tuple[str, list[str]]) -> tuple[str, list[str]]:
    """Worst verdict; the policy reasons first (they are what the business asked for)."""
    return _worst([base[0], policy[0]]), policy[1] + [r for r in base[1] if r not in policy[1]]


def extraction_prompt(policy_text: str) -> list[dict[str, str]]:
    """Chat messages asking the model to draft rules from a policy. The documents, fields and checks are spelled out
    with one worked example of the format; its answer still goes through normalize_rules."""
    docs = "\n".join(
        f'- document "{kind}": fields ' + ", ".join(f'"{f}" ({t})' for f, t in fs.items())
        for kind, fs in POLICY_FIELDS.items()
    )
    system = (
        "You turn a business's policy into checks on documents its customers send. Each rule has these keys:\n"
        '  "document": which document it applies to -- "pay_stub" (payslip) or "receipt"\n'
        '  "field": one field of that document:\n'
        f"{docs}\n"
        '  "check": one of\n'
        '    "required" (the field must be shown; no value),\n'
        '    "min" / "max" (number fields; value = the amount as a plain number, ₦70,000 -> 70000),\n'
        '    "equals" / "one_of" (text fields; value = the text, or a list of texts, e.g. "NGN"),\n'
        '    "max_age_days" (date fields; value = days: 3 months = 92, 6 months = 183, 30 days = 30),\n'
        '    "not_in_future" (date fields; no value),\n'
        '    "period_max_days" (only document "pay_stub", field "pay_period_end": the pay period may cover at most '
        "value days)\n"
        '  "value": as above\n'
        '  "severity": "reject" when the policy says such a document is not accepted / refused, otherwise "review"\n'
        '  "text": one short plain sentence a customer would understand\n'
        '  "source": the exact sentence of the policy the rule comes from\n'
        "Example of the FORMAT only (do not copy its content): "
        '{"document": "receipt", "field": "amount", "check": "max", "value": 250000, "severity": "review", '
        '"text": "Receipts above ₦250,000 are reviewed.", "source": "Any receipt above N250,000 must be reviewed."}\n'
        "Only write rules the policy clearly states and these fields can check; write one rule per requirement; skip "
        "anything else. Answer with JSON only: {\"rules\": [ ... ]}"
    )
    return [
        {"role": "system", "content": system},
        {"role": "user", "content": "POLICY DOCUMENT:\n\n" + policy_text[:24000]},
    ]


def parse_model_json(text: str) -> Any:
    """The model's JSON answer, tolerating code fences and prose around it."""
    t = (text or "").strip()
    t = re.sub(r"^```(?:json)?\s*|\s*```$", "", t)
    try:
        return json.loads(t)
    except ValueError:
        m = re.search(r"\{.*\}", t, re.S)
        if m:
            try:
                return json.loads(m.group(0))
            except ValueError:
                return None
    return None
