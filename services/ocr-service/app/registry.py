"""SPEC-W67 — cross-tenant document-fraud registry (platform-scoped).

THE ONLY module that may touch ocr_doc_registry / ocr_doc_registry_tenants.

Posture (lead rulings, BINDING):
  * G1: the registry is DISABLED when OCR_REGISTRY_HMAC_SECRET is unset
    (fail-closed-disabled, KYC_HASH_SECRET idiom): the extract path is
    unaffected and every upsert is a no-op decided by the CALLER; this
    module refuses to run without a secret (defense in depth).
  * G2: sha256-EXACT only. The perceptual hash NEVER enters this module
    (invertible-thumbnail disclosure + enumeration oracle cross-tenant).
  * G3: registry_key = HMAC-SHA256(OCR_REGISTRY_HMAC_SECRET, sha256).
    The raw sha256 is NEVER written to the registry tables.
  * G4: both tables are FORCE RLS with a single policy whose ONLY pass is
    the app.ocr_registry GUC. That GUC is set in EXACTLY ONE place —
    registry_conn() below (store.Database.sweep_conn() idiom: SET LOCAL
    inside a transaction, so it dies with the transaction). Request paths
    (tenant_conn) can NEVER read or write the registry. Grep gate:
    'app.ocr_registry' appears only here, in the DDL policy (store.py DDL
    + infra/postgres/init-scripts/31-ocr-schema.sql), and in tests.
  * G5: first-use-wins. Emission is decided by "did this membership add
    join a key that already has >=1 OTHER tenant" — only the LATER tenant
    is ever alerted; earlier tenants learn NOTHING.
  * G7: subject erasure never touches the registry (no subject linkage);
    TenantDelete removes ONLY the tenant's own memberships and the parent
    row is dropped when the last membership leaves (ON DELETE CASCADE
    covers the FK; the parent drop is an explicit re-count delete).

Atomicity: register_document() runs the parent upsert + membership insert
+ recount + alert-mark in ONE transaction. The parent
INSERT ... ON CONFLICT (registry_key) takes the PK row lock, which
serializes concurrent first-uploads of the same hash — the second
transaction observes the first's committed membership, so exactly one
cross-tenant match is detected per joining tenant.
"""

from __future__ import annotations

import asyncio
import hashlib
import hmac
import uuid
from contextlib import asynccontextmanager
from datetime import datetime, timedelta, timezone
from typing import Any

from prometheus_client import Counter

from .logging import get_logger

log = get_logger(__name__)

# G5 platform-side observability: counters + the audit log lines below +
# the SPEC-W77 aggregate-only registry_stats() read (count-not-identity);
# there is NO per-key/tenant read endpoint (alerts stay the triage path).
REGISTRY_KEYS = Counter(
    "opendesk_ocr_registry_keys_total",
    "Cross-tenant registry fingerprint keys created (SPEC-W67 G5).",
)
REGISTRY_MATCHES = Counter(
    "opendesk_ocr_registry_matches_total",
    "Cross-tenant exact-duplicate matches detected (SPEC-W67 G5).",
)

# SPEC-W76: TTL-membership reconciler telemetry (same counters-only
# observability pattern as the W67 counters above).
REGISTRY_TTL_MEMBERSHIPS_EXPIRED = Counter(
    "opendesk_ocr_registry_ttl_memberships_expired_total",
    "Registry memberships expired by the TTL reconciler (SPEC-W76 R1).",
)
REGISTRY_TTL_KEYS_DROPPED = Counter(
    "opendesk_ocr_registry_ttl_keys_dropped_total",
    "Registry parent rows dropped with zero memberships (SPEC-W76 R6a).",
)
REGISTRY_TTL_DRIFT_REPAIRED = Counter(
    "opendesk_ocr_registry_ttl_drift_repaired_total",
    "Registry parent tenant_count values repaired to the real count (SPEC-W76 R6b).",
)

# --- SQL (pinned semantics; tests/conftest.py fakes replicate these in ---
# --- lockstep, dispatched on these exact constants) ----------------------
SQL_UPSERT_PARENT = """
INSERT INTO ocr_doc_registry (registry_key, first_seen, last_seen, tenant_count, doc_count)
VALUES ($1, $2, $2, 0, 1)
ON CONFLICT (registry_key) DO UPDATE
   SET first_seen = LEAST(ocr_doc_registry.first_seen, EXCLUDED.first_seen),
       last_seen  = GREATEST(ocr_doc_registry.last_seen, EXCLUDED.last_seen),
       doc_count  = ocr_doc_registry.doc_count + 1
RETURNING doc_count
"""

SQL_ADD_MEMBERSHIP = """
INSERT INTO ocr_doc_registry_tenants (registry_key, tenant_id, first_seen, alerted)
VALUES ($1, $2, $3, false)
ON CONFLICT (registry_key, tenant_id) DO NOTHING
"""

SQL_RECOUNT = """
UPDATE ocr_doc_registry
   SET tenant_count = (SELECT COUNT(*) FROM ocr_doc_registry_tenants
                        WHERE registry_key = $1)
 WHERE registry_key = $1
"""

SQL_TENANT_COUNT = """
SELECT tenant_count FROM ocr_doc_registry WHERE registry_key = $1
"""

SQL_OTHER_TENANTS = """
SELECT tenant_id FROM ocr_doc_registry_tenants
 WHERE registry_key = $1 AND tenant_id <> $2
 ORDER BY tenant_id
"""

SQL_MARK_ALERTED = """
UPDATE ocr_doc_registry_tenants
   SET alerted = true
 WHERE registry_key = $1 AND tenant_id = $2
"""

SQL_DELETE_TENANT_MEMBERSHIPS = """
DELETE FROM ocr_doc_registry_tenants
 WHERE tenant_id = $1
RETURNING registry_key
"""

SQL_DROP_EMPTY_KEY = """
DELETE FROM ocr_doc_registry r
 WHERE r.registry_key = $1
   AND NOT EXISTS (SELECT 1 FROM ocr_doc_registry_tenants m
                    WHERE m.registry_key = r.registry_key)
"""

# SPEC-W76 R1: TTL-membership expiry. Membership age = the membership's own
# first_seen (immutable per membership); $1 is the age cutoff
# (now - OCR_REGISTRY_TTL_DAYS). Age-keyed, NEVER hash-keyed — no HMAC
# needed (R3). R4 (intended): an expired membership that later re-uploads
# is a NEW membership (alerted=false) and MAY alert again — the stale
# protection had lapsed.
SQL_EXPIRE_MEMBERSHIPS = """
DELETE FROM ocr_doc_registry_tenants
 WHERE first_seen < $1
RETURNING registry_key
"""

# SPEC-W76 R6: drift sweep — returns every parent row that is empty
# (zero memberships, R6a) OR whose cached tenant_count disagrees with the
# real membership count (R6b, re-count never trust). doc_count is
# cumulative history and is NEVER rewritten.
SQL_DRIFT_SWEEP = """
SELECT r.registry_key, r.tenant_count,
       (SELECT COUNT(*) FROM ocr_doc_registry_tenants m
         WHERE m.registry_key = r.registry_key) AS real_count
  FROM ocr_doc_registry r
 WHERE r.tenant_count <> (SELECT COUNT(*) FROM ocr_doc_registry_tenants m
                           WHERE m.registry_key = r.registry_key)
    OR NOT EXISTS (SELECT 1 FROM ocr_doc_registry_tenants m
                    WHERE m.registry_key = r.registry_key)
"""

# SPEC-W77 R1 (count-not-identity): platform-aggregate stats for the
# platform-admin registry page. Aggregate COUNT(*) INTEGERS ONLY — no
# registry_key, no tenant_id, no per-key rows, no timestamps ever leave
# this module. Zero schema change: counts ride the existing columns
# (tenant_count for the multi-tenant aggregate, alerted for the alert
# count). Fakes replicate these in lockstep, dispatched on the constants.
SQL_STATS_KEYS_TOTAL = """
SELECT COUNT(*) FROM ocr_doc_registry
"""

SQL_STATS_MEMBERSHIPS_TOTAL = """
SELECT COUNT(*) FROM ocr_doc_registry_tenants
"""

SQL_STATS_MULTI_TENANT_KEYS = """
SELECT COUNT(*) FROM ocr_doc_registry
 WHERE tenant_count >= 2
"""

SQL_STATS_ALERTED_MEMBERSHIPS = """
SELECT COUNT(*) FROM ocr_doc_registry_tenants
 WHERE alerted
"""


class RegistryDisabled(RuntimeError):
    """register_document() without a secret — G1 fail-closed."""


def registry_key(sha256: str, secret: str) -> str:
    """G3: registry_key = HMAC-SHA256(secret, sha256).hexdigest().

    The raw sha256 NEVER enters the registry store — a registry dump is
    useless for dictionary attack or external hash-list correlation
    (platform KYC_HASH_SECRET precedent). Rotation = re-key backfill.
    """
    if not secret:
        raise RegistryDisabled("OCR_REGISTRY_HMAC_SECRET unset — registry disabled")
    return hmac.new(secret.encode("utf-8"), sha256.encode("utf-8"), hashlib.sha256).hexdigest()


def log_startup_posture(secret: str) -> None:
    """G1: log the registry posture ONCE at startup (lifespan hook)."""
    if secret:
        log.info(
            "ocr_registry.enabled",
            msg="cross-tenant document-fraud registry ENABLED (HMAC-keyed, sha256-exact only)",
        )
    else:
        log.warning(
            "ocr_registry.disabled",
            msg="OCR_REGISTRY_HMAC_SECRET unset — cross-tenant document-fraud "
            "registry DISABLED (fail-closed). Extract path unaffected; "
            "registry upserts are no-ops and the backfill endpoint refuses.",
        )


@asynccontextmanager
async def registry_conn(db):
    """Yield a connection with the app.ocr_registry GUC escape SET LOCAL.

    THE ONLY place the registry GUC is set (grep gate). Mirrors
    store.Database.sweep_conn(): the GUC lives and dies with the
    transaction. INTERNAL ONLY — request paths never use this.
    """
    pool = getattr(db, "_pool", None)
    if pool is None:
        # In-memory test double (tests/conftest.py) — same contract: a
        # dedicated scope whose statements run under the registry escape.
        async with db.registry_conn() as conn:
            yield conn
        return
    async with pool.acquire() as conn:
        async with conn.transaction():
            await conn.execute("SELECT set_config('app.ocr_registry', 'on', true)")
            yield conn


async def register_document(
    db,
    *,
    secret: str,
    tenant_id: str,
    sha256: str,
    first_seen: Any,
) -> dict[str, Any]:
    """Register one extracted document's fingerprint (live path AND
    backfill call THIS function — SPEC pinned "SAME upsert fn").

    first_seen MUST be the document row's created_at (true chronology —
    backfill walks (created_at, id) order which IS first-use-wins order),
    never wall-clock.

    Returns {"registry_key", "created", "membership_added", "match",
    "tenant_count", "other_tenant_ids"}. match=True exactly when this call
    added a NEW tenant membership to a key that already had >=1 OTHER
    tenant (G5); the membership's alerted flag is set in the same
    transaction, giving at-most-one emission per (tenant, hash) — the
    CALLER emits the pinned match event when match is True.
    """
    key = registry_key(sha256, secret)  # raises RegistryDisabled when unset
    tenant_uuid = uuid.UUID(str(tenant_id))
    async with registry_conn(db) as conn:
        row = await conn.fetchrow(SQL_UPSERT_PARENT, key, first_seen)
        # doc_count == 1 only on the initial INSERT (the conflict path
        # increments), so it doubles as the created flag.
        created = int(row["doc_count"]) == 1
        res = await conn.execute(SQL_ADD_MEMBERSHIP, key, tenant_uuid, first_seen)
        membership_added = res.split()[-1] == "1"
        match = False
        tenant_count = None
        other_tenant_ids: list[str] = []
        if membership_added:
            await conn.execute(SQL_RECOUNT, key)
            tenant_count = int(await conn.fetchval(SQL_TENANT_COUNT, key))
            rows = await conn.fetch(SQL_OTHER_TENANTS, key, tenant_uuid)
            other_tenant_ids = [str(r["tenant_id"]) for r in rows]
            if len(other_tenant_ids) >= 1:
                match = True
                await conn.execute(SQL_MARK_ALERTED, key, tenant_uuid)
    if created:
        REGISTRY_KEYS.inc()
    return {
        "registry_key": key,
        "created": created,
        "membership_added": membership_added,
        "match": match,
        "tenant_count": tenant_count,
        "other_tenant_ids": other_tenant_ids,
    }


async def remove_tenant_memberships(db, tenant_id: str) -> dict[str, int]:
    """G7 TenantDelete: remove ONLY the tenant's own memberships; drop the
    parent fingerprint row when its last membership leaves (re-counted, not
    trusted). Other tenants' memberships — and their protection — are
    untouched. No HMAC needed (delete is keyed by tenant_id), so this runs
    even when the registry is disabled (memberships could predate a key
    rotation/unset).
    """
    tenant_uuid = uuid.UUID(str(tenant_id))
    async with registry_conn(db) as conn:
        rows = await conn.fetch(SQL_DELETE_TENANT_MEMBERSHIPS, tenant_uuid)
        keys = sorted({str(r["registry_key"]) for r in rows})
        keys_dropped = 0
        for key in keys:
            await conn.execute(SQL_RECOUNT, key)
            res = await conn.execute(SQL_DROP_EMPTY_KEY, key)
            keys_dropped += int(res.split()[-1])
    log.info(
        "ocr_registry.tenant_memberships_removed",
        tenant_id=str(tenant_id),
        memberships_removed=len(rows),
        keys_dropped=keys_dropped,
    )
    return {"memberships_removed": len(rows), "keys_dropped": keys_dropped}


async def reconcile_registry_ttl(db, *, now, ttl_days: int) -> dict[str, int]:
    """SPEC-W76: TTL-membership reconciler — ONE registry_conn transaction.

    R1: expire memberships whose first_seen < now - ttl_days; per affected
    key, recount + drop the parent when its last membership left. R6 drift
    sweep over ALL keys: drop empty parents, repair drifted tenant_count
    (re-count, never trust; doc_count is cumulative history — untouched).

    R3: age-keyed deletes only — NO HMAC secret required, so this runs even
    when OCR_REGISTRY_HMAC_SECRET is unset (memberships could predate a
    rotation/unset). Still rides registry_conn() — the ONLY GUC access path.

    Returns {"memberships_expired", "keys_dropped", "keys_drift_repaired"}.
    """
    cutoff = now - timedelta(days=int(ttl_days))
    async with registry_conn(db) as conn:
        rows = await conn.fetch(SQL_EXPIRE_MEMBERSHIPS, cutoff)
        keys = sorted({str(r["registry_key"]) for r in rows})
        keys_dropped = 0
        for key in keys:
            await conn.execute(SQL_RECOUNT, key)
            res = await conn.execute(SQL_DROP_EMPTY_KEY, key)
            keys_dropped += int(res.split()[-1])
        keys_drift_repaired = 0
        for row in await conn.fetch(SQL_DRIFT_SWEEP):
            key = str(row["registry_key"])
            if int(row["real_count"]) == 0:
                res = await conn.execute(SQL_DROP_EMPTY_KEY, key)
                keys_dropped += int(res.split()[-1])
            else:
                await conn.execute(SQL_RECOUNT, key)
                keys_drift_repaired += 1
    stats = {
        "memberships_expired": len(rows),
        "keys_dropped": keys_dropped,
        "keys_drift_repaired": keys_drift_repaired,
    }
    if stats["memberships_expired"]:
        REGISTRY_TTL_MEMBERSHIPS_EXPIRED.inc(stats["memberships_expired"])
    if stats["keys_dropped"]:
        REGISTRY_TTL_KEYS_DROPPED.inc(stats["keys_dropped"])
    if stats["keys_drift_repaired"]:
        REGISTRY_TTL_DRIFT_REPAIRED.inc(stats["keys_drift_repaired"])
    log.info(
        "ocr_registry.ttl_reconciled",
        ttl_days=int(ttl_days),
        cutoff=cutoff.isoformat(),
        **stats,
    )
    return stats


async def registry_reconcile_loop(db, settings) -> None:
    """SPEC-W76 R2: periodic TTL reconcile until cancelled (lifespan-managed
    asyncio task), mirroring sweeper.sweeper_loop. Cadence from
    settings.registry_reconcile_minutes (the caller never starts the loop
    when it is 0). A failed run is logged and the loop continues."""
    interval_minutes = int(settings.registry_reconcile_minutes)
    interval_s = max(interval_minutes, 1) * 60
    log.info("ocr_registry.reconcile_started", interval_minutes=interval_minutes)
    while True:
        await asyncio.sleep(interval_s)
        try:
            await reconcile_registry_ttl(
                db,
                now=datetime.now(timezone.utc),
                ttl_days=int(settings.registry_ttl_days),
            )
        except Exception as exc:  # a failed run never kills the loop
            log.error("ocr_registry.reconcile_failed", error=str(exc))


async def registry_stats(
    db,
    *,
    secret_set: bool,
    ttl_days: int,
    reconcile_minutes: int,
) -> dict[str, Any]:
    """SPEC-W77 R1/R4: count-not-identity aggregate stats for the
    platform-admin registry page.

    Returns EXACTLY {enabled, keys_total, memberships_total,
    multi_tenant_keys, alerted_memberships, ttl_days, reconcile_minutes}
    — aggregate integers + posture echo ONLY. NO registry_key, NO
    tenant_id, NO per-key rows: the W67 zero-disclosure posture (G5) is
    preserved by returning nothing that identifies a key or a tenant.

    R4: when `secret_set` is False the registry is DISABLED (G1) and the
    disabled payload is returned WITHOUT touching the tables at all
    (cheap, honest — never an error page). Otherwise exactly four
    aggregate COUNT(*) queries run in ONE registry_conn() scope — the
    ONLY GUC access path; request paths (tenant_conn/sweep_conn) can
    never reach these tables.
    """
    stats: dict[str, Any] = {
        "enabled": bool(secret_set),
        "keys_total": 0,
        "memberships_total": 0,
        "multi_tenant_keys": 0,
        "alerted_memberships": 0,
        "ttl_days": int(ttl_days),
        "reconcile_minutes": int(reconcile_minutes),
    }
    if not secret_set:
        return stats
    async with registry_conn(db) as conn:
        stats["keys_total"] = int(await conn.fetchval(SQL_STATS_KEYS_TOTAL))
        stats["memberships_total"] = int(
            await conn.fetchval(SQL_STATS_MEMBERSHIPS_TOTAL)
        )
        stats["multi_tenant_keys"] = int(
            await conn.fetchval(SQL_STATS_MULTI_TENANT_KEYS)
        )
        stats["alerted_memberships"] = int(
            await conn.fetchval(SQL_STATS_ALERTED_MEMBERSHIPS)
        )
    return stats
