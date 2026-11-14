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

import hashlib
import hmac
import uuid
from contextlib import asynccontextmanager
from typing import Any

from prometheus_client import Counter

from .logging import get_logger

log = get_logger(__name__)

# G5 platform-side observability: counters + the audit log lines below are
# the ONLY registry telemetry; there is no read endpoint, no tenant
# endpoint, no platform-admin page (v1 alerts-only design).
REGISTRY_KEYS = Counter(
    "opendesk_ocr_registry_keys_total",
    "Cross-tenant registry fingerprint keys created (SPEC-W67 G5).",
)
REGISTRY_MATCHES = Counter(
    "opendesk_ocr_registry_matches_total",
    "Cross-tenant exact-duplicate matches detected (SPEC-W67 G5).",
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
