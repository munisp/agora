"""SPEC-W87 tests: durable cross-call verification grants (voice side).

- The strict anchor rule: grants honored ONLY on the PSTN channel with the
  carrier-claimed phone and an absent-or-matching supplied phone.
- Fall-through: web channel, disabled setting, no claim, mismatched
  supplied phone, check error/unavailable -> byte-identical OTP refusals.
- check_grant client mapping (verified / no_grant / error / unavailable).
REDIRECTION PROOF: fails on the base tree (no verification_grants_enabled
setting, no _honor_verification_grant, no check_grant).
"""

from __future__ import annotations

import httpx
import pytest

from app import metrics
from app.config import Settings, load_settings
from app.session_state import SessionState
from app.tenant_context import TenantContext
from app.tools import (
    ToolLayer,
    _verification_required_payload,
)
from app.verification import BookingPortalVerifier, OtpResult

from conftest import FakeDapr

PHONE = "+234 803 555 0101"
PHONE_NORM = "+2348035550101"


@pytest.fixture(autouse=True)
def _fresh_registry():
    metrics.reset_registry()
    yield


def _ctx() -> TenantContext:
    return TenantContext(site_slug="demo", tenant_id="t-uuid", tenant_slug="acme")


def _settings(**over) -> Settings:
    base = dict(
        booking_url="http://booking:7002",
        voice_booking_internal_token="book-tok",
        phone_hash_salt="test-salt",
    )
    base.update(over)
    return Settings(**base)


class _ScriptedVerifier:
    """Stands in for BookingPortalVerifier; check_grant is scripted."""

    def __init__(self, result: OtpResult) -> None:
        self.result = result
        self.calls: list[str] = []

    async def check_grant(self, phone: str) -> OtpResult:
        self.calls.append(phone)
        return self.result

    async def aclose(self) -> None:
        return None


def _layer(
    *,
    channel: str = "phone",
    claimed: str | None = PHONE_NORM,
    verified: str | None = None,
    grant_result: OtpResult | None = None,
    settings: Settings | None = None,
) -> tuple[ToolLayer, SessionState, _ScriptedVerifier]:
    session = SessionState(conversation_id="conv-1", site_slug="demo")
    session.channel = channel
    session.claimed_phone = claimed
    if verified:
        session.mark_verified(verified)
    verifier = _ScriptedVerifier(grant_result or OtpResult(False, "no_grant"))
    layer = ToolLayer(
        dapr=FakeDapr(),  # type: ignore[arg-type]
        settings=settings or _settings(),
        ctx=_ctx(),
        session=session,
        plugin_tools=[],
        verifier_factory=lambda: verifier,  # type: ignore[return-value]
    )
    return layer, session, verifier


# ---------------------------------------------------------------------------
# Anchor rule: honored only when EVERY condition holds
# ---------------------------------------------------------------------------
class TestGrantAnchorRule:
    async def test_grant_honored_on_pstn_claimed(self):
        layer, session, verifier = _layer(
            grant_result=OtpResult(True, "verified", payload={"method": "otp"})
        )
        got = await layer._require_verified(PHONE_NORM)
        assert got == PHONE_NORM
        assert session.verified_phone == PHONE_NORM
        assert verifier.calls == [PHONE_NORM]
        text = metrics.render()
        assert 'voice_stepup_verified_total{method="grant"} 1' in text

    async def test_grant_not_honored_on_web(self):
        layer, session, verifier = _layer(
            channel="web",
            grant_result=OtpResult(True, "verified"),
        )
        got = await layer._require_verified(PHONE_NORM)
        assert got == _verification_required_payload()
        assert session.verified_phone is None
        assert verifier.calls == []  # web never even asks

    async def test_grant_not_honored_when_disabled(self):
        layer, session, verifier = _layer(
            settings=_settings(verification_grants_enabled=False),
            grant_result=OtpResult(True, "verified"),
        )
        got = await layer._require_verified(PHONE_NORM)
        assert got == _verification_required_payload()
        assert verifier.calls == []

    async def test_grant_not_honored_without_claim(self):
        layer, session, verifier = _layer(
            claimed=None,
            grant_result=OtpResult(True, "verified"),
        )
        got = await layer._require_verified(PHONE_NORM)
        assert got == _verification_required_payload()
        assert verifier.calls == []

    async def test_grant_not_honored_on_supplied_mismatch(self):
        layer, session, verifier = _layer(
            grant_result=OtpResult(True, "verified"),
        )
        got = await layer._require_verified("+2349000000000")
        assert got == _verification_required_payload()
        assert verifier.calls == []

    async def test_grant_honored_when_supplied_absent(self):
        layer, session, verifier = _layer(
            grant_result=OtpResult(True, "verified"),
        )
        got = await layer._require_verified(None)
        assert got == PHONE_NORM
        assert session.verified_phone == PHONE_NORM


# ---------------------------------------------------------------------------
# Fall-through: no-grant and check failures land on the byte-identical
# pre-W87 refusal payloads
# ---------------------------------------------------------------------------
class TestGrantFallthrough:
    async def test_no_grant_falls_through(self):
        layer, session, _ = _layer(grant_result=OtpResult(False, "no_grant"))
        got = await layer._require_verified(PHONE_NORM)
        assert got == _verification_required_payload()
        assert session.verified_phone is None

    async def test_check_error_falls_through(self):
        layer, session, _ = _layer(grant_result=OtpResult(False, "error", "boom"))
        got = await layer._require_verified(PHONE_NORM)
        assert got == _verification_required_payload()
        assert session.verified_phone is None

    async def test_check_unavailable_falls_through(self):
        layer, session, _ = _layer(grant_result=OtpResult(False, "unavailable"))
        got = await layer._require_verified(PHONE_NORM)
        assert got == _verification_required_payload()
        assert session.verified_phone is None

    async def test_already_verified_skips_check(self):
        layer, session, verifier = _layer(
            verified=PHONE_NORM,
            grant_result=OtpResult(False, "no_grant"),
        )
        got = await layer._require_verified(PHONE_NORM)
        assert got == PHONE_NORM
        assert verifier.calls == []  # verified session never re-checks

    async def test_verified_still_rejects_mismatch(self):
        layer, session, _ = _layer(verified=PHONE_NORM)
        got = await layer._require_verified("+2349000000000")
        assert got["status"] == "error"
        assert got["error"] == "phone_mismatch"


# ---------------------------------------------------------------------------
# check_grant client mapping
# ---------------------------------------------------------------------------
def _grant_client(handler) -> BookingPortalVerifier:
    transport = httpx.MockTransport(handler)
    client = httpx.AsyncClient(transport=transport)
    return BookingPortalVerifier(
        base_url="http://booking:7002",
        internal_token="book-tok",
        site_slug="demo",
        client=client,
    )


class TestCheckGrantClient:
    async def test_verified_maps_ok(self):
        seen = {}

        def handler(request: httpx.Request) -> httpx.Response:
            seen["path"] = request.url.path
            seen["token"] = request.headers.get("x-internal-token")
            return httpx.Response(200, json={"verified": True, "method": "otp"})

        v = _grant_client(handler)
        res = await v.check_grant(PHONE_NORM)
        assert res.ok and res.status == "verified"
        assert seen["path"] == "/internal/sites/demo/verification/check"
        assert seen["token"] == "book-tok"
        await v.aclose()

    async def test_unverified_maps_no_grant(self):
        v = _grant_client(lambda r: httpx.Response(200, json={"verified": False}))
        res = await v.check_grant(PHONE_NORM)
        assert not res.ok and res.status == "no_grant"
        await v.aclose()

    async def test_unexpected_status_maps_error(self):
        v = _grant_client(lambda r: httpx.Response(500, json={"error": "boom"}))
        res = await v.check_grant(PHONE_NORM)
        assert not res.ok and res.status == "error"
        await v.aclose()

    async def test_transport_error_maps_error(self):
        def handler(request: httpx.Request) -> httpx.Response:
            raise httpx.ConnectError("refused", request=request)

        v = _grant_client(handler)
        res = await v.check_grant(PHONE_NORM)
        assert not res.ok and res.status == "error"
        await v.aclose()

    async def test_unconfigured_maps_unavailable(self):
        v = BookingPortalVerifier(
            base_url="", internal_token="", site_slug="demo",
            client=httpx.AsyncClient(transport=httpx.MockTransport(
                lambda r: httpx.Response(200, json={"verified": True}))),
        )
        res = await v.check_grant(PHONE_NORM)
        assert not res.ok and res.status == "unavailable"
        await v.aclose()


# ---------------------------------------------------------------------------
# Settings
# ---------------------------------------------------------------------------
class TestGrantSettings:
    def test_grants_enabled_default_and_env(self, monkeypatch):
        assert Settings().verification_grants_enabled is True
        monkeypatch.setenv("VOICE_VERIFICATION_GRANTS", "0")
        s = load_settings()
        assert s.verification_grants_enabled is False
