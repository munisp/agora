"""SPEC-W59 A5/P8: AV scan tests.

Unit tests for app/av.py reply parsing + zINSTREAM framing against the
fake-clamd ThreadingTCPServer fixture (TEST-ONLY, conftest.py), and the
route-level fail-posture matrix:

  infected            => deterministic 415 (av_infected, signature named),
                         NOTHING stored (no MinIO object, no DB row),
                         critical ops alert on opendesk.ops.alerts.
  clean               => 200 pass-through.
  clamd down          => kyc_* 503 ALWAYS (even CLAMAV_REQUIRED=false);
                         receipt 503 when CLAMAV_REQUIRED=true;
                         receipt 200-with-skip-signal (metric + ops alert)
                         when CLAMAV_REQUIRED=false.
  CLAMAV_HOST empty   => CRITICAL boot log + skip (P10).

EICAR is wrapped in make_png() — raw EICAR dies at the magic-byte gate.
"""

from __future__ import annotations

import pytest

from app import main as main_module
from app.av import AvClient, AvUnavailable, Clean, Infected
from app.main import AV_SCANS
from conftest import EICAR, EICAR_SIGNATURE, INTERNAL_TOKEN, TENANT_A, closed_port, make_png

EXTRACT = "/v1/ocr/extract"
CHUNK = 2 * 1024 * 1024


def _counter(result: str) -> float:
    return AV_SCANS.labels(result=result)._value.get()


def _post(client, *, data=None, kind="receipt", tenant=TENANT_A, filename="doc.png"):
    return client.post(
        EXTRACT,
        files={"file": (filename, data if data is not None else make_png(), "image/png")},
        data={"kind": kind, "tenant": tenant},
        headers={"X-Internal-Token": INTERNAL_TOKEN},
    )


# ---------------------------------------------------------------------------
# av.py unit tests (reply parsing + framing) against the fake clamd
# ---------------------------------------------------------------------------
async def test_scan_clean_returns_clean(av_client, fake_clamd):
    data = make_png()
    result = await av_client.scan(data)
    assert isinstance(result, Clean)
    assert fake_clamd.scans == [(data, [len(data)])]


async def test_scan_eicar_returns_infected(av_client):
    result = await av_client.scan(make_png() + EICAR)
    assert isinstance(result, Infected)
    assert result.signature == EICAR_SIGNATURE


async def test_scan_error_reply_raises_unavailable(av_client, fake_clamd):
    # P3: "INSTREAM size limit exceeded" is a scan ERROR, never clean.
    fake_clamd.forced_reply = b"stream: INSTREAM size limit exceeded. ERROR\n"
    with pytest.raises(AvUnavailable, match="scan error"):
        await av_client.scan(make_png())


async def test_scan_garbage_reply_raises_unavailable(av_client, fake_clamd):
    fake_clamd.forced_reply = b"this is not a clamd reply\n"
    with pytest.raises(AvUnavailable, match="unexpected clamd reply"):
        await av_client.scan(make_png())


async def test_scan_timeout_raises_unavailable(fake_clamd):
    fake_clamd.hang_s = 2.0
    host, port = fake_clamd.address
    client = AvClient(host=host, port=port, timeout_s=0.2)
    with pytest.raises(AvUnavailable):
        await client.scan(make_png())


async def test_scan_unreachable_raises_unavailable():
    client = AvClient(host="127.0.0.1", port=closed_port(), timeout_s=2.0)
    with pytest.raises(AvUnavailable):
        await client.scan(make_png())


async def test_scan_connection_drop_raises_unavailable(av_client, fake_clamd):
    # Server accepts the stream but closes without any reply bytes.
    fake_clamd.forced_reply = b""
    with pytest.raises(AvUnavailable, match="without a reply"):
        await av_client.scan(make_png())


async def test_chunked_framing_25_mib(av_client, fake_clamd):
    size = 25 * 1024 * 1024
    data = make_png() + bytes(size - len(make_png()))
    result = await av_client.scan(data)
    assert isinstance(result, Clean)
    [(received, chunk_sizes)] = fake_clamd.scans
    assert received == data
    # 4-byte BE length framing in 2 MiB chunks + zero terminator:
    # 25 MiB -> 12 full 2 MiB chunks + one 1 MiB tail.
    assert chunk_sizes == [CHUNK] * 12 + [size - 12 * CHUNK]


# ---------------------------------------------------------------------------
# route-level P8 matrix
# ---------------------------------------------------------------------------
def test_infected_upload_415_no_store_alert_metric(client, deps, av_client, fake_clamd):
    deps.av = av_client
    infected_before = _counter("infected")
    resp = _post(client, data=make_png() + EICAR, kind="kyc_nin_slip")
    assert resp.status_code == 415, resp.text
    detail = resp.json()["detail"]
    assert detail["error"] == "av_infected"
    assert detail["signature"] == EICAR_SIGNATURE
    assert EICAR_SIGNATURE in detail["detail"]
    # No bytes echoed/logged: the EICAR payload itself never appears.
    assert "EICAR-STANDARD" not in resp.text
    # NOTHING stored: no MinIO object, no ocr_documents row.
    assert deps.documents.objects == {}
    assert deps.db.rows == {}
    # Critical ops alert on opendesk.ops.alerts — exact payload keys,
    # no PII/bytes/filename (P5); NOT usage/ocr topics.
    assert len(deps.dapr.published) == 1
    pubsub, topic, payload = deps.dapr.published[0]
    assert topic == "opendesk.ops.alerts"
    assert payload["type"] == "com.opendesk.ocr.AvScanAlert"
    assert set(payload["data"]) == {"severity", "tenant_id", "kind", "sha256", "signature"}
    assert payload["data"]["severity"] == "critical"
    assert payload["data"]["tenant_id"] == TENANT_A
    assert payload["data"]["kind"] == "kyc_nin_slip"
    assert payload["data"]["signature"] == EICAR_SIGNATURE
    assert len(payload["data"]["sha256"]) == 64
    assert _counter("infected") == infected_before + 1


def test_clean_upload_200_metric_and_metrics_endpoint(client, deps, av_client):
    deps.av = av_client
    clean_before = _counter("clean")
    resp = _post(client)
    assert resp.status_code == 200, resp.text
    assert resp.json()["status"] == "extracted"
    assert _counter("clean") == clean_before + 1
    # No ops alert for a clean scan; the normal two events still fire.
    topics = [t for _, t, _ in deps.dapr.published]
    assert topics == ["opendesk.usage.events", "opendesk.ocr.events"]
    metrics = client.get("/metrics")
    assert metrics.status_code == 200
    assert 'opendesk_ocr_av_scans_total{result="clean"}' in metrics.text


def test_clamd_down_kyc_503_always(client, deps, monkeypatch):
    # Hard-coded fail-closed on is_kyc_kind, regardless of CLAMAV_REQUIRED.
    monkeypatch.setattr("app.config.settings.clamav_required", False)
    deps.av = AvClient(host="127.0.0.1", port=closed_port(), timeout_s=2.0)
    resp = _post(client, kind="kyc_passport")
    assert resp.status_code == 503, resp.text
    assert resp.json()["detail"] == {"error": "av_unavailable"}
    assert deps.documents.objects == {}
    assert deps.db.rows == {}


def test_clamd_down_receipt_503_when_required(client, deps, monkeypatch):
    monkeypatch.setattr("app.config.settings.clamav_required", True)
    deps.av = AvClient(host="127.0.0.1", port=closed_port(), timeout_s=2.0)
    error_before = _counter("error")
    resp = _post(client, kind="receipt")
    assert resp.status_code == 503, resp.text
    assert resp.json()["detail"] == {"error": "av_unavailable"}
    assert deps.documents.objects == {}
    assert deps.db.rows == {}
    assert _counter("error") == error_before + 1


def test_clamd_down_receipt_fail_open_skip_is_loud(client, deps, monkeypatch):
    monkeypatch.setattr("app.config.settings.clamav_required", False)
    deps.av = AvClient(host="127.0.0.1", port=closed_port(), timeout_s=2.0)
    skipped_before = _counter("skipped")
    resp = _post(client, kind="receipt")
    assert resp.status_code == 200, resp.text
    assert _counter("skipped") == skipped_before + 1
    # Never silent: warning-severity ops alert precedes the normal events.
    topics = [t for _, t, _ in deps.dapr.published]
    assert topics == ["opendesk.ops.alerts", "opendesk.usage.events", "opendesk.ocr.events"]
    alert = deps.dapr.published[0][2]
    assert alert["type"] == "com.opendesk.ocr.AvScanAlert"
    assert alert["data"]["severity"] == "warning"
    assert alert["data"]["reason"] == "av_unavailable"
    assert alert["data"]["kind"] == "receipt"
    assert set(alert["data"]) == {"severity", "tenant_id", "kind", "sha256", "reason"}


def test_av_disabled_skips_scan(client, deps):
    # FakeDeps default av=None (CLAMAV_HOST-empty posture): skip + metric,
    # the pre-existing no-AV behavior stays green (P10).
    assert deps.av is None
    skipped_before = _counter("skipped")
    resp = _post(client)
    assert resp.status_code == 200, resp.text
    assert _counter("skipped") == skipped_before + 1


def test_clamav_host_empty_critical_boot_log(monkeypatch, capfd):
    monkeypatch.setattr("app.config.settings.clamav_host", "")
    assert main_module._build_av_client() is None
    out = capfd.readouterr().out
    assert "clamav.disabled" in out
    assert '"level": "critical"' in out


def test_clamav_host_set_builds_client(monkeypatch):
    monkeypatch.setattr("app.config.settings.clamav_host", "clamav")
    monkeypatch.setattr("app.config.settings.clamav_port", 3310)
    client = main_module._build_av_client()
    assert isinstance(client, AvClient)


def test_config_defaults_and_env_parity():
    from app.config import Settings

    s = Settings()
    assert s.clamav_host == "clamav"
    assert s.clamav_port == 3310
    assert s.clamav_timeout_s == 30.0
    assert s.clamav_required is True
    s2 = Settings(
        _env_file=None,
        CLAMAV_HOST="",
        CLAMAV_PORT="3311",
        CLAMAV_TIMEOUT_S="5",
        CLAMAV_REQUIRED="false",
    )
    assert s2.clamav_host == ""
    assert s2.clamav_port == 3311
    assert s2.clamav_timeout_s == 5.0
    assert s2.clamav_required is False
