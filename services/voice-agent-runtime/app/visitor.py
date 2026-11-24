"""Visitor details collected before a public chat or call (2026-11-21).

The public page asks for name, phone and email before the chat opens or the call starts. They reach the runtime
in the /voice/chat and /voice/session bodies (``visitor``), and for calls also in the caller's LiveKit token
metadata, so the worker can read them when the caller joins.

Trust: these are SELF-CLAIMED. The phone lands in ``session.claimed_phone`` (the existing web self-claim slot):
a prompt hint and the conversation's contact marker, never an authorization. Booking still reads the details
back for confirmation, and lookups/changes still need the one-time-code verification.
"""

from __future__ import annotations

import json
import re
from typing import Any

from pydantic import BaseModel, Field, field_validator

from . import verification

_EMAIL = re.compile(r"^[^@\s]{1,64}@[^@\s]{1,190}\.[A-Za-z]{2,24}$")


def canonical_phone(raw: str | None) -> str | None:
    """E.164 for a Nigerian local number ("0806 555 3124" -> "+2348065553124") or any E.164 input; None if unusable."""
    digits = re.sub(r"[^\d+]", "", raw or "")
    if re.fullmatch(r"0\d{10}", digits):
        digits = "+234" + digits[1:]
    elif re.fullmatch(r"234\d{10}", digits):
        digits = "+" + digits
    return verification.canonical_e164(digits) or None


class Visitor(BaseModel):
    name: str = Field(min_length=1, max_length=120)
    phone: str = Field(min_length=5, max_length=32)
    email: str | None = Field(default=None, max_length=254)

    @field_validator("name")
    @classmethod
    def _name(cls, v: str) -> str:
        v = " ".join(v.split())
        if not v:
            raise ValueError("name is required")
        return v

    @field_validator("phone")
    @classmethod
    def _phone(cls, v: str) -> str:
        p = canonical_phone(v)
        if not p:
            raise ValueError("phone must be a valid phone number")
        return p

    @field_validator("email")
    @classmethod
    def _email(cls, v: str | None) -> str | None:
        v = (v or "").strip()
        if not v:
            return None
        if not _EMAIL.match(v):
            raise ValueError("email is not valid")
        return v.lower()


def apply(session: Any, visitor: "Visitor | None") -> None:
    """Record the form on the session (first form wins; a resumed session keeps what it has)."""
    if visitor is None or getattr(session, "visitor_form", False):
        return
    session.caller_name = session.caller_name or visitor.name
    session.claimed_phone = session.claimed_phone or visitor.phone
    session.caller_email = getattr(session, "caller_email", None) or visitor.email
    session.visitor_form = True


def from_session(session: Any) -> dict[str, str] | None:
    """The form details to render in the prompt, or None when the visitor filled no form."""
    if not getattr(session, "visitor_form", False):
        return None
    out = {"name": session.caller_name or "", "phone": session.claimed_phone or "", "email": getattr(session, "caller_email", None) or ""}
    return {k: v for k, v in out.items() if v} or None


def prompt_block(details: dict[str, str] | None) -> str:
    if not details:
        return ""
    lines = [f"- Name: {details['name']}" if details.get("name") else "",
             f"- Phone: {details['phone']}" if details.get("phone") else "",
             f"- Email: {details['email']}" if details.get("email") else ""]
    return (
        "\nVISITOR DETAILS (known before this conversation: a pre-chat form, or the address an e-mail came from; "
        "self-reported, not verified)\n"
        + "\n".join(line for line in lines if line)
        + "\n- Greet them by first name when it is listed. Do NOT ask again for any detail listed above: use it for "
        "any booking. Ask only for what is missing and needed (for example a phone number before booking). Before "
        "booking, read the details back once and ask the visitor to confirm, and use any correction they give. "
        "Looking up, changing or cancelling an existing booking still needs the one-time-code verification.\n"
    )


def token_metadata(language: str, visitor: "Visitor | None") -> str:
    """The caller's LiveKit participant metadata: chosen language plus the form details."""
    data: dict[str, Any] = {}
    if language:
        data["language"] = language
    if visitor is not None:
        data["visitor"] = visitor.model_dump(exclude_none=True)
    return json.dumps(data) if data else ""


def from_metadata(raw: Any) -> "Visitor | None":
    """The form details from a participant's metadata JSON (None when absent or invalid)."""
    if not raw:
        return None
    try:
        data = json.loads(raw) if isinstance(raw, str) else raw
        v = data.get("visitor") if isinstance(data, dict) else None
        return Visitor(**v) if isinstance(v, dict) else None
    except (ValueError, TypeError):
        return None
