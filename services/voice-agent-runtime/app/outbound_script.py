"""What the receptionist says and does on a call the PLATFORM placed (POST /v1/calls, SPEC-W101), 2026-11-22.

An outbound call is not an inbound call: the person did not ring us, so we never thank them for calling or ask
for details we already have. The dialer passes a purpose and a few facts as SIP participant attributes
(``agora.*``, set by outbound.place_outbound_call); this module turns them into the opening line and a prompt block.

Privacy: nothing about the appointment is said until the person confirms they are the contact (anyone may answer
a phone). Security: dialing a number proves nothing about who answers, so the existing one-time-code verification
still gates looking up, changing or cancelling a booking; a brand-new booking keeps the read-back confirmation.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from .prompts import AI_DISCLOSURE_LINE, RECORDING_NOTICE_LINE

ATTR_PREFIX = "agora."
PURPOSES = ("confirm_appointment", "waitlist_offer", "no_show_rebook", "callback")
# Facts a dialer may pass (all optional, free text the receptionist reads out): what, with whom, when, why.
CONTEXT_KEYS = ("offering", "team_member", "when", "reason", "booking_ref")
MAX_VALUE = 160


@dataclass(frozen=True)
class OutboundBrief:
    purpose: str = ""  # one of PURPOSES, or "" for a general call
    contact_name: str = ""
    context: dict[str, str] = field(default_factory=dict)

    @property
    def first_name(self) -> str:
        return self.contact_name.split()[0] if self.contact_name.strip() else ""


def _clean(value: Any) -> str:
    return " ".join(str(value or "").split())[:MAX_VALUE]


def to_attributes(purpose: str | None, contact_name: str | None, context: dict[str, Any] | None) -> dict[str, str]:
    """The dialer side: the brief as SIP participant attributes (LiveKit carries them into the room)."""
    attrs: dict[str, str] = {}
    if purpose:
        attrs[ATTR_PREFIX + "purpose"] = purpose
    if _clean(contact_name):
        attrs[ATTR_PREFIX + "contact_name"] = _clean(contact_name)
    for key in CONTEXT_KEYS:
        value = _clean((context or {}).get(key))
        if value:
            attrs[ATTR_PREFIX + key] = value
    return attrs


def from_attributes(attrs: dict[str, str] | None) -> OutboundBrief:
    """The receptionist side: read the brief back (unknown purposes become a general call)."""
    attrs = attrs or {}
    purpose = attrs.get(ATTR_PREFIX + "purpose", "")
    return OutboundBrief(
        purpose=purpose if purpose in PURPOSES else "",
        contact_name=_clean(attrs.get(ATTR_PREFIX + "contact_name")),
        context={k: _clean(attrs[ATTR_PREFIX + k]) for k in CONTEXT_KEYS if _clean(attrs.get(ATTR_PREFIX + k))},
    )


def _on(when: str) -> str:
    relative = ("on ", "at ", "in ", "this ", "next ", "last ", "earlier", "today", "tonight", "tomorrow", "yesterday")
    return when if when.lower().startswith(relative) else f"on {when}"


def _about(brief: OutboundBrief) -> str:
    """'your Registration Call with Aisha Bello on Tuesday 24 November at 9 a.m.' from whatever facts we have."""
    c = brief.context
    parts = [f"your {c.get('offering') or 'appointment'}"]
    if c.get("team_member"):
        parts.append(f"with {c['team_member']}")
    if c.get("when"):
        parts.append(_on(c["when"]))
    return " ".join(parts)


def _end(text: str) -> str:
    """Close a sentence once: 'at 9 a.m.' + '.' must not read 'a.m..'."""
    return text if text.endswith((".", "?", "!")) else text + "."


def purpose_line(brief: OutboundBrief) -> str:
    """What the receptionist says once the person has confirmed who they are."""
    c = brief.context
    if brief.purpose == "confirm_appointment":
        return f"{_end(f'I am calling to confirm {_about(brief)}')} Can you still make it?"
    if brief.purpose == "waitlist_offer":
        bits = ([_on(c["when"])] if c.get("when") else []) + ([f"with {c['team_member']}"] if c.get("team_member") else [])
        slot = " ".join(bits) or "now"
        return (f"You asked to be told when a {c.get('offering') or 'appointment'} slot opens up. "
                f"{_end('One is free ' + slot)} Would you like it?")
    if brief.purpose == "no_show_rebook":
        return f"We missed you at {_about(brief)}, and I would like to help you find a new time. Would that be okay?"
    if brief.purpose == "callback":
        why = f" about {c['reason']}" if c.get("reason") else ""
        return f"{_end(f'You asked us to call you back{why}')} How can I help?"
    return "How can I help you today?"


def greeting(ctx: Any, brief: OutboundBrief) -> str:
    """Opening line of an outbound call: recording notice, who is calling, then an identity check (no details yet)."""
    business = getattr(ctx, "display_name", "") or getattr(ctx, "site_slug", "") or "us"
    line = f"Hello, this is the virtual assistant at {business}."
    if brief.contact_name:
        line += f" Am I speaking with {brief.contact_name}?"
    else:
        line += f" {purpose_line(brief)}"
    disclosure = getattr(ctx, "disclosure", None)
    if isinstance(disclosure, dict) and disclosure.get("spokenAiDisclosure"):
        line = f"{AI_DISCLOSURE_LINE} {line}"
    return f"{RECORDING_NOTICE_LINE} {line}"


_PURPOSE_RULES = {
    "confirm_appointment": (
        "- If they can make it: thank them, tell them a reminder will come before the appointment, and end the call "
        "warmly. If they cannot: offer to find a new time (check availability first); changing an existing booking "
        "needs the one-time-code verification as usual. If they want to cancel, follow the normal cancel flow."
    ),
    "waitlist_offer": (
        "- If they want the slot: check it is still free, read back the slot and their name once, and book it as soon "
        "as they confirm. If not: thank them and say they stay on the waiting list."
    ),
    "no_show_rebook": (
        "- Be kind; never blame them. Offer the next free times, read back the choice once and book it as soon as they "
        "confirm. "
        "If they would rather not rebook now, thank them and end the call."
    ),
    "callback": "- Help with what they asked about, exactly as you would on an inbound call.",
    "": "- Help with whatever they need, exactly as you would on an inbound call.",
}


def prompt_block(brief: OutboundBrief, callee_phone: str = "", whatsapp: bool = False) -> str:
    """System-prompt block for an outbound call (appended to the normal receptionist prompt)."""
    facts = [f"- Person we are calling: {brief.contact_name}" if brief.contact_name else "- Person we are calling: (name not given)"]
    if callee_phone:
        facts.append(f"- Number we dialed: {callee_phone} (we reached them on it, so it is already confirmed: use exactly "
                     "this number for any booking and do not read it back)")
    for key, label in (("offering", "Service"), ("team_member", "With"), ("when", "When"), ("booking_ref", "Booking reference"), ("reason", "Reason")):
        if brief.context.get(key):
            facts.append(f"- {label}: {brief.context[key]}")
    identity = (
        f"- Your opening line asked whether you are speaking with {brief.contact_name}. Say nothing about the "
        f"appointment until they confirm. Once they do, say: \"{purpose_line(brief)}\". If it is someone else, ask "
        "politely whether the person is available or when to call back; share no details with them. If it is a "
        "voicemail or answering machine, leave no details: say you will call back, and end the call."
        if brief.contact_name else
        "- Your opening line already said why you are calling."
    )
    return (
        "\nOUTBOUND CALL (you placed this call; the person did NOT call you)\n"
        "- Never say \"thank you for calling\". Do NOT ask for their name, phone number or email: you already "
        "have them below (offer to update the email only if they bring it up).\n"
        + "\n".join(facts) + "\n" + identity + "\n" + _PURPOSE_RULES.get(brief.purpose, _PURPOSE_RULES[""]) + "\n"
        + ("- Once they confirm the appointment or you book one, call send_whatsapp_confirmation once with the service, "
           "who it is with and the day and time. Only if it answers \"sent\", tell them a WhatsApp confirmation is on "
           "its way; never mention WhatsApp before that.\n" if whatsapp else
           "- Do not offer or mention WhatsApp messages on this call.\n")
        + "- Keep it short: this person did not plan to be on the phone now. If it is a bad time, offer to call back.\n"
        + "- Just before you say goodbye, call record_call_outcome once: confirmed, declined, rescheduled, resolved, "
          "call_back_later, wrong_person or no_answer (voicemail).\n"
    )
