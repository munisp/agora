"""SPEC-W98 tests: I17 seed parity — scripts/seed-demo-all.sh.

A threaded in-process fake gateway (stdlib http.server) emulates the exact
route surface the script calls, INCLUDING the gateway's tenant resolution
rule (W34 GF4 + SPEC-W44 K1: X-Tenant-Slug is stripped; the booking
middleware resolves tenant_slugs[0] from the bearer token). The script is
driven end-to-end via subprocess with TOKEN-mode fake JWTs — the same
one-tenant-per-token discipline the script enforces in production.

Scenarios:
- Full run (one single-claim token per tenant): 4 tenants with correct
  industries/terminology; offerings count == pack offerings count; team
  member + Mon-Sat availability; site published; knowledge doc;
  BOOKINGS_PER_TENANT bookings each, deterministic idempotency keys,
  source=demo-seed, referencing the offering/team ids the fake issued
  (sequencing proof); every /v1 call landed on the RIGHT tenant (the
  fake resolves the claim — cross-tenant writes would corrupt another
  tenant's buckets and fail the assertions).
- Re-run idempotency: no duplicate offerings/bookings, exit 0.
- Multi-claim token with the wrong first claim: non-zero exit, no writes.
- Subset: DEMO_TENANTS="demo-clinic" seeds exactly one tenant.
- Failure honesty: tenant create 500 -> non-zero exit.
- shellcheck -S warning (skipped when shellcheck is absent).

REDIRECTION PROOFS (SPEC-W98 gates):
  R1 booking loop removed -> bookings assertions FAIL;
  R2 site publish removed -> published assertion FAILs;
  R3 idempotency keys removed -> re-run duplication test FAILs.
"""

from __future__ import annotations

import base64
import json
import os
import shutil
import subprocess
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

REPO = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
SCRIPT = os.path.join(REPO, "scripts", "seed-demo-all.sh")
PACK_DIR = os.path.join(REPO, "industries")

ALL_TENANTS = ["demo-salon", "demo-clinic", "demo-consultancy", "demo-logistics"]


def fake_jwt(slugs: list[str]) -> str:
    payload = base64.urlsafe_b64encode(
        json.dumps({"sub": "seed-user", "tenant_slugs": slugs}).encode()
    ).rstrip(b"=")
    return f"hdr.{payload.decode()}.sig"


def pack_offerings_count(pack: str) -> int:
    count = 0
    in_block = False
    for raw in open(os.path.join(PACK_DIR, f"{pack}.yaml"), encoding="utf-8"):
        line = raw.rstrip("\n")
        if line.startswith("offerings:"):
            in_block = True
            continue
        if in_block:
            if line and line[0].isalpha():
                break
            if line.startswith("- name:"):
                count += 1
    return count


# --------------------------------------------------------------------------
# Fake gateway
# --------------------------------------------------------------------------
class FakeGateway(BaseHTTPRequestHandler):
    state: dict  # set on the server class below

    def log_message(self, *args):  # silence
        pass

    def _tenant(self) -> str:
        auth = self.headers.get("Authorization", "")
        token = auth.removeprefix("Bearer ")
        try:
            payload = token.split(".")[1]
            payload += "=" * (-len(payload) % 4)
            claims = json.loads(base64.urlsafe_b64decode(payload))
            slugs = claims.get("tenant_slugs") or []
            return slugs[0] if slugs else ""
        except Exception:
            return ""

    def _send(self, code: int, body) -> None:
        data = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _body(self):
        length = int(self.headers.get("content-length") or 0)
        return json.loads(self.rfile.read(length) or b"{}")

    def _bucket(self, tenant: str, kind: str) -> list:
        return self.state["tenants"].setdefault(tenant, {}).setdefault(kind, [])

    # -- routing -----------------------------------------------------------
    def do_POST(self):
        path, _, query = self.path.partition("?")
        tenant = self._tenant()
        body = self._body()
        if path == "/api/identity/v1/tenants":
            if self.state.get("fail_tenant_create"):
                return self._send(500, {"error": "boom"})
            slug = body["slug"]
            if slug in self.state["tenants"]:
                return self._send(409, {"error": "exists"})
            self.state["tenants"][slug] = {
                "industry": body.get("industry"),
                "terminology": body.get("terminology"),
                "offerings": [], "members": [], "docs": [],
                "site": {"published": False}, "bookings": [],
            }
            return self._send(201, {"slug": slug})
        if path == "/api/bookings/v1/offerings":
            self.state["calls"].append(("offerings", tenant))
            doc = dict(body)
            doc["id"] = f"off-{tenant}-{len(self._bucket(tenant, 'offerings'))}"
            self._bucket(tenant, "offerings").append(doc)
            return self._send(201, doc)
        if path == "/api/bookings/v1/team-members":
            self.state["calls"].append(("team-members", tenant))
            doc = dict(body)
            doc["id"] = f"mem-{tenant}-0"
            self._bucket(tenant, "members").append(doc)
            return self._send(201, doc)
        if path == "/api/knowledge/v1/documents":
            self.state["calls"].append(("docs", tenant))
            self._bucket(tenant, "docs").append(dict(body))
            return self._send(201, {"ok": True})
        if path.startswith("/api/bookings/public/sites/"):
            parts = path.split("/")
            slug = parts[5]
            if parts[6] == "bookings":
                ikey = body.get("idempotency_key")
                bookings = self._bucket(slug, "bookings")
                if ikey and any(b.get("idempotency_key") == ikey for b in bookings):
                    return self._send(200, {"id": "dup", "deduplicated": True})
                bookings.append(dict(body))
                return self._send(201, {"id": f"bk-{slug}-{len(bookings)}"})
        return self._send(404, {"error": f"no route POST {path}"})

    def do_GET(self):
        path, _, query = self.path.partition("?")
        tenant = self._tenant()
        if path == "/api/bookings/v1/offerings":
            self.state["calls"].append(("offerings", tenant))
            return self._send(200, self._bucket(tenant, "offerings"))
        if path == "/api/bookings/v1/team-members":
            self.state["calls"].append(("team-members", tenant))
            return self._send(200, self._bucket(tenant, "members"))
        if path == "/api/knowledge/v1/documents":
            for kv in query.split("&"):
                if kv.startswith("tenant="):
                    tenant = kv.split("=", 1)[1]
            self.state["calls"].append(("docs", tenant))
            return self._send(200, self._bucket(tenant, "docs"))
        if path.startswith("/api/bookings/public/sites/"):
            parts = path.split("/")
            slug = parts[5]
            if parts[6] == "context":
                return self._send(200, {"site": {"name": slug}, "tenant": {"slug": slug}})
            if parts[6] == "availability":
                members = self._bucket(slug, "members")
                if not members:
                    return self._send(200, {"slots": []})
                return self._send(200, {"slots": [{
                    "starts_at": "2026-11-21T09:00:00+01:00",
                    "ends_at": "2026-11-21T10:00:00+01:00",
                    "team_member_id": members[0]["id"],
                    "remaining_capacity": 1,
                }]})
        return self._send(404, {"error": f"no route GET {path}"})

    def do_PUT(self):
        path = self.path
        tenant = self._tenant()
        body = self._body()
        if path.startswith("/api/bookings/v1/team-members/") and path.endswith("/availability"):
            self.state["calls"].append(("availability", tenant))
            self.state["tenants"][tenant]["availability"] = body["rules"]
            return self._send(200, {"ok": True})
        if path == "/api/bookings/v1/site":
            self.state["calls"].append(("site", tenant))
            self.state["tenants"][tenant]["site"].update(body)
            return self._send(200, self.state["tenants"][tenant]["site"])
        return self._send(404, {"error": f"no route PUT {path}"})


@pytest.fixture()
def gateway():
    server = ThreadingHTTPServer(("127.0.0.1", 0), FakeGateway)
    server.state = {"tenants": {}, "calls": []}  # type: ignore[attr-defined]
    FakeGateway.state = server.state  # type: ignore[attr-defined]
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    yield server
    server.shutdown()
    thread.join(timeout=5)


def run_script(gateway, token: str, tenants: str, bookings: str = "3"):
    env = dict(
        os.environ,
        GATEWAY=f"http://127.0.0.1:{gateway.server_address[1]}",
        TOKEN=token,
        DEMO_TENANTS=tenants,
        BOOKINGS_PER_TENANT=bookings,
        PACK_DIR=PACK_DIR,
    )
    return subprocess.run(
        ["bash", SCRIPT], env=env, capture_output=True, text=True, timeout=120
    )


# --------------------------------------------------------------------------
# Tests
# --------------------------------------------------------------------------
class TestFullRun:
    def test_four_tenants_fully_seeded(self, gateway):
        for slug in ALL_TENANTS:
            result = run_script(gateway, fake_jwt([slug]), slug)
            assert result.returncode == 0, result.stderr + result.stdout

        state = gateway.state  # type: ignore[attr-defined]
        assert sorted(state["tenants"]) == sorted(ALL_TENANTS)
        for slug in ALL_TENANTS:
            pack = slug.removeprefix("demo-")
            bucket = state["tenants"][slug]
            # industry + terminology from the pack
            assert bucket["industry"] == pack
            assert bucket["terminology"].get("team_member")
            # offerings == pack offerings count
            assert len(bucket["offerings"]) == pack_offerings_count(pack)
            # team member + Mon-Sat availability
            assert len(bucket["members"]) == 1
            rules = bucket["availability"]
            assert [r["weekday"] for r in rules] == [1, 2, 3, 4, 5, 6]
            assert rules[0]["start_min"] == 540 and rules[0]["end_min"] == 1020
            # site published
            assert bucket["site"]["published"] is True
            # knowledge doc
            assert len(bucket["docs"]) == 1
            # bookings: count, idempotency keys, source, REAL references
            bookings = bucket["bookings"]
            assert len(bookings) == 3
            offering_id = bucket["offerings"][0]["id"]
            member_id = bucket["members"][0]["id"]
            for i, booking in enumerate(bookings, start=1):
                assert booking["idempotency_key"] == f"demo-{slug}-{i}"
                assert booking["source"] == "demo-seed"
                assert booking["offering_id"] == offering_id
                assert booking["team_member_id"] == member_id
                assert booking["contact"]["name"] == f"Demo Customer {i}"
        # no cross-tenant writes: every mutating /v1 call landed on the
        # tenant whose token made it (the fake resolves tenant_slugs[0])
        for kind, tenant in state["calls"]:
            if kind in ("offerings", "team-members", "availability", "site"):
                assert tenant in ALL_TENANTS

    def test_rerun_is_idempotent(self, gateway):
        slug = "demo-clinic"
        first = run_script(gateway, fake_jwt([slug]), slug)
        assert first.returncode == 0, first.stderr + first.stdout
        second = run_script(gateway, fake_jwt([slug]), slug)
        assert second.returncode == 0, second.stderr + second.stdout
        bucket = gateway.state["tenants"][slug]  # type: ignore[attr-defined]
        assert len(bucket["offerings"]) == pack_offerings_count("clinic")
        assert len(bucket["members"]) == 1
        assert len(bucket["bookings"]) == 3  # idempotency keys dedup
        assert len(bucket["docs"]) == 1

    def test_wrong_first_claim_dies_without_writes(self, gateway):
        # token_slugs[0] != the tenant being seeded -> honest death
        result = run_script(
            gateway, fake_jwt(["demo-salon", "demo-clinic"]), "demo-clinic"
        )
        assert result.returncode != 0
        assert "tenant_slugs[0]" in (result.stderr + result.stdout)
        assert "demo-clinic" not in gateway.state["tenants"]  # type: ignore[attr-defined]

    def test_subset_seeds_exactly_one(self, gateway):
        result = run_script(gateway, fake_jwt(["demo-clinic"]), "demo-clinic")
        assert result.returncode == 0, result.stderr + result.stdout
        assert list(gateway.state["tenants"]) == ["demo-clinic"]  # type: ignore[attr-defined]

    def test_tenant_create_500_fails(self, gateway):
        gateway.state["fail_tenant_create"] = True  # type: ignore[attr-defined]
        result = run_script(gateway, fake_jwt(["demo-salon"]), "demo-salon")
        assert result.returncode != 0


class TestLint:
    def test_bash_syntax(self):
        result = subprocess.run(
            ["bash", "-n", SCRIPT], capture_output=True, text=True
        )
        assert result.returncode == 0, result.stderr

    def test_shellcheck(self):
        shellcheck = shutil.which("shellcheck")
        if shellcheck is None:
            pytest.skip("shellcheck not installed")
        result = subprocess.run(
            [shellcheck, "-S", "warning", SCRIPT],
            capture_output=True, text=True,
        )
        assert result.returncode == 0, result.stdout + result.stderr

    def test_pack_offerings_parser_counts(self):
        # the harness's independent count agrees with the packs on disk
        assert pack_offerings_count("salon") >= 2
        assert pack_offerings_count("clinic") >= 2
        assert pack_offerings_count("consultancy") >= 2
        assert pack_offerings_count("logistics") >= 2
