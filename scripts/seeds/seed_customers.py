#!/usr/bin/env python3
"""seed_customers.py — synthetic customers into ``cac.customers`` (SPEC-W17, Agent B).

Cardinality: ``scaled(200_000)`` rows (``SEED_SCALE`` env / ``--scale``).

Loader idiom (contract B, via _lib): deterministic id from natural key
``customer:<i:08d>`` (COORDINATION: Agent D's collision_guard hashes its
BVN-shaped dictionary through THIS SAME path) → ``delete_by_ids`` →
``upsert_rows`` → ``log_seed_run`` → commit → ``emit_seed_report``. Fail loud
(non-zero exit) on any exception. ``--dry-run`` prints counts, no DB/writes.

Schema conformance (Agent A's contract-D DDL is authoritative):
  cac.customers(id, name_hash, phone_hash, channel_id -> cac.channels.id,
                lga_id -> cac.lgas.id, acquired_on, is_synthetic, seeded_at)
  channel_of_first_touch (spec §Agent B) is stored as ``channel_id`` — the
  deterministic channel id (``channel:<channel_code>``) of one of Agent A's
  32 channels. NOTE/FLAGGED: the DDL has no preferred_language/notes columns,
  so the embedded Pidgin (pcm) seed strings + language weighting below drive
  GENERATION (name-list selection per zone language mix) and the dry-run
  sample, but are not persisted. If persistence is wanted, Agent A owns the
  DDL change.

Generation stack (SPEC-W17 header §3): mimesis only — Nigerian realism via
the embedded en_NG-style name lists + Pidgin strings (no faker, no network).
Per-customer RNG is seeded from the customer index, so rows are reproducible
and reseeds converge (ids stable; hash_pii digests are per-run by design).

Distribution: channels weighted toward the Nigerian field mix (agent/POS/USSD
heavy — CHANNEL_WEIGHT); LGAs zone-weighted like seed_agents (documented
ASSUMPTION, no offline census). ``acquired_on`` spreads over the trailing 24
months so W13 funnel analytics have a realistic time base.

PII: name and +23480[0-9]XXXXXXX-style phone are generated ONLY to be hashed
(contract A); raw PII never persists.
"""

from __future__ import annotations

import sys
from datetime import date, timedelta
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import _lib  # noqa: E402  (contract A lib, owned by Agent A)
import seed_agents  # noqa: E402  (LGA resolution, zone weights, RNG/phone helpers)
import seed_channels  # noqa: E402  (Agent A — canonical 32 channel codes)

TABLE = "cac.customers"
CARDINALITY = 200_000
COLUMNS = ["id", "name_hash", "phone_hash", "channel_id", "lga_id", "acquired_on"]

FIRST_NAMES = seed_agents.FIRST_NAMES
LAST_NAMES = seed_agents.LAST_NAMES

# Pidgin (pcm) seed strings — embedded per SPEC-W17 header §3. They drive the
# pcm branch of the language mix below (pcm customers draw from the full
# cross-cutting name lists, matching Pidgin's linguistically-mixed speakers)
# and surface in the --dry-run sample. Not persisted (see docstring FLAG).
PIDGIN_NOTES = [
    "Customer say make we call am for evening time.",
    "E like the USSD option well well.",
    "Wetin dey sup: na agent refer am for market.",
    "Customer dey interested, but network no too good for im area.",
    "Abeg call am back tomorrow morning.",
    "E don try the app small, e wan make person explain am better.",
    "Na WhatsApp e take first hear about us.",
    "Customer talk say price dey okay for am.",
]

LANGUAGES = ("en", "pcm", "ha", "yo", "ig")
# Language mix per geopolitical zone (en + pcm everywhere; ha north, yo/ig south).
LANG_WEIGHT = {
    "North West": {"en": 0.15, "pcm": 0.15, "ha": 0.65, "yo": 0.02, "ig": 0.03},
    "North East": {"en": 0.15, "pcm": 0.15, "ha": 0.62, "yo": 0.03, "ig": 0.05},
    "North Central": {"en": 0.30, "pcm": 0.30, "ha": 0.28, "yo": 0.06, "ig": 0.06},
    "South West": {"en": 0.30, "pcm": 0.30, "ha": 0.02, "yo": 0.36, "ig": 0.02},
    "South East": {"en": 0.30, "pcm": 0.30, "ha": 0.02, "yo": 0.02, "ig": 0.36},
    "South South": {"en": 0.32, "pcm": 0.45, "ha": 0.02, "yo": 0.03, "ig": 0.18},
}

# Name-list routing per language: ha → northern block, yo → Yoruba block,
# ig → Igbo block, en/pcm → full list (pcm = Pidgin, linguistically mixed).
_NAME_BLOCKS = {
    "ha": FIRST_NAMES[20:30],
    "yo": FIRST_NAMES[0:10],
    "ig": FIRST_NAMES[10:20],
}

# Relative first-touch weights per channel_code (Agent A's slugs; field
# channels dominate, matching Nigerian agent-led acquisition). Codes absent
# here fall back to weight 1.0.
CHANNEL_WEIGHT = {
    "agent-network": 14.0, "pos-agents": 12.0, "ussd": 10.0,
    "field-reps-door-to-door": 8.0, "referral-program": 7.0,
    "whatsapp-business": 6.0, "market-associations": 5.0,
    "cooperatives": 4.0, "churches": 3.5, "mosques": 3.5, "sms": 3.0,
    "radio-network-fm": 2.5, "town-hall-meetings": 2.5, "voice-ivr": 2.0,
    "social-media-paid": 1.8, "online-display": 1.5, "roadshows-activations": 1.5,
    "community-events": 1.2, "keke-okada-networks": 1.2, "telemarketing-outbound": 1.0,
    "radio-hausa-fm": 1.0, "radio-yoruba-fm": 0.9, "radio-igbo-fm": 0.9,
    "micro-influencers": 0.8, "campus-ambassadors": 0.6, "nysc-cds-groups": 0.6,
    "tv-national": 0.5, "tv-regional": 0.5, "trade-fairs-expos": 0.5,
    "newspaper-national": 0.4, "billboard-outdoor": 0.4, "transit-branding": 0.4,
}

# Registration window: customers first seen over the trailing 24 months.
ACQUIRED_WINDOW_DAYS = 730


def natural_key(index: int) -> str:
    """Customer natural key — COORDINATION: Agent D's collision guard reuses this."""
    return f"customer:{index:08d}"


def channel_code_to_id(code: str) -> str:
    """channel_id FK — natural key identical to Agent A's seed_channels."""
    return _lib.deterministic_id(f"channel:{code}")


def resolve_channel_codes(conn: object | None) -> list[str]:
    """Channel codes: cac.channels (DB) → Agent A's embedded 32-code list."""
    if conn is not None:
        cur = conn.cursor()
        cur.execute("SELECT channel_code FROM cac.channels ORDER BY channel_code")
        codes = [str(r[0]) for r in cur.fetchall()]
        if not codes:
            raise RuntimeError("cac.channels is empty — run seed_channels.py first (bootstrap step 4)")
        return codes
    return [code for code, *_ in seed_channels.CHANNELS]


def build_rows(
    count: int,
    lgas: list[dict[str, str]],
    channel_codes: list[str],
    today: date | None = None,
) -> list[dict[str, object]]:
    """Pure builder: ``count`` deterministic synthetic customer rows (contract B)."""
    if not lgas:
        raise ValueError("lgas reference set is empty")
    if not channel_codes:
        raise ValueError("channel reference set is empty")
    today = today or date.today()
    lga_weights = [seed_agents.ZONE_WEIGHT.get(str(l["zone"]), 1.0) for l in lgas]
    chan_weights = [CHANNEL_WEIGHT.get(c, 1.0) for c in channel_codes]

    rows: list[dict[str, object]] = []
    for i in range(count):
        rng = seed_agents.seeded_rng("customer", i)
        lga = seed_agents.weighted_pick(rng, lgas, lga_weights)
        code = seed_agents.weighted_pick(rng, channel_codes, chan_weights)
        lang_mix = LANG_WEIGHT.get(str(lga.get("zone") or ""), LANG_WEIGHT["North Central"])
        lang = seed_agents.weighted_pick(rng, list(LANGUAGES), [lang_mix[l] for l in LANGUAGES])
        first_pool = _NAME_BLOCKS.get(lang, FIRST_NAMES)
        full_name = f"{first_pool[rng.randrange(len(first_pool))]} {LAST_NAMES[rng.randrange(len(LAST_NAMES))]}"
        phone = seed_agents.make_phone(rng)
        acquired_on = today - timedelta(days=int(rng.random() * ACQUIRED_WINDOW_DAYS))
        rows.append(
            {
                "id": _lib.deterministic_id(natural_key(i)),
                "name_hash": _lib.hash_pii(full_name),
                "phone_hash": _lib.hash_pii(phone),
                "channel_id": channel_code_to_id(code),
                "lga_id": str(lga["id"]),
                "acquired_on": acquired_on,
            }
        )
    return rows


def main(argv: list[str] | None = None) -> int:
    args = _lib.seed_argparser("Seed cac.customers (synthetic customers)").parse_args(argv)
    scale = _lib.apply_scale_arg(args.scale)
    count = int(CARDINALITY * scale)

    conn = None
    if not args.dry_run:
        try:
            conn = _lib.get_conn()
        except Exception as exc:  # noqa: BLE001 - fail loud
            print(f"[seed_customers] FAILED: {exc}", file=sys.stderr)
            return 1
    lgas = seed_agents.resolve_lgas(conn)
    codes = resolve_channel_codes(conn)
    rows = build_rows(count, lgas, codes)
    print(
        f"[seed_customers] rows={len(rows)} (scale={scale}, LGAs={len(lgas)}, channels={len(codes)}); "
        f"pidgin-sample: {PIDGIN_NOTES[0]!r}"
    )
    if args.dry_run:
        print("[seed_customers] dry-run: no DB writes")
        return 0
    try:
        _lib.delete_by_ids(conn, TABLE, [str(r["id"]) for r in rows])
        _lib.upsert_rows(conn, TABLE, COLUMNS, rows)
        _lib.log_seed_run(TABLE, len(rows), conn)
        _lib.commit(conn)
    except Exception as exc:  # noqa: BLE001 - fail loud, non-zero exit
        print(f"[seed_customers] FAILED: {exc}", file=sys.stderr)
        return 1
    _lib.emit_seed_report(TABLE, len(rows), _lib.runner_id(), _lib.git_sha())
    print(f"[seed_customers] seeded {len(rows)} rows into {TABLE}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
