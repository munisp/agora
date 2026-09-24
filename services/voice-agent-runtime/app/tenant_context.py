"""Session bootstrap: tenant context + knowledge snippets (SPEC §11).

Resolution order (tenant-safe: the server resolves org from slug, never from
the model — SPEC §1):
1. booking-service public endpoint `GET /public/sites/{slug}/context` (via
   Dapr invoke, app-id `booking`) -> site, tenant display context, offerings,
   team members. The site's `tenant_id`/`tenant_slug` scope everything else.
2. identity-service `GET /v1/tenants/{slug}` (app-id `identity`) for the
   canonical terminology/timezone/currency/locale payload (best-effort
   enrichment of step 1).
3. knowledge-service `GET /v1/context?tenant=&q=` (app-id `knowledge`) for a
   few grounding snippets injected into the system prompt.
"""

from __future__ import annotations

import asyncio
import copy
import time
from collections import OrderedDict
from dataclasses import dataclass, field
from typing import Any

from .agent_definition import AgentDefinition
from .config import Settings
from .dapr_client import DaprClient, DaprError
from .logging import get_logger

log = get_logger("bootstrap")


@dataclass
class TenantContext:
    site_slug: str
    tenant_id: str  # UUID — used as CloudEvents `tenantid` ext
    tenant_slug: str  # used as CloudEvents `subject`
    display_name: str = ""
    timezone: str = "UTC"
    currency: str = "USD"
    locale: str = "en-US"
    terminology: dict[str, Any] = field(default_factory=dict)
    industry: str = ""  # SPEC-CRM §C: industry pack id (e.g. salon, clinic)
    agent_persona: str = ""  # pack agentPersona, appended to the system prompt
    # Pack multi-agent crew + plugin tools (SPEC-W3 §4), set by _apply_pack.
    agents: list[dict[str, Any]] = field(default_factory=list)
    custom_tools: list[dict[str, Any]] = field(default_factory=list)
    # SPEC-W9 Part C: pack `mcpServers: [{name, url}]`, set by _apply_pack
    # (mirrors custom_tools); consumed by mcp_client.tenant_mcp_servers.
    mcp_servers: list[dict[str, Any]] = field(default_factory=list)
    # Pack `languages: [en, es]` (Wave 5 #3): languages this tenant's
    # receptionist supports; bounds the whisper auto-language switch
    # (app/multilang.py). Empty = unconstrained.
    languages: list[str] = field(default_factory=list)
    offerings: list[dict[str, Any]] = field(default_factory=list)
    team_members: list[dict[str, Any]] = field(default_factory=list)
    knowledge_snippets: list[str] = field(default_factory=list)
    # SPEC-W38 F2: declarative agent definition from the agents registry
    # (app/agent_definition.py), merged over the pack/env context by
    # merge_definition after bootstrap. None = legacy pack/env behaviour.
    agent_definition: AgentDefinition | None = None

    def offering_summary(self) -> str:
        parts = []
        for o in self.offerings:
            price_cents = o.get("price_cents")
            price = (
                f"{price_cents / 100:.2f} {self.currency}"
                if isinstance(price_cents, (int, float))
                else f"price in {self.currency}"
            )
            parts.append(
                f"- {o.get('name')} (id {o.get('id')}): "
                f"{o.get('duration_min')} min, {price}"
            )
        return "\n".join(parts) or "- (catalog unavailable)"

    def team_summary(self) -> str:
        parts = [f"- {m.get('name')} (id {m.get('id')})" for m in self.team_members]
        return "\n".join(parts) or "- (team unavailable)"


def _apply_pack(ctx: TenantContext, tenant_payload: dict[str, Any]) -> None:
    """SPEC-CRM §C4: expose the industry pack (id + agentPersona) from an
    identity tenant payload on the context. Guarded: tenants without a
    resolved pack (or pre-CRM identity responses) leave the defaults."""
    if not isinstance(tenant_payload, dict):
        return
    industry = tenant_payload.get("industry")
    if isinstance(industry, str) and industry:
        ctx.industry = industry
    pack = tenant_payload.get("pack")
    if isinstance(pack, dict):
        persona = pack.get("agentPersona")
        if isinstance(persona, str) and persona.strip():
            ctx.agent_persona = persona.strip()
        agents = pack.get("agents")
        if isinstance(agents, list):
            ctx.agents = [a for a in agents if isinstance(a, dict) and a.get("id")]
        custom_tools = pack.get("customTools")
        if isinstance(custom_tools, list):
            ctx.custom_tools = [t for t in custom_tools if isinstance(t, dict)]
        # SPEC-W9 Part C: pack `mcpServers` passthrough (identity forwards it
        # unvalidated, like customTools; validated at consumption in
        # mcp_client.tenant_mcp_servers).
        mcp_servers = pack.get("mcpServers")
        if isinstance(mcp_servers, list):
            ctx.mcp_servers = [s for s in mcp_servers if isinstance(s, dict)]
        # Wave 5 #3: pack `languages: [en, es]`. Identity (Go) passes packs
        # through unvalidated, so the voice runtime validates at consumption
        # (app/multilang.validate_pack_languages): invalid entries drop out
        # with a warning, never fatal.
        if "languages" in pack:
            from .multilang import validate_pack_languages

            ctx.languages = validate_pack_languages(pack.get("languages"))


# ------------------------------------------------------- TTL cache (W46 P1/P2)
# Per-process tenant-context cache keyed by site_slug, shared by every
# fetch_tenant_context caller (chat turns, ElevenLabs tool webhooks, voice
# session bootstrap). Fresh entries are served for `tenant_ctx_ttl_s`; on a
# refresh failure a stale entry is served for up to `tenant_ctx_stale_s`
# (stale-while-error). Callers receive a DEEP COPY so per-session
# mutations (persona_override, merge_definition, list edits) never leak
# into the cache or across sessions. Bounded: oldest entries are evicted
# past the cap.


@dataclass
class _CtxCacheEntry:
    ctx: TenantContext
    expires_at: float  # monotonic deadline for the fresh window
    stale_until: float  # monotonic deadline for the stale-while-error window


_CTX_CACHE_MAX = 512
_ctx_cache: "OrderedDict[str, _CtxCacheEntry]" = OrderedDict()
_ctx_locks: dict[str, asyncio.Lock] = {}


def _ctx_lock(site_slug: str) -> asyncio.Lock:
    lock = _ctx_locks.get(site_slug)
    if lock is None:
        lock = asyncio.Lock()
        _ctx_locks[site_slug] = lock
        if len(_ctx_locks) > _CTX_CACHE_MAX:
            # Bound the lock map alongside the cache.
            for key in list(_ctx_locks)[: len(_ctx_locks) - _CTX_CACHE_MAX]:
                _ctx_locks.pop(key, None)
    return lock


def clear_tenant_context_cache() -> None:
    """Test/admin hook: drop every cached tenant context."""
    _ctx_cache.clear()


def _cache_store(site_slug: str, ctx: TenantContext, ttl_s: float, stale_s: float) -> None:
    now = time.monotonic()
    _ctx_cache[site_slug] = _CtxCacheEntry(
        ctx=ctx,
        expires_at=now + max(ttl_s, 0.0),
        stale_until=now + max(ttl_s, 0.0) + max(stale_s, 0.0),
    )
    _ctx_cache.move_to_end(site_slug)
    while len(_ctx_cache) > _CTX_CACHE_MAX:
        _ctx_cache.popitem(last=False)


async def fetch_tenant_context(
    dapr: DaprClient, settings: Settings, site_slug: str
) -> TenantContext:
    """Resolve the tenant context for `site_slug`, cached per slug.

    Fresh-cache hits skip the 3-invoke bootstrap entirely (P1/P2: it used
    to run on every chat turn and every ElevenLabs tool call). A refresh
    failure falls back to the stale entry when one is still inside the
    stale window; with no usable entry the error propagates exactly as the
    pre-cache code did (fail-closed bootstrap preserved).
    """
    now = time.monotonic()
    entry = _ctx_cache.get(site_slug)
    if entry is not None and entry.expires_at > now:
        return copy.deepcopy(entry.ctx)

    async with _ctx_lock(site_slug):
        # Recheck under the lock: a concurrent turn may have refreshed.
        now = time.monotonic()
        entry = _ctx_cache.get(site_slug)
        if entry is not None and entry.expires_at > now:
            return copy.deepcopy(entry.ctx)
        try:
            ctx = await _fetch_tenant_context_uncached(dapr, settings, site_slug)
        except Exception as exc:
            if entry is not None and entry.stale_until > now:
                log.warning(
                    "tenant context refresh failed; serving stale entry",
                    site_slug=site_slug,
                    error=str(exc)[:200],
                )
                return copy.deepcopy(entry.ctx)
            raise
        _cache_store(
            site_slug, ctx, settings.tenant_ctx_ttl_s, settings.tenant_ctx_stale_s
        )
        return copy.deepcopy(ctx)


async def _fetch_tenant_context_uncached(
    dapr: DaprClient, settings: Settings, site_slug: str
) -> TenantContext:
    """Bootstrap the per-session tenant context. Raises DaprError when the
    site cannot be resolved at all (session should not start)."""
    ctx_payload = await dapr.invoke_get(
        settings.booking_app_id, f"public/sites/{site_slug}/context"
    )
    if not isinstance(ctx_payload, dict):
        raise DaprError(f"empty site context for slug {site_slug}")

    site = ctx_payload.get("site") or {}
    tenant = ctx_payload.get("tenant") or {}
    tenant_slug = site.get("tenant_slug") or tenant.get("slug") or site_slug
    tenant_id = str(site.get("tenant_id") or tenant.get("id") or "")

    ctx = TenantContext(
        site_slug=site_slug,
        tenant_id=tenant_id,
        tenant_slug=tenant_slug,
        display_name=site.get("display_name") or tenant.get("name") or site_slug,
        timezone=tenant.get("timezone") or "UTC",
        currency=tenant.get("currency") or "USD",
        locale=tenant.get("locale") or "en-US",
        terminology=tenant.get("terminology") or {},
        offerings=ctx_payload.get("offerings") or [],
        team_members=ctx_payload.get("team_members") or [],
    )
    # SPEC-CRM §C4: the booking public context proxies identity's tenant
    # payload — pick up the industry pack persona when present.
    _apply_pack(ctx, tenant)

    # 2+3 (W46 P1): identity enrichment and knowledge grounding are
    # independent best-effort legs — fetch them concurrently instead of
    # sequentially (one RTT window instead of two).
    async def _identity_leg() -> None:
        # 2. Canonical tenant record from identity (best-effort enrichment).
        # SPEC-W44 K2: identity gates tenant-scoped reads for service callers
        # behind X-Internal-Token; the enrichment stays soft-fail when unset.
        try:
            headers = (
                {"X-Internal-Token": settings.identity_internal_token}
                if settings.identity_internal_token
                else None
            )
            identity_tenant = await dapr.invoke_get(
                settings.identity_app_id, f"v1/tenants/{tenant_slug}", headers=headers
            )
            if isinstance(identity_tenant, dict):
                ctx.timezone = identity_tenant.get("timezone") or ctx.timezone
                ctx.currency = identity_tenant.get("currency") or ctx.currency
                ctx.locale = identity_tenant.get("locale") or ctx.locale
                ctx.terminology = identity_tenant.get("terminology") or ctx.terminology
                ctx.tenant_id = str(identity_tenant.get("id") or ctx.tenant_id)
                _apply_pack(ctx, identity_tenant)
        except Exception as exc:  # noqa: BLE001 - enrichment only
            log.warning("identity tenant fetch failed", slug=tenant_slug, error=str(exc))

    async def _knowledge_leg() -> None:
        # 3. Knowledge snippets for grounding (best-effort).
        try:
            kb = await dapr.invoke_get(
                settings.knowledge_app_id,
                "v1/context",
                params={"tenant": tenant_slug, "q": settings.knowledge_query},
            )
            items = []
            if isinstance(kb, dict):
                items = kb.get("snippets") or kb.get("results") or []
            elif isinstance(kb, list):
                items = kb
            for item in items[: settings.knowledge_snippet_count]:
                if isinstance(item, dict):
                    text = item.get("content") or item.get("text") or item.get("title")
                else:
                    text = str(item)
                if text:
                    ctx.knowledge_snippets.append(str(text))
        except Exception as exc:  # noqa: BLE001 - grounding is optional
            log.warning("knowledge context fetch failed", slug=tenant_slug, error=str(exc))

    await asyncio.gather(_identity_leg(), _knowledge_leg())

    log.info(
        "tenant context bootstrapped",
        site_slug=site_slug,
        tenant_slug=ctx.tenant_slug,
        offerings=len(ctx.offerings),
        snippets=len(ctx.knowledge_snippets),
    )
    return ctx
