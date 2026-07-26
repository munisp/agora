"""Avatar presence providers (SPEC-W9 Part A).

Importing this package registers all built-in providers
(``tavus``, ``musetalk``) into the registry in ``base``.
"""

from __future__ import annotations

from .base import (
    PROVIDER_NONE,
    STATUSES,
    AvatarProvider,
    AvatarStatus,
    create_provider,
    provider_names,
    register_provider,
    resolve_provider_name,
)

# Importing the provider modules registers them (each calls
# register_provider at module import).
from . import musetalk, tavus  # noqa: E402,F401

__all__ = [
    "PROVIDER_NONE",
    "STATUSES",
    "AvatarProvider",
    "AvatarStatus",
    "create_provider",
    "provider_names",
    "register_provider",
    "resolve_provider_name",
    "musetalk",
    "tavus",
]
