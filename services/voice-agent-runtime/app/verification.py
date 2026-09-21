"""SPEC-W45 K15(c)/(f): caller verification (OTP) + event phone hashing.

K15(c) — verified sessions: mutating tools (lookup/reschedule/cancel) may
only run for a session whose phone was verified out-of-band. Verification
reuses the booking customer-portal magic-code flow (booking-service
internal/httpapi/portal.go):

1. ``request_code(phone)`` -> POST {BOOKING_URL}/public/sites/{slug}/portal/request
   booking-service generates a 6-digit code and hands it to
   notification-worker (SMS/email) — always 202, anti-enumeration.
2. ``verify_code(phone, code)`` -> POST .../portal/verify — 200 means the
   caller proved possession of the number; the session becomes verified.

Both calls carry X-Internal-Token (VOICE_BOOKING_INTERNAL_TOKEN, K2
pattern). FAIL-CLOSED: when BOOKING_URL or the token is unset the client
reports ``configured == False`` and the tool layer answers
``verification_unavailable`` (honest 503-equivalent + error log) — there is
NO fallback to self-asserted or carrier-asserted numbers.

K15(f) — phone hashing for events: ``hash_phone`` is the W28 scheme
(graph-sync internal/graph.PhoneHash family: tenant-bound, digits-only
normalization), upgraded to HMAC-SHA256 keyed by PHONE_HASH_SALT. When the
salt is unset the caller MUST omit the field (never emit plaintext).

The httpx client is injectable for tests; production wiring uses the
default AsyncClient with the configured timeout.
"""

from __future__ import annotations

import hashlib
import hmac
from dataclasses import dataclass
from typing import Any

import httpx

from .logging import get_logger

log = get_logger("verification")

# 6-digit portal codes: bound the code input before it ever leaves the
# process (defense in depth — booking-service enforces its own checks).
MAX_CODE_LEN = 12


def normalize_digits(phone: str) -> str:
    """W28 normalization: digits only (leading +, spaces, dashes stripped)."""
    return "".join(ch for ch in str(phone).strip().lower() if ch.isdigit())


def hash_phone(salt: str, tenant_id: str, phone: str) -> str:
    """K15(f) event hash: HMAC-SHA256(salt, '{tenant}|{digits}') hex.

    Tenant-bound like W28 graph.PhoneHash so hashes are unlinkable across
    tenants; HMAC-keyed so a leaked hash is not brute-forcible without the
    salt. Callers must check ``salt`` for emptiness BEFORE calling and omit
    the field entirely when unset (fail-closed, no plaintext fallback)."""
    digits = normalize_digits(phone)
    msg = f"{tenant_id}|{digits}".encode("utf-8")
    return hmac.new(salt.encode("utf-8"), msg, hashlib.sha256).hexdigest()


@dataclass
class OtpResult:
    """Outcome of a portal OTP call, translated for the tool layer."""

    ok: bool
    status: str  # code_sent|verified|invalid_code|rate_limited|unavailable|error
    detail: str = ""
    payload: dict[str, Any] | None = None


class BookingPortalVerifier:
    """Client for the booking customer-portal magic-code endpoints."""

    def __init__(
        self,
        *,
        base_url: str,
        internal_token: str,
        site_slug: str,
        timeout_s: float = 15.0,
        client: httpx.AsyncClient | None = None,
    ) -> None:
        self._base = base_url.rstrip("/")
        self._token = internal_token
        self._site_slug = site_slug
        self._client = client or httpx.AsyncClient(
            timeout=httpx.Timeout(timeout_s)
        )
        self._owns_client = client is None

    @property
    def configured(self) -> bool:
        """Fail-closed gate: both BOOKING_URL and the internal token must
        be set before any verification (and therefore any mutation) can
        happen."""
        return bool(self._base and self._token)

    async def aclose(self) -> None:
        if self._owns_client:
            await self._client.aclose()

    def _headers(self) -> dict[str, str]:
        return {"X-Internal-Token": self._token}

    async def request_code(self, phone: str) -> OtpResult:
        """Ask booking-service to send a one-time code to ``phone``."""
        if not self.configured:
            log.error(
                "caller verification unavailable: BOOKING_URL or "
                "VOICE_BOOKING_INTERNAL_TOKEN unset (fail closed)"
            )
            return OtpResult(False, "unavailable", "verification not configured")
        try:
            resp = await self._client.post(
                f"{self._base}/public/sites/{self._site_slug}/portal/request",
                json={"phone": phone},
                headers=self._headers(),
            )
        except Exception as exc:  # noqa: BLE001 - surfaced as honest degrade
            log.warning("portal request-code call failed", error=str(exc)[:200])
            return OtpResult(False, "error", f"booking portal unreachable: {exc}")
        if resp.status_code == 202:
            return OtpResult(True, "code_sent")
        if resp.status_code == 429:
            return OtpResult(False, "rate_limited", "too many code requests")
        log.warning(
            "portal request-code unexpected status", status=resp.status_code
        )
        return OtpResult(
            False, "error", f"booking portal answered {resp.status_code}"
        )

    async def verify_code(self, phone: str, code: str) -> OtpResult:
        """Verify the code the caller read back. ok=True => verified."""
        if not self.configured:
            log.error(
                "caller verification unavailable: BOOKING_URL or "
                "VOICE_BOOKING_INTERNAL_TOKEN unset (fail closed)"
            )
            return OtpResult(False, "unavailable", "verification not configured")
        code = (code or "").strip()
        if not code or len(code) > MAX_CODE_LEN or not code.isdigit():
            return OtpResult(False, "invalid_code", "malformed code")
        try:
            resp = await self._client.post(
                f"{self._base}/public/sites/{self._site_slug}/portal/verify",
                json={"phone": phone, "code": code},
                headers=self._headers(),
            )
        except Exception as exc:  # noqa: BLE001
            log.warning("portal verify-code call failed", error=str(exc)[:200])
            return OtpResult(False, "error", f"booking portal unreachable: {exc}")
        if resp.status_code == 200:
            payload: dict[str, Any] = {}
            try:
                payload = resp.json()
            except Exception:  # noqa: BLE001 - body is informational
                payload = {}
            return OtpResult(True, "verified", payload=payload)
        if resp.status_code == 401:
            return OtpResult(False, "invalid_code")
        if resp.status_code == 429:
            return OtpResult(
                False, "rate_limited", "too many attempts — request a new code"
            )
        log.warning("portal verify-code unexpected status", status=resp.status_code)
        return OtpResult(
            False, "error", f"booking portal answered {resp.status_code}"
        )
