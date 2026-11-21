"""SPEC-W112: USSD grounding-quality metrics.

One counter on the default prometheus_client registry (exposed via the
existing /metrics endpoint in main.py):

    conversation_grounding_turns_total{tenant,channel,result}

Incremented exactly once per ELIGIBLE USSD text-mode turn (grounding
enabled, not menu mode, non-empty parsed query). Result labels:

    composed         — KB snippets found and LLM-composed answer used
    raw              — KB snippets found, raw grounded reply used
    compose_fallback — LLM compose enabled but failed; raw reply used
    ungrounded       — KB lookup returned no usable snippets
    error            — KB lookup raised; static ack fallback
    disabled         — grounding flag off (counted by the route only when
                       the turn WOULD have been eligible otherwise)

Tenant labels are sanitized and cardinality-capped: at most
Config.grounding_metrics_max_tenants distinct tenant label values get
their own series; all further tenants aggregate under tenant="overflow".
Channel is bounded to a known set ("ussd" today; the counter is generic
so future channels reuse it). Metric recording NEVER raises into the
request path — callers wrap in the helpers here.
"""

from __future__ import annotations

import hashlib
import re
import threading
from typing import Any

from prometheus_client import Counter

_SAFE_LABEL_CHARS = re.compile(r"[^a-zA-Z0-9_.-]+")
_KNOWN_CHANNELS = frozenset({"ussd", "web", "whatsapp", "telegram", "voice"})


def _bounded_label_value(value: Any, *, max_len: int) -> str:
    """Safe, length-bounded label value (stable hash suffix on truncation)."""
    raw = str(value or "unknown").strip().lower()
    cleaned = _SAFE_LABEL_CHARS.sub("_", raw).strip("_") or "unknown"
    if len(cleaned) <= max_len:
        return cleaned
    digest = hashlib.sha256(raw.encode("utf-8")).hexdigest()[:12]
    return f"{cleaned[: max_len - 13].rstrip('_')}-{digest}"


def _channel_label(channel: Any) -> str:
    label = _bounded_label_value(channel, max_len=32)
    return label if label in _KNOWN_CHANNELS else "other"


grounding_turns = Counter(
    "conversation_grounding_turns_total",
    "USSD/channel knowledge grounding outcomes by tenant and channel.",
    ("tenant", "channel", "result"),
)

_seen_tenants: set[str] = set()
_tenant_lock = threading.Lock()


def record_grounding_turn(
    *,
    tenant: str,
    channel: str,
    result: str,
    max_tenants: int = 100,
) -> None:
    """Record one grounding outcome. Never raises."""
    try:
        tenant_label = _bounded_label_value(tenant, max_len=64)
        with _tenant_lock:
            if tenant_label not in _seen_tenants:
                if max_tenants <= 0 or len(_seen_tenants) >= max_tenants:
                    tenant_label = "overflow"
                else:
                    _seen_tenants.add(tenant_label)
        grounding_turns.labels(
            tenant=tenant_label,
            channel=_channel_label(channel),
            result=_bounded_label_value(result, max_len=32),
        ).inc()
    except Exception:  # noqa: BLE001 - metrics must never break a turn
        pass


def reset_for_tests() -> None:
    """Test isolation: clear the tenant-cap cache and counter samples."""
    with _tenant_lock:
        _seen_tenants.clear()
    grounding_turns._metrics.clear()  # noqa: SLF001 - prometheus test idiom
