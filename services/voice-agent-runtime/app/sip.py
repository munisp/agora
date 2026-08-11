"""SIP telephony inbound bootstrap (Wave 5 #1, STRATEGY §3).

Room/dispatch contract (deploy/livekit-sip/dispatch-rule.yaml): a LiveKit SIP
*callee* dispatch rule with room prefix ``call-`` creates one room per dialed
number named ``call-{dialed E.164}``. The worker (app/livekit_worker.py)
treats any ``call-*`` room — or any room joined by a SIP participant — as an
inbound PSTN call and runs this bootstrap before the normal receptionist
flow:

1. Tenant resolution: the DIALED number (which of our provisioned numbers
   the customer called) selects the tenant via ``TENANT_PHONE_MAP``, a JSON
   object ``{"+15551234567": "acme"}`` mapping E.164 number -> site/tenant
   slug. This static env map is DEV-MODE: the production design stores the
   number -> tenant assignment in a ``phone_numbers`` table owned by
   booking-service (provisioning API + per-tenant number inventory, see
   docs/telephony.md §number-mapping); the env map mirrors its lookup
   semantics 1:1 so the swap is transparent to this module.
2. Caller identity: the SIP caller ID is extracted from the LiveKit SIP
   participant (attributes ``sip.phoneNumber`` / identity ``sip_<from>``,
   falling back to the room name) and attached to session state as the
   *confirmed* phone.

POLICY — caller-ID confirmation bypass: the phone-confirmation policy
(session_state.require_confirmed_phone) exists because in web chat the
caller types their own number and a mis-heard number must be read back.
On the PSTN the calling number is asserted by the carrier in the SIP
``From``/``P-Asserted-Identity`` headers and delivered by LiveKit as the
participant's authenticated phone attribute — reading it back adds a turn
without adding assurance. SIP sessions therefore start with
``confirmed_phone`` already set (carrier-asserted). The bypass is scoped to
SIP-originated sessions only; the web/chat path keeps the two-step
confirmation. Disable with ``PHONE_CONFIRMATION_REQUIRED=false`` semantics
unchanged — a missing/anonymous caller ID simply means no pre-confirmed
phone and the normal two-step flow applies.

This module is deliberately free of livekit imports (duck-typed
participants) so it stays unit-testable without the server SDK.
"""

from __future__ import annotations

import json
import re
from dataclasses import dataclass, field
from typing import Any, Iterable

from .logging import get_logger
from .session_state import SessionState

log = get_logger("sip")

CALL_ROOM_PREFIX = "call-"

# LiveKit SIP participant metadata (documented attribute keys).
SIP_ATTR_CALLER = "sip.phoneNumber"  # caller ID (From / P-Asserted-Identity)
SIP_ATTR_DIALED = "sip.trunkPhoneNumber"  # dialed number (the trunk's number)
SIP_IDENTITY_PREFIX = "sip_"
SIP_KIND_NAMES = {"SIP", "sip"}  # livekit ParticipantInfo.Kind.SIP

_PHONE_STRIP_RE = re.compile(r"[\s\-().]")
_E164_RE = re.compile(r"^\+[1-9]\d{4,14}$")


class SipTenantResolutionError(RuntimeError):
    """The dialed number maps to no tenant and no default site is set."""


def normalize_phone(number: str | None) -> str:
    """Normalize a phone string to a canonical compare form: strips spaces,
    dashes, dots and parentheses, keeps the leading ``+``. Returns "" for
    empty/anonymous input."""
    if not number:
        return ""
    return _PHONE_STRIP_RE.sub("", str(number).strip())


def parse_tenant_phone_map(raw: str | dict | None) -> dict[str, str]:
    """Parse ``TENANT_PHONE_MAP`` (JSON ``{"+1555…": "tenant-slug"}``).

    Tolerant by design (env hygiene must not crash the worker): entries with
    non-E.164 keys or empty slugs are dropped with a warning; invalid JSON
    yields an empty map."""
    if not raw:
        return {}
    data: Any = raw
    if isinstance(raw, str):
        try:
            data = json.loads(raw)
        except json.JSONDecodeError:
            log.warning("TENANT_PHONE_MAP is not valid JSON; ignoring", raw=raw[:80])
            return {}
    if not isinstance(data, dict):
        log.warning("TENANT_PHONE_MAP must be a JSON object; ignoring")
        return {}
    out: dict[str, str] = {}
    for number, slug in data.items():
        n = normalize_phone(str(number))
        s = str(slug).strip()
        if not _E164_RE.match(n):
            log.warning("TENANT_PHONE_MAP: dropping non-E.164 key", number=str(number)[:40])
            continue
        if not s:
            log.warning("TENANT_PHONE_MAP: dropping empty tenant slug", number=n)
            continue
        out[n] = s
    return out


def is_sip_room(room_name: str) -> bool:
    return bool(room_name) and room_name.startswith(CALL_ROOM_PREFIX)


def is_sip_participant(participant: Any) -> bool:
    """Duck-typed check: a LiveKit SIP participant has kind SIP, an
    ``sip_<from>`` identity, or ``sip.*`` attributes."""
    if participant is None:
        return False
    kind = getattr(participant, "kind", None)
    kind_name = getattr(kind, "name", kind)
    if isinstance(kind_name, str) and kind_name in SIP_KIND_NAMES:
        return True
    identity = getattr(participant, "identity", "") or ""
    if identity.startswith(SIP_IDENTITY_PREFIX):
        return True
    attrs = getattr(participant, "attributes", None) or {}
    return any(str(k).startswith("sip.") for k in attrs)


def _participant_attr(participant: Any, key: str) -> str:
    attrs = getattr(participant, "attributes", None) or {}
    value = attrs.get(key, "")
    return str(value).strip() if value else ""


def _caller_from_identity(identity: str) -> str:
    """``sip_+15551234567_abc123`` -> ``+15551234567`` (LiveKit prefixes the
    SIP From user; a random suffix may follow a second underscore)."""
    if not identity.startswith(SIP_IDENTITY_PREFIX):
        return ""
    rest = identity[len(SIP_IDENTITY_PREFIX):]
    candidate = rest.split("_", 1)[0] if "_" in rest else rest
    return normalize_phone(candidate)


@dataclass
class InboundCallContext:
    """Result of the SIP inbound bootstrap."""

    site_slug: str  # tenant/site slug the receptionist session binds to
    caller_phone: str = ""  # normalized caller ID (may be "" when anonymous)
    dialed_number: str = ""
    tenant_source: str = ""  # registry|map|default — for logs/metrics
    attributes: dict[str, str] = field(default_factory=dict)
    # SPEC-W38 F1: agent record when the dialed number resolved via the
    # agents registry (app/agents_registry.py); None on the legacy paths.
    agent_record: Any = None


def extract_call_info(
    room_name: str, participants: Iterable[Any] = ()
) -> tuple[str, str, dict[str, str]]:
    """Extract (caller_phone, dialed_number, sip attributes).

    Precedence: SIP participant attributes (carrier/LiveKit asserted) ->
    participant identity -> room name (``call-{number}`` gives the DIALED
    number per the callee dispatch rule)."""
    caller, dialed = "", ""
    attrs: dict[str, str] = {}
    for p in participants:
        if not is_sip_participant(p):
            continue
        caller = caller or normalize_phone(_participant_attr(p, SIP_ATTR_CALLER))
        dialed = dialed or normalize_phone(_participant_attr(p, SIP_ATTR_DIALED))
        caller = caller or _caller_from_identity(getattr(p, "identity", "") or "")
        raw_attrs = getattr(p, "attributes", None) or {}
        attrs.update({str(k): str(v) for k, v in raw_attrs.items()})
        if caller or dialed:
            break
    room_number = normalize_phone(room_name[len(CALL_ROOM_PREFIX):]) if is_sip_room(room_name) else ""
    # The room name carries the DIALED number (callee dispatch); only treat
    # it as caller ID when nothing else is available AND it cannot be the
    # dialed number (individual dispatch variants prefix the caller).
    dialed = dialed or room_number
    return caller, dialed, attrs


def resolve_tenant(
    dialed_number: str, phone_map: dict[str, str], default_site: str = ""
) -> tuple[str, str]:
    """Map a dialed number to a tenant/site slug. Returns (slug, source)."""
    number = normalize_phone(dialed_number)
    if number and number in phone_map:
        return phone_map[number], "map"
    if default_site:
        return default_site, "default"
    raise SipTenantResolutionError(
        f"no tenant mapped for dialed number {number or '<unknown>'}"
    )


async def resolve_agent_for_dialed(
    settings: Any,
    dialed_number: str,
    registry: Any = None,
) -> tuple[str, str, Any]:
    """SPEC-W38 F1: dialed number -> (tenant slug, source, agent record).

    Resolution order:
    1. agents registry (conversation-service, app/agents_registry.py) —
       fail-open: 404/network/timeout yield None and we fall through;
    2. legacy TENANT_PHONE_MAP;
    3. SIP_DEFAULT_SITE.

    Returns (site_slug, source, AgentRecord | None) where source is
    ``registry|map|default``; the agent record is None on the legacy paths.
    Raises SipTenantResolutionError exactly like resolve_tenant when nothing
    resolves. ``registry`` is injectable for tests; when omitted the shared
    client is built from settings (None when AGENTS_REGISTRY_URL is empty).
    """
    from . import agents_registry, metrics

    number = normalize_phone(dialed_number)
    client = registry
    if client is None:
        client = agents_registry.get_registry_client(settings)
    if number and client is not None:
        record = await client.resolve_agent_by_phone(number)
        if record is not None:
            metrics.agent_resolution("registry")
            slug = record.tenant_slug
            if not slug:
                # The registry resolved the agent but did not carry a tenant
                # slug; reuse the legacy map/default purely for the slug so
                # the session can still bootstrap a TenantContext.
                slug, _ = resolve_tenant(
                    number,
                    getattr(settings, "tenant_phone_map", None) or {},
                    getattr(settings, "sip_default_site", "") or "",
                )
            log.info(
                "dialed number resolved via agents registry",
                dialed=number,
                agent_id=record.id,
                tenant=slug,
            )
            return slug, "registry", record
    phone_map = getattr(settings, "tenant_phone_map", None) or {}
    default_site = getattr(settings, "sip_default_site", "") or ""
    slug, source = resolve_tenant(number, phone_map, default_site)
    metrics.agent_resolution("env_map" if source == "map" else "default")
    return slug, source, None


def attach_caller_id(session: SessionState, caller_phone: str) -> bool:
    """POLICY (see module docstring): the SIP caller ID is carrier-asserted,
    so it enters the session as the *confirmed* phone, bypassing the two-step
    read-back. Returns True when a phone was attached. Anonymous/withheld
    caller IDs leave the session untouched (normal confirmation applies)."""
    phone = normalize_phone(caller_phone)
    if not phone:
        return False
    session.confirmed_phone = phone
    session.pending_phone = None
    session.touch()
    return True


def bootstrap_inbound_call(
    settings: Any,
    room_name: str,
    participants: Iterable[Any] = (),
    session: SessionState | None = None,
) -> InboundCallContext:
    """Resolve tenant + caller identity for an inbound SIP call.

    ``settings`` carries ``tenant_phone_map`` (parsed TENANT_PHONE_MAP) and
    ``sip_default_site``. Raises SipTenantResolutionError when the dialed
    number is unmapped and no default site is configured."""
    phone_map = getattr(settings, "tenant_phone_map", None) or {}
    default_site = getattr(settings, "sip_default_site", "") or ""
    caller, dialed, attrs = extract_call_info(room_name, participants)
    site_slug, source = resolve_tenant(dialed, phone_map, default_site)
    ctx = InboundCallContext(
        site_slug=site_slug,
        caller_phone=caller,
        dialed_number=dialed,
        tenant_source=source,
        attributes=attrs,
    )
    if session is not None:
        attached = attach_caller_id(session, ctx.caller_phone)
        log.info(
            "sip caller id attached",
            confirmed=attached,
            caller=ctx.caller_phone or "<anonymous>",
        )
    log.info(
        "sip inbound call bootstrapped",
        room=room_name,
        dialed=ctx.dialed_number or "<unknown>",
        tenant=site_slug,
        tenant_source=source,
        caller=ctx.caller_phone or "<anonymous>",
    )
    return ctx


async def bootstrap_inbound_call_async(
    settings: Any,
    room_name: str,
    participants: Iterable[Any] = (),
    session: SessionState | None = None,
    registry: Any = None,
) -> InboundCallContext:
    """SPEC-W38 F1 variant of bootstrap_inbound_call: registry-first tenant
    resolution via resolve_agent_for_dialed (agents registry, fail-open to
    the legacy TENANT_PHONE_MAP/SIP_DEFAULT_SITE path). The resolved agent
    record (when any) rides on ``ctx.agent_record`` for the worker to merge
    its definition; caller-ID handling is unchanged."""
    caller, dialed, attrs = extract_call_info(room_name, participants)
    site_slug, source, record = await resolve_agent_for_dialed(
        settings, dialed, registry=registry
    )
    ctx = InboundCallContext(
        site_slug=site_slug,
        caller_phone=caller,
        dialed_number=dialed,
        tenant_source=source,
        attributes=attrs,
        agent_record=record,
    )
    if session is not None:
        attached = attach_caller_id(session, ctx.caller_phone)
        log.info(
            "sip caller id attached",
            confirmed=attached,
            caller=ctx.caller_phone or "<anonymous>",
        )
    log.info(
        "sip inbound call bootstrapped",
        room=room_name,
        dialed=ctx.dialed_number or "<unknown>",
        tenant=site_slug,
        tenant_source=source,
        agent_id=getattr(record, "id", "") or "",
        caller=ctx.caller_phone or "<anonymous>",
    )
    return ctx
