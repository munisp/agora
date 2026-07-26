"""Agent-driven UI actions (SPEC-W9 Part B).

The receptionist can ask the web widget to perform small, safe UI actions on
the visitor's page: navigate to another page, highlight an element (e.g. the
booking form) or pre-select an offering in the booking form. Actions are
*validated here, executed client-side* — the server never touches the DOM;
validated actions are attached to the chat turn's outgoing payload and the
widget (embed.js / embed page bridge) applies them with its own guards.

EXACT action set:
- ``{"type": "navigate", "path"}`` — same-origin path only: MUST start with
  ``/``, no scheme, no host (protocol-relative ``//host`` is rejected).
- ``{"type": "highlight", "selector"}`` — CSS selector, sanitized to the
  charset ``[a-zA-Z0-9\\-_#. :\\[\\]="'>]``, at most 120 chars.
- ``{"type": "prefill_booking", "offering_id"}`` — UUID string.

Each ``validate_*`` returns the normalized action dict, or ``None`` when the
input is invalid — the calling tool then returns an error string to the LLM
and the action is dropped (never reaches the client).
"""

from __future__ import annotations

import re
import uuid
from typing import Any

NAVIGATE = "navigate"
HIGHLIGHT = "highlight"
PREFILL_BOOKING = "prefill_booking"

MAX_PATH_LEN = 2048
MAX_SELECTOR_LEN = 120

# Allowed selector charset (SPEC-W9 B1): letters, digits, - _ # . space
# : [ ] = " ' > — enough for id/class/attribute/descendant selectors while
# excluding everything that could smuggle script or break out of CSS syntax.
_SELECTOR_RE = re.compile(r"^[a-zA-Z0-9\-_#. :\[\]=\"'>]{1,120}$")

# Anything that looks like a scheme (http:, javascript:, ...) — belt-and-braces
# on top of the leading-slash rule.
_SCHEME_RE = re.compile(r"^[a-zA-Z][a-zA-Z0-9+.\-]*:")


def validate_navigate(path: Any) -> dict[str, str] | None:
    """Validate a navigate action. Same-origin paths only."""
    if not isinstance(path, str):
        return None
    path = path.strip()
    if not path or len(path) > MAX_PATH_LEN:
        return None
    if not path.startswith("/"):
        return None
    if path.startswith("//"):  # protocol-relative URL = host smuggling
        return None
    if _SCHEME_RE.match(path) or "://" in path:
        return None
    if any(c.isspace() for c in path):
        return None
    return {"type": NAVIGATE, "path": path}


def validate_highlight(selector: Any) -> dict[str, str] | None:
    """Validate a highlight action: sanitized CSS selector, <=120 chars."""
    if not isinstance(selector, str):
        return None
    if not _SELECTOR_RE.fullmatch(selector):
        return None
    return {"type": HIGHLIGHT, "selector": selector}


def validate_prefill_booking(offering_id: Any) -> dict[str, str] | None:
    """Validate a prefill_booking action: offering id must be a UUID."""
    if not isinstance(offering_id, str):
        return None
    try:
        normalized = str(uuid.UUID(offering_id.strip()))
    except (ValueError, AttributeError, TypeError):
        return None
    return {"type": PREFILL_BOOKING, "offering_id": normalized}
