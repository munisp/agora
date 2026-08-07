"""PII redaction for transcript events (SPEC-W34 GF3).

Mirrors the semantics of the Fluvio `pii-redact` smartmodule
(infra/fluvio/pii-redact/src/lib.rs) — same phone/email regexes — so text
published directly via Dapr to ``opendesk.conversation.transcripts`` (which
bypasses the smartmodule) is redacted identically before it lands in
Iceberg ``bronze.transcripts``. Pure stdlib.

The smartmodule replaces matches with "[PHONE REDACTED]"/"[EMAIL REDACTED]";
this module uses the stable placeholders ``[REDACTED-PHONE]`` /
``[REDACTED-EMAIL]`` per SPEC-W34 GF3.
"""

from __future__ import annotations

import re

PHONE_REPLACEMENT = "[REDACTED-PHONE]"
EMAIL_REPLACEMENT = "[REDACTED-EMAIL]"

# E.164-ish: optional leading `+`, then 8-15 digits allowing common
# separators (space, dash, dot, parentheses) between digit groups.
# Requires at least 8 digits to avoid redacting ordinary short numbers.
# (Same pattern as infra/fluvio/pii-redact/src/lib.rs.)
_PHONE_RE = re.compile(r"\+?\d[\d .()\-]{6,17}\d")
_EMAIL_RE = re.compile(r"[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}")


def redact_text(text: str) -> str:
    """Redact phone numbers and email addresses from free text.

    Emails are redacted first, then phones (same order as the smartmodule,
    so an email's numeric local part is not eaten by the phone pattern).
    """
    without_emails = _EMAIL_RE.sub(EMAIL_REPLACEMENT, text)
    return _PHONE_RE.sub(PHONE_REPLACEMENT, without_emails)
