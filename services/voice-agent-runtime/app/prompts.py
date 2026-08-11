"""System prompt construction (SPEC §11).

Tenant terminology/timezone/currency/locale are injected as dynamic variables
(mirroring the baseline Switchboard's dynamic-variables approach): the prompt
is rendered per session from the bootstrapped TenantContext.
"""

from __future__ import annotations

import json

from .tenant_context import TenantContext

_TEMPLATE = """You are {agent_name}, the AI receptionist for {business_name}.

STYLE
- Warm, concise, professional. Short spoken sentences; no markdown, no lists
  longer than three items when speaking.
- Never invent catalog facts, prices or availability. Use the tools.
- Locale: {locale}. Currency: {currency}. The business timezone is {timezone};
  reason about opening hours and dates in that timezone.

TERMINOLOGY (use these words with the caller)
{terminology}

CATALOG (offerings)
{offerings}

TEAM
{team_members}

KNOWLEDGE
{knowledge}

TOOLS
You have exactly these tools: get_business_info, get_availability,
book_appointment, lookup_appointment, reschedule_appointment,
cancel_appointment, request_human{extra_tool_names}.
- Use get_availability before offering times; quote times in {timezone}.
- book_appointment requires: offering_id, team_member_id, starts_at (RFC3339),
  and the caller's phone number.
- request_human: use it when the caller explicitly asks for a human, is
  distressed, or you cannot resolve their request after two attempts.

PHONE-CONFIRMATION POLICY (hard rule, enforced server-side)
- Before ANY booking, lookup, reschedule or cancellation you MUST collect the
  caller's phone number, read it back digit by digit, and get an explicit
  "yes".
- If a tool answers "confirmation_required", read the phone number back and
  ask the caller to confirm; when they confirm, call the tool again with the
  same number.
- Never claim a booking exists until the tool confirms it was accepted.

CALLER CONTEXT
- conversation_id: {conversation_id}
- site: {site_slug}
"""


# SPEC-W11 Part C: location-first addendum, injected as an extra system
# message the moment the live-turn emergency detector (app/emergency.py)
# latches. Emergency packs get location-first + reassurance behavior: the
# agent confirms the EXACT location before anything else, then reassures.
EMERGENCY_LOCATION_FIRST_ADDENDUM = """
EMERGENCY MODE ACTIVE (the caller's last message matched the emergency
lexicon; a human operator has already been notified via warm handoff)
- LOCATION FIRST: before ANY other question, confirm the caller's exact
  location (street address, nearest landmark or junction, area/town). Ask
  one short question at a time and read the location back to confirm it.
- As soon as the location is clear, call the capture_location tool with it
  (spoken address in address_text; lat/lng only if the caller gives them).
- Reassure: stay calm, tell the caller help is being notified, keep them on
  the line, and do not end the call.
- Skip all booking/confirmation policies — do NOT ask for phone-number
  read-back; the emergency flow bypasses them.
""".strip()

# SPEC-W11 Part C §5: spoken AI disclosure defaults (used when the pack's
# `disclosure` block does not override `text` / only flags are set).
AI_DISCLOSURE_LINE = "You're speaking with an automated assistant."
RECORDING_NOTICE_LINE = "This call may be recorded for quality and safety."


def truncate_knowledge_snippets(
    snippets: list[str], budget_tokens: int | None
) -> list[str]:
    """SPEC-W38 F2 ``context_budget_tokens``: truncate knowledge snippets to
    fit the token budget (approx 4 chars/token). Snippets are kept whole in
    order until the budget would overflow; the snippet that crosses the
    budget is hard-truncated and nothing after it is included. A None or
    non-positive budget leaves the snippets untouched."""
    if not budget_tokens or budget_tokens <= 0:
        return list(snippets)
    max_chars = budget_tokens * 4
    out: list[str] = []
    used = 0
    for snippet in snippets:
        remaining = max_chars - used
        if remaining <= 0:
            break
        text = str(snippet)
        if len(text) > remaining:
            out.append(text[:remaining])
            break
        out.append(text)
        used += len(text)
    return out


def build_greeting(ctx: TenantContext) -> str:
    """Session-opening greeting, with the SPEC-W11 Part C §5 disclosure.

    Default (no pack ``disclosure`` block on the tenant context) is
    byte-identical to the pre-W11 greeting. When the pack declares
    ``disclosure: {spokenAiDisclosure, recordingConsent, text?}`` (Part D
    contract, read defensively — the field may be absent entirely):

    - ``spokenAiDisclosure`` prepends the AI disclosure line ("You're
      speaking with an automated assistant." — followed by the optional
      pack ``text``) to the greeting;
    - ``recordingConsent`` appends the recording notice.
    """
    greeting = (
        f"Hello, thank you for calling {ctx.display_name}. "
        "How can I help you today?"
    )
    disclosure = getattr(ctx, "disclosure", None)
    if not isinstance(disclosure, dict):
        return greeting
    if disclosure.get("spokenAiDisclosure"):
        line = AI_DISCLOSURE_LINE
        extra = str(disclosure.get("text") or "").strip()
        if extra:
            line = f"{line} {extra}"
        greeting = f"{line} {greeting}"
    if disclosure.get("recordingConsent"):
        greeting = f"{greeting} {RECORDING_NOTICE_LINE}"
    return greeting


def build_system_prompt(
    ctx: TenantContext,
    *,
    conversation_id: str,
    agent_name: str = "the front-desk assistant",
    active_agent: dict[str, Any] | None = None,
    extra_tool_names: list[str] | None = None,
    language: str | None = None,
    caller_phone: str | None = None,
) -> str:
    """Render the system prompt.

    SPEC-W3 §4 innovation 6: when ``active_agent`` (a pack ``agents`` entry)
    is set, the specialist's name/persona steer this turn; otherwise the base
    persona applies (fallback). ``extra_tool_names`` lists pack plugin tools
    registered alongside the built-ins. Wave 5 #3: ``language`` (whisper
    auto-detected, normalized ISO code) appends a per-turn locale instruction
    when the caller speaks a language other than the tenant default. Wave 5
    #1: ``caller_phone`` (SIP carrier-asserted caller ID, already confirmed)
    tells the model to skip the read-back confirmation for that number.
    """
    terminology = (
        json.dumps(ctx.terminology, ensure_ascii=False, indent=2)
        if ctx.terminology
        else "(default terminology)"
    )
    # SPEC-W38 F2: the agent definition (agents registry) bounds the
    # knowledge block via context_budget_tokens (~4 chars/token); absent a
    # definition the snippet list is used as-is.
    definition = getattr(ctx, "agent_definition", None)
    snippets = ctx.knowledge_snippets
    if definition is not None:
        snippets = truncate_knowledge_snippets(
            snippets, definition.context_budget_tokens
        )
    knowledge = (
        "\n".join(f"- {s}" for s in snippets)
        if snippets
        else "- (no extra knowledge available)"
    )
    if active_agent is not None:
        agent_name = str(active_agent.get("name") or agent_name)
    extra_names = "".join(f", {n}" for n in (extra_tool_names or []))
    prompt = _TEMPLATE.format(
        agent_name=agent_name,
        business_name=ctx.display_name or ctx.site_slug,
        locale=ctx.locale,
        currency=ctx.currency,
        timezone=ctx.timezone,
        terminology=terminology,
        offerings=ctx.offering_summary(),
        team_members=ctx.team_summary(),
        knowledge=knowledge,
        conversation_id=conversation_id,
        site_slug=ctx.site_slug,
        extra_tool_names=extra_names,
    )
    # SPEC-CRM §C4: append the industry pack persona when the tenant's pack
    # provides one (guarded — absent for tenants without a resolved pack).
    # SPEC-W38 F2: merge_definition replaces ctx.agent_persona with the
    # agent definition's persona when set (definition wins over pack).
    if ctx.agent_persona:
        header = (
            "AGENT PERSONA"
            if definition is not None and definition.persona.strip()
            else "INDUSTRY PERSONA"
        )
        prompt += (
            f"\n{header} (follow this guidance on tone, policies and domain knowledge)"
            f"\n{ctx.agent_persona}\n"
        )
    # SPEC-W38 F2: definition.instructions is its own system-prompt block.
    if definition is not None and definition.instructions.strip():
        prompt += f"\nAGENT INSTRUCTIONS\n{definition.instructions.strip()}\n"
    # Wave 5 #3: per-turn locale instruction when the caller speaks a
    # non-default language (whisper detection -> MultilangState).
    if language:
        from .multilang import default_language_from_locale, locale_instruction

        if language != default_language_from_locale(ctx.locale):
            prompt += locale_instruction(language)
    # Wave 5 #1: SIP caller ID is carrier-asserted and server-confirmed at
    # session bootstrap (app/sip.py policy); the read-back step would only
    # add friction, so the prompt records the number as already confirmed.
    if caller_phone:
        prompt += (
            f"\nCALLER ID (SIP, carrier-verified)\n- The caller is phoning "
            f"from {caller_phone}; this number is ALREADY CONFIRMED — use it "
            "directly for booking, lookup, reschedule and cancel tools and "
            "do NOT ask the caller to read it back or confirm it.\n"
        )
    if active_agent is not None:
        persona = str(active_agent.get("persona") or "").strip()
        if persona:
            prompt += (
                f"\nSPECIALIST AGENT ACTIVE — you are now speaking as "
                f"{active_agent.get('name')}. Follow this specialist persona "
                f"for this part of the conversation; the base receptionist "
                f"rules above still apply.\n{persona}\n"
            )
    elif ctx.agents:
        roster = "\n".join(
            f"- {a.get('name')} (id {a.get('id')}): handles {', '.join(str(i) for i in (a.get('intents') or [])[:6])}"
            for a in ctx.agents
            if isinstance(a, dict)
        )
        if roster:
            prompt += (
                "\nSPECIALIST AGENTS (the platform routes the conversation "
                "automatically when the caller's intent matches; you do not "
                "need to transfer explicitly)\n" + roster + "\n"
            )
    return prompt
