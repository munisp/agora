"""SPEC-W97 tests: G8 Dapr statestore session persistence.

- Round-trip: to_dict/from_dict (all persisted fields; ivr_menu excluded;
  junk-safe from_dict).
- DaprSessionStore over a REAL DaprClient + httpx MockTransport (W93
  pattern): create -> PUT with TTL metadata + key prefix; "restart"
  (fresh store, same backing state) + correct secret -> resume; wrong
  secret -> NO resume + new cid + targeted envelope untouched (K15(b));
  miss -> fresh; GET 500 -> fresh + metric (fail-open); save 500 ->
  DaprError propagates + metric.
- History: round-trip; bounded at 40.
- ChatService end-to-end "restart": a SECOND ChatService + store against
  the same backing state resumes confirmed/verified phone (K15(b)).
- Helpers: memory backend hooks are no-ops (ChatHistory untouched).
- Config: SESSION_STORE_BACKEND / SESSION_TTL_S / SESSION_STATE_STORE
  defaults, overrides, bad value -> memory, TTL clamp.

REDIRECTION PROOFS (SPEC-W97 gates):
  R1 save_state PUT removed -> restart-resume tests FAIL;
  R2 load-side secret check removed -> wrong-secret test FAILs;
  R3 chat turn-end persist removed -> chat restart test FAILs.
"""

from __future__ import annotations

import json
from types import SimpleNamespace

import httpx
import pytest

from app import metrics
from app.chat import ChatService
from app.config import load_settings
from app.dapr_client import DaprClient, DaprError
from app.session_state import (
    DaprSessionStore,
    SessionState,
    SessionStore,
    store_get,
    store_get_or_create,
    store_load_history,
    store_save_history,
    store_save_session,
)

from conftest import FakeDapr

from test_w93_elevenlabs_dtmf import _ctx, _settings


@pytest.fixture(autouse=True)
def _fresh_registry():
    metrics.reset_registry()
    yield


# ------------------------------------------------------- fake state backend


class FakeStateBackend:
    """In-memory stand-in for daprd's /v1.0/state endpoints, driven through
    a REAL DaprClient over httpx.MockTransport (no client mocks)."""

    def __init__(self, *, fail_get: bool = False, fail_save: bool = False):
        self.state: dict[str, object] = {}
        self.saved: list[tuple[str, dict]] = []  # (key, raw entry)
        self.fail_get = fail_get
        self.fail_save = fail_save

    def handler(self, request: httpx.Request) -> httpx.Response:
        path = request.url.path
        assert path.startswith("/v1.0/state/statestore")
        key = path[len("/v1.0/state/statestore") :].lstrip("/")
        if request.method == "POST":
            if self.fail_save:
                return httpx.Response(500, text="redis down")
            for entry in request.read() and json.loads(request.content):
                self.state[entry["key"]] = entry["value"]
                self.saved.append((entry["key"], entry))
            return httpx.Response(204)
        if self.fail_get:
            return httpx.Response(500, text="redis down")
        if key in self.state:
            return httpx.Response(200, json=self.state[key])
        return httpx.Response(204)

    def dapr(self) -> DaprClient:
        client = DaprClient("http://daprd-test:3500")
        client._client = httpx.AsyncClient(transport=httpx.MockTransport(self.handler))
        return client


def _store(backend: FakeStateBackend, **kw) -> DaprSessionStore:
    return DaprSessionStore(backend.dapr(), **kw)


def _rendered(counter) -> str:
    return "\n".join(counter.render())


# ------------------------------------------------------- serialization


class TestSessionSerialization:
    def _session(self) -> SessionState:
        s = SessionState(conversation_id="conv-1", site_slug="demo")
        s.pending_phone = "+2348000000001"
        s.confirmed_phone = "+2348000000001"
        s.verified_phone = "+2348000000001"
        s.claimed_phone = "+2348000000002"
        s.caller_name = "Ada"
        s.last_booking_ids = ["b1", "b2"]
        s.active_agent = "clinic-nurse"
        s.escalation_room = "escalation-conv-1"
        s.active_language = "yo"
        s.channel = "whatsapp"
        s.resolved_agent_id = "agent-9"
        s.ivr_menu = ("transient",)  # derived per-call — never persisted
        return s

    def test_round_trip_all_fields(self):
        s = self._session()
        restored = SessionState.from_dict(s.to_dict())
        assert restored is not None
        assert restored.conversation_id == "conv-1"
        assert restored.site_slug == "demo"
        assert restored.session_secret == s.session_secret
        assert restored.pending_phone == "+2348000000001"
        assert restored.confirmed_phone == "+2348000000001"
        assert restored.verified_phone == "+2348000000001"
        assert restored.claimed_phone == "+2348000000002"
        assert restored.caller_name == "Ada"
        assert restored.last_booking_ids == ["b1", "b2"]
        assert restored.active_agent == "clinic-nurse"
        assert restored.escalation_room == "escalation-conv-1"
        assert restored.active_language == "yo"
        assert restored.channel == "whatsapp"
        assert restored.resolved_agent_id == "agent-9"
        assert restored.created_at == s.created_at
        assert restored.touched_at == s.touched_at

    def test_ivr_menu_not_persisted(self):
        data = self._session().to_dict()
        assert "ivr_menu" not in data
        restored = SessionState.from_dict(data)
        assert restored.ivr_menu is None

    def test_from_dict_junk_safe(self):
        assert SessionState.from_dict(None) is None
        assert SessionState.from_dict("nope") is None
        assert SessionState.from_dict({}) is None
        assert SessionState.from_dict({"v": 999, "conversation_id": "x",
                                       "site_slug": "y", "session_secret": "z"}) is None
        # missing required identity fields
        assert SessionState.from_dict({"v": 1, "conversation_id": "",
                                       "site_slug": "y", "session_secret": "z"}) is None
        # unknown extra fields ignored
        data = self._session().to_dict()
        data["future_field"] = {"anything": True}
        assert SessionState.from_dict(data) is not None


# ------------------------------------------------------- DaprSessionStore


class TestDaprSessionStore:
    async def test_create_then_save_persists_envelope_with_ttl(self):
        backend = FakeStateBackend()
        store = _store(backend, ttl_s=1800)
        session = await store.get_or_create(None, "demo")
        session.mark_verified("+2348000000001")
        await store.save(session)
        key = f"voice-session:{session.conversation_id}"
        assert key in backend.state
        envelope = backend.state[key]
        assert envelope["v"] == 1
        assert envelope["verified_phone"] == "+2348000000001"
        # TTL metadata rode the save
        saved_entry = dict((k, e) for k, e in backend.saved)[key]
        assert saved_entry["metadata"] == {"ttlInSeconds": "1800"}

    async def test_restart_resume_with_secret(self):
        backend = FakeStateBackend()
        store1 = _store(backend)
        session = await store1.get_or_create(None, "demo")
        session.mark_verified("+2348000000001")
        session.escalation_room = "escalation-conv-1"
        await store1.save(session)
        cid, secret = session.conversation_id, session.session_secret

        # "Restart": a brand-new store instance over the same backing state.
        store2 = _store(backend)
        resumed = await store2.get_or_create(cid, "demo", session_secret=secret)
        assert resumed.conversation_id == cid
        assert resumed.session_secret == secret
        assert resumed.verified_phone == "+2348000000001"
        assert resumed.confirmed_phone == "+2348000000001"
        assert resumed.escalation_room == "escalation-conv-1"

    async def test_wrong_secret_never_resumes(self):
        backend = FakeStateBackend()
        store1 = _store(backend)
        session = await store1.get_or_create(None, "demo")
        session.mark_verified("+2348000000001")
        await store1.save(session)
        cid = session.conversation_id
        original_envelope = dict(backend.state[f"voice-session:{cid}"])

        store2 = _store(backend)
        fresh = await store2.get_or_create(cid, "demo", session_secret="WRONG")
        assert fresh.conversation_id != cid  # new id, targeted untouched
        assert fresh.verified_phone is None
        assert fresh.confirmed_phone is None
        assert backend.state[f"voice-session:{cid}"] == original_envelope

    async def test_missing_secret_never_resumes(self):
        backend = FakeStateBackend()
        store1 = _store(backend)
        session = await store1.get_or_create(None, "demo")
        await store1.save(session)
        store2 = _store(backend)
        fresh = await store2.get_or_create(session.conversation_id, "demo")
        assert fresh.conversation_id != session.conversation_id
        assert fresh.verified_phone is None

    async def test_miss_creates_fresh_with_presented_id(self):
        backend = FakeStateBackend()
        store = _store(backend)
        session = await store.get_or_create("brand-new-id", "demo")
        assert session.conversation_id == "brand-new-id"
        rendered = _rendered(metrics.get_registry().session_store_ops)
        assert 'op="load",result="miss"' in rendered

    async def test_get_error_fails_open_to_fresh(self):
        backend = FakeStateBackend(fail_get=True)
        store = _store(backend)
        session = await store.get_or_create("conv-x", "demo")
        assert session.conversation_id == "conv-x"  # degraded, not broken
        rendered = _rendered(metrics.get_registry().session_store_ops)
        assert 'op="load",result="error"' in rendered

    async def test_save_error_raises_and_counts(self):
        backend = FakeStateBackend(fail_save=True)
        store = _store(backend)
        session = await store.get_or_create(None, "demo")
        with pytest.raises(DaprError):
            await store.save(session)
        rendered = _rendered(metrics.get_registry().session_store_ops)
        assert 'op="save",result="error"' in rendered


# ------------------------------------------------------- history


class TestHistoryPersistence:
    async def test_history_round_trip(self):
        backend = FakeStateBackend()
        store = _store(backend)
        msgs = [{"role": "user", "content": "hello"},
                {"role": "assistant", "content": "hi there"}]
        await store.save_history("conv-1", msgs)
        loaded = await store.load_history("conv-1")
        assert loaded == msgs
        key = "voice-history:conv-1"
        saved_entry = dict((k, e) for k, e in backend.saved)[key]
        assert saved_entry["metadata"]["ttlInSeconds"] == "3600"

    async def test_history_bounded_at_40(self):
        backend = FakeStateBackend()
        store = _store(backend)
        msgs = [{"role": "user", "content": f"m{i}"} for i in range(50)]
        await store.save_history("conv-1", msgs)
        loaded = await store.load_history("conv-1")
        assert len(loaded) == 40
        assert loaded[0]["content"] == "m10"  # oldest dropped

    async def test_history_miss_returns_none(self):
        backend = FakeStateBackend()
        store = _store(backend)
        assert await store.load_history("never-seen") is None

    async def test_history_save_error_raises_and_counts(self):
        backend = FakeStateBackend(fail_save=True)
        store = _store(backend)
        with pytest.raises(DaprError):
            await store.save_history("conv-1", [])
        rendered = _rendered(metrics.get_registry().session_store_ops)
        assert 'op="history_save",result="error"' in rendered


# ------------------------------------------------------- helpers (memory)


class TestMemoryBackendHooks:
    async def test_memory_hooks_are_noops(self):
        store = SessionStore()
        session = store.get_or_create("conv-1", "demo")
        got = await store_get_or_create(store, "conv-1", "demo",
                                        session_secret=session.session_secret)
        assert got is session
        assert await store_get(store, "conv-1") is session
        await store_save_session(store, session)  # no-op, no raise
        assert await store_load_history(store, "conv-1") is None
        await store_save_history(store, "conv-1", [{"role": "user", "content": "x"}])


# ------------------------------------------------------- chat end-to-end


class _StubLLM:
    """One-shot tool-free LLM (house seam: chat_with_tools /
    stream_with_tools — tests drive the seam, production keeps the real
    client)."""

    def __init__(self, reply: str = "Sure, I can help with that."):
        self._reply = reply

    async def chat_with_tools(self, messages, tools):
        return SimpleNamespace(content=self._reply, tool_calls=[])

    async def stream_with_tools(self, messages, tools):
        reply = self._reply

        async def gen():
            yield SimpleNamespace(
                choices=[
                    SimpleNamespace(
                        delta=SimpleNamespace(content=reply, tool_calls=[])
                    )
                ]
            )

        return gen()


class TestChatRestartResume:
    def _service(self, monkeypatch, backend_store) -> ChatService:
        from app import chat as chat_module

        async def fake_fetch(dapr, settings_, site_slug):
            return _ctx()

        monkeypatch.setattr(chat_module, "fetch_tenant_context", fake_fetch)
        return ChatService(
            settings=_settings(),
            dapr=FakeDapr(),
            llm=_StubLLM(),  # type: ignore[arg-type]
            sessions=backend_store,
        )

    async def test_chat_session_survives_restart(self, monkeypatch):
        backend = FakeStateBackend()
        store1 = _store(backend)
        service1 = self._service(monkeypatch, store1)
        # First turn pins a VERIFIED identity (internal-token channel pin).
        result1 = await service1.handle_message(
            site_slug="demo",
            message="hello",
            conversation_id=None,
            channel="whatsapp",
            pin_verified_identity="+2348000000001",
        )
        cid, secret = result1["conversation_id"], result1["session_secret"]
        assert result1["phone_verified"] is True
        # The turn-end persist actually wrote the envelope.
        assert f"voice-session:{cid}" in backend.state

        # "Restart": new store + new service, same backing state.
        store2 = _store(backend)
        service2 = self._service(monkeypatch, store2)
        result2 = await service2.handle_message(
            site_slug="demo",
            message="again",
            conversation_id=cid,
            session_secret=secret,
            channel="whatsapp",
        )
        assert result2["conversation_id"] == cid
        # Verified identity resumed from the persisted envelope — the
        # second turn did NOT need the pin to report verified.
        assert result2["phone_verified"] is True
        assert result2["phone_confirmed"] is True

    async def test_chat_restart_wrong_secret_does_not_resume(self, monkeypatch):
        backend = FakeStateBackend()
        store1 = _store(backend)
        service1 = self._service(monkeypatch, store1)
        result1 = await service1.handle_message(
            site_slug="demo",
            message="hello",
            conversation_id=None,
            channel="whatsapp",
            pin_verified_identity="+2348000000001",
        )
        cid = result1["conversation_id"]

        service2 = self._service(monkeypatch, _store(backend))
        result2 = await service2.handle_message(
            site_slug="demo",
            message="again",
            conversation_id=cid,
            session_secret="WRONG",
            channel="whatsapp",
        )
        assert result2["conversation_id"] != cid
        assert result2["phone_verified"] is False

    async def test_chat_memory_backend_still_works(self, monkeypatch):
        service = self._service(monkeypatch, SessionStore())
        result = await service.handle_message(
            site_slug="demo", message="hello", conversation_id=None
        )
        assert result["conversation_id"]

    async def test_stream_persist_runs(self, monkeypatch):
        backend = FakeStateBackend()
        store = _store(backend)
        service = self._service(monkeypatch, store)
        frames = []
        async for frame in service.handle_message_stream(
            site_slug="demo", message="hello", conversation_id=None
        ):
            frames.append(frame)
        done = frames[-1]
        assert done["done"] is True
        assert f"voice-session:{done['conversation_id']}" in backend.state


# ------------------------------------------------------- config


class TestPersistenceConfig:
    def test_defaults(self, monkeypatch):
        for var in ("SESSION_STORE_BACKEND", "SESSION_TTL_S", "SESSION_STATE_STORE"):
            monkeypatch.delenv(var, raising=False)
        settings = load_settings()
        assert settings.session_store_backend == "memory"
        assert settings.session_ttl_s == 3600
        assert settings.session_state_store == "statestore"

    def test_overrides(self, monkeypatch):
        monkeypatch.setenv("SESSION_STORE_BACKEND", "dapr")
        monkeypatch.setenv("SESSION_TTL_S", "1800")
        monkeypatch.setenv("SESSION_STATE_STORE", "statestore")
        settings = load_settings()
        assert settings.session_store_backend == "dapr"
        assert settings.session_ttl_s == 1800

    def test_bad_backend_degrades_to_memory(self, monkeypatch):
        monkeypatch.setenv("SESSION_STORE_BACKEND", "postgres")
        assert load_settings().session_store_backend == "memory"

    def test_ttl_clamped(self, monkeypatch):
        monkeypatch.setenv("SESSION_TTL_S", "5")
        assert load_settings().session_ttl_s == 60
        monkeypatch.setenv("SESSION_TTL_S", "99999999")
        assert load_settings().session_ttl_s == 86400
