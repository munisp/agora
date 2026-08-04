#!/usr/bin/env python3
"""collision_guard.py — §8.7 #3 synthetic/real idspace collision guard (SPEC-W17, Agent D).

Hashes a small embedded dictionary of REAL-BVN-SHAPED values — 1,000
deterministic 11-digit strings in the Nigerian Bank Verification Number
shape (``22`` prefix, 11 digits) — through the SAME deterministic customer-id
path used by the seeders (contract A: ``deterministic_id`` from
``scripts/seeds/_lib.py``, sha256(SEED_SALT + "|" + natural_key) hex) and
asserts ZERO collisions against the seeded customer idspace.

Rationale: if a real person's BVN-shaped natural key can land on an id the
seeders already own, an erasure/anonymization run could hit a synthetic row
or vice versa. The guard makes that impossible to ship silently.

Seeded idspace contract (Agent B, via lead relay): customers are keyed
``deterministic_id(f"customer:{i:08d}")`` for ``i in [0, scaled(200000))``
and agents ``deterministic_id(f"agent:{i:06d}")`` for ``i in [0, scaled(5000))``.
The guard REGENERATES that exact idspace locally (DB-free, deterministic) as
the default comparison set; ``--idspace FILE`` or a reachable ``DATABASE_URL``
(live ``SELECT id FROM cac.customers``) can be used instead for an as-written
cross-check.

Checks:
  1. the 1,000 guard BVNs are unique,
  2. their deterministic ids are unique (self-collision check) — each BVN is
     hashed in both natural-key shapes a real subject could arrive in
     (``<bvn>`` bare and ``bvn:<bvn>`` prefixed),
  3. guard ids ∩ seeded (customer ∪ agent) ids == ∅.

Exit: 0 pass, 1 collision/consistency failure, 2 usage error. The JSON
summary line on stdout is the CI artifact; diagnostics go to stderr.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import sys
from pathlib import Path

SEEDS_DIR = Path(__file__).resolve().parent
sys.path.insert(0, str(SEEDS_DIR))  # contract A: scripts/seeds/_lib.py

GUARD_COUNT = 1000
GUARD_SALT = "opendesk-collision-guard-v1"  # fixed; guard dictionary must be stable
CUSTOMER_CARDINALITY = 200_000  # SPEC-W17 Agent B (pre-scale)
AGENT_CARDINALITY = 5_000       # SPEC-W17 Agent B (pre-scale)


def _load_lib():
    """The SAME deterministic-id path as the seeders (contract A). Falls back
    to the contract formula inline — with a loud warning — only when _lib is
    not importable (cross-agent build order)."""
    try:
        import _lib  # type: ignore

        return _lib.deterministic_id, _lib.scaled, "scripts/seeds/_lib.py"
    except Exception as exc:  # noqa: BLE001 — any import failure falls back
        salt = os.environ.get("SEED_SALT", "opendesk-dev-seed-salt-change-in-prod")
        print(
            f"[collision_guard] WARNING: _lib import failed ({exc!r}); using "
            "inline contract-A formula sha256(SEED_SALT|key) — identical only "
            "if _lib implements contract A exactly",
            file=sys.stderr,
        )

        def deterministic_id(natural_key: str) -> str:
            return hashlib.sha256(f"{salt}|{natural_key}".encode("utf-8")).hexdigest()

        def scaled(cardinality: int) -> int:
            return int(cardinality * float(os.environ.get("SEED_SCALE", "1.0")))

        return deterministic_id, scaled, "inline-fallback(contract A)"


def guard_bvns(count: int = GUARD_COUNT) -> list[str]:
    """The embedded real-BVN-shaped dictionary: `count` deterministic 11-digit
    strings with the common BVN ``22`` prefix, derived from a fixed salt so the
    dictionary is stable across runs and machines (no RNG state, no I/O)."""
    out: list[str] = []
    i = 0
    while len(out) < count:
        digest = hashlib.sha256(f"{GUARD_SALT}|{i}".encode("utf-8")).hexdigest()
        digits = "".join(str(int(c, 16)) for c in digest)  # hex -> decimal digits
        candidate = "22" + digits[:9]
        if candidate not in out:  # paranoid uniqueness; sha256 makes dupes ~impossible
            out.append(candidate)
        i += 1
    return out


def seeded_idspace(deterministic_id, scaled) -> tuple[set[str], str]:
    """Regenerate the seeded customer+agent idspace from the Agent-B natural-key
    contract — deterministic, DB-free, always available."""
    ids: set[str] = set()
    n_customers = scaled(CUSTOMER_CARDINALITY)
    n_agents = scaled(AGENT_CARDINALITY)
    for i in range(n_customers):
        ids.add(deterministic_id(f"customer:{i:08d}"))
    for i in range(n_agents):
        ids.add(deterministic_id(f"agent:{i:06d}"))
    return ids, f"regenerated(customer:{n_customers},agent:{n_agents})"


def load_idspace_file_db(path: str | None) -> tuple[set[str] | None, str]:
    """Optional as-written idspace: --idspace file, else live DATABASE_URL."""
    if path:
        p = Path(path)
        if not p.is_file():
            raise FileNotFoundError(f"--idspace file not found: {path}")
        ids = {ln.strip() for ln in p.read_text(encoding="utf-8").splitlines() if ln.strip()}
        return ids, f"file:{path}"
    if os.environ.get("DATABASE_URL"):
        try:
            import _lib  # type: ignore

            conn = _lib.get_conn()
            try:
                cur = conn.cursor()
                cur.execute("SELECT id FROM cac.customers")
                rows = cur.fetchall()
            finally:
                close = getattr(conn, "close", None)
                if callable(close):
                    close()
            return {r[0] for r in rows}, "db:cac.customers"
        except Exception as exc:  # noqa: BLE001
            print(
                f"[collision_guard] WARNING: DATABASE_URL set but idspace query "
                f"failed ({exc!r}); falling back to regenerated idspace",
                file=sys.stderr,
            )
    return None, "unavailable"


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="§8.7 #3 collision guard: real-BVN-shaped guard dictionary vs seeded customer idspace")
    ap.add_argument("--idspace", help="file with one seeded customer id per line (default: regenerate the contract idspace; DATABASE_URL used when set)")
    ap.add_argument("--expect", type=int, default=GUARD_COUNT, help="guard dictionary size (default 1000)")
    args = ap.parse_args(argv)

    deterministic_id, scaled, id_path = _load_lib()
    bvns = guard_bvns(args.expect)

    failures: list[str] = []
    # 1. guard dictionary uniqueness
    if len(set(bvns)) != len(bvns):
        failures.append(f"guard BVN dictionary not unique: {len(set(bvns))}/{len(bvns)}")

    # 2. deterministic-id self-collision (both natural-key shapes a real
    #    BVN-keyed subject could take through the customer-id path)
    guard_ids: set[str] = set()
    for b in bvns:
        guard_ids.add(deterministic_id(b))
        guard_ids.add(deterministic_id(f"bvn:{b}"))
    if len(guard_ids) != 2 * len(bvns):
        failures.append(f"self-collision in guard ids: {len(guard_ids)}/{2 * len(bvns)} unique")

    # 3. cross-check vs the seeded idspace: explicit --idspace/DATABASE_URL
    #    when given, else the regenerated contract idspace (DB-free default).
    idspace, source = load_idspace_file_db(args.idspace)
    if idspace is None:
        idspace, source = seeded_idspace(deterministic_id, scaled)
    collisions = sorted(guard_ids & idspace)
    if collisions:
        failures.append(f"{len(collisions)} guard ids collide with seeded ids ({source})")

    summary = {
        "gate": "collision_guard(§8.7 #3)",
        "guard_bvns": len(bvns),
        "guard_ids_unique": len(guard_ids),
        "id_path": id_path,
        "idspace_source": source,
        "idspace_size": len(idspace),
        "collisions": len(collisions),
        "collision_examples": collisions[:5],
        "status": "FAIL" if failures else "PASS",
    }
    print(json.dumps(summary, sort_keys=True))
    for f in failures:
        print(f"[collision_guard] FAIL: {f}", file=sys.stderr)
    return 1 if failures else 0


if __name__ == "__main__":
    raise SystemExit(main())
