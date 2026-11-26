"""WhatsApp appointment messages (2026-11-22). The text is always composed here from the booking's facts, never by a
model: used by the dashboard's "WhatsApp reminder" button (POST /voice-admin/whatsapp) and by the receptionist's
send_whatsapp_confirmation tool."""
from __future__ import annotations

KINDS = ("reminder", "confirmation")


def _clean(v: str | None, n: int) -> str:
    return " ".join((v or "").split())[:n]


def appointment_text(kind: str, business: str, contact_name: str | None, offering: str, when: str,
                     team_member: str | None = None) -> str:
    first = _clean(contact_name, 120).split(" ")[0] if _clean(contact_name, 120) else ""
    what = f"{_clean(offering, 120)}{(' with ' + _clean(team_member, 80)) if _clean(team_member, 80) else ''}"
    when = _clean(when, 80)
    hello = f"Hello{(' ' + first) if first else ''}, this is {_clean(business, 120)}."
    if kind == "confirmation":
        body = f"✅ Your {what} on {when} is confirmed.\n\nWe'll send you a reminder before it."
    else:
        body = f"⏰ Reminder: your {what} is on {when}."
    return f"{hello}\n\n{body} Reply here if you need to change anything."
