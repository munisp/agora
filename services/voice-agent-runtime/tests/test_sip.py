"""Tests for the SIP inbound bootstrap (Wave 5 #1, app/sip.py)."""

from __future__ import annotations

from types import SimpleNamespace

import pytest

from app import metrics, sip
from app.config import load_settings
from app.session_state import SessionState


# --------------------------------------------------------------------------
# normalize_phone / parse_tenant_phone_map
# --------------------------------------------------------------------------

@pytest.mark.parametrize(
    "raw,expected",
    [
        ("+1 (555) 123-4567", "+15551234567"),
        ("  +44 20 7946 0958 ", "+442079460958"),
        ("+15551234567", "+15551234567"),
        ("", ""),
        (None, ""),
        ("anonymous", "anonymous"),
    ],
)
def test_normalize_phone(raw, expected):
    assert sip.normalize_phone(raw) == expected


def test_parse_tenant_phone_map_happy():
    m = sip.parse_tenant_phone_map('{"+15551234567": "acme", "+442079460958": "globex"}')
    assert m == {"+15551234567": "acme", "+442079460958": "globex"}


def test_parse_tenant_phone_map_normalizes_keys():
    m = sip.parse_tenant_phone_map('{"+1 (555) 123-4567": "acme"}')
    assert m == {"+15551234567": "acme"}


def test_parse_tenant_phone_map_tolerant():
    assert sip.parse_tenant_phone_map("not json") == {}
    assert sip.parse_tenant_phone_map('["+15551234567"]') == {}
    assert sip.parse_tenant_phone_map('{"bad-number": "acme"}') == {}
    assert sip.parse_tenant_phone_map('{"+15551234567": ""}') == {}
    assert sip.parse_tenant_phone_map("") == {}
    assert sip.parse_tenant_phone_map(None) == {}


# --------------------------------------------------------------------------
# SIP detection
# --------------------------------------------------------------------------

def test_is_sip_room():
    assert sip.is_sip_room("call-+15551234567")
    assert not sip.is_sip_room("site-acme")
    assert not sip.is_sip_room("")


def _participant(kind=None, identity="", attributes=None):
    return SimpleNamespace(kind=kind, identity=identity, attributes=attributes or {})


def test_is_sip_participant_by_kind():
    assert sip.is_sip_participant(_participant(kind=SimpleNamespace(name="SIP")))
    assert sip.is_sip_participant(_participant(kind="sip"))


def test_is_sip_participant_by_identity_and_attrs():
    assert sip.is_sip_participant(_participant(identity="sip_+15551234567_x"))
    assert sip.is_sip_participant(_participant(attributes={"sip.phoneNumber": "+1"}))
    assert not sip.is_sip_participant(_participant(identity="web-abc"))
    assert not sip.is_sip_participant(None)


# --------------------------------------------------------------------------
# Call info extraction
# --------------------------------------------------------------------------

def test_extract_call_info_from_attributes():
    p = _participant(
        kind=SimpleNamespace(name="SIP"),
        identity="sip_+15559876543_ab12",
        attributes={
            "sip.phoneNumber": "+1 (555) 987-6543",
            "sip.trunkPhoneNumber": "+15551234567",
        },
    )
    caller, dialed, attrs = sip.extract_call_info("call-+15551234567", [p])
    assert caller == "+15559876543"
    assert dialed == "+15551234567"
    assert attrs["sip.phoneNumber"].startswith("+1")


def test_extract_call_info_falls_back_to_identity_then_room():
    p = _participant(identity="sip_+15559876543_ab12")
    caller, dialed, _ = sip.extract_call_info("call-+15551234567", [p])
    assert caller == "+15559876543"
    assert dialed == "+15551234567"  # room name = dialed number (callee dispatch)


def test_extract_call_info_room_only():
    caller, dialed, _ = sip.extract_call_info("call-+15551234567", [])
    assert caller == ""
    assert dialed == "+15551234567"


# --------------------------------------------------------------------------
# Tenant resolution
# --------------------------------------------------------------------------

def test_resolve_tenant_map_hit():
    slug, src = sip.resolve_tenant("+1 555 123-4567", {"+15551234567": "acme"})
    assert (slug, src) == ("acme", "map")


def test_resolve_tenant_default_fallback():
    slug, src = sip.resolve_tenant("+19999999999", {"+15551234567": "acme"}, "frontdesk")
    assert (slug, src) == ("frontdesk", "default")


def test_resolve_tenant_unmapped_rejected():
    with pytest.raises(sip.SipTenantResolutionError):
        sip.resolve_tenant("+19999999999", {})


# --------------------------------------------------------------------------
# Caller ID attachment — SPEC-W45 K15(c): CLAIMED, never confirmed
# --------------------------------------------------------------------------

def test_attach_caller_id_marks_claimed_not_confirmed():
    s = SessionState(conversation_id="c1", site_slug="acme")
    assert sip.attach_caller_id(s, "+1 (555) 987-6543") is True
    assert s.claimed_phone == "+15559876543"
    # K15(c) / OOS-03: the carrier-asserted bypass is REMOVED — caller ID is
    # not proof of possession, so no confirmed/verified phone is set.
    assert s.confirmed_phone is None
    assert s.verified_phone is None
    assert s.pending_phone is None


def test_attach_caller_id_anonymous_noop():
    s = SessionState(conversation_id="c1", site_slug="acme")
    assert sip.attach_caller_id(s, "") is False
    assert s.claimed_phone is None


def test_attach_caller_id_does_not_clobber_confirmed():
    s = SessionState(conversation_id="c1", site_slug="acme")
    s.confirmed_phone = "+11111111111"
    sip.attach_caller_id(s, "+12222222222")
    assert s.confirmed_phone == "+11111111111"
    assert s.claimed_phone == "+12222222222"


# --------------------------------------------------------------------------
# Full bootstrap
# --------------------------------------------------------------------------

def _settings(monkeypatch, phone_map="", default_site=""):
    monkeypatch.setenv("TENANT_PHONE_MAP", phone_map)
    monkeypatch.setenv("SIP_DEFAULT_SITE", default_site)
    return load_settings()


def test_bootstrap_inbound_call_maps_tenant_and_claims_caller(monkeypatch):
    settings = _settings(monkeypatch, '{"+15551234567": "acme"}')
    p = _participant(
        kind=SimpleNamespace(name="SIP"),
        identity="sip_+15559876543_ab",
        attributes={"sip.phoneNumber": "+15559876543"},
    )
    session = SessionState(conversation_id="c1", site_slug="")
    ctx = sip.bootstrap_inbound_call(settings, "call-+15551234567", [p], session)
    assert ctx.site_slug == "acme"
    assert ctx.tenant_source == "map"
    assert ctx.dialed_number == "+15551234567"
    # K15(c): claimed (unverified) only.
    assert session.claimed_phone == "+15559876543"
    assert session.confirmed_phone is None


def test_bootstrap_inbound_call_unmapped_raises(monkeypatch):
    settings = _settings(monkeypatch)
    with pytest.raises(sip.SipTenantResolutionError):
        sip.bootstrap_inbound_call(settings, "call-+19999999999", [])


def test_bootstrap_inbound_call_default_site(monkeypatch):
    settings = _settings(monkeypatch, default_site="frontdesk")
    ctx = sip.bootstrap_inbound_call(settings, "call-+19999999999", [])
    assert ctx.site_slug == "frontdesk"
    assert ctx.tenant_source == "default"


# --------------------------------------------------------------------------
# SPEC-W38 F1: registry-first resolution (resolve_agent_for_dialed)
# --------------------------------------------------------------------------

class _StubRegistry:
    """Scriptable agents-registry client stand-in."""

    def __init__(self, record=None, calls=None):
        self._record = record
        self.calls = calls if calls is not None else []

    async def resolve_agent_by_phone(self, phone):
        self.calls.append(phone)
        return self._record


def _agent_record(tenant_slug="acme"):
    return SimpleNamespace(
        id="agent-1",
        tenant_slug=tenant_slug,
        tenant_id="t-uuid",
        definition=None,
    )


async def test_registry_resolution_wins_over_map(monkeypatch):
    settings = _settings(monkeypatch, '{"+15551234567": "map-tenant"}')
    registry = _StubRegistry(_agent_record(tenant_slug="acme"))
    slug, source, record = await sip.resolve_agent_for_dialed(
        settings, "+15551234567", registry=registry
    )
    assert (slug, source) == ("acme", "registry")
    assert record is not None and record.id == "agent-1"
    assert registry.calls == ["+15551234567"]
    assert metrics.get_registry().agent_resolution._series["registry"] == 1


async def test_registry_miss_falls_back_to_map(monkeypatch):
    settings = _settings(monkeypatch, '{"+15551234567": "acme"}')
    registry = _StubRegistry(None)
    slug, source, record = await sip.resolve_agent_for_dialed(
        settings, "+15551234567", registry=registry
    )
    assert (slug, source) == ("acme", "map")
    assert record is None
    assert registry.calls == ["+15551234567"]  # consulted, failed open


async def test_registry_down_falls_back_and_metrics_count(monkeypatch):
    settings = _settings(monkeypatch, default_site="frontdesk")
    slug, source, record = await sip.resolve_agent_for_dialed(
        settings, "+19999999999", registry=_StubRegistry(None)
    )
    assert (slug, source) == ("frontdesk", "default")
    assert record is None
    assert metrics.get_registry().agent_resolution._series["default"] == 1


async def test_no_registry_url_keeps_legacy_path(monkeypatch):
    """Shared-client path: with AGENTS_REGISTRY_URL unset the legacy map is
    used and no registry call happens (dev-mode compat)."""
    from app import agents_registry

    monkeypatch.setenv("TENANT_PHONE_MAP", '{"+15551234567": "acme"}')
    monkeypatch.setenv("AGENTS_REGISTRY_URL", "")
    agents_registry.set_registry_client(None)
    settings = load_settings()
    try:
        slug, source, record = await sip.resolve_agent_for_dialed(
            settings, "+15551234567"
        )
    finally:
        agents_registry.set_registry_client(None)
    assert (slug, source) == ("acme", "map")
    assert record is None


async def test_bootstrap_async_carries_agent_record(monkeypatch):
    settings = _settings(monkeypatch)
    record = _agent_record(tenant_slug="acme")
    p = _participant(
        kind=SimpleNamespace(name="SIP"),
        attributes={"sip.phoneNumber": "+15559876543"},
    )
    ctx = await sip.bootstrap_inbound_call_async(
        settings, "call-+15551234567", [p], registry=_StubRegistry(record)
    )
    assert ctx.site_slug == "acme"
    assert ctx.tenant_source == "registry"
    assert ctx.agent_record is record
    assert ctx.caller_phone == "+15559876543"
