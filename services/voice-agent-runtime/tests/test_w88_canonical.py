"""SPEC-W88 tests: E.164 pin-canonicalization of the pin/OTP compare paths.

- canonical_e164: ONE session identity form ("+"-prefixed E.164) for the
  WhatsApp wa_id (digits) and telegram/SIP ("+") forms; non-conforming
  identities (too short, leading-0 national) canonicalize to "".
- Control-plane pin: digits-only wa_id pins the canonical form; junk pins
  NOTHING (supersedes the W85-documented leniency — see NOTES-W88).
- Verification gates compare digits-equal (fail-closed): the "+"-vs-no-"+"
  divergence stops false-mismatching; genuinely different numbers and
  national-vs-international forms still refuse.
- OTP request/verify send + store the canonical form (best-effort;
  national read-outs fall back to the legacy normalize_phone form).

REDIRECTION PROOF: fails on the base tree (no canonical_e164, no
_phones_match, form-preserving pin, raw != compares).
"""

from __future__ import annotations

import httpx
import pytest

try:  # pragma: no cover - import shim (real SDK present in the worker image)
    import app.livekit_worker as _lw  # noqa: F401
except ModuleNotFoundError:  # pragma: no cover - import shim
    import test_live_tts_chain  # noqa: F401 - installs the livekit stubs

from app import chat as chat_module
from app import metrics
from app.config import Settings
from app.control_plane import create_app
from app.session_state import SessionState
from app.tenant_context import TenantContext
from app.tools import ToolLayer, _phones_match
from app.verification import OtpResult, canonical_e164, normalize_digits

from conftest import FakeDapr

WA_ID = "2348035550101"  # Meta wa_id form: E.164 without '+'
E164 = "+2348035550101"
ALT_E164 = "+2348099998888"


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


class TestCanonicalE164:
    def test_wa_id_digits_gain_plus(self):
        assert canonical_e164(WA_ID) == E164

    def test_plus_and_separators_converge(self):
        assert canonical_e164("+234 803 555 0101") == E164
        assert canonical_e164(" +234-803-555-0101 ") == E164
        assert canonical_e164("tel:+234.803.555.0101") == E164

    def test_non_conforming_rejected(self):
        assert canonical_e164("") == ""
        assert canonical_e164(None) == ""
        assert canonical_e164("12345") == ""  # too short
        assert canonical_e164("08035550101") == ""  # national trunk-0: no folding
        assert canonical_e164("abcdefghijk") == ""
        assert canonical_e164("+23480355501012345") == ""  # > 15 digits

    def test_phones_match_digits_equal_fail_closed(self):
        assert _phones_match(E164, WA_ID)
        assert _phones_match(WA_ID, "+234 803 555 0101")
        assert not _phones_match(E164, ALT_E164)
        assert not _phones_match(E164, "")
        assert not _phones_match("", "")
        assert not _phones_match(None, E164)  # type: ignore[arg-type]
        # National vs international forms never fold (trunk-0 kept).
        assert not _phones_match("08035550101", E164)


class _ScriptedVerifier:
    """Captures the phone handed to the booking portal endpoints."""

    def __init__(self, result: OtpResult) -> None:
        self.result = result
        self.calls: list[tuple[str, tuple]] = []

    async def request_code(self, phone: str) -> OtpResult:
        self.calls.append(("request", (phone,)))
        return self.result

    async def verify_code(self, phone: str, code: str) -> OtpResult:
        self.calls.append(("verify", (phone, code)))
        return self.result

    async def check_grant(self, phone: str) -> OtpResult:
        self.calls.append(("check_grant", (phone,)))
        return self.result

    async def aclose(self) -> None:
        return None


def _layer(
    *,
    channel: str = "phone",
    claimed: str | None = None,
    verified: str | None = None,
    verifier: _ScriptedVerifier | None = None,
) -> tuple[ToolLayer, SessionState, _ScriptedVerifier]:
    session = SessionState(conversation_id="conv-1", site_slug="demo")
    session.channel = channel
    session.claimed_phone = claimed
    if verified:
        session.mark_verified(verified)
    v = verifier or _ScriptedVerifier(OtpResult(True, "verified"))
    layer = ToolLayer(
        dapr=FakeDapr(),  # type: ignore[arg-type]
        settings=_settings(),
        ctx=_ctx(),
        session=session,
        plugin_tools=[],
        verifier_factory=lambda: v,  # type: ignore[return-value]
    )
    return layer, session, v


class TestGateCrossFormCompare:
    async def test_digits_pin_matches_plus_supplied(self):
        # Legacy-session form: verified phone pinned digits-only (WhatsApp
        # wa_id); the model reads back the '+'-form. Same subscriber —
        # MUST NOT false-mismatch.
        layer, _, _ = _layer(verified=WA_ID)
        got = await layer._require_verified(E164)
        assert got == WA_ID  # the verified identity wins, unchanged

    async def test_plus_verified_matches_digits_supplied(self):
        layer, _, _ = _layer(verified=E164)
        got = await layer._require_verified(WA_ID)
        assert got == E164

    async def test_genuinely_different_still_refused(self):
        layer, _, _ = _layer(verified=E164)
        got = await layer._require_verified(ALT_E164)
        assert isinstance(got, dict) and got["error"] == "phone_mismatch"

    async def test_national_form_never_folds(self):
        layer, _, _ = _layer(verified=E164)
        got = await layer._require_verified("08035550101")
        assert isinstance(got, dict) and got["error"] == "phone_mismatch"

    async def test_grant_anchor_cross_form_supplied(self):
        # SIP claim is the '+'-form; the model supplies digits-only. The
        # anchor rule must see them as the same phone and check the grant.
        v = _ScriptedVerifier(OtpResult(True, "verified", payload={"method": "otp"}))
        layer, session, _ = _layer(claimed=E164, verifier=v)
        got = await layer._require_verified(WA_ID)
        assert got == E164
        assert session.verified_phone == E164
        assert v.calls == [("check_grant", (E164,))]
        assert 'voice_stepup_verified_total{method="grant"} 1' in metrics.render()

    async def test_grant_anchor_different_number_no_check(self):
        v = _ScriptedVerifier(OtpResult(True, "verified"))
        layer, session, _ = _layer(claimed=E164, verifier=v)
        got = await layer._require_verified(ALT_E164)
        assert isinstance(got, dict) and got["status"] == "verification_required"
        assert session.verified_phone is None
        assert v.calls == []  # mismatched supplied phone never reaches booking


class TestOtpCanonicalForm:
    async def test_request_sends_canonical(self):
        v = _ScriptedVerifier(OtpResult(True, "code_sent"))
        layer, _, _ = _layer(verifier=v)
        resp = await layer.request_verification_code("234 803 555 0101")
        assert resp["status"] == "code_sent"
        assert v.calls == [("request", (E164,))]

    async def test_verify_marks_canonical_verified(self):
        v = _ScriptedVerifier(OtpResult(True, "verified"))
        layer, session, _ = _layer(verifier=v)
        resp = await layer.verify_caller_code(WA_ID, "123456")
        assert resp["status"] == "verified"
        assert v.calls == [("verify", (E164, "123456"))]
        assert session.verified_phone == E164

    async def test_national_readout_falls_back_unrejected(self):
        # No country inference (W61 ruling): a national read-out keeps the
        # legacy normalize_phone form rather than being rejected.
        v = _ScriptedVerifier(OtpResult(True, "code_sent"))
        layer, _, _ = _layer(verifier=v)
        resp = await layer.request_verification_code("0803-555-0101")
        assert resp["status"] == "code_sent"
        assert v.calls == [("request", ("08035550101",))]


class TestControlPlaneCanonicalPin:
    PATH = "/voice/chat"

    async def _post(self, settings: Settings, payload: dict, headers: dict):
        app = create_app(settings)
        async with httpx.AsyncClient(
            transport=httpx.ASGITransport(app=app), base_url="http://test"
        ) as client:
            return await client.post(self.PATH, json=payload, headers=headers)

    async def test_wa_id_pins_canonical_form(self, monkeypatch):
        captured: dict = {}

        async def fake_handle(self, **kwargs):  # noqa: ANN001 - test stub
            captured.update(kwargs)
            return {"conversation_id": "c1", "session_secret": "s", "reply": "ok"}

        monkeypatch.setattr(chat_module.ChatService, "handle_message", fake_handle)
        settings = _settings(voice_admin_internal_token="admin-tok")
        resp = await self._post(
            settings,
            {"site_slug": "acme", "message": "hi", "channel": "whatsapp",
             "channel_identity": WA_ID},
            {"X-Internal-Token": "admin-tok"},
        )
        assert resp.status_code == 200, resp.text
        assert captured["pin_verified_identity"] == E164

    async def test_non_conforming_identity_pins_nothing(self, monkeypatch):
        captured: dict = {}

        async def fake_handle(self, **kwargs):  # noqa: ANN001 - test stub
            captured.update(kwargs)
            return {"conversation_id": "c1", "session_secret": "s", "reply": "ok"}

        monkeypatch.setattr(chat_module.ChatService, "handle_message", fake_handle)
        settings = _settings(voice_admin_internal_token="admin-tok")
        resp = await self._post(
            settings,
            {"site_slug": "acme", "message": "hi", "channel": "telegram",
             "channel_identity": "123"},
            {"X-Internal-Token": "admin-tok"},
        )
        assert resp.status_code == 200, resp.text
        assert captured["pin_verified_identity"] is None
