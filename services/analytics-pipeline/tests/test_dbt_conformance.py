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


def test_usage_events_columns_match_dbt():
    if not os.path.exists(DBT_SCHEMA):
        print("SKIP: dbt schema.yml not present in this checkout")
        return
    assert list(USAGE_EVENT_COLUMNS) == _dbt_source_columns("usage_events")


if __name__ == "__main__":
    for name, fn in sorted({k: v for k, v in globals().items() if k.startswith("test_")}.items()):
        fn()
        print(f"PASS {name}")
    print("all conformance tests passed")
