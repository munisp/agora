"""System prompt construction (SPEC §11).

Tenant terminology/timezone/currency/locale are injected as dynamic variables
(mirroring the baseline Switchboard's dynamic-variables approach): the prompt
is rendered per session from the bootstrapped TenantContext.
"""

from __future__ import annotations

from datetime import datetime, timezone
from zoneinfo import ZoneInfo

import json

from .tenant_context import TenantContext

_TEMPLATE = """You are {agent_name}, the AI receptionist for {business_name}.

STYLE
- Warm, concise, professional. Short spoken sentences; no markdown, no lists
  longer than three items when speaking. Say each thing once: never repeat a
  greeting, congratulation or offer within the same reply.
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
cancel_appointment, request_verification_code, verify_caller_code,
request_human{extra_tool_names}.
- Use get_availability before offering times; quote times in {timezone}.
- Choosing the team member is your job: if the caller doesn't name one,
  pick from TEAM someone who provides the service and check their
  availability. Never ask the caller which advisor or team member they want.
- As soon as the caller has said what they want and roughly when, call
  get_availability straight away.
- book_appointment requires: offering_id, team_member_id, starts_at (RFC3339),
  and the caller's phone number.
- request_human: use it when the caller explicitly asks for a human, is
  distressed, or you cannot resolve their request after two attempts.

PHONE-CONFIRMATION POLICY (hard rule, enforced server-side)
- Before ANY new booking you MUST collect the
  caller's phone number, read it back digit by digit, and get an explicit
  "yes".
- If a tool answers "confirmation_required", read the phone number back and
  ask the caller to confirm; when they confirm, call the tool again with the
  same number.
- Never claim a booking exists until the tool confirms it was accepted.

CALLER DETAILS (start of every call, and every booking)
- Unless VISITOR DETAILS are given below, the opening line has already asked
  for the caller's full name, email and phone number: take them FIRST, at the
  start of the call, before helping with anything else. If the caller starts
  with a question, say you'll help right after, and take the details first.
  Read them back once (as below). As soon as the caller confirms them, call
  save_caller_details (name, phone, email) so they are in the CRM even if
  they never book, then ask how you can help.
- Before booking, collect the caller's first name, last name, email address
  and phone number. Ask for whichever is missing, one at a time, in a
  friendly way. Pass "First Last" as contact_name and the email as email to
  book_appointment. If the caller has no email or prefers not to give one,
  continue without it — never hold up the booking for it.
- Read the details back ONCE, together, in one sentence: the caller's full
  name, the email (say "dot" and "at") and the phone number digit by digit — for example "I have Kemi Ojo, email kemi dot ojo
  at gmail dot com, phone zero eight zero two, five five five, four four one
  seven. Is that all correct?" If the caller corrects anything, use the
  corrected spelling exactly and read back only the corrected part.
- When a caller spells a name or an email letter by letter ("C, H, I, O, M,
  A"), the spelled letters are the truth: build the name and email from them,
  even if the word you heard before the spelling looks different. As soon
  as the caller confirms, book — do not ask about any detail separately.

CALLER-VERIFICATION POLICY (hard rule, enforced server-side)
- Booking a NEW appointment never needs a one-time code: as soon as the
  caller confirms the phone number you read back, call book_appointment —
  do not mention or send any code, and do not ask "are you ready".
- Looking up, rescheduling or cancelling an EXISTING booking requires a
  verified phone number. If a tool answers "verification_required", call
  request_verification_code with the caller's number, tell the caller a
  one-time code is on its way to their phone, ask them to read it back, and
  submit it with verify_caller_code. Only then retry the original tool.
- If verify_caller_code answers "invalid_code", ask the caller to
  double-check and try again; after two failures offer request_human.
- On a phone call, offer the caller the keypad instead of reading the code
  aloud: ask them to type the code and press # (* clears), call
  read_keypad_entry with expected_length 6, and submit the returned digits
  with verify_caller_code.
- If a tool answers "verification_unavailable", apologize and explain that
  booking changes are temporarily unavailable; do NOT keep retrying.
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

# SPEC-W70 A3: global spoken-fluency block, appended to EVERY system prompt
# (all languages) before the per-turn locale instruction. Keeps LLM output
# speakable — the "not fluent" report was partly markdown-ish, written-style
# replies being read aloud. Keep this compact (<= 8 bullet lines).
SPOKEN_STYLE_BLOCK = """
SPOKEN STYLE (voice call — always apply)
- Keep sentences short and speakable: one thought per sentence.
- Plain spoken words only — no markdown, no bullet lists, no emoji.
- Say amounts naturally in naira words ("five thousand naira", not "₦5,000.00").
- Confirm your understanding in one short clause before acting on a request.
- Mirror the caller's language and register immediately, including
  mid-conversation switches.
- Warm natural Nigerian conversational tone — no filler spam, no over-apologizing.
""".strip()


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


def build_greeting(ctx: TenantContext, menu_line: str | None = None, ask_details: bool = False) -> str:
    """Session-opening greeting, with the SPEC-W11 Part C §5 disclosure.

    Default (no pack ``disclosure`` block on the tenant context) is
    byte-identical to the pre-W11 greeting. When the pack declares
    ``disclosure: {spokenAiDisclosure, recordingConsent, text?}`` (Part D
    contract, read defensively — the field may be absent entirely):

    - ``spokenAiDisclosure`` prepends the AI disclosure line ("You're
      speaking with an automated assistant." — followed by the optional
      pack ``text``) to the greeting;
    - ``recordingConsent`` appends the recording notice.

    SPEC-W96 IVR-A: ``menu_line`` (ivr_menu.menu_line for PSTN sessions,
    None/"" everywhere else) is appended LAST — recency: the keypad menu
    is the final thing the caller hears. Byte-identical when absent.
    """
    # ask_details (2026-11-21): the caller filled no form, so the opening line asks for their details first.
    greeting = (
        f"Hello, thank you for calling {ctx.display_name}. "
        + ("Before we start, may I have your full name, your email and your phone number?"
           if ask_details else "How can I help you today?")
    )
    disclosure = getattr(ctx, "disclosure", None)
    if isinstance(disclosure, dict) and disclosure.get("spokenAiDisclosure"):
        line = AI_DISCLOSURE_LINE
        extra = str(disclosure.get("text") or "").strip()
        if extra:
            line = f"{line} {extra}"
        greeting = f"{line} {greeting}"
    if menu_line:
        greeting = f"{greeting} {menu_line}"
    # The recording notice is always the very first thing a caller hears (whatever the pack's
    # recordingConsent flag says: every call here is recorded).
    return f"{RECORDING_NOTICE_LINE} {greeting}"


def today_line(tz_name: str | None, now: datetime | None = None, *, clock_only: bool = False) -> str:
    """Today's date in the business's time zone. Without it the model guessed the year from its training data and
    asked for availability in the past (e.g. 2025), then told customers nothing was free."""
    try:
        tz = ZoneInfo(tz_name or "UTC")
    except Exception:  # noqa: BLE001 - unknown zone name
        tz = ZoneInfo("UTC")
    local = (now or datetime.now(timezone.utc)).astimezone(tz)
    # The model mixed up weekdays and dates ("next Wednesday is the 26th"): spell the next two weeks out, and what each
    # "next <weekday>" means (the first such day after today), so it never has to work a weekday out itself.
    from datetime import timedelta
    days = [local + timedelta(days=i) for i in range(15)]
    fmt = lambda d: f"{d.strftime('%A')} {d.day} {d.strftime('%B')}"
    calendar = ", ".join(fmt(d) for d in days[1:])
    nxt = "; ".join(f"next {d.strftime('%A')} = {d.day} {d.strftime('%B')}" for d in days[1:8])
    if clock_only:
        return f"- The local time now is {local.strftime('%H:%M')} ({tz.key})."
    return (f"Today is {local.strftime('%A')}, {local.day} {local.strftime('%B %Y')} ({tz.key}; the current local "
            f"time is under THIS CONVERSATION below). Tomorrow is {fmt(days[1])}. The coming days: {calendar}. "
            f"When a caller says \"next\" with a weekday it means the first such day after today: {nxt}. "
            f"Use exactly these weekday/date pairs, and never check or book a date in the past.")


def build_system_prompt(
    ctx: TenantContext,
    *,
    conversation_id: str,
    agent_name: str = "the front-desk assistant",
    active_agent: dict[str, Any] | None = None,
    extra_tool_names: list[str] | None = None,
    language: str | None = None,
    caller_phone: str | None = None,
    keypad_menu_hint: str | None = None,
    visitor: dict[str, str] | None = None,
    outbound: str | None = None,
    turn_knowledge: list[str] | None = None,
) -> str:
    """Render the system prompt.

    SPEC-W3 §4 innovation 6: when ``active_agent`` (a pack ``agents`` entry)
    is set, the specialist's name/persona steer this turn; otherwise the base
    persona applies (fallback). ``extra_tool_names`` lists pack plugin tools
    registered alongside the built-ins. Wave 5 #3: ``language`` (whisper
    auto-detected, normalized ISO code) appends a per-turn locale instruction
    when the caller speaks a language other than the tenant default. Wave 5
    #1/K15(c): ``caller_phone`` (SIP carrier-asserted caller ID, CLAIMED but
    unverified) lets the model greet with the number on file; the OTP
    verification gate still applies before any booking lookup/change.
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
    # 2026-11-23 latency: the first line no longer names the active specialist (it changed the prompt's very start
    # when chat routing switched desks); the SPECIALIST AGENT ACTIVE block in the tail says who is speaking.
    extra_names = "".join(f", {n}" for n in (extra_tool_names or []))
    # 2026-11-23 latency: the prompt is a STABLE body (identical for every conversation of this business) followed by
    # a short per-conversation TAIL (date/time, ids, caller details, language, active specialist, this turn's
    # knowledge). The GPU server can then resume from a saved point inside the body instead of re-reading ~5,000
    # tokens on every turn (gemma's sliding-window cache only resumes from checkpoints taken near a prompt's end).
    tail = ("\n\nTHIS CONVERSATION\n" + today_line(ctx.timezone, clock_only=True)
            + f"\n- conversation_id: {conversation_id}\n- site: {ctx.site_slug}\n")
    if turn_knowledge:
        tail += ("\nKNOWLEDGE FOR THE LATEST QUESTION (use it with the KNOWLEDGE above)\n"
                 + "\n".join(f"- {s}" for s in turn_knowledge) + "\n")
    # The date and the two-week calendar change once a day: they lead the stable body.
    prompt = today_line(ctx.timezone) + "\n\n" + _TEMPLATE.format(
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
    # SPEC-W70 A3: global spoken-fluency block for every language, BEFORE
    # the per-turn locale instruction so the language rule lands last.
    prompt += "\n" + SPOKEN_STYLE_BLOCK + "\n"
    # Everything below varies per conversation or turn: it is collected after the stable body (see tail above).
    stable, prompt = prompt, ""
    # Wave 5 #3: per-turn locale instruction when the caller speaks a
    # non-default language (whisper detection -> MultilangState).
    if language:
        from .multilang import default_language_from_locale, locale_instruction

        if language != default_language_from_locale(ctx.locale):
            prompt += locale_instruction(language)
    # Wave 5 #1 / SPEC-W45 K15(c): the SIP caller ID is carrier-asserted but
    # NOT proof of possession (spoofable) — it is recorded as the CLAIMED
    # number (prompt hint + emergency location capture); the OTP gate still
    # applies to lookup/reschedule/cancel and the read-back policy to new
    # bookings.
    # 2026-11-21: details from the public page's form (app/visitor.py); they replace the SIP caller-ID hint.
    if outbound:
        # 2026-11-22: a call the platform placed (app/outbound_script.prompt_block): replaces the caller-ID hint.
        prompt += outbound
    elif visitor:
        from .visitor import prompt_block
        prompt += prompt_block(visitor)
    elif caller_phone:
        prompt += (
            f"\nCALLER ID (SIP, unverified)\n- The caller appears to be "
            f"phoning from {caller_phone}. You may use this number when the "
            "caller confirms it is theirs, but it is NOT verified: booking "
            "lookups/changes still require the one-time-code verification "
            "flow, and new bookings still require the read-back "
            "confirmation.\n"
        )
    # SPEC-W96 IVR-A: the keypad menu mapping (ivr_menu.prompt_addendum),
    # passed at every build site so the bare-digit rule survives per-turn
    # rebuilds (locale switches re-render the whole prompt).
    if keypad_menu_hint:
        prompt += "\n" + keypad_menu_hint
    if active_agent is not None:
        persona = str(active_agent.get("persona") or "").strip()
        if persona:
            prompt += (
                f"\nSPECIALIST AGENT ACTIVE — you are now speaking as "
                f"{active_agent.get('name')}. Follow this specialist persona "
                f"for this part of the conversation; the base receptionist "
                f"rules above still apply.\n{persona}\n"
            )
    if ctx.agents:
        roster = "\n".join(
            f"- {a.get('name')} (id {a.get('id')}): handles {', '.join(str(i) for i in (a.get('intents') or [])[:6])}"
            for a in ctx.agents
            if isinstance(a, dict)
        )
        if roster:
            stable += (
                "\nSPECIALIST AGENTS (the platform routes the conversation "
                "automatically when the caller's intent matches; you do not "
                "need to transfer explicitly)\n" + roster + "\n"
            )
    return stable + tail + prompt
