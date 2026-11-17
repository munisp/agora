"""SPEC-W73 A2: pre-rendered greetings.

The session-path greeting (``agent.say(build_greeting(ctx))`` in
app/livekit_worker.py) pays full TTS synthesis latency on a cold cache.
This module warms the TTS LRU at boot for KNOWN tenants so that greeting
is a guaranteed cache hit:

- ``prerender_greetings(settings, build_tts, tenant_texts)`` synthesizes
  each tenant's greeting text through the REAL TTS node (the FallbackTTS
  chain carrying the TtsLruCache, built by the caller's ``build_tts``
  factory) with bounded concurrency; a per-tenant failure is non-fatal
  (logged + ``voice_greeting_prerender_total{result="error"}``) and the
  remaining tenants still warm;
- ``collect_greeting_texts`` renders each slug's greeting through the REAL
  seams (``fetch_tenant_context`` + ``build_greeting``) and resolves the
  session-path voice exactly like ``build_voice_agent`` does
  (``resolve_tts_voice`` on the tenant's default language + the
  SPEC-W38 F2 definition override), so the warmed cache key
  (voice, text, format) is IDENTICAL to the session-path key;
- ``boot_prerender`` is the sync worker-boot hook: disabled or an empty
  tenant list is an honest no-op (there is NO tenant-enumeration seam at
  boot — agents_registry/tenant_context resolve single tenants only, so
  nothing was reused); the total boot delay is bounded by
  ``voice_greeting_prerender_timeout_s`` and a timeout proceeds with the
  partially warmed cache (``result="timeout"``).

Process note: livekit-agents 0.10.2 spawns job processes
(``mp.get_context("spawn")`` in livekit/agents/worker.py L240), so the
worker wires this
into BOTH the supervisor boot (``main()``) and ``prewarm_fnc`` (each warm
job process) — a supervisor-only warm would never reach the job processes
that actually serve sessions.
"""

from __future__ import annotations

import asyncio
from typing import Any, Awaitable, Callable

from . import metrics
from .config import Settings
from .dapr_client import DaprClient
from .logging import get_logger
from .multilang import MultilangState, resolve_tts_voice
from .prompts import build_greeting
from .tenant_context import fetch_tenant_context

log = get_logger("greeting-prerender")


def parse_tenant_slugs(raw: str) -> list[str]:
    """Parse the VOICE_GREETING_PRERENDER_TENANTS csv (deduped, order-stable)."""
    seen: set[str] = set()
    slugs: list[str] = []
    for part in (raw or "").split(","):
        slug = part.strip()
        if slug and slug not in seen:
            seen.add(slug)
            slugs.append(slug)
    return slugs


def session_greeting_voice(settings: Settings, ctx: Any) -> str:
    """The voice the session path will use for this tenant's greeting.

    Mirrors build_voice_agent: resolve_tts_voice on the tenant's default
    (locale-derived) language, then the SPEC-W38 F2 definition override
    (piper / provider-less voices only)."""
    voice = resolve_tts_voice(
        MultilangState.from_context(ctx).active_language,
        settings.tts_voice_map,
        settings.piper_voice_map,
        settings.piper_voice,
    )
    definition = getattr(ctx, "agent_definition", None)
    if (
        definition is not None
        and definition.voice.voice_id
        and definition.voice.provider in ("", "piper")
    ):
        voice = definition.voice.voice_id
    return voice


async def prerender_greetings(
    settings: Settings,
    build_tts: Callable[[], Any],
    tenant_texts: dict[str, str],
    *,
    tenant_voices: dict[str, str] | None = None,
    registry: Any | None = None,
) -> Any:
    """Synthesize each tenant's greeting through the real TTS node.

    ``build_tts`` is a zero-arg factory returning the TTS chain that carries
    the session LRU (the worker passes ``lambda: _build_tts(settings)``);
    ``tenant_texts`` maps tenant slug -> the exact ``build_greeting(ctx)``
    text the session path will speak. ``tenant_voices`` optionally pins the
    per-tenant session voice so the warmed (voice, text, format) cache key
    is identical to the session-path key; tenants are grouped by voice so
    the shared chain's ``voice`` attribute never races under concurrency.
    Returns the (warmed) TTS instance so the caller can pin it for session
    reuse. Per-tenant failure is non-fatal: logged, counted as
    ``result="error"``, and the remaining tenants still warm."""
    reg = registry or metrics.get_registry()
    tts = build_tts()
    texts = {
        slug: text.strip()
        for slug, text in tenant_texts.items()
        if (text or "").strip()
    }
    if not texts:
        return tts
    concurrency = max(1, int(settings.voice_greeting_prerender_concurrency))
    semaphore = asyncio.Semaphore(concurrency)

    async def _one(slug: str, text: str) -> None:
        async with semaphore:
            try:
                await tts.synthesize_pcm(text)
            except asyncio.CancelledError:
                raise
            except Exception as exc:  # noqa: BLE001 - per-tenant non-fatal
                reg.greeting_prerender.inc(result="error")
                log.warning(
                    "greeting prerender failed",
                    tenant=slug,
                    error=str(exc)[:200],
                )
            else:
                reg.greeting_prerender.inc(result="ok")
                log.info("greeting prerendered", tenant=slug)

    # Group by session voice: one voice is set on the shared chain at a
    # time, tenants sharing a voice still synthesize concurrently.
    groups: dict[str, list[tuple[str, str]]] = {}
    for slug, text in texts.items():
        voice = (tenant_voices or {}).get(slug, "")
        groups.setdefault(voice, []).append((slug, text))
    for voice, items in groups.items():
        if voice and hasattr(tts, "voice"):
            tts.voice = voice
        await asyncio.gather(*(_one(slug, text) for slug, text in items))
    return tts


async def collect_greeting_texts(
    settings: Settings,
    slugs: list[str],
    *,
    fetch_ctx: Callable[..., Awaitable[Any]] = fetch_tenant_context,
) -> tuple[dict[str, str], dict[str, str]]:
    """Render each slug's greeting via the real tenant-context seam.

    Returns (texts, voices): slug -> build_greeting(ctx) text and slug ->
    session greeting voice. Per-tenant fetch/render failures are non-fatal
    (the slug is skipped — its session greeting simply synthesizes cold,
    today's behavior)."""
    texts: dict[str, str] = {}
    voices: dict[str, str] = {}
    if not slugs:
        return texts, voices
    dapr = DaprClient(
        settings.dapr_base_url,
        settings.http_timeout_s,
        direct_bases={settings.booking_app_id: settings.booking_base_url},
    )
    try:
        for slug in slugs:
            try:
                ctx = await fetch_ctx(dapr, settings, slug)
                text = build_greeting(ctx)
                voice = session_greeting_voice(settings, ctx)
            except Exception as exc:  # noqa: BLE001 - skip, stay bootable
                log.warning(
                    "greeting text render failed; tenant skipped",
                    tenant=slug,
                    error=str(exc)[:200],
                )
                continue
            if text.strip():
                texts[slug] = text
                voices[slug] = voice
    finally:
        try:
            await dapr.aclose()
        except Exception:  # noqa: BLE001 - shutdown must not raise
            pass
    return texts, voices


async def _prerender_all(
    settings: Settings,
    build_tts: Callable[[], Any],
    slugs: list[str],
) -> Any:
    """Text collection + synthesis under one timeout umbrella. The TTS
    instance is created BEFORE the bounded wait, so a timeout still returns
    it with the partially warmed cache."""
    tts = build_tts()

    async def _inner() -> None:
        texts, voices = await collect_greeting_texts(settings, slugs)
        await prerender_greetings(
            settings, lambda: tts, texts, tenant_voices=voices
        )

    try:
        await asyncio.wait_for(
            _inner(), timeout=max(0.1, settings.voice_greeting_prerender_timeout_s)
        )
    except asyncio.TimeoutError:
        metrics.get_registry().greeting_prerender.inc(result="timeout")
        log.warning(
            "greeting prerender timed out; proceeding with partial warm",
            timeout_s=settings.voice_greeting_prerender_timeout_s,
        )
    return tts


def boot_prerender(
    settings: Settings,
    build_tts: Callable[[], Any],
) -> Any | None:
    """Sync worker-boot hook (called before ``agents.cli.run_app`` and from
    the per-process prewarm hook). Returns the warmed TTS instance, or None
    when disabled / no tenants configured / the warm failed outright.
    NEVER raises — worker boot must survive a broken prerender."""
    if not settings.voice_greeting_prerender_enabled:
        return None
    slugs = parse_tenant_slugs(settings.voice_greeting_prerender_tenants)
    if not slugs:
        log.info("greeting prerender skipped: no tenants configured")
        return None
    try:
        tts = asyncio.run(_prerender_all(settings, build_tts, slugs))
    except Exception as exc:  # noqa: BLE001 - never block worker boot
        log.warning("greeting prerender failed; boot continues", error=str(exc)[:200])
        return None
    log.info(
        "greeting prerender pass complete",
        tenants=len(slugs),
        warmed=tts is not None,
    )
    return tts
