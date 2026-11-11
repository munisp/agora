"""SPEC-W47 MW-H1: DAPR_HOST robustness.

The shipped compose file sets ``DAPR_HOST: ${DAPR_HOST:-}`` — an EMPTY
override that used to beat the code default and point every Dapr call at
an unresolvable host. The setting must fall back to the daprd-analytics
default WITH a loud error log.
"""

from __future__ import annotations

import logging

from analytics_pipeline.config import DEFAULT_DAPR_HOST, load_settings


def test_dapr_host_unset_uses_default(monkeypatch):
    monkeypatch.delenv("DAPR_HOST", raising=False)
    assert load_settings().dapr_host == DEFAULT_DAPR_HOST


def test_dapr_host_empty_falls_back_with_loud_log(monkeypatch, caplog):
    monkeypatch.setenv("DAPR_HOST", "")
    with caplog.at_level(logging.ERROR, logger="analytics_pipeline.config"):
        settings = load_settings()
    assert settings.dapr_host == DEFAULT_DAPR_HOST
    assert any(
        "DAPR_HOST is set but EMPTY" in rec.message for rec in caplog.records
    ), "empty DAPR_HOST must log loudly, not fall back silently"


def test_dapr_host_whitespace_falls_back(monkeypatch, caplog):
    monkeypatch.setenv("DAPR_HOST", "   ")
    with caplog.at_level(logging.ERROR, logger="analytics_pipeline.config"):
        assert load_settings().dapr_host == DEFAULT_DAPR_HOST
    assert any("EMPTY" in rec.message for rec in caplog.records)


def test_dapr_host_explicit_value_wins(monkeypatch, caplog):
    monkeypatch.setenv("DAPR_HOST", "daprd-shared")
    with caplog.at_level(logging.ERROR, logger="analytics_pipeline.config"):
        assert load_settings().dapr_host == "daprd-shared"
    assert not caplog.records
