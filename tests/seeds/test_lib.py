"""Tests for scripts/seeds/_lib.py — SPEC-W17 contract A. All DB-free."""

from __future__ import annotations

import hashlib
import json

import _lib
import pytest


# --- deterministic_id --------------------------------------------------------
def test_deterministic_id_matches_contract_formula():
    expected = hashlib.sha256(
        f"{_lib.DEFAULT_SEED_SALT}|lga:Lagos:Ikeja".encode("utf-8")
    ).hexdigest()
    assert _lib.deterministic_id("lga:Lagos:Ikeja") == expected


def test_deterministic_id_stable_across_calls():
    assert _lib.deterministic_id("channel:ussd") == _lib.deterministic_id("channel:ussd")
    assert _lib.deterministic_id("channel:ussd") != _lib.deterministic_id("channel:sms")


def test_deterministic_id_uses_env_salt(monkeypatch):
    monkeypatch.setenv("SEED_SALT", "other-salt")
    monkeypatch.setattr(_lib, "SEED_SALT_CONSTANT", "other-salt")
    expected = hashlib.sha256(b"other-salt|k").hexdigest()
    assert _lib.deterministic_id("k") == expected


# --- hash_pii ------------------------------------------------------------------
def test_hash_pii_argon2_digest_shape():
    digest = _lib.hash_pii("+2348012345678")
    assert digest.startswith("argon2$")  # argon2-cffi pinned in requirements-seeds
    parts = digest.split("$")
    assert len(parts) == 3 and all(parts[1:])


def test_hash_pii_random_salt_means_digests_differ_and_hide_plaintext():
    d1, d2 = _lib.hash_pii("Ada Lovelace"), _lib.hash_pii("Ada Lovelace")
    assert d1 != d2  # per-call random salt (verification-only digest)
    assert "Ada" not in d1 and "Lovelace" not in d1


# --- scaled --------------------------------------------------------------------
def test_scaled_default_and_env_override(monkeypatch):
    monkeypatch.delenv("SEED_SCALE", raising=False)
    assert _lib.scaled(5000) == 5000
    monkeypatch.setenv("SEED_SCALE", "0.05")
    assert _lib.scaled(5000) == 250
    monkeypatch.setenv("SEED_SCALE", "not-a-float")
    assert _lib.scaled(5000) == 5000  # loud fallback to 1.0


def test_apply_scale_arg_cli_overrides_env(monkeypatch):
    monkeypatch.setenv("SEED_SCALE", "0.05")
    assert _lib.apply_scale_arg(0.5) == 0.5
    assert _lib.apply_scale_arg(None) == 0.05
    with pytest.raises(SystemExit):
        _lib.apply_scale_arg(0)


# --- emit_seed_report -----------------------------------------------------------
def test_emit_seed_report_jsonl_default(monkeypatch, tmp_path):
    out = tmp_path / "reports.jsonl"
    monkeypatch.setenv("SEED_REPORT_PATH", str(out))
    monkeypatch.setenv("SEED_KAFKA", "off")
    event = _lib.emit_seed_report("cac.lgas", 774, "runner@host", "abc123")
    lines = out.read_text(encoding="utf-8").splitlines()
    assert len(lines) == 1
    on_disk = json.loads(lines[0])
    for payload in (event, on_disk):
        assert payload["specversion"] == "1.0"
        assert payload["type"] == "com.opendesk.cac.SeedReport"
        assert payload["subject"] == "cac.lgas"
        assert payload["data"] == {
            "table": "cac.lgas",
            "rowcount": 774,
            "runner_id": "runner@host",
            "git_sha": "abc123",
        }


def test_emit_seed_report_never_raises_on_io_failure(monkeypatch):
    monkeypatch.setenv("SEED_KAFKA", "off")
    monkeypatch.setenv("SEED_REPORT_PATH", "/nonexistent-dir-xyz/reports.jsonl")
    event = _lib.emit_seed_report("cac.wards", 8812, "r", "s")  # must not raise
    assert event["data"]["rowcount"] == 8812


def test_emit_seed_report_kafka_failure_falls_back_to_jsonl(monkeypatch, tmp_path):
    out = tmp_path / "reports.jsonl"
    monkeypatch.setenv("SEED_REPORT_PATH", str(out))
    monkeypatch.setenv("SEED_KAFKA", "on")

    def boom(topic, event):
        raise RuntimeError("no broker")

    monkeypatch.setattr(_lib, "_kafka_publish", boom)
    _lib.emit_seed_report("cac.channels", 32, "r", "s")
    assert len(out.read_text(encoding="utf-8").splitlines()) == 1


# --- DB helpers (fake conn) -----------------------------------------------------
def test_delete_by_ids_sql_shape(fake_conn):
    n = _lib.delete_by_ids(fake_conn, "cac.lgas", ["a", "b"])
    assert n == 2
    sql, params = fake_conn.executed[0]
    assert sql == "DELETE FROM cac.lgas WHERE id = ANY(%s)"
    assert params == (["a", "b"],)


def test_upsert_rows_on_conflict_do_update_shape(fake_conn):
    rows = [{"id": "x", "name": "n", "v": 1}, {"id": "y", "name": "m", "v": 2}]
    n = _lib.upsert_rows(fake_conn, "cac.t", ["id", "name", "v"], rows)
    assert n == 2
    sql, params = fake_conn.executed[0]
    assert sql.startswith("INSERT INTO cac.t (id, name, v) VALUES (%s, %s, %s)")
    assert "ON CONFLICT (id) DO UPDATE SET" in sql
    assert "name = EXCLUDED.name" in sql and "v = EXCLUDED.v" in sql
    assert "id = EXCLUDED.id" not in sql  # PK never updated
    assert params == ("x", "n", 1)


def test_log_seed_run_upserts_seed_run_log(fake_conn):
    _lib.log_seed_run("cac.lgas", 774, fake_conn)
    sql, params = fake_conn.executed[0]
    assert "INSERT INTO cac.seed_run_log" in sql
    assert "ON CONFLICT (id) DO UPDATE" in sql
    assert params[0] == _lib.deterministic_id("seed_run_log:cac.lgas")
    assert params[1] == "cac.lgas" and params[2] == 774


def test_seed_argparser_supports_dry_run_and_scale():
    parser = _lib.seed_argparser("x")
    args = parser.parse_args(["--dry-run", "--scale", "0.5"])
    assert args.dry_run is True and args.scale == 0.5
    args = parser.parse_args([])
    assert args.dry_run is False and args.scale is None
