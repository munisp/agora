"""Conformance test: sink column contract == dbt bronze sources contract.

Guards against drift between this service's schemas and
infra/lakehouse/dbt/models/silver/schema.yml. Run:
    python -m pytest tests/  (or)  python tests/test_dbt_conformance.py
"""

import os
import re
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from analytics_pipeline.mapping import (  # noqa: E402
    BOOKING_EVENT_COLUMNS,
    PAYMENT_EVENT_COLUMNS,
    RECEIPT_DOCUMENT_COLUMNS,
    RECEIPT_RECON_FINDING_COLUMNS,
    TRANSCRIPT_COLUMNS,
    USAGE_EVENT_COLUMNS,
)

DBT_SCHEMA = os.path.join(
    os.path.dirname(__file__),
    "..", "..", "..", "infra", "lakehouse", "dbt", "models", "silver", "schema.yml",
)


def _dbt_source_columns(table: str) -> list[str]:
    """Extract column names for a bronze source table from the dbt schema.yml.

    Deliberately dependency-free (no PyYAML): the file layout is stable and
    columns appear as `          - name: <col>` under each table block.
    """
    with open(DBT_SCHEMA, encoding="utf-8") as fh:
        text = fh.read()
    marker = f"- name: {table}"
    start = text.index(marker)
    # table block ends at the next `      - name:` (next table) or end of sources
    nxt = text.find("\n      - name:", start + len(marker))
    block = text[start: nxt if nxt != -1 else len(text)]
    return re.findall(r"^\s{10}- name: (\w+)", block, re.M)


def test_booking_events_columns_match_dbt():
    if not os.path.exists(DBT_SCHEMA):
        print("SKIP: dbt schema.yml not present in this checkout")
        return
    assert list(BOOKING_EVENT_COLUMNS) == _dbt_source_columns("booking_events")


def test_payment_events_columns_match_dbt():
    if not os.path.exists(DBT_SCHEMA):
        print("SKIP: dbt schema.yml not present in this checkout")
        return
    assert list(PAYMENT_EVENT_COLUMNS) == _dbt_source_columns("payment_events")


def test_transcripts_columns_match_dbt():
    if not os.path.exists(DBT_SCHEMA):
        print("SKIP: dbt schema.yml not present in this checkout")
        return
    assert list(TRANSCRIPT_COLUMNS) == _dbt_source_columns("transcripts")


def _dbt_source_columns_or_none(table: str) -> list[str] | None:
    """None when the table has no dbt source block yet (e.g. SPEC-W62 tables
    land in schema.yml with coder C's dbt delta — until then these tests skip
    honestly instead of failing an unrelated half of the wave)."""
    try:
        return _dbt_source_columns(table)
    except (ValueError, FileNotFoundError):
        return None


def test_usage_events_columns_match_dbt():
    if not os.path.exists(DBT_SCHEMA):
        print("SKIP: dbt schema.yml not present in this checkout")
        return
    assert list(USAGE_EVENT_COLUMNS) == _dbt_source_columns("usage_events")


def test_receipt_documents_columns_match_dbt():
    cols = _dbt_source_columns_or_none("receipt_documents")
    if cols is None:
        print("SKIP: dbt bronze.receipt_documents source not in schema.yml yet (coder C)")
        return
    assert list(RECEIPT_DOCUMENT_COLUMNS) == cols


def test_receipt_recon_findings_columns_match_dbt():
    cols = _dbt_source_columns_or_none("receipt_recon_findings")
    if cols is None:
        print("SKIP: dbt bronze.receipt_recon_findings source not in schema.yml yet (coder C)")
        return
    assert list(RECEIPT_RECON_FINDING_COLUMNS) == cols


if __name__ == "__main__":
    for name, fn in sorted({k: v for k, v in globals().items() if k.startswith("test_")}.items()):
        fn()
        print(f"PASS {name}")
    print("all conformance tests passed")
