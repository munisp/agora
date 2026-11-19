"""SPEC-W84 tests: per-tenant pack stepUpPolicy tool tiers.

- parse_pack_tool_tiers (app/config.py): the TOLERANT pack-variant parser —
  malformed entries drop with a warning, never raise (a tenant payload must
  never crash a session); built-in floors are never weakened; pack plugin
  tools may take any tier 0|1|2 including 0.
- tenant_context._apply_pack wiring: pack.stepUpPolicy.tools lands on
  ctx.pack_tool_tiers; malformed blocks drop without failing the pack apply.
- ToolLayer._tool_tier precedence: pack > env (VOICE_TOOL_TIERS) > built-in
  default; plugin default is 1 (a safe default, not a floor); a pack
  raising a built-in to tier 2 is hard-gated at the dispatch choke point.
"""

from __future__ import annotations

import pytest

from app import metrics
from app.config import (
    DEFAULT_VOICE_TOOL_TIERS,
    Settings,
    parse_pack_tool_tiers,
)
from app.session_state import SessionState
from app.tenant_context import TenantContext, _apply_pack
from app.tools import ToolLayer, _verification_required_payload

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


class _FakePlugin:
    def __init__(self, name: str) -> None:
        self.name = name
        self.executed_with: list[dict] = []

    def schema(self) -> dict:
        return {"type": "function", "function": {"name": self.name, "parameters": {}}}

    async def execute(self, arguments: dict) -> dict:
        self.executed_with.append(dict(arguments))
        return {"status": "ok", "tool": self.name}


def _layer(
    session: SessionState | None = None,
    settings: Settings | None = None,
    ctx: TenantContext | None = None,
    plugins: list | None = None,
) -> ToolLayer:
    return ToolLayer(
        dapr=FakeDapr(),  # type: ignore[arg-type]
        settings=settings or _settings(),
        ctx=ctx or _ctx(),
        session=session or SessionState(conversation_id="conv-1", site_slug="demo"),
        plugin_tools=plugins or [],
    )


# ---------------------------------------------------------------------------
# parse_pack_tool_tiers: tolerant drop matrix
# ---------------------------------------------------------------------------
class TestParsePackToolTiers:
    def test_none_and_non_mapping_yield_empty(self):
        assert parse_pack_tool_tiers(None) == {}
        assert parse_pack_tool_tiers("not-a-mapping") == {}
        assert parse_pack_tool_tiers([("a", 1)]) == {}
        assert parse_pack_tool_tiers(42) == {}

    def test_valid_map_parsed(self):
        assert parse_pack_tool_tiers(
            {"book_appointment": 2, "plugin_tool": 0}
        ) == {"book_appointment": 2, "plugin_tool": 0}

    def test_bad_names_dropped(self):
        out = parse_pack_tool_tiers(
            {"good_tool": 1, "bad-name": 1, "9tool": 2, "": 1, 42: 1}
        )
        assert out == {"good_tool": 1}

    def test_bad_tiers_dropped(self):
        out = parse_pack_tool_tiers(
            {
                "ok": 1,
                "neg": -1,
                "high": 3,
                "text": "2",
                "float": 1.5,
                "bool_true": True,
                "bool_false": False,
                "none": None,
            }
        )
        assert out == {"ok": 1}

    def test_never_weakens_builtin_floors(self):
        # Built-in floors: get_availability=0, book_appointment=1,
        # cancel_appointment=2. Lowering any of them must drop.
        out = parse_pack_tool_tiers(
            {"book_appointment": 0, "cancel_appointment": 1, "cancel_appointment_2": 1}
        )
        assert "book_appointment" not in out
        assert "cancel_appointment" not in out
        # Unknown (plugin) name at tier 1 survives.
        assert out == {"cancel_appointment_2": 1}

    def test_builtin_floor_equal_and_raise_kept(self):
        out = parse_pack_tool_tiers(
            {"book_appointment": 1, "cancel_appointment": 2, "get_business_info": 2}
        )
        assert out == {
            "book_appointment": 1,
            "cancel_appointment": 2,
            "get_business_info": 2,
        }

    def test_plugin_tool_may_lower_to_zero(self):
        # Plugin default of 1 is a safe default, NOT a floor: a pack may
        # explicitly open a plugin tool to anonymous callers.
        assert parse_pack_tool_tiers({"opening_hours_plus": 0}) == {
            "opening_hours_plus": 0
        }

    def test_never_raises_on_garbage(self):
        # A hostile tenant payload must never crash a session.
        out = parse_pack_tool_tiers({"a": object(), "b": [1], "c": {"x": 1}})
        assert out == {}


# ---------------------------------------------------------------------------
# _apply_pack wiring
# ---------------------------------------------------------------------------
class TestApplyPackStepUp:
    def test_pack_stepup_lands_on_context(self):
        ctx = _ctx()
        _apply_pack(ctx, {"pack": {"stepUpPolicy": {"tools": {"book_appointment": 2}}}})
        assert ctx.pack_tool_tiers == {"book_appointment": 2}

    def test_pack_without_stepup_leaves_default(self):
        ctx = _ctx()
        _apply_pack(ctx, {"pack": {"terminology": {"offering": "service"}}})
        assert ctx.pack_tool_tiers == {}

    def test_malformed_stepup_drops_without_crash(self):
        ctx = _ctx()
        _apply_pack(ctx, {"pack": {"stepUpPolicy": "not-a-mapping"}})
        assert ctx.pack_tool_tiers == {}
        _apply_pack(ctx, {"pack": {"stepUpPolicy": {"tools": {"bad name": 9}}}})
        assert ctx.pack_tool_tiers == {}

    def test_floor_weakening_dropped_at_consumption(self):
        ctx = _ctx()
        _apply_pack(
            ctx,
            {"pack": {"stepUpPolicy": {"tools": {"cancel_appointment": 0, "ok_tool": 0}}}},
        )
        # The identity boundary is shape-only; consumption re-enforces the
        # never-weakening invariant (defense in depth).
        assert ctx.pack_tool_tiers == {"ok_tool": 0}


# ---------------------------------------------------------------------------
# _tool_tier precedence: pack > env > built-in default
# ---------------------------------------------------------------------------
class TestPackTierPrecedence:
    def test_pack_overrides_env_and_default(self):
        ctx = _ctx()
        ctx.pack_tool_tiers = {"book_appointment": 2, "loyalty_signup": 0}
        layer = _layer(
            ctx=ctx,
            settings=_settings(
                voice_tool_tiers={**DEFAULT_VOICE_TOOL_TIERS, "loyalty_signup": 1}
            ),
        )
        # Pack raises built-in above both env map and default floor.
        assert layer._tool_tier("book_appointment") == 2
        # Pack lowers plugin below env map entry (plugin default not a floor).
        assert layer._tool_tier("loyalty_signup") == 0
        # Untouched built-ins keep env/default resolution.
        assert layer._tool_tier("cancel_appointment") == 2
        assert layer._tool_tier("get_availability") == 0

    def test_env_wins_when_pack_silent(self):
        layer = _layer(
            settings=_settings(
                voice_tool_tiers={**DEFAULT_VOICE_TOOL_TIERS, "book_appointment": 2}
            )
        )
        assert layer._tool_tier("book_appointment") == 2

    def test_empty_pack_map_is_noop(self):
        ctx = _ctx()
        ctx.pack_tool_tiers = {}
        layer = _layer(ctx=ctx)
        assert layer._tool_tier("some_pack_tool") == 1
        assert layer._tool_tier("get_availability") == 0


# ---------------------------------------------------------------------------
# Dispatch gating with pack tiers
# ---------------------------------------------------------------------------
class TestPackTierDispatch:
    async def test_pack_raise_to_tier2_gates_builtin(self):
        """The clinic pack (stepUpPolicy.tools.book_appointment=2) scenario:
        booking now requires an OTP-verified caller."""
        ctx = _ctx()
        _apply_pack(ctx, {"pack": {"stepUpPolicy": {"tools": {"book_appointment": 2}}}})
        layer = _layer(ctx=ctx)
        result = await layer.dispatch(
            "book_appointment",
            {
                "offering_id": "o",
                "team_member_id": "t",
                "starts_at": "2030-01-01T10:00:00+00:00",
                "phone": PHONE,
            },
        )
        assert result == _verification_required_payload()

    async def test_pack_raised_builtin_passes_when_verified(self):
        ctx = _ctx()
        _apply_pack(ctx, {"pack": {"stepUpPolicy": {"tools": {"book_appointment": 2}}}})
        session = SessionState(conversation_id="conv-1", site_slug="demo")
        session.mark_verified(PHONE_NORM)
        layer = _layer(ctx=ctx, session=session)
        result = await layer.dispatch(
            "book_appointment",
            {
                "offering_id": "o",
                "team_member_id": "t",
                "starts_at": "2030-01-01T10:00:00+00:00",
                "phone": PHONE,
            },
        )
        # Tier satisfied: falls through to the built-in's own tier-1
        # confirmation flow (booking backend is unreachable in tests, but the
        # read-back confirmation payload precedes any network call).
        assert result["status"] == "confirmation_required"

    async def test_pack_tier0_plugin_passes_anonymous(self):
        plugin = _FakePlugin("opening_hours_plus")
        ctx = _ctx()
        _apply_pack(
            ctx, {"pack": {"stepUpPolicy": {"tools": {"opening_hours_plus": 0}}}}
        )
        layer = _layer(ctx=ctx, plugins=[plugin])
        result = await layer.dispatch("opening_hours_plus", {})
        assert result["status"] == "ok"
        assert plugin.executed_with == [{}]

    async def test_pack_tier2_plugin_refused_anonymous(self):
        plugin = _FakePlugin("loyalty_points")
        ctx = _ctx()
        _apply_pack(ctx, {"pack": {"stepUpPolicy": {"tools": {"loyalty_points": 2}}}})
        layer = _layer(ctx=ctx, plugins=[plugin])
        result = await layer.dispatch("loyalty_points", {"phone": PHONE})
        assert result == _verification_required_payload()
        assert plugin.executed_with == []

    async def test_pack_tier2_plugin_passes_when_verified(self):
        plugin = _FakePlugin("loyalty_points")
        ctx = _ctx()
        _apply_pack(ctx, {"pack": {"stepUpPolicy": {"tools": {"loyalty_points": 2}}}})
        session = SessionState(conversation_id="conv-1", site_slug="demo")
        session.mark_verified(PHONE_NORM)
        layer = _layer(ctx=ctx, session=session, plugins=[plugin])
        assert (await layer.dispatch("loyalty_points", {}))["status"] == "ok"

    async def test_unaffected_builtin_behavior_unchanged(self):
        """With a pack tier on ONE tool, all other built-ins keep their
        pre-W84 behavior (no collateral gating)."""
        ctx = _ctx()
        _apply_pack(ctx, {"pack": {"stepUpPolicy": {"tools": {"book_appointment": 2}}}})
        layer = _layer(ctx=ctx)
        result = await layer.dispatch("lookup_appointment", {"phone": PHONE})
        assert result == _verification_required_payload()
