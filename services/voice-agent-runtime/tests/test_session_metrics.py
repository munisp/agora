"""Per-session call-quality accumulator (app/metrics.py SessionMetrics) and
the enriched SessionEnded payload shape (app/events.session_lifecycle_data).

The accumulator feeds the `quality` object on the SessionEnded CloudEvent
consumed by crm-sync-service (Twenty call-summary note).
"""

from __future__ import annotations

from app import metrics
from app.events import new_cloudevent, session_lifecycle_data

QUALITY_KEYS = {
    "duration_s",
    "turn_count",
    "tool_calls",
    "avg_llm_latency_ms",
    "max_llm_latency_ms",
    "stt_calls",
    "tts_calls",
    "llm_fallback_used",
    "escalated",
    "confirmed_phone",
    # SPEC-W11 Part C: additive emergency flag on the quality payload.
    "emergency",
}


def test_empty_session_produces_no_quality():
    sm = metrics.SessionMetrics("conv-1")
    assert sm.has_data() is False
    assert sm.quality_payload() is None


def test_turn_only_session_has_data():
    sm = metrics.SessionMetrics("conv-1")
    sm.record_turn()
    assert sm.has_data() is True
    payload = sm.quality_payload()
    assert payload is not None
    assert payload["turn_count"] == 1


def test_llm_latency_avg_and_max_ms():
    sm = metrics.SessionMetrics("conv-1")
    sm.record_llm_latency(0.8)  # 800 ms
    sm.record_llm_latency(1.4)  # 1400 ms
    sm.record_llm_latency(0.25)  # 250 ms
    payload = sm.quality_payload()
    assert payload is not None
    # avg = (800+1400+250)/3 = 816.66 -> 817, max = 1400 (rounded ints, ms).
    assert payload["avg_llm_latency_ms"] == 817
    assert payload["max_llm_latency_ms"] == 1400


def test_no_llm_calls_yield_null_latency_fields():
    sm = metrics.SessionMetrics("conv-1")
    sm.record_stt()
    payload = sm.quality_payload()
    assert payload is not None
    assert payload["avg_llm_latency_ms"] is None
    assert payload["max_llm_latency_ms"] is None


def test_tool_call_counts_per_name():
    sm = metrics.SessionMetrics("conv-1")
    sm.record_tool_call("book_appointment")
    sm.record_tool_call("book_appointment")
    sm.record_tool_call("get_availability")
    payload = sm.quality_payload()
    assert payload is not None
    assert payload["tool_calls"] == {"book_appointment": 2, "get_availability": 1}
    # The snapshot is a copy — later mutations must not leak into it.
    sm.record_tool_call("book_appointment")
    assert payload["tool_calls"]["book_appointment"] == 2


def test_stt_tts_counts_and_fallback_flag():
    sm = metrics.SessionMetrics("conv-1")
    sm.record_stt()
    sm.record_stt()
    sm.record_tts()
    sm.record_llm_fallback()
    payload = sm.quality_payload()
    assert payload is not None
    assert payload["stt_calls"] == 2
    assert payload["tts_calls"] == 1
    assert payload["llm_fallback_used"] is True


def test_fallback_defaults_to_false():
    sm = metrics.SessionMetrics("conv-1")
    sm.record_turn()
    payload = sm.quality_payload()
    assert payload is not None
    assert payload["llm_fallback_used"] is False


def test_duration_uses_injected_clock():
    t = [1000.0]
    sm = metrics.SessionMetrics("conv-1", clock=lambda: t[0])
    t[0] = 1000.0 + 95.24
    sm.record_turn()
    payload = sm.quality_payload()
    assert payload is not None
    assert payload["duration_s"] == 95.2  # rounded to 0.1s


def test_escalated_and_confirmed_phone_passthrough():
    sm = metrics.SessionMetrics("conv-1")
    sm.record_turn()
    payload = sm.quality_payload(escalated=True, confirmed_phone="+1555000111")
    assert payload is not None
    assert payload["escalated"] is True
    assert payload["confirmed_phone"] == "+1555000111"


def test_emergency_flag_defaults_false_and_latches():
    """SPEC-W11 Part C: additive `emergency` bool in the quality payload."""
    sm = metrics.SessionMetrics("conv-1")
    sm.record_turn()
    payload = sm.quality_payload()
    assert payload is not None
    assert payload["emergency"] is False

    sm.record_emergency()
    payload = sm.quality_payload()
    assert payload is not None
    assert payload["emergency"] is True


def test_emergency_only_session_still_emits_quality():
    """An emergency-flagged session must ship a quality payload even if no
    other signals were recorded."""
    sm = metrics.SessionMetrics("conv-1")
    assert sm.quality_payload() is None
    sm.record_emergency()
    payload = sm.quality_payload()
    assert payload is not None
    assert payload["emergency"] is True


def test_payload_shape_is_exactly_the_crm_contract():
    sm = metrics.SessionMetrics("conv-1")
    sm.record_turn()
    payload = sm.quality_payload(escalated=False, confirmed_phone=None)
    assert payload is not None
    assert set(payload) == QUALITY_KEYS
    assert payload["confirmed_phone"] is None


def test_active_session_contextvar_routes_recordings():
    sm = metrics.activate_session(metrics.SessionMetrics("conv-ctx"))
    try:
        metrics.session_turn()
        metrics.session_stt()
        metrics.session_tts()
        metrics.session_tool_call("lookup_appointment")
        metrics.session_llm_latency(0.82)
        metrics.session_llm_fallback()
    finally:
        metrics.activate_session(None)  # reset contextvar for other tests
    payload = sm.quality_payload(escalated=False, confirmed_phone="+1")
    assert payload is not None
    assert payload["turn_count"] == 1
    assert payload["stt_calls"] == 1
    assert payload["tts_calls"] == 1
    assert payload["tool_calls"] == {"lookup_appointment": 1}
    assert payload["avg_llm_latency_ms"] == 820
    assert payload["llm_fallback_used"] is True


def test_recording_helpers_noop_without_active_session():
    metrics.activate_session(None)
    # Must not raise even with no active session.
    metrics.session_turn()
    metrics.session_stt()
    metrics.session_tts()
    metrics.session_tool_call("x")
    metrics.session_llm_latency(1.0)
    metrics.session_llm_fallback()


def test_session_lifecycle_data_includes_quality_only_when_present():
    base = session_lifecycle_data(
        conversation_id="c1", channel="voice", site_slug="acme"
    )
    assert "quality" not in base
    assert base == {"conversationId": "c1", "channel": "voice", "siteSlug": "acme"}

    quality = {"duration_s": 12.0, "turn_count": 3}
    enriched = session_lifecycle_data(
        conversation_id="c1", channel="voice", site_slug="acme", quality=quality
    )
    assert enriched["quality"] == quality


def test_session_ended_cloudevent_carries_quality():
    sm = metrics.SessionMetrics("conv-1")
    sm.record_turn()
    sm.record_tool_call("book_appointment")
    quality = sm.quality_payload(escalated=False, confirmed_phone="+1555000111")
    event = new_cloudevent(
        type_="com.opendesk.conversation.SessionEnded",
        subject="acme",
        tenant_uuid="tenant-uuid",
        data=session_lifecycle_data(
            conversation_id="conv-1",
            channel="voice",
            site_slug="acme",
            quality=quality,
        ),
    )
    assert event["type"] == "com.opendesk.conversation.SessionEnded"
    assert set(event["data"]["quality"]) == QUALITY_KEYS
    assert event["data"]["quality"]["confirmed_phone"] == "+1555000111"
def test_session_lifecycle_data_includes_agent_id_only_when_resolved():
    # SPEC-W38 F3/G3: registry-resolved calls emit camelCase `agentId`
    # (the conversation-service capture consumer's primary key); unresolved
    # (web / legacy TENANT_PHONE_MAP) calls keep the pre-W38 shape.
    base = session_lifecycle_data(
        conversation_id="c1", channel="voice", site_slug="acme"
    )
    assert "agentId" not in base

    resolved = session_lifecycle_data(
        conversation_id="c1", channel="voice", site_slug="acme", agent_id="agent-42"
    )
    assert resolved["agentId"] == "agent-42"
    # Falsy agent ids ("" / None) behave like an unresolved call.
    falsy = session_lifecycle_data(
        conversation_id="c1", channel="voice", site_slug="acme", agent_id=""
    )
    assert "agentId" not in falsy


def test_session_ended_cloudevent_carries_resolved_agent_id():
    quality = {"duration_s": 12.0, "turn_count": 3}
    event = new_cloudevent(
        type_="com.opendesk.conversation.SessionEnded",
        subject="acme",
        tenant_uuid="tenant-uuid",
        data=session_lifecycle_data(
            conversation_id="conv-1",
            channel="voice",
            site_slug="acme",
            quality=quality,
            agent_id="agent-42",
        ),
    )
    assert event["type"] == "com.opendesk.conversation.SessionEnded"
    assert event["data"]["agentId"] == "agent-42"
    assert event["data"]["quality"] == quality


def test_session_state_retains_resolved_agent_id_for_lifecycle():
    # Mirrors app/livekit_worker.build_voice_agent: the SIP bootstrap's
    # agent_record.id rides on SessionState until _publish_lifecycle emits
    # SessionEnded with it.
    from app.session_state import SessionState

    session = SessionState(conversation_id="conv-1", site_slug="acme")
    assert session.resolved_agent_id is None

    class _Record:  # stand-in for app/agents_registry.AgentRecord
        id = "agent-42"

    record = _Record()
    session.resolved_agent_id = (getattr(record, "id", "") or "").strip() or None
    data = session_lifecycle_data(
        conversation_id=session.conversation_id,
        channel="voice",
        site_slug=session.site_slug,
        agent_id=session.resolved_agent_id,
    )
    assert data["agentId"] == "agent-42"

    # No record resolved -> SessionEnded emits no agentId key.
    unresolved = SessionState(conversation_id="conv-2", site_slug="acme")
    data = session_lifecycle_data(
        conversation_id=unresolved.conversation_id,
        channel="voice",
        site_slug=unresolved.site_slug,
        agent_id=unresolved.resolved_agent_id,
    )
    assert "agentId" not in data
