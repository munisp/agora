"""TTS provider layer (SPEC-W10 Part A).

Pluggable text-to-speech providers behind one protocol, consumed by
:mod:`app.tts_providers.chain` (FallbackTTS) and the control-plane voice
endpoints. Providers:

- ``piper``   — existing Piper sidecar/subprocess (adapter lives in chain.py)
- ``mms``     — Meta MMS sidecar (services/mms-tts), Nigerian languages
- ``xtts``    — Coqui XTTS-v2 sidecar (services/xtts-tts), brand-voice cloning
- ``azure``   — Azure Cognitive Speech direct (en-NG neural voices)
- ``spitch``  — Spitch API (Yoruba/Hausa/Igbo voices)

All providers return RIFF ``audio/wav`` bytes from ``synthesize``; the chain
converts to PCM for the LiveKit pipeline via app.pipeline.tts._wav_to_pcm.
"""

from __future__ import annotations

from dataclasses import asdict, dataclass, field
from typing import Any, Protocol


@dataclass(frozen=True)
class Voice:
    """One selectable voice on a provider (GET /voice/voices payload item)."""

    id: str
    languages: list[str] = field(default_factory=list)
    gender: str = ""
    labels: dict[str, Any] = field(default_factory=dict)

    def as_dict(self) -> dict[str, Any]:
        return asdict(self)


class TTSProvider(Protocol):
    """Provider contract consumed by FallbackTTS and the control plane."""

    name: str
    default_voice: str

    async def list_voices(self) -> list[Voice]:
        """Voice catalog (static or sidecar-fetched)."""
        ...

    async def synthesize(self, text: str, voice: str, language: str) -> bytes:
        """Synthesize `text` with `voice` -> RIFF wav bytes."""
        ...

    async def available(self) -> bool:
        """Cheap readiness probe (sidecar reachable / credentials present)."""
        ...

    async def aclose(self) -> None:
        """Release underlying HTTP resources (no-op for stateless providers)."""
        ...


def split_voice_spec(spec: str | None) -> tuple[str | None, str]:
    """Split a provider-qualified voice spec: ``"mms:pcm"`` -> ``("mms", "pcm")``.

    Bare voice ids (``"en_US-lessac-medium"``) return ``(None, spec)`` and are
    treated as piper voices (legacy PIPER_VOICE_MAP semantics).
    """
    spec = (spec or "").strip()
    if not spec:
        return None, ""
    if ":" in spec:
        provider, _, voice_id = spec.partition(":")
        provider = provider.strip().lower()
        voice_id = voice_id.strip()
        if provider and voice_id:
            return provider, voice_id
    return None, spec
