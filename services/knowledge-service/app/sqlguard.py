"""sqlguard — guardrails for the text-to-SQL endpoint (SPEC-W3 §3, innovation 8).

AST-lite SQL hardening without a parser dependency:

* single SELECT only (no WITH, no second statement, no trailing junk);
* rejects ``;`` (statement chaining) and ``--``/``/*`` comments OUTSIDE
  string literals (string contents are masked before scanning);
* rejects DDL/DML keywords outside strings (drop/insert/update/delete/...);
* rejects set operations outright — UNION/INTERSECT/EXCEPT would let a
  second, non-tenant-scoped SELECT branch smuggle cross-tenant rows past
  the single injected top-level predicate (SPEC-W43 Y-02);
* rejects subselects that lack their own ``tenant_id =`` predicate — the
  platform predicate is injected at the TOP level only, so every
  parenthesized SELECT must filter the tenant itself (SPEC-W43 Y-02);
* table allowlist — only the four gold analytics tables may be referenced
  (bare, ``gold.``- or ``iceberg.gold.``-qualified);
* injects the tenant predicate ``tenant_id = '<tenant>'`` (WHERE ... or AND)
  at the top level so cross-tenant reads are impossible;
* enforces ``LIMIT 500`` (appends when missing, clamps when larger).

The validator raises SqlGuardError with a human-readable reason; callers
return 400 together with the offending LLM SQL for debugging.
"""

from __future__ import annotations

import re

MAX_LIMIT = 500

# Basenames of the gold-layer analytics tables the LLM may query.
ALLOWED_TABLES = frozenset(
    {
        "daily_bookings_per_tenant",
        "revenue_daily",
        "no_show_rate",
        "agent_containment_rate",
    }
)

_FORBIDDEN_KEYWORDS = frozenset(
    {
        "insert",
        "update",
        "delete",
        "drop",
        "alter",
        "create",
        "truncate",
        "grant",
        "revoke",
        "merge",
        "upsert",
        "replace",
        "call",
        "execute",
        "exec",
        "set",
        "use",
        "show",
        "describe",
        "explain",
        "analyze",
        "vacuum",
        "comment",
        "rename",
        "commit",
        "rollback",
        "start",
        "with",
        # SPEC-W43 Y-02: set operations are rejected outright — a UNION
        # branch would bypass the single top-level tenant predicate.
        "union",
        "intersect",
        "except",
    }
)

_CLAUSE_KEYWORDS = ("where", "group by", "having", "order by", "limit", "offset")
_TABLE_REF_RE = re.compile(r"\b(?:from|join)\s+([a-zA-Z_][\w$]*(?:\.[a-zA-Z_][\w$]*)*)", re.IGNORECASE)
_WORD_RE = re.compile(r"\b([a-zA-Z_][\w$]*)\b")
_LIMIT_RE = re.compile(r"\blimit\s+(\d+)", re.IGNORECASE)


class SqlGuardError(ValueError):
    """Raised when an LLM-generated statement violates the guardrails."""

    def __init__(self, reason: str, sql: str):
        super().__init__(reason)
        self.reason = reason
        self.sql = sql


def _mask_strings(sql: str) -> str:
    """Replace the CONTENTS of '...' string literals with spaces (positions
    preserved), so keyword/comment/semicolon scans never look inside strings.
    Handles '' escaping; double-quoted identifiers are masked too."""
    out = list(sql)
    i, n = 0, len(sql)
    while i < n:
        if sql[i] in "'\"":
            quote = sql[i]
            i += 1
            while i < n:
                if sql[i] == quote:
                    # doubled quote = escaped literal quote
                    if i + 1 < n and sql[i + 1] == quote:
                        out[i] = out[i + 1] = " "
                        i += 2
                        continue
                    break
                out[i] = " "
                i += 1
        i += 1
    return "".join(out)


def _word_boundary(s: str, start: int, end: int) -> bool:
    left = start == 0 or not (s[start - 1].isalnum() or s[start - 1] == "_")
    right = end >= len(s) or not (s[end].isalnum() or s[end] == "_")
    return left and right


def _top_level_clause_positions(masked: str) -> list[tuple[int, str]]:
    """Positions of clause keywords (WHERE/GROUP BY/...) at paren depth 0,
    so subqueries never confuse the tenant-predicate injection."""
    lower = masked.lower()
    hits: list[tuple[int, str]] = []
    depth = 0
    i, n = 0, len(lower)
    while i < n:
        ch = lower[i]
        if ch == "(":
            depth += 1
            i += 1
            continue
        if ch == ")":
            depth = max(0, depth - 1)
            i += 1
            continue
        if depth == 0 and ch.isalpha():
            for kw in _CLAUSE_KEYWORDS:
                if lower.startswith(kw, i) and _word_boundary(lower, i, i + len(kw)):
                    hits.append((i, kw))
                    i += len(kw)
                    break
            else:
                i += 1
            continue
        i += 1
    return hits


_SUBSELECT_TENANT_RE = re.compile(r"\btenant_id\b\s*=", re.IGNORECASE)


def _check_subselect_tenant_predicate(masked: str, sql: str) -> None:
    """Every parenthesized SELECT (subselect) must carry its own
    ``tenant_id =`` predicate (SPEC-W43 Y-02): the platform-injected
    predicate only guards the top-level query, and an unfiltered subselect
    over a gold table would read across tenants."""
    depth = 0
    stack: list[int] = []
    groups: list[str] = []
    for i, ch in enumerate(masked):
        if ch == "(":
            stack.append(i)
            depth += 1
        elif ch == ")":
            if stack:
                start = stack.pop()
                groups.append(masked[start + 1:i])
            depth = max(0, depth - 1)
    for group in groups:
        if re.search(r"\bselect\b", group, re.IGNORECASE) and (
            not _SUBSELECT_TENANT_RE.search(group)
        ):
            raise SqlGuardError(
                "subquery must filter tenant_id itself: subselect without a "
                "tenant_id = predicate is not allowed",
                sql,
            )


def _escape_literal(value: str) -> str:
    return value.replace("'", "''")


def validate_and_bind(sql: str, tenant_id: str) -> str:
    """Validate LLM SQL and return the executable, tenant-scoped statement.

    Raises SqlGuardError(reason, sql) on any violation.
    """
    if not sql or not sql.strip():
        raise SqlGuardError("empty SQL", sql)
    stmt = sql.strip()
    # Tolerate exactly one trailing semicolon (LLM habit); anything else is
    # statement chaining and rejected below.
    if stmt.endswith(";"):
        stmt = stmt[:-1].rstrip()

    masked = _mask_strings(stmt)

    if ";" in masked:
        raise SqlGuardError("multiple statements are not allowed", sql)
    if "--" in masked or "/*" in masked or "*/" in masked:
        raise SqlGuardError("SQL comments are not allowed", sql)

    stripped = masked.lstrip()
    if not re.match(r"select\b", stripped, re.IGNORECASE):
        raise SqlGuardError("only a single SELECT statement is allowed", sql)

    for m in _WORD_RE.finditer(masked):
        word = m.group(1).lower()
        if word in _FORBIDDEN_KEYWORDS:
            raise SqlGuardError(f"forbidden keyword outside strings: {word!r}", sql)

    for m in _TABLE_REF_RE.finditer(masked):
        ref = m.group(1)
        basename = ref.split(".")[-1].lower()
        if basename not in ALLOWED_TABLES:
            raise SqlGuardError(f"table not in gold allowlist: {ref!r}", sql)

    # SPEC-W43 Y-02: subselects must carry their own tenant predicate.
    _check_subselect_tenant_predicate(masked, sql)

    # --- inject the tenant predicate at the top level -------------------
    predicate = f"tenant_id = '{_escape_literal(tenant_id)}'"
    clauses = _top_level_clause_positions(masked)
    where_pos = next((p for p, kw in clauses if kw == "where"), None)
    if where_pos is not None:
        insert_at = where_pos + len("where")
        stmt = stmt[:insert_at] + f" {predicate} AND" + stmt[insert_at:]
        masked = _mask_strings(stmt)
        clauses = _top_level_clause_positions(masked)
    else:
        first_clause = next(
            (p for p, kw in clauses if kw in ("group by", "having", "order by", "limit", "offset")),
            None,
        )
        if first_clause is not None:
            stmt = stmt[:first_clause] + f"WHERE {predicate} " + stmt[first_clause:]
        else:
            stmt = stmt + f" WHERE {predicate}"
        masked = _mask_strings(stmt)

    # --- enforce LIMIT 500 ----------------------------------------------
    clauses = _top_level_clause_positions(masked)
    limit_pos = next((p for p, kw in clauses if kw == "limit"), None)
    if limit_pos is not None:
        m = _LIMIT_RE.match(masked, limit_pos)
        if m and int(m.group(1)) > MAX_LIMIT:
            # match offsets are absolute (match on the full string)
            stmt = stmt[: m.start(1)] + str(MAX_LIMIT) + stmt[m.end(1):]
    else:
        stmt = stmt + f" LIMIT {MAX_LIMIT}"

    return stmt
