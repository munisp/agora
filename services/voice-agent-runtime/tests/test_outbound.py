"""SPEC-W101 tests: outbound PSTN calls over LiveKit SIP.

C2: E.164 validation, OutboundPacer token bucket (fake clock — never sleeps
real seconds), place_outbound_call room/request/honest errors, C4 outcall
room binding. C3: POST /v1/calls auth matrix + pacing-before-dial wiring.
C1: outbound trunk deploy YAML schema.
"""

from __future__ import annotations

from pathlib import Path
from types import SimpleNamespace

import httpx
import pytest
import yaml

import sys
import types

from app import outbound, sip
from app.config import Settings

# Same import shim as test_avatar.py: app.control_plane imports
# app.livekit_worker (the heavy LiveKit Agents SDK stack, not installed in
# this test env) only for ROOM_PREFIX — stub it JUST for the import, then
# remove it so sibling suites that importorskip("app.livekit_worker") are
# not poisoned.
try:  # pragma: no cover - import shim
    import app.livekit_worker  # noqa: F401
except ModuleNotFoundError:  # pragma: no cover - import shim
    _lw_stub = types.ModuleType("app.livekit_worker")
    _lw_stub.ROOM_PREFIX = "site-"
    sys.modules["app.livekit_worker"] = _lw_stub
    import app.control_plane  # noqa: F401 - binds ROOM_PREFIX while stubbed
    del sys.modules["app.livekit_worker"]

from app.control_plane import create_app

REPO_ROOT = Path(__file__).resolve().parents[3]
SIP_DIR = REPO_ROOT / "deploy" / "livekit-sip"


# ---------------------------------------------------------------------------
# C2: E.164 validation
# ---------------------------------------------------------------------------
class TestValidateCallee:
    def test_accepts_e164(self):
        assert outbound.validate_callee("+2348030000000") == "+2348030000000"

    def test_normalizes_formatting(self):
        assert outbound.validate_callee("+234 803 000 0000") == "+2348030000000"

    @pytest.mark.parametrize(
        "bad", ["", "+", "08030000000", "2348030000000", "+1234", "+234abc",
                "+234803000000012345678"]
    )
    def test_rejects_garbage(self, bad):
        with pytest.raises(outbound.InvalidNumber) as exc:
            outbound.validate_callee(bad)
        assert exc.value.status_code == 422


# ---------------------------------------------------------------------------
# C4: outcall room name <-> slug
# ---------------------------------------------------------------------------
class TestOutboundRoom:
    def test_roundtrip(self):
        room = outbound.outbound_room_name("acme", "a" * 32)
        assert room == "outcall-acme-" + "a" * 32
        assert outbound.site_slug_from_outbound_room(room) == "acme"

    def test_slug_with_dashes(self):
        room = outbound.outbound_room_name("front-desk", "b" * 32)
        assert outbound.site_slug_from_outbound_room(room) == "front-desk"

    @pytest.mark.parametrize(
        "room",
        ["site-acme", "call-+15551234567", "outcall-", "outcall-acme",
         "outcall-acme-short", "outcall-acme-" + "z" * 32, "outcall--" + "a" * 32],
    )
    def test_rejects_non_outbound(self, room):
        assert outbound.site_slug_from_outbound_room(room) is None

    def test_build_context_binds_slug_and_callee(self):
        callee_participant = SimpleNamespace(
            attributes={
                "sip.trunkPhoneNumber": "+15551234567",
                "sip.phoneNumber": "+2348030000000",
            },
            identity="sip-+2348030000000",
            kind=None,
        )
        # is_sip_participant checks attributes for the trunk phone number.
        ctx = outbound.build_outbound_call_context(
            "outcall-acme-" + "c" * 32, [callee_participant]
        )
        assert ctx is not None
        assert ctx.site_slug == "acme"
        assert ctx.tenant_source == "outbound"
        assert ctx.caller_phone == "+2348030000000"
        assert ctx.dialed_number == ""

    def test_build_context_none_for_inbound_rooms(self):
        assert outbound.build_outbound_call_context("call-+15551234567", []) is None
        assert outbound.build_outbound_call_context("site-acme", []) is None


# ---------------------------------------------------------------------------
# C2: OutboundPacer token bucket (fake clock)
# ---------------------------------------------------------------------------
class FakeClock:
    def __init__(self):
        self.now = 0.0
        self.slept: list[float] = []

    def time(self) -> float:
        return self.now

    async def sleep(self, s: float) -> None:
        self.slept.append(s)
        self.now += s


class TestOutboundPacer:
    async def test_first_start_immediate_then_serialized(self):
        clock = FakeClock()
        pacer = outbound.OutboundPacer(cps=2.0, clock=clock.time, sleep=clock.sleep)
        w1 = await pacer.acquire()
        assert w1 == 0.0  # capacity 1: first start is immediate
        w2 = await pacer.acquire()
        assert w2 == pytest.approx(0.5)  # 2 cps -> 0.5s interval
        w3 = await pacer.acquire()
        assert w3 == pytest.approx(0.5)

    async def test_burst_serializes_at_cps(self):
        clock = FakeClock()
        pacer = outbound.OutboundPacer(cps=1.0, clock=clock.time, sleep=clock.sleep)
        for _ in range(5):
            await pacer.acquire()
        # 5 starts at 1/s: total simulated wait = 4s (first is immediate).
        assert sum(clock.slept) == pytest.approx(4.0)

    async def test_idle_period_replenishes(self):
        clock = FakeClock()
        pacer = outbound.OutboundPacer(cps=1.0, clock=clock.time, sleep=clock.sleep)
        await pacer.acquire()
        clock.now += 10.0  # long idle: the bucket must not bank burst debt
        w = await pacer.acquire()
        assert w == 0.0


# ---------------------------------------------------------------------------
# C2: place_outbound_call against a stubbed LiveKit SIP API
# ---------------------------------------------------------------------------
class _FakeSipService:
    def __init__(self, fail: bool = False):
        self.fail = fail
        self.requests: list = []

    async def create_sip_participant(self, request):
        self.requests.append(request)
        if self.fail:
            raise ConnectionError("livekit unreachable (stub)")
        return SimpleNamespace(sip_call_id="SCL_test")


class _FakeApi:
    def __init__(self, fail: bool = False):
        self.sip = _FakeSipService(fail)
        self.closed = False

    async def aclose(self):
        self.closed = True


class _CreateSIPParticipantRequest:
    """Records the exact fields the production builder sends."""

    def __init__(self, **kwargs):
        self.kwargs = kwargs


@pytest.fixture()
def sip_proto_stub(monkeypatch):
    """Stub livekit.protocol.sip with JUST the request type the builder
    looks for — exercises the real _build_participant_request field map."""
    sip_mod = types.ModuleType("livekit.protocol.sip")
    sip_mod.CreateSIPParticipantRequest = _CreateSIPParticipantRequest
    protocol_mod = sys.modules.get("livekit.protocol") or types.ModuleType(
        "livekit.protocol"
    )
    protocol_mod.sip = sip_mod
    livekit_mod = sys.modules.get("livekit") or types.ModuleType("livekit")
    livekit_mod.protocol = protocol_mod
    monkeypatch.setitem(sys.modules, "livekit", livekit_mod)
    monkeypatch.setitem(sys.modules, "livekit.protocol", protocol_mod)
    monkeypatch.setitem(sys.modules, "livekit.protocol.sip", sip_mod)
    return sip_mod


def _dial_settings(**over):
    base = dict(
        booking_url="http://booking:7002",
        livekit_url="ws://livekit:7880",
        livekit_api_key="devkey",
        livekit_api_secret="secret",
        outbound_trunk_id="ST_outbound1",
    )
    base.update(over)
    return Settings(**base)


class TestPlaceOutboundCall:
    async def test_dials_with_room_and_trunk(self, sip_proto_stub):
        api = _FakeApi()
        call = await outbound.place_outbound_call(
            _dial_settings(), "acme", "+2348030000000", api=api
        )
        assert call.site_slug == "acme"
        assert call.phone_number == "+2348030000000"
        assert call.room.startswith("outcall-acme-")
        assert len(call.call_id) == 32
        assert len(api.sip.requests) == 1
        req = api.sip.requests[0]
        assert req.kwargs["sip_trunk_id"] == "ST_outbound1"
        assert req.kwargs["sip_call_to"] == "+2348030000000"
        assert req.kwargs["room_name"] == call.room
        assert req.kwargs["participant_identity"] == "out-+2348030000000"
        assert "participant_attributes" not in req.kwargs  # no purpose given -> a general call, nothing attached

    async def test_purpose_and_facts_ride_as_participant_attributes(self, sip_proto_stub):
        # 2026-11-22: the receptionist reads these back (app/outbound_script.py) for its opening line and prompt.
        api = _FakeApi()
        await outbound.place_outbound_call(
            _dial_settings(), "acme", "+2348030000000", api=api, purpose="confirm_appointment",
            contact_name="Ngozi Okafor", context={"team_member": "Aisha Bello", "when": "Tuesday at 9 a.m."},
        )
        assert api.sip.requests[0].kwargs["participant_attributes"] == {
            "agora.purpose": "confirm_appointment", "agora.contact_name": "Ngozi Okafor",
            "agora.team_member": "Aisha Bello", "agora.when": "Tuesday at 9 a.m.",
        }

    async def test_missing_proto_type_is_honest_502(self, monkeypatch):
        # livekit-api without the request type: honest 502, never a crash.
        sip_mod = types.ModuleType("livekit.protocol.sip")
        protocol_mod = types.ModuleType("livekit.protocol")
        protocol_mod.sip = sip_mod
        livekit_mod = types.ModuleType("livekit")
        livekit_mod.protocol = protocol_mod
        monkeypatch.setitem(sys.modules, "livekit", livekit_mod)
        monkeypatch.setitem(sys.modules, "livekit.protocol", protocol_mod)
        monkeypatch.setitem(sys.modules, "livekit.protocol.sip", sip_mod)
        with pytest.raises(outbound.OutboundDialError) as exc:
            await outbound.place_outbound_call(
                _dial_settings(), "acme", "+2348030000000", api=_FakeApi()
            )
        assert exc.value.status_code == 502
        assert "livekit-api" in str(exc.value)

    async def test_trunk_unset_is_honest_503(self):
        with pytest.raises(outbound.TrunkNotConfigured) as exc:
            await outbound.place_outbound_call(
                _dial_settings(outbound_trunk_id=""), "acme", "+2348030000000",
                api=_FakeApi(),
            )
        assert exc.value.status_code == 503

    async def test_invalid_number_dials_nothing(self):
        api = _FakeApi()
        with pytest.raises(outbound.InvalidNumber):
            await outbound.place_outbound_call(
                _dial_settings(), "acme", "not-a-number", api=api
            )
        assert api.sip.requests == []

    @pytest.mark.parametrize("slug", ["", "a/b", "..", "a..b", "x\\y"])
    async def test_unsafe_slug_rejected(self, slug):
        with pytest.raises(outbound.InvalidNumber):
            await outbound.place_outbound_call(
                _dial_settings(), slug, "+2348030000000", api=_FakeApi()
            )

    async def test_livekit_failure_is_honest_502(self):
        with pytest.raises(outbound.OutboundDialError) as exc:
            await outbound.place_outbound_call(
                _dial_settings(), "acme", "+2348030000000", api=_FakeApi(fail=True)
            )
        assert exc.value.status_code == 502


# ---------------------------------------------------------------------------
# C3: POST /v1/calls endpoint
# ---------------------------------------------------------------------------
def _cp_settings(**over):
    base = dict(
        booking_url="http://booking:7002",
        voice_admin_internal_token="adm-tok",
        outbound_trunk_id="ST_outbound1",
        outbound_cps=1000.0,  # tests must not pace-sleep
    )
    base.update(over)
    return Settings(**base)


def _client(settings) -> httpx.AsyncClient:
    app = create_app(settings)
    return httpx.AsyncClient(
        transport=httpx.ASGITransport(app=app), base_url="http://test"
    )


class TestCallsEndpoint:
    async def _post(self, client, body, token=None):
        headers = {"X-Internal-Token": token} if token else {}
        return await client.post("/v1/calls", json=body, headers=headers)

    async def test_401_without_token(self):
        async with _client(_cp_settings()) as c:
            r = await self._post(c, {"site_slug": "acme", "phone_number": "+2348030000000"})
            assert r.status_code == 401
            r = await self._post(
                c, {"site_slug": "acme", "phone_number": "+2348030000000"}, token="wrong"
            )
            assert r.status_code == 401

    async def test_503_when_token_unset_fail_closed(self):
        async with _client(_cp_settings(voice_admin_internal_token="")) as c:
            r = await self._post(c, {"site_slug": "acme", "phone_number": "+2348030000000"})
            assert r.status_code == 503

    async def test_422_invalid_number(self):
        async with _client(_cp_settings()) as c:
            r = await self._post(
                c, {"site_slug": "acme", "phone_number": "garbage"}, token="adm-tok"
            )
            assert r.status_code == 422

    async def test_503_trunk_unconfigured(self):
        async with _client(_cp_settings(outbound_trunk_id="")) as c:
            r = await self._post(
                c, {"site_slug": "acme", "phone_number": "+2348030000000"},
                token="adm-tok",
            )
            assert r.status_code == 503

    async def test_200_dials_and_reports_dialing(self, monkeypatch):
        dialed: list[tuple] = []

        async def fake_place(settings, slug, number, **kw):
            dialed.append((slug, number))
            return outbound.OutboundCall(
                room=outbound.outbound_room_name(slug, "d" * 32),
                call_id="d" * 32,
                site_slug=slug,
                phone_number=number,
            )

        monkeypatch.setattr(outbound, "place_outbound_call", fake_place)
        async with _client(_cp_settings()) as c:
            r = await self._post(
                c, {"site_slug": "acme", "phone_number": "+2348030000000"},
                token="adm-tok",
            )
        assert r.status_code == 200, r.text
        body = r.json()
        assert body["status"] == "dialing"
        assert body["room"] == "outcall-acme-" + "d" * 32
        assert dialed == [("acme", "+2348030000000")]

    async def test_pacer_acquires_before_dial(self, monkeypatch):
        """R1 wiring: the endpoint MUST pace before every dial."""
        order: list[str] = []

        class RecordingPacer:
            async def acquire(self):
                order.append("pace")
                return 0.0

        async def fake_place(settings, slug, number, **kw):
            order.append("dial")
            return outbound.OutboundCall(
                room="r", call_id="c" * 32, site_slug=slug, phone_number=number
            )

        monkeypatch.setattr(outbound, "place_outbound_call", fake_place)
        app = create_app(_cp_settings())
        app.state.outbound_pacer = RecordingPacer()
        # Rebind the route's closure pacer: the endpoint captured its own
        # reference, so patch the module-level accessor instead.
        monkeypatch.setattr(
            "app.control_plane.outbound.OutboundPacer.acquire",
            RecordingPacer.acquire,
            raising=True,
        )
        async with httpx.AsyncClient(
            transport=httpx.ASGITransport(app=app), base_url="http://test"
        ) as c:
            r = await c.post(
                "/v1/calls",
                json={"site_slug": "acme", "phone_number": "+2348030000000"},
                headers={"X-Internal-Token": "adm-tok"},
            )
        assert r.status_code == 200, r.text
        assert order == ["pace", "dial"]


# ---------------------------------------------------------------------------
# C1: outbound trunk deploy YAML
# ---------------------------------------------------------------------------
class TestOutboundTrunkYaml:
    def test_example_yaml_schema(self):
        cfg = yaml.safe_load((SIP_DIR / "outbound-trunk-config.example.yaml").read_text())
        assert cfg["name"] == "opendesk-outbound"
        assert isinstance(cfg["address"], str) and cfg["address"]
        assert isinstance(cfg["numbers"], list) and cfg["numbers"]
        for n in cfg["numbers"]:
            assert n.startswith("+")

    def test_setup_script_provisions_outbound(self):
        sh = (SIP_DIR / "setup.sh").read_text()
        assert "sip outbound create" in sh
        assert "OUTBOUND_TRUNK_ID" in sh
