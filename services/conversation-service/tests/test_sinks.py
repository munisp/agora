"""Sink selection unit tests (SPEC-W48 B): all three sinks — raw transcript,
intel enriched, call quality — honor TRANSCRIPT_SINK=kafka|fluvio, and
FluvioSink connects with an explicit FluvioConfig built from FLUVIO_ENDPOINT
(never the default ~/.fluvio profile). The fluvio client module is mocked at
the sys.modules seam (unit-level mock of the external client; the production
path uses the real client). Offline: no Kafka/Fluvio/Postgres."""

from __future__ import annotations

import json
import sys
import types

import pytest

sys.path.insert(0, ".")

from app import config as config_mod  # noqa: E402
from app.config import Config  # noqa: E402
from app.sinks import FluvioSink, KafkaSink, build_sink  # noqa: E402

pytestmark = pytest.mark.asyncio


def _cfg(**overrides) -> Config:
    return Config(**overrides)


class _FakeProducer:
    def __init__(self) -> None:
        self.sent: list[str] = []

    def send_string(self, payload: str):
        self.sent.append(payload)
        return object()


class _FakeClient:
    def __init__(self) -> None:
        self.producers: dict[str, _FakeProducer] = {}

    def topic_producer(self, topic: str) -> _FakeProducer:
        producer = _FakeProducer()
        self.producers[topic] = producer
        return producer


class _FakeFluvio:
    """Stands in for fluvio.Fluvio; records how the client was created."""

    configs: list[object] = []
    clients: list[_FakeClient] = []

    @classmethod
    def reset(cls) -> None:
        cls.configs = []
        cls.clients = []

    @classmethod
    def connect(cls):
        raise AssertionError(
            "Fluvio.connect() default-profile path must not be used (W48-B)"
        )

    @classmethod
    def connect_with_config(cls, config):
        cls.configs.append(config)
        client = _FakeClient()
        cls.clients.append(client)
        return client


class _FakeFluvioConfig:
    """Stands in for fluvio.FluvioConfig; records new(addr) calls."""

    addrs: list[str] = []

    def __init__(self, addr: str) -> None:
        self.addr = addr

    @classmethod
    def reset(cls) -> None:
        cls.addrs = []

    @classmethod
    def new(cls, addr: str):
        cls.addrs.append(addr)
        return cls(addr)


@pytest.fixture
def fake_fluvio(monkeypatch):
    _FakeFluvio.reset()
    _FakeFluvioConfig.reset()
    mod = types.ModuleType("fluvio")
    mod.Fluvio = _FakeFluvio
    mod.FluvioConfig = _FakeFluvioConfig
    monkeypatch.setitem(sys.modules, "fluvio", mod)
    return mod


TOPICS = {
    "transcript": "opendesk.transcripts-raw",
    "enriched": "opendesk.conversation.enriched",
    "quality": "opendesk.conversation.quality",
}


def _build_all(cfg: Config) -> dict[str, object]:
    """The three sinks exactly as main.py wires them."""
    return {
        "transcript": build_sink(cfg),
        "enriched": build_sink(cfg, cfg.enriched_topic),
        "quality": build_sink(cfg, cfg.quality_topic),
    }


async def test_kafka_default_selects_kafka_for_all_three_sinks():
    sinks = _build_all(_cfg())  # transcript_sink defaults to "kafka"
    for name, sink in sinks.items():
        assert isinstance(sink, KafkaSink), name
        assert sink._brokers == ["kafka:9092"], name
        assert sink._topic == TOPICS[name], name


async def test_fluvio_selects_fluvio_for_all_three_sinks(fake_fluvio):
    sinks = _build_all(_cfg(transcript_sink="fluvio"))
    for name, sink in sinks.items():
        assert isinstance(sink, FluvioSink), name
        assert sink._topic == TOPICS[name], name
        assert sink._endpoint == "fluvio:9003", name


async def test_fluvio_endpoint_env_override(fake_fluvio, monkeypatch):
    monkeypatch.setenv("TRANSCRIPT_SINK", "fluvio")
    monkeypatch.setenv("FLUVIO_ENDPOINT", "fluvio-staging:9013")
    cfg = config_mod.load()
    assert cfg.fluvio_endpoint == "fluvio-staging:9013"
    sink = build_sink(cfg)
    assert isinstance(sink, FluvioSink)
    assert sink._endpoint == "fluvio-staging:9013"


async def test_fluvio_sink_connects_with_explicit_endpoint_config(fake_fluvio):
    sink = FluvioSink("opendesk.transcripts-raw", "fluvio:9003")
    await sink.start()
    # Explicit profile: FluvioConfig.new(endpoint) + connect_with_config,
    # never the default-profile Fluvio.connect().
    assert _FakeFluvioConfig.addrs == ["fluvio:9003"]
    assert len(_FakeFluvio.configs) == 1
    assert _FakeFluvio.configs[0].addr == "fluvio:9003"
    assert len(_FakeFluvio.clients) == 1
    assert list(_FakeFluvio.clients[0].producers) == ["opendesk.transcripts-raw"]

    record = {"conversationId": "c1", "tenantId": "t1", "role": "user",
              "text": "hello", "ts": 1}
    await sink.publish(record)
    sent = _FakeFluvio.clients[0].producers["opendesk.transcripts-raw"].sent
    assert [json.loads(p) for p in sent] == [record]

    await sink.close()
    assert sink._producer is None


async def test_fluvio_publish_before_start_asserts(fake_fluvio):
    sink = FluvioSink("opendesk.transcripts-raw", "fluvio:9003")
    with pytest.raises(AssertionError):
        await sink.publish({"conversationId": "c1"})


async def test_fluvio_unavailable_falls_back_to_kafka(monkeypatch):
    # Import guard: no fluvio package -> RuntimeError -> KafkaSink fallback.
    monkeypatch.setitem(sys.modules, "fluvio", None)
    sinks = _build_all(_cfg(transcript_sink="fluvio"))
    for name, sink in sinks.items():
        assert isinstance(sink, KafkaSink), name
        assert sink._topic == TOPICS[name], name


async def test_unknown_sink_value_defaults_to_kafka():
    sink = build_sink(_cfg(transcript_sink="nats"))
    assert isinstance(sink, KafkaSink)
    assert sink._topic == TOPICS["transcript"]
