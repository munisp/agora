"""FallbackTTS — ordered TTS provider chain with per-provider circuit
breakers (SPEC-W10 Part A; mirrors app/pipeline/llm.py FallbackLLM /
CircuitBreaker semantics).

Implements the EXISTING ``TTSInterface`` (app/pipeline/tts.py:
``sample_rate`` attr + ``synthesize_pcm(text) -> pcm bytes``), so it is a
drop-in anywhere PiperTTS is consumed. The chain order comes from
``TTS_PROVIDER_CHAIN`` (default ``"piper"`` — behavior identical to the
pre-W10 PiperTTS path). Piper is always the implicit last resort when
configured.

Voice routing: the ``voice`` attribute (or the ``voice`` argument to
:meth:`synthesize`) may be provider-qualified (``"mms:pcm"``,
``"azure:en-NG-EzinneNeural"`` — see app/multilang.py resolve_tts_voice /
TTS_VOICE_MAP). A qualified spec routes synthesis to that provider FIRST,
then falls through the remaining chain order; each provider receives its own
voice id (the qualified id on the pinned provider, its default otherwise).
Bare ids keep legacy piper semantics.

Failure handling: every provider hop is guarded by a per-provider
CircuitBreaker (``tts_cb_failures`` consecutive failures -> open for
``tts_cb_cooldown_s``, then one half-open probe) and recorded on
``tts_provider_failures_total{provider}`` (app/metrics.py).
"""

from __future__ import annotations

import time
from typing import Any, Callable

from .. import metrics
from ..logging import get_logger
from ..pipeline.llm import CircuitBreaker
from ..pipeline.tts import PiperTTS, _wav_to_pcm
from .azure import AzureTTS
from .base import TTSProvider, Voice, split_voice_spec
from .mms import MmsTTS
from .spitch import SpitchTTS
from .xtts import XttsTTS

log = get_logger("tts.chain")

#: Providers buildable from Settings (chain names).
PROVIDER_NAMES = ("piper", "mms", "xtts", "azure", "spitch")


class PiperProvider:
    """Adapts the legacy PiperTTS (http sidecar / subprocess) to TTSProvider."""

    name = "piper"

    def __init__(self, piper: PiperTTS, *, voices: list[str] | None = None) -> None:
        self._piper = piper
        self._voices = list(voices or [piper.voice])

    @property
    def default_voice(self) -> str:
        return self._piper.voice

    async def synthesize(self, text: str, voice: str, language: str) -> bytes:
        return await self._piper.synthesize_wav(text, voice or None)

    async def list_voices(self) -> list[Voice]:
        return [
            Voice(id=v, languages=[], labels={"engine": "piper"})
            for v in dict.fromkeys(self._voices)
        ]

    async def available(self) -> bool:
        if self._piper.mode != "http":
            return True  # subprocess mode: local binary, no probe
        try:
            resp = await self._piper._http().get(f"{self._piper.http_url}/healthz")
            return resp.status_code == 200
        except Exception:  # noqa: BLE001 - probe must never raise
            return False

    async def aclose(self) -> None:
        await self._piper.aclose()


class FallbackTTS:
    """TTSInterface over an ordered provider chain with circuit breakers."""

    def __init__(
        self,
        providers: dict[str, TTSProvider],
        order: list[str],
        *,
        failure_threshold: int = 3,
        cooldown_s: float = 60.0,
        clock: Callable[[], float] = time.monotonic,
        sample_rate: int = 22050,
    ) -> None:
        self._providers = dict(providers)
        # Stable chain order: configured order first, then any implicit
        # providers (piper last-resort appended by the factory).
        self._order = [n for n in order if n in self._providers]
        for n in self._providers:
            if n not in self._order:
                self._order.append(n)
        self._breakers = {
            n: CircuitBreaker(failure_threshold, cooldown_s, clock)
            for n in self._providers
        }
        self.sample_rate = sample_rate
        # Routing state (set by callers the same way PiperTTS.voice is set).
        self.voice = ""
        self.language = ""

    # ------------------------------------------------------------- accessors
    @property
    def providers(self) -> dict[str, TTSProvider]:
        return self._providers

    @property
    def order(self) -> list[str]:
        return list(self._order)

    @property
    def breakers(self) -> dict[str, CircuitBreaker]:
        return self._breakers

    def provider(self, name: str) -> TTSProvider | None:
        return self._providers.get(name)

    def providers_in_order(self) -> list[tuple[str, TTSProvider]]:
        return [(n, self._providers[n]) for n in self._order]

    async def aclose(self) -> None:
        for provider in self._providers.values():
            try:
                await provider.aclose()
            except Exception:  # noqa: BLE001 - shutdown must not raise
                pass

    # ----------------------------------------------------------- routing
    def _attempt_order(self, voice_spec: str, provider_pin: str | None) -> list[str]:
        """Provider attempt sequence for one synthesis call.

        - explicit ``provider`` pin: that provider only;
        - provider-qualified voice spec present in the chain: pinned first,
          then the rest of the chain as fallback;
        - otherwise: chain order (piper implicit last).
        """
        if provider_pin:
            return [provider_pin]
        pref, _ = split_voice_spec(voice_spec)
        if pref and pref in self._providers:
            return [pref] + [n for n in self._order if n != pref]
        return list(self._order)

    def _voice_for(self, name: str, voice_spec: str) -> str:
        """Effective voice id for provider `name` under spec `voice_spec`."""
        pref, voice_id = split_voice_spec(voice_spec)
        if pref:
            return voice_id if pref == name else self._providers[name].default_voice
        # Bare spec: legacy piper voice id; other providers use defaults.
        if voice_id and name == "piper":
            return voice_id
        return self._providers[name].default_voice

    # ----------------------------------------------------------- synthesis
    async def synthesize(
        self,
        text: str,
        *,
        voice: str = "",
        language: str = "",
        provider: str | None = None,
    ) -> bytes:
        """Synthesize `text` -> RIFF wav bytes through the chain.

        Raises ValueError for an unknown provider pin, RuntimeError when every
        provider in the attempt order failed (or is circuit-open).
        """
        text = text.strip()
        if not text:
            return b""
        spec = (voice or "").strip() or self.voice
        lang = (language or "").strip() or self.language
        if provider is not None and provider not in self._providers:
            raise ValueError(f"unknown TTS provider: {provider!r}")
        attempts = self._attempt_order(spec, provider)

        last_exc: Exception | None = None
        attempted = False
        for name in attempts:
            breaker = self._breakers[name]
            if not breaker.primary_allowed():
                log.info("tts provider circuit open; skipping", provider=name)
                continue
            attempted = True
            provider_impl = self._providers[name]
            try:
                wav = await provider_impl.synthesize(
                    text, self._voice_for(name, spec), lang
                )
                breaker.record_success()
                return wav
            except Exception as exc:  # noqa: BLE001 - any failure fails over
                last_exc = exc
                breaker.record_failure()
                metrics.tts_provider_failure(name)
                log.warning(
                    "tts provider failed; trying next in chain",
                    provider=name,
                    error=str(exc)[:200],
                    circuit_open=breaker.is_open,
                )
        detail = f"last error: {last_exc}" if last_exc else "all circuits open"
        raise RuntimeError(
            f"all TTS providers failed ({detail})" if attempted
            else f"no TTS provider available ({detail})"
        )

    async def synthesize_pcm(self, text: str) -> bytes:
        """TTSInterface: signed-16-bit mono PCM at ``sample_rate``."""
        text = text.strip()
        if not text:
            return b""
        try:
            with metrics.get_registry().tts_latency.time():
                wav = await self.synthesize(text)
                pcm, rate = _wav_to_pcm(wav)
        finally:
            metrics.session_tts()  # per-session quality accumulator
        if rate != self.sample_rate:
            log.warning(
                "tts sample rate mismatch; audio may be pitched",
                expected=self.sample_rate,
                got=rate,
            )
        return pcm


def build_provider(name: str, settings: Any) -> TTSProvider:
    """Instantiate one provider from Settings (raises on unknown names)."""
    if name == "piper":
        piper = PiperTTS(
            mode=settings.piper_mode,
            http_url=settings.piper_http_url,
            voice=settings.piper_voice,
            piper_bin=settings.piper_bin,
            model_dir=settings.piper_model_dir,
            sample_rate=settings.piper_sample_rate,
        )
        voices = [settings.piper_voice, *settings.piper_voice_map.values()]
        return PiperProvider(piper, voices=voices)
    if name == "mms":
        return MmsTTS(settings.mms_tts_url)
    if name == "xtts":
        return XttsTTS(settings.xtts_tts_url)
    if name == "azure":
        return AzureTTS(
            api_key=settings.azure_speech_key, region=settings.azure_speech_region
        )
    if name == "spitch":
        return SpitchTTS(
            api_key=settings.spitch_api_key, base_url=settings.spitch_base_url
        )
    raise ValueError(f"unknown TTS provider: {name!r}")


def parse_chain(raw: str | None) -> list[str]:
    """``"azure,mms,piper"`` -> ``["azure", "mms", "piper"]`` (known names,
    deduped, order preserved; unknown names dropped with a warning)."""
    out: list[str] = []
    for part in (raw or "").split(","):
        name = part.strip().lower()
        if not name:
            continue
        if name not in PROVIDER_NAMES:
            log.warning("TTS_PROVIDER_CHAIN: dropping unknown provider", name=name)
            continue
        if name not in out:
            out.append(name)
    return out or ["piper"]


def build_fallback_tts(settings: Any) -> FallbackTTS:
    """Factory from Settings: providers for every name in TTS_PROVIDER_CHAIN,
    plus piper as the implicit last resort when not listed."""
    order = parse_chain(settings.tts_provider_chain)
    names = list(order)
    if "piper" not in names:
        names.append("piper")  # implicit last resort (always configured)
    providers = {name: build_provider(name, settings) for name in names}
    return FallbackTTS(
        providers,
        order,
        failure_threshold=settings.tts_cb_failures,
        cooldown_s=settings.tts_cb_cooldown_s,
        sample_rate=settings.piper_sample_rate,
    )
