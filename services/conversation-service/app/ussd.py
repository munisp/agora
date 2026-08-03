"""USSD session → conversation mapping (SPEC-W12, cross-agent contract §1/§2).

messaging-gateway terminates the Africa's Talking USSD callback (form fields
``sessionId, serviceCode, phoneNumber, text`` — ``text`` is the CUMULATIVE
``1*2*3`` input, empty on the first request of a session) and invokes this
service synchronously via Dapr (``POST /v1/ussd/turns``). This module owns:

- the deterministic session → conversation key: ``uuid5(tenant, sessionId)``
  so every callback of one USSD session lands in the SAME conversation and
  session resumption needs no lookup table (no extra state store — the
  180s session TTL from contract §1 is enforced by messaging-gateway's
  session store);
- low-literacy MENU MODE: when the tenant pack defines ``ussd.menu`` the
  gateway passes the resolved menu (list of ``{key,label,action}``) in the
  invoke body and this module drives the numeric navigation —
  ``1``/``2``/… select an item, ``0`` = back, ``00`` = main menu;
- the request/reply contract: the user turn text is the cumulative input
  parsed down to the LAST selection; the invoke response body carries the
  reply text plus a ``continue`` flag, and messaging-gateway renders the
  wire format (``CON <reply>`` when continue=true, ``END <reply>`` when
  false, text/plain per contract §1).

Channel enum: the conversation is persisted with ``channel="ussd"``
(contract §2). USER turns flow through the SAME enrichment + incident
classifier path as every other channel (routes.py ``_persist_turn``); the
classifier treats ussd as web-like (incidents._CHANNEL_MAP: ussd→web).
Agent/system turns are never incident-classified (existing rule).
"""

from __future__ import annotations

import uuid
from typing import Any

from pydantic import BaseModel, Field

CHANNEL = "ussd"

# Low-literacy navigation keys (contract with messaging-gateway + tenant
# pack authors): "0" goes back one level, "00" returns to the main menu.
# Our menus are single-level, so both re-render the main menu.
KEY_BACK = "0"
KEY_MAIN_MENU = "00"

CONFIRMATION_SUFFIX = (
    " Thank you — your request has been received and you will get a "
    "confirmation SMS shortly."
)
INVALID_SELECTION_PREFIX = "Invalid selection. Please try again.\n"


# ---------------------------------------------------------------------------
# Request/response models (Dapr invoke body contract with messaging-gateway)
# ---------------------------------------------------------------------------


class UssdMenuItem(BaseModel):
    """One tenant-pack ``ussd.menu`` entry ({key,label,action})."""

    key: str = Field(min_length=1, max_length=8)
    label: str = Field(min_length=1, max_length=160)
    action: str = ""


class UssdTurnRequest(BaseModel):
    """One Africa's Talking callback, normalized by messaging-gateway.

    ``text`` is the cumulative session input (``1*2*3``; empty on the first
    request). ``menu`` is the tenant pack's ``ussd.menu`` when defined —
    its presence switches the session to menu mode; absent = pass-through
    text mode (free text forwarded to the conversation).
    """

    tenant_id: uuid.UUID
    site_slug: str = Field(min_length=1, max_length=128)
    session_id: str = Field(min_length=1, max_length=128)
    service_code: str = Field(default="", max_length=32)
    phone_number: str | None = Field(default=None, max_length=64)
    text: str = Field(default="", max_length=182)  # AT caps USSD input ~182 chars
    menu: list[UssdMenuItem] | None = None


# ---------------------------------------------------------------------------
# Session → conversation mapping
# ---------------------------------------------------------------------------


def session_conversation_id(tenant_id: uuid.UUID, session_id: str) -> uuid.UUID:
    """Deterministic conversation key for one USSD session (contract §1).

    uuid5(tenant, sessionId) — the same callback stream always maps to the
    same conversation, for every tenant, without any lookup state.
    """
    return uuid.uuid5(
        uuid.NAMESPACE_URL, f"opendesk:ussd:{tenant_id}:{session_id}"
    )


# ---------------------------------------------------------------------------
# Cumulative-input parsing (Africa's Talking ``text`` field)
# ---------------------------------------------------------------------------


def selection_path(text: str) -> list[str]:
    """Split cumulative input into the selection path ("1*2*3" -> [1,2,3])."""
    if not text:
        return []
    return [seg.strip() for seg in text.split("*") if seg.strip()]


def parse_last_selection(text: str) -> str:
    """The LAST selection of the cumulative input ("" on the first request)."""
    path = selection_path(text)
    return path[-1] if path else ""


# ---------------------------------------------------------------------------
# Menu mode (low-literacy numeric navigation)
# ---------------------------------------------------------------------------


def render_menu(menu: list[UssdMenuItem]) -> str:
    """Numbered main-menu text with the 0/00 navigation footer."""
    lines = [f"{item.key}. {item.label}" for item in menu]
    lines.append(f"{KEY_BACK}. Back")
    lines.append(f"{KEY_MAIN_MENU}. Main menu")
    return "\n".join(lines)


def match_selection(
    menu: list[UssdMenuItem], selection: str
) -> UssdMenuItem | None:
    for item in menu:
        if item.key == selection:
            return item
    return None


def menu_reply(
    menu: list[UssdMenuItem], text: str
) -> tuple[str, bool, UssdMenuItem | None]:
    """Menu-mode state machine. Returns (reply, continue, selected_item).

    - first request (empty text), ``0`` (back), ``00`` (main menu):
      re-render the menu, session continues;
    - a key matching a menu item: confirmation text, session ENDS
      (single-level menus — the leaf action is terminal);
    - anything else: invalid-selection prompt + menu, session continues.
    """
    selection = parse_last_selection(text)
    if not selection or selection in (KEY_BACK, KEY_MAIN_MENU):
        return render_menu(menu), True, None
    item = match_selection(menu, selection)
    if item is not None:
        return f"{item.label}.{CONFIRMATION_SUFFIX}", False, item
    return INVALID_SELECTION_PREFIX + render_menu(menu), True, None


def build_reply(
    body: UssdTurnRequest, text_mode_reply: str
) -> tuple[str, bool, UssdMenuItem | None]:
    """(reply, continue, selected_item) for one callback.

    Menu mode when the pack menu was passed, else pass-through text mode
    (the configurable acknowledgement; the gateway owns session length).
    """
    if body.menu:
        return menu_reply(body.menu, body.text)
    return text_mode_reply, True, None


# ---------------------------------------------------------------------------
# User-turn text (cumulative input parsed to the LAST selection)
# ---------------------------------------------------------------------------


def user_turn_text(body: UssdTurnRequest) -> str:
    """The user-turn text persisted for one callback.

    Menu mode stores the human-readable label of the LAST selection (so the
    transcript stays meaningful for low-literacy sessions and the incident
    classifier still sees e.g. a "Report emergency" pick); text mode stores
    the raw last selection. The first request records a session-start
    marker so the conversation timeline shows how it began.
    """
    selection = parse_last_selection(body.text)
    if not selection:
        via = f" via {body.service_code}" if body.service_code else ""
        return f"(ussd session started{via})"
    if body.menu:
        if selection in (KEY_BACK, KEY_MAIN_MENU):
            return "(ussd: back to main menu)"
        item = match_selection(body.menu, selection)
        if item is not None:
            return f"{item.label} (ussd menu option {item.key})"
        return f"ussd invalid selection: {selection}"
    return selection


def idempotency_key(body: UssdTurnRequest) -> str:
    """Per-callback dedupe key: identical AT retries collapse to one turn.

    The cumulative text makes each step of a session unique; a retry of the
    SAME callback carries the same text and is deduped by the existing
    Idempotency-Key machinery in routes.py.
    """
    return f"ussd:{body.session_id}:{body.text or '<start>'}"


def response_payload(
    *,
    conversation_id: uuid.UUID,
    reply: str,
    continue_session: bool,
    body: UssdTurnRequest,
    selected: UssdMenuItem | None,
) -> dict[str, Any]:
    """Invoke response body consumed by messaging-gateway.

    Contract (docs/runbook-wave12.md + docs/channels-ussd.md): the gateway
    prefixes ``CON `` when ``continue`` is true, ``END `` otherwise, and
    answers Africa's Talking as text/plain.
    """
    return {
        "conversation_id": str(conversation_id),
        "reply": reply,
        "continue": continue_session,
        "mode": "menu" if body.menu else "text",
        "selection": parse_last_selection(body.text),
        "action": selected.action if selected else "",
    }
