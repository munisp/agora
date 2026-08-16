"""Settings regression tests (PY-004): the default HTTP port must not
collide with fraud-engine (:7017) or any other repo service port."""

from __future__ import annotations

from credit_bureau.config import Settings, load_settings


def test_default_port_is_7022_not_fraud_engine_collision(monkeypatch) -> None:
    monkeypatch.delenv("PORT", raising=False)
    settings = load_settings()
    assert settings.port == 7022
    # The collision this guards against (fraud-engine owns :7017).
    assert settings.port != 7017


def test_port_env_override_still_wins(monkeypatch) -> None:
    monkeypatch.setenv("PORT", "7999")
    assert load_settings().port == 7999


def test_settings_dataclass_default_matches(monkeypatch) -> None:
    # Direct dataclass construction (no env) agrees with load_settings.
    monkeypatch.delenv("PORT", raising=False)
    assert Settings().port == 7022
