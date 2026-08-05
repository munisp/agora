"""D3 capture_velocity: >30 captures in any rolling 60 min => medium;
sustained 3 windows => high."""

from fraud_engine.detectors.d3_velocity import CaptureVelocityDetector

from conftest import NOW, TENANT, burst_captures


def test_fires_medium_over_threshold(client, graph, settings):
    # 31 captures inside 31 minutes (30s spacing) -> one hot rolling window
    burst_captures(graph, TENANT, "agent-fast", 31, start_minutes_ago=40)
    findings = CaptureVelocityDetector().detect(client, TENANT, settings, NOW)
    assert len(findings) == 1
    f = findings[0]
    assert f.type == "capture_velocity" and f.severity == "medium"
    assert f.agent_id == "agent-fast" and f.person_id is None
    assert f.evidence["max_captures_in_window"] == 31
    assert f.evidence["threshold"] == 30


def test_silent_at_exactly_threshold(client, graph, settings):
    burst_captures(graph, TENANT, "agent-ok", 30, start_minutes_ago=40)
    assert CaptureVelocityDetector().detect(client, TENANT, settings, NOW) == []


def test_high_when_sustained_three_windows(client, graph, settings):
    # 31 over-threshold captures in each of 3 consecutive 60-min slots.
    for slot in range(3):
        burst_captures(
            graph, TENANT, "agent-machines", 31,
            start_minutes_ago=slot * 60 + 40, lead_prefix=f"s{slot}",
        )
    findings = CaptureVelocityDetector().detect(client, TENANT, settings, NOW)
    assert len(findings) == 1
    assert findings[0].severity == "high"
    assert findings[0].evidence["sustained_windows"] is True


def test_silent_when_spread_out(client, graph, settings):
    # 60 captures but 3 minutes apart -> ~20 per rolling hour, under threshold
    burst_captures(graph, TENANT, "agent-steady", 60, start_minutes_ago=200,
                   spacing_seconds=180.0)
    assert CaptureVelocityDetector().detect(client, TENANT, settings, NOW) == []
