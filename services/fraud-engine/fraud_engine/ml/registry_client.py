"""registry_client.py — model-registry service consumer (SPEC-W33 §4 C1
consumer clause, wave W33-C).

Resolves the tenant's PRODUCTION ``fraud-ml`` artifact through the
model-registry REST service instead of scanning the filesystem bootstrap
registry. The contract (coded against the service spec, not its code)::

    GET {MODEL_REGISTRY_URL}/v1/registry/fraud-ml/{tenant_id}/production
    200 -> {"family": "fraud-ml", "tenant_id": ..., "version": ...,
            "artifact_uri": "file://<abs path>", "stage": "production",
            "seed": ..., "dataset_hash": ..., ...}
    404 -> no promoted production model for this tenant

Honest bootstrap fallback (I1), mirroring the booking-service
``score_client.go`` pattern: ``MODEL_REGISTRY_URL`` unset/empty, a hard
500ms budget exceeded, transport error, non-200 status, malformed JSON,
or a record failing validation ALL return None — never an exception — and
the caller falls back to the W31/W33-B local-dir scan.

Fail-closed validation (I4 cross-tenant honesty): the record's ``family``
must be ``fraud-ml``, ``tenant_id`` must equal the requested tenant,
``stage`` must be ``"production"``, ``version`` must be a non-empty
string, and ``artifact_uri`` must parse. Any mismatch -> None.

``artifact_uri`` schemes: only ``file://<abs path>`` is supported this
wave (registry and consumer share the artifact volume in compose); any
other scheme -> None + warning (honest unsupported, no silent fetch).

TORCH GUARD DISCIPLINE (I5): stdlib + urllib only — NO requests
dependency, NO torch import — so this module imports cleanly in
torch-absent heuristic deployments.
"""

from __future__ import annotations

import json
import logging
import os
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field
from typing import Any, Mapping

log = logging.getLogger("fraud_engine.ml.registry_client")

#: Model family this service consumes from the registry.
FAMILY = "fraud-ml"

#: Env knob (read here directly — no config.py change this wave). Unset or
#: empty disables the registry service entirely (pure bootstrap, I1).
REGISTRY_URL_ENV = "MODEL_REGISTRY_URL"

#: Hard per-call budget (booking-service score_client.go pattern).
DEFAULT_TIMEOUT_S = 0.5

_MAX_BODY_BYTES = 65536


@dataclass(frozen=True)
class ProductionRecord:
    """A validated production record from the model-registry service."""

    family: str
    tenant_id: str
    version: str
    artifact_uri: str
    stage: str
    seed: int | None
    dataset_hash: str | None
    raw: Mapping[str, Any] = field(default_factory=dict)


def registry_base_url(explicit: str | None = None) -> str | None:
    """Effective registry base URL, or None when disabled (unset/empty)."""
    raw = explicit if explicit is not None else os.getenv(REGISTRY_URL_ENV, "")
    stripped = (raw or "").strip().rstrip("/")
    return stripped or None


def production_url(base_url: str, family: str, tenant_id: str) -> str:
    """Canonical record URL; tenant is URL-encoded into the path (I4)."""
    return (
        f"{base_url}/v1/registry/"
        f"{urllib.parse.quote(family, safe='')}/"
        f"{urllib.parse.quote(tenant_id, safe='')}/production"
    )


def _validate(payload: Any, family: str, tenant_id: str) -> ProductionRecord | None:
    """Fail-closed record validation; None (+ warning) on any violation."""
    if not isinstance(payload, dict):
        log.warning("model-registry record is not a JSON object; bootstrap fallback")
        return None
    if payload.get("family") != family:
        log.warning(
            "model-registry record family %r != %r; bootstrap fallback",
            payload.get("family"),
            family,
        )
        return None
    if payload.get("tenant_id") != tenant_id:
        log.warning(
            "model-registry record tenant %r != requested %r (cross-tenant "
            "record refused); bootstrap fallback",
            payload.get("tenant_id"),
            tenant_id,
        )
        return None
    if payload.get("stage") != "production":
        log.warning(
            "model-registry record stage %r != 'production'; bootstrap fallback",
            payload.get("stage"),
        )
        return None
    version = payload.get("version")
    if not isinstance(version, str) or not version.strip():
        log.warning("model-registry record has empty version; bootstrap fallback")
        return None
    artifact_uri = payload.get("artifact_uri")
    if not isinstance(artifact_uri, str) or not artifact_uri.strip():
        log.warning("model-registry record has empty artifact_uri; bootstrap fallback")
        return None
    seed = payload.get("seed")
    return ProductionRecord(
        family=family,
        tenant_id=tenant_id,
        version=version,
        artifact_uri=artifact_uri,
        stage="production",
        seed=seed if isinstance(seed, int) else None,
        dataset_hash=payload.get("dataset_hash")
        if isinstance(payload.get("dataset_hash"), str)
        else None,
        raw=payload,
    )


def fetch_production(
    tenant_id: str,
    family: str = FAMILY,
    *,
    base_url: str | None = None,
    timeout_s: float = DEFAULT_TIMEOUT_S,
) -> ProductionRecord | None:
    """GET the production record; None on disabled/404/timeout/error.

    NEVER raises: every failure mode degrades to the bootstrap local-dir
    scan in the caller (I1).
    """
    base = registry_base_url(base_url)
    if base is None:
        return None  # registry disabled -> pure bootstrap
    url = production_url(base, family, tenant_id)
    try:
        request = urllib.request.Request(url, headers={"Accept": "application/json"})
        with urllib.request.urlopen(request, timeout=timeout_s) as response:
            status = getattr(response, "status", response.getcode())
            if status != 200:
                log.warning(
                    "model-registry GET %s -> HTTP %s; bootstrap fallback", url, status
                )
                return None
            body = response.read(_MAX_BODY_BYTES + 1)
    except urllib.error.HTTPError as exc:
        if exc.code == 404:
            log.info(
                "model-registry has no production %s model for tenant %s (404); "
                "bootstrap fallback",
                family,
                tenant_id,
            )
        else:
            log.warning(
                "model-registry GET %s -> HTTP %s; bootstrap fallback", url, exc.code
            )
        return None
    except Exception as exc:  # noqa: BLE001 - timeout/transport -> fallback
        log.warning("model-registry GET %s failed (%s); bootstrap fallback", url, exc)
        return None
    if len(body) > _MAX_BODY_BYTES:
        log.warning("model-registry record body too large; bootstrap fallback")
        return None
    try:
        payload = json.loads(body.decode("utf-8"))
    except (UnicodeDecodeError, ValueError) as exc:
        log.warning("model-registry record is malformed JSON (%s); bootstrap fallback", exc)
        return None
    return _validate(payload, family, tenant_id)


def resolve_artifact_dir(
    tenant_id: str,
    family: str = FAMILY,
    *,
    base_url: str | None = None,
    timeout_s: float = DEFAULT_TIMEOUT_S,
) -> tuple[str, ProductionRecord] | None:
    """Resolve the production record to a LOCAL artifact scope dir.

    Returns ``(artifact_dir, record)`` on success — ``artifact_dir`` uses
    the W33-B scope layout (``{prefix}{N}/model.pt + meta.json`` subdirs)
    so the existing loaders consume it unchanged. None (+ warning) for any
    unsupported/missing artifact — the caller falls back to bootstrap (I1).
    """
    record = fetch_production(tenant_id, family, base_url=base_url, timeout_s=timeout_s)
    if record is None:
        return None
    parsed = urllib.parse.urlparse(record.artifact_uri)
    if parsed.scheme != "file":
        log.warning(
            "model-registry artifact_uri scheme %r unsupported (file:// only "
            "this wave); bootstrap fallback",
            parsed.scheme,
        )
        return None
    if parsed.netloc not in ("", "localhost"):
        log.warning(
            "model-registry artifact_uri host %r unsupported (shared local "
            "volume only); bootstrap fallback",
            parsed.netloc,
        )
        return None
    path = urllib.parse.unquote(parsed.path)
    if not path.startswith("/"):
        log.warning(
            "model-registry artifact_uri %r is not an absolute file path; "
            "bootstrap fallback",
            record.artifact_uri,
        )
        return None
    if not os.path.isdir(path):
        log.warning(
            "model-registry artifact dir %s missing on the shared volume; "
            "bootstrap fallback",
            path,
        )
        return None
    return path, record
