"""Avatar presence provider abstraction (SPEC-W9 Part A / A1).

An *avatar provider* joins a visual presence into the LiveKit room a voice
session was minted for. Providers are fire-and-forget from the session's
point of view: joining is attempted in the background and ANY failure
degrades to ``AvatarStatus(status="unavailable")`` + a warning log — it must
never block or fail session creation.

Providers are looked up by name in the registry below. ``none`` (and any
unknown name) resolves to no provider.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import TYPE_CHECKING, Any, Protocol, runtime_checkable

from ..logging import get_logger

if TYPE_CHECKING:  # pragma: no cover - typing only
    from ..config import Settings

log = get_logger("avatar")

#: Valid AvatarStatus.status values.
STATUSES = ("off", "joining", "unavailable")

#: Provider name that disables avatar presence (default).
PROVIDER_NONE = "none"


@dataclass(frozen=True)
class AvatarStatus:
    """Result/intent of asking a provider to join a room."""

    provider: str
    status: str  # "off" | "joining" | "unavailable"
    detail: str | None = None

    def __post_init__(self) -> None:
        if self.status not in STATUSES:
            raise ValueError(f"invalid avatar status {self.status!r}")

    def as_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {"provider": self.provider, "status": self.status}
        if self.detail:
            payload["detail"] = self.detail
        return payload


@runtime_checkable
class AvatarProvider(Protocol):
    """Joins an avatar presence into a LiveKit room.

    Implementations MUST NOT raise from ``join_room`` — every failure path
    returns ``AvatarStatus(status="unavailable", detail=...)`` instead.
    """

    name: str

    def check_ready(self) -> str | None:
        """Cheap synchronous configuration check. Returns None when the
        provider is configured enough to attempt a join, else a human
        detail string explaining the misconfiguration (session response can
        then report ``unavailable`` immediately without a network call)."""
        ...

    async def join_room(self, room: str, *, tenant_ctx: Any = None) -> AvatarStatus:
        """Attempt to bring the avatar into ``room``. Never raises."""
        ...


# --------------------------------------------------------------------------
# Registry
# --------------------------------------------------------------------------
# name -> provider class (constructed with Settings). Kept as classes (not
# instances) so providers read the per-process Settings at construction.
_REGISTRY: dict[str, type] = {}


def register_provider(name: str, provider_cls: type) -> None:
    """Register a provider class under ``name`` (idempotent re-register)."""
    if not name or name == PROVIDER_NONE:
        raise ValueError(f"reserved/empty avatar provider name {name!r}")
    _REGISTRY[name] = provider_cls


def provider_names() -> list[str]:
    return sorted(_REGISTRY)


def resolve_provider_name(settings: Settings, tenant_ctx: Any = None) -> str:
    """Effective provider name: per-tenant override wins when present.

    The tenant override is read defensively via ``getattr`` — TenantContext
    does not declare ``avatar_provider`` yet, so pre-override deployments
    (and tenants without the attribute) fall through to the env-level
    ``AVATAR_PROVIDER``. Unknown/empty names fall back to ``none``.
    """
    override = getattr(tenant_ctx, "avatar_provider", None) if tenant_ctx else None
    name = override if isinstance(override, str) and override.strip() else None
    name = (name or getattr(settings, "avatar_provider", PROVIDER_NONE) or PROVIDER_NONE).strip()
    if name != PROVIDER_NONE and name not in _REGISTRY:
        log.warning("unknown avatar provider, disabling", provider=name)
        return PROVIDER_NONE
    return name


def create_provider(name: str, settings: Settings) -> AvatarProvider | None:
    """Instantiate the registered provider for ``name``; None for
    ``none``/unknown names."""
    cls = _REGISTRY.get(name)
    if cls is None:
        return None
    return cls(settings)
