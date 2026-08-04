#!/usr/bin/env python3
"""seed_agents.py — synthetic field/sales agents into ``cac.agents`` (SPEC-W17, Agent B).

Cardinality: ``scaled(5000)`` rows (``SEED_SCALE`` env / ``--scale`` override).

Loader idiom (contract B, via _lib): deterministic id from natural key
``agent:<i:06d>`` → ``delete_by_ids`` → ``upsert_rows`` (INSERT ... ON
CONFLICT (id) DO UPDATE) → ``log_seed_run`` → commit → ``emit_seed_report``.
Non-zero exit on any failure (fail loud). ``--dry-run`` prints counts and the
manifest summary with no DB connection and no writes.

Also emits ``scripts/seeds/out/tigerbeetle_accounts.json`` — one synthetic
TigerBeetle account per agent (contract C / spec §8.8):

  - account_type = 90            -- synthetic seed accounts (spec §8.8)
  - code         = 302           -- AccountAgentFloat (W14 chart of accounts in
    services/booking-service/internal/referrals/model.go: 300 commission_
    payable, 301 commission_expense, 302 agent_float, 303 house_clearing) —
    the per-agent account a real payout would settle against; documents the
    TB adapter seam in referrals/ledger.go ("beneficiary sub-ledgers use TB
    user_data_128 = beneficiary id hash").
  - ledger       = 1             -- NGN ledger id (documented assumption; TB
                                   ledger ids are deployment-scoped)
  - id           = uint128, first 16 bytes of the deterministic agent id
                   (big-endian) — stable across re-seeds
  - user_data_128 = the full deterministic agent id (hex)
  - flags        = 0
  The manifest is a config artifact only — no live TB cluster here (spec §6).

PII posture (contract A/C): Nigerian names and +23480[0-9]XXXXXXX-style phones
are generated ONLY to be hashed; rows store ``hash_pii`` digests (non-stable
across runs by design) + ``is_synthetic=true`` + ``seeded_at``. Raw PII never
persists. Ids and LGA/state assignment ARE stable, so reseeds converge.

State/LGA assignment: real runs read ``cac.lgas``; DB-free dry-runs read
``data/nigeria_lgas.csv`` (Agent A) and fall back to an embedded 37-entry
(36 states + FCT) reference set. Zone-weighted (ZONE_WEIGHT — documented
ASSUMPTION, no offline census). LGA natural key matches Agent A:
``lga:<state>:<lga_name>``.

Substitutions documented per SPEC-W17 header: mimesis (MIT) is the only
generator stack; Nigerian realism comes from the embedded en_NG-style lists.
"""

from __future__ import annotations

import csv
import json
import random
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import _lib  # noqa: E402  (contract A lib, owned by Agent A)

TABLE = "cac.agents"
CARDINALITY = 5_000
COLUMNS = ["id", "name_hash", "phone_hash", "state", "lga_id", "active"]
DATA_CSV = Path(__file__).resolve().parent / "data" / "nigeria_lgas.csv"
MANIFEST_PATH = Path(__file__).resolve().parent / "out" / "tigerbeetle_accounts.json"

# TB manifest constants (spec §8.8 + W14 seam — see module docstring).
TB_ACCOUNT_TYPE_SYNTHETIC = 90
TB_CODE_AGENT_FLOAT = 302
TB_LEDGER_NGN = 1

# ---------------------------------------------------------------------------
# Embedded en_NG-style data lists (mimesis-only stack — SPEC-W17 header §3).
# ---------------------------------------------------------------------------
FIRST_NAMES = [
    # Yoruba
    "Adewale", "Adebayo", "Adekunle", "Olufemi", "Olumide", "Yetunde",
    "Funmilayo", "Temitope", "Oluwaseun", "Damilola",
    # Igbo
    "Chinedu", "Chukwuemeka", "Ngozi", "Adaeze", "Obinna", "Chiamaka",
    "Ikenna", "Uchechi", "Somtochi", "Ebubechukwu",
    # Hausa / northern
    "Aminu", "Ibrahim", "Fatima", "Aisha", "Musa", "Zainab", "Suleiman",
    "Halima", "Usman", "Khadija",
    # Cross-cutting
    "Blessing", "Chinonso", "Osas", "Efe", "Nneka", "Tunde",
]
LAST_NAMES = [
    "Okafor", "Adeyemi", "Abubakar", "Nwachukwu", "Ogunleye", "Eze",
    "Danjuma", "Olawale", "Obi", "Adeleke", "Igwe", "Yakubu", "Oladipo",
    "Chukwu", "Garba", "Balogun", "Okonkwo", "Akinola", "Mohammed",
    "Osei", "Nnaji", "Oyekan", "Sadiq", "Umeh", "Fashola", "Aliyu",
    "Ojo", "Nwosu", "Bello", "Ajayi",
]

# Minimal fallback LGA reference set (one well-known LGA per state + FCT) so
# --dry-run works even without data/nigeria_lgas.csv. Real (non-dry-run)
# seeds ALWAYS read cac.lgas from the database.
FALLBACK_LGAS: tuple[tuple[str, str, str], ...] = tuple(
    (state, lga, zone)
    for state, lga, zone in [
        ("Abia", "Aba North", "South East"), ("Adamawa", "Yola North", "North East"),
        ("Akwa Ibom", "Uyo", "South South"), ("Anambra", "Awka South", "South East"),
        ("Bauchi", "Bauchi", "North East"), ("Bayelsa", "Yenagoa", "South South"),
        ("Benue", "Makurdi", "North Central"), ("Borno", "Maiduguri", "North East"),
        ("Cross River", "Calabar Municipal", "South South"), ("Delta", "Warri South", "South South"),
        ("Ebonyi", "Abakaliki", "South East"), ("Edo", "Oredo", "South South"),
        ("Ekiti", "Ado Ekiti", "South West"), ("Enugu", "Enugu North", "South East"),
        ("FCT", "Abuja Municipal", "North Central"), ("Gombe", "Gombe", "North East"),
        ("Imo", "Owerri Municipal", "South East"), ("Jigawa", "Dutse", "North West"),
        ("Kaduna", "Kaduna North", "North West"), ("Kano", "Kano Municipal", "North West"),
        ("Katsina", "Katsina", "North West"), ("Kebbi", "Birnin Kebbi", "North West"),
        ("Kogi", "Lokoja", "North Central"), ("Kwara", "Ilorin West", "North Central"),
        ("Lagos", "Ikeja", "South West"), ("Nasarawa", "Lafia", "North Central"),
        ("Niger", "Minna", "North Central"), ("Ogun", "Abeokuta South", "South West"),
        ("Ondo", "Akure South", "South West"), ("Osun", "Osogbo", "South West"),
        ("Oyo", "Ibadan North", "South West"), ("Plateau", "Jos North", "North Central"),
        ("Rivers", "Port Harcourt", "South South"), ("Sokoto", "Sokoto North", "North West"),
        ("Taraba", "Jalingo", "North East"), ("Yobe", "Damaturu", "North East"),
        ("Zamfara", "Gusau", "North West"),
    ]
)

# Population-ish assignment weights per geopolitical zone (ASSUMPTION — no
# offline census; documented per spec §8.2). Lagos-heavy South West and the
# northern metros carry more agents, matching field-force reality.
ZONE_WEIGHT = {
    "North West": 1.25, "North East": 0.9, "North Central": 1.0,
    "South West": 1.6, "South East": 0.85, "South South": 0.9,
}


def seeded_rng(*parts: object) -> random.Random:
    """Deterministic per-entity RNG — reproducible generation ⇒ idempotent upserts."""
    import hashlib

    material = "|".join(str(p) for p in parts)
    seed = int.from_bytes(hashlib.sha256(material.encode()).digest()[:8], "big")
    return random.Random(seed)


def natural_key(index: int) -> str:
    """Agent natural key — COORDINATION: Agent D's collision guard reuses this."""
    return f"agent:{index:06d}"


def make_phone(rng: random.Random) -> str:
    """+23480[0-9]XXXXXXX-style synthetic mobile (spec §Agent B)."""
    return f"+23480{rng.randrange(10)}{rng.randrange(10**6, 10**7)}"


def lga_ref(state: str, lga_name: str, zone: str) -> dict[str, str]:
    # Natural key identical to Agent A's seed_lgas.build_rows.
    return {
        "id": _lib.deterministic_id(f"lga:{state}:{lga_name}"),
        "lga_name": lga_name,
        "state": state,
        "zone": zone,
    }


def resolve_lgas(conn: object | None, csv_path: Path = DATA_CSV) -> list[dict[str, str]]:
    """LGA reference set: cac.lgas (DB) → Agent A's CSV → embedded fallback."""
    if conn is not None:
        cur = conn.cursor()
        cur.execute("SELECT id, lga_name, state, zone FROM cac.lgas ORDER BY id")
        rows = [
            {"id": str(r[0]), "lga_name": str(r[1]), "state": str(r[2]), "zone": str(r[3])}
            for r in cur.fetchall()
        ]
        if not rows:
            raise RuntimeError("cac.lgas is empty — run seed_lgas.py first (bootstrap step 3)")
        return rows
    if csv_path.exists():
        rows = [
            lga_ref(rec["state"].strip(), rec["lga_name"].strip(), rec["zone"].strip())
            for rec in csv.DictReader(csv_path.open(newline="", encoding="utf-8"))
        ]
        if rows:
            return rows
    return [lga_ref(state, lga, zone) for state, lga, zone in FALLBACK_LGAS]


def weighted_pick(rng: random.Random, items: list, weights: list[float]):
    total = sum(weights)
    pick = rng.random() * total
    acc = 0.0
    for item, w in zip(items, weights):
        acc += w
        if pick <= acc:
            return item
    return items[-1]


def build_rows(count: int, lgas: list[dict[str, str]]) -> list[dict[str, object]]:
    """Pure builder: ``count`` deterministic synthetic agent rows (contract B)."""
    if not lgas:
        raise ValueError("lgas reference set is empty")
    weights = [ZONE_WEIGHT.get(str(l["zone"]), 1.0) for l in lgas]
    rows: list[dict[str, object]] = []
    for i in range(count):
        rng = seeded_rng("agent", i)
        full_name = f"{FIRST_NAMES[rng.randrange(len(FIRST_NAMES))]} {LAST_NAMES[rng.randrange(len(LAST_NAMES))]}"
        phone = make_phone(rng)
        lga = weighted_pick(rng, lgas, weights)
        rows.append(
            {
                "id": _lib.deterministic_id(natural_key(i)),
                "name_hash": _lib.hash_pii(full_name),
                "phone_hash": _lib.hash_pii(phone),
                "state": str(lga["state"]),
                "lga_id": str(lga["id"]),
                "active": True,
            }
        )
    return rows


def build_tb_manifest(rows: list[dict[str, object]], scale: float) -> dict[str, object]:
    """One synthetic TigerBeetle account per agent (spec §8.8, account_type=90)."""
    accounts = []
    for r in rows:
        digest = bytes.fromhex(str(r["id"]))
        accounts.append(
            {
                "id": int.from_bytes(digest[:16], "big"),
                "ledger": TB_LEDGER_NGN,
                "code": TB_CODE_AGENT_FLOAT,
                "account_type": TB_ACCOUNT_TYPE_SYNTHETIC,
                "flags": 0,
                "user_data_128": str(r["id"]),
                "agent_ref": str(r["id"]),
                "is_synthetic": True,
            }
        )
    return {
        "manifest_version": 1,
        "kind": "tigerbeetle_accounts",
        "generated_by": "scripts/seeds/seed_agents.py",
        "seed_scale": scale,
        "account_type": TB_ACCOUNT_TYPE_SYNTHETIC,
        "code_meanings": {
            "90": "synthetic seed account (spec §8.8)",
            "302": "agent_float — W14 commission ledger AccountAgentFloat",
        },
        "accounts": accounts,
    }


def write_tb_manifest(manifest: dict[str, object], path: Path = MANIFEST_PATH) -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    return path


def main(argv: list[str] | None = None) -> int:
    args = _lib.seed_argparser("Seed cac.agents (synthetic field agents) + TigerBeetle manifest").parse_args(argv)
    scale = _lib.apply_scale_arg(args.scale)
    count = int(CARDINALITY * scale)

    conn = None
    if not args.dry_run:
        try:
            conn = _lib.get_conn()
        except Exception as exc:  # noqa: BLE001 - fail loud
            print(f"[seed_agents] FAILED: {exc}", file=sys.stderr)
            return 1
    lgas = resolve_lgas(conn)
    rows = build_rows(count, lgas)
    manifest = build_tb_manifest(rows, scale)
    print(f"[seed_agents] rows={len(rows)} (scale={scale}, LGAs={len(lgas)}); tb_accounts={len(manifest['accounts'])}")
    if args.dry_run:
        print(f"[seed_agents] dry-run: no DB writes; manifest would land at {MANIFEST_PATH}")
        return 0
    try:
        _lib.delete_by_ids(conn, TABLE, [str(r["id"]) for r in rows])
        _lib.upsert_rows(conn, TABLE, COLUMNS, rows)
        _lib.log_seed_run(TABLE, len(rows), conn)
        _lib.commit(conn)
        write_tb_manifest(manifest)
    except Exception as exc:  # noqa: BLE001 - fail loud, non-zero exit
        print(f"[seed_agents] FAILED: {exc}", file=sys.stderr)
        return 1
    _lib.emit_seed_report(TABLE, len(rows), _lib.runner_id(), _lib.git_sha())
    print(f"[seed_agents] seeded {len(rows)} rows into {TABLE}; TB manifest -> {MANIFEST_PATH}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
