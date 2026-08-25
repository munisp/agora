"""sqlguard unit tests (SPEC-W3 §3 innovation 8) — no LLM, no Trino needed."""

from __future__ import annotations

import pytest

from app.sqlguard import SqlGuardError, validate_and_bind

TENANT = "11111111-2222-3333-4444-555555555555"


def test_simple_select_gets_where_and_limit():
    out = validate_and_bind("SELECT day, bookings_created FROM gold.daily_bookings_per_tenant", TENANT)
    assert f"WHERE tenant_id = '{TENANT}'" in out
    assert out.endswith("LIMIT 500")
    assert "bookings_created" in out


def test_existing_where_gets_and_predicate():
    out = validate_and_bind(
        "SELECT day FROM gold.revenue_daily WHERE day > current_date - interval '7' day ORDER BY day",
        TENANT,
    )
    assert f"WHERE tenant_id = '{TENANT}' AND day > current_date" in out
    assert "ORDER BY day" in out


def test_predicate_injected_before_group_by():
    out = validate_and_bind(
        "SELECT currency, sum(net_revenue_cents) FROM gold.revenue_daily GROUP BY currency",
        TENANT,
    )
    assert f"WHERE tenant_id = '{TENANT}' GROUP BY currency" in out


def test_large_limit_is_clamped():
    out = validate_and_bind("SELECT day FROM gold.no_show_rate LIMIT 100000", TENANT)
    assert "LIMIT 500" in out
    assert "100000" not in out


def test_small_limit_is_kept():
    out = validate_and_bind("SELECT day FROM gold.no_show_rate LIMIT 10", TENANT)
    assert "LIMIT 10" in out


def test_trailing_semicolon_tolerated_but_chaining_rejected():
    out = validate_and_bind("SELECT day FROM gold.revenue_daily;", TENANT)
    assert out.count(";") == 0
    with pytest.raises(SqlGuardError, match="multiple statements"):
        validate_and_bind("SELECT day FROM gold.revenue_daily; SELECT 1", TENANT)


def test_comments_rejected():
    with pytest.raises(SqlGuardError, match="comments"):
        validate_and_bind("SELECT day FROM gold.revenue_daily -- drop everything", TENANT)
    with pytest.raises(SqlGuardError, match="comments"):
        validate_and_bind("SELECT day /* sneaky */ FROM gold.revenue_daily", TENANT)


def test_ddl_dml_rejected():
    for bad in (
        "DROP TABLE gold.revenue_daily",
        "DELETE FROM gold.revenue_daily",
        "UPDATE gold.revenue_daily SET day = current_date",
        "INSERT INTO gold.revenue_daily VALUES (1)",
        "SELECT day FROM gold.revenue_daily WHERE 1=1; DROP TABLE gold.revenue_daily",
        "WITH x AS (SELECT 1) SELECT * FROM x",
    ):
        with pytest.raises(SqlGuardError):
            validate_and_bind(bad, TENANT)


def test_non_select_rejected():
    with pytest.raises(SqlGuardError, match="single SELECT"):
        validate_and_bind("SHOW TABLES", TENANT)


def test_table_allowlist_enforced():
    with pytest.raises(SqlGuardError, match="allowlist"):
        validate_and_bind("SELECT * FROM silver.bookings", TENANT)
    with pytest.raises(SqlGuardError, match="allowlist"):
        validate_and_bind(
            "SELECT * FROM gold.revenue_daily JOIN pg_catalog.pg_tables ON true", TENANT
        )
    # catalog-qualified allowlisted table is fine
    out = validate_and_bind("SELECT day FROM iceberg.gold.agent_containment_rate", TENANT)
    assert "iceberg.gold.agent_containment_rate" in out


def test_keywords_inside_strings_are_allowed():
    out = validate_and_bind(
        "SELECT day FROM gold.revenue_daily WHERE currency = 'US;D -- drop'",
        TENANT,
    )
    assert "'US;D -- drop'" in out


def test_subquery_without_tenant_predicate_rejected():
    """SPEC-W43 Y-02: the platform predicate is injected at the TOP level
    only, so a subselect lacking its own tenant_id predicate would read
    across tenants — rejected outright."""
    sql = (
        "SELECT day FROM gold.daily_bookings_per_tenant "
        "WHERE bookings_created > (SELECT avg(bookings_created) FROM gold.daily_bookings_per_tenant)"
    )
    with pytest.raises(SqlGuardError, match="subquery must filter tenant_id"):
        validate_and_bind(sql, TENANT)


def test_subquery_with_tenant_predicate_allowed():
    sql = (
        "SELECT day FROM gold.daily_bookings_per_tenant "
        f"WHERE bookings_created > (SELECT avg(bookings_created) FROM gold.daily_bookings_per_tenant WHERE tenant_id = '{TENANT}')"
    )
    out = validate_and_bind(sql, TENANT)
    # top-level injection still happens (into the OUTER where)
    assert f"WHERE tenant_id = '{TENANT}' AND bookings_created > (SELECT" in out
    # and the subselect kept its own predicate
    assert out.count(f"tenant_id = '{TENANT}'") == 2


def test_set_operations_rejected():
    """SPEC-W43 Y-02: UNION/INTERSECT/EXCEPT branches would bypass the
    single injected top-level tenant predicate."""
    for op in ("UNION", "UNION ALL", "INTERSECT", "EXCEPT"):
        bad = (
            "SELECT day FROM gold.revenue_daily "
            f"{op} SELECT day FROM gold.no_show_rate"
        )
        with pytest.raises(SqlGuardError, match="forbidden keyword"):
            validate_and_bind(bad, TENANT)
    # ...even lowercase and even when the second branch also names an
    # allowlisted table (allowlist alone is not tenant isolation).
    with pytest.raises(SqlGuardError, match="forbidden keyword"):
        validate_and_bind(
            "SELECT day FROM gold.revenue_daily union select day from gold.revenue_daily",
            TENANT,
        )


def test_tenant_id_is_escaped():
    out = validate_and_bind("SELECT day FROM gold.revenue_daily", "o'neil")
    assert "tenant_id = 'o''neil'" in out


def test_empty_sql_rejected():
    with pytest.raises(SqlGuardError):
        validate_and_bind("   ", TENANT)
