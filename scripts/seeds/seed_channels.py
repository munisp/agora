#!/usr/bin/env python3
"""seed_channels.py — 32 hand-curated Nigerian acquisition channels (SPEC-W17).

From the CAC playbook: USSD, SMS, WhatsApp Business, voice/IVR, radio, agent
networks, POS agents, cooperatives, churches/mosques, market associations,
etc. Each row carries channel_code, name, class (above/below-the-line) and
typical unit economics (unit_desc + base_cost_ngn anchor used by
seed_channel_costs.py). Cardinality is FIXED at 32 (reference data; --scale
accepted per contract B but is a no-op).

--dry-run prints counts, no DB. Non-zero exit on any failure (contract B).
"""

from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import _lib  # noqa: E402

TABLE = "cac.channels"
COLUMNS = ["id", "channel_code", "name", "channel_class", "unit_desc", "base_cost_ngn", "notes"]
ATL = "above-the-line"
BTL = "below-the-line"

# (channel_code, name, class, unit_desc, base_cost_ngn, notes)
CHANNELS: list[tuple[str, str, str, str, float, str]] = [
    # --- Above-the-line: mass media -------------------------------------
    ("radio-network-fm", "Network FM Radio (Pidgin/English)", ATL, "per 30s primetime slot", 250000.00, "National reach; strongest mass channel for fintech/consumer"),
    ("radio-hausa-fm", "Hausa-language FM Radio", ATL, "per 30s primetime slot", 120000.00, "Northern belt; pairs with Hausa i18n pack blocks"),
    ("radio-yoruba-fm", "Yoruba-language FM Radio", ATL, "per 30s primetime slot", 110000.00, "South West reach"),
    ("radio-igbo-fm", "Igbo-language FM Radio", ATL, "per 30s primetime slot", 100000.00, "South East reach"),
    ("tv-national", "National TV (terrestrial + satellite)", ATL, "per 30s primetime spot", 800000.00, "High cost, brand-building; poor direct attribution"),
    ("tv-regional", "Regional TV stations", ATL, "per 30s primetime spot", 250000.00, "State-level bursts around activations"),
    ("newspaper-national", "National newspaper print", ATL, "per half-page insertion", 600000.00, "Declining reach; credibility for B2B/government packs"),
    ("billboard-outdoor", "Billboards & outdoor (OOH)", ATL, "per board per month", 1500000.00, "Lagos/Abuja/Kano corridors; awareness only"),
    ("transit-branding", "Transit branding (danfo/BRT/keke)", ATL, "per vehicle per month", 85000.00, "High-frequency commuter exposure"),
    ("online-display", "Programmatic online display", ATL, "per 1,000 impressions (CPM)", 1800.00, "Attributed via UTM; feeds cac.events impressions"),
    ("social-media-paid", "Paid social (Meta/Instagram/X)", ATL, "per 1,000 impressions (CPM)", 1500.00, "Primary digital scale channel"),
    # --- Below-the-line: direct / community ------------------------------
    ("ussd", "USSD self-service funnel", BTL, "per completed session", 25.00, "Feature-phone first; works without data"),
    ("sms", "SMS campaigns", BTL, "per SMS delivered", 4.50, "Termii/Africa's Talking routes; see messaging-gateway"),
    ("whatsapp-business", "WhatsApp Business API", BTL, "per conversation (24h window)", 35.00, "Highest engagement of digital channels"),
    ("voice-ivr", "Outbound voice / IVR", BTL, "per connected minute", 18.00, "LiveKit voice runtime; vernacular prompts"),
    ("agent-network", "Field agent network", BTL, "per activated signup", 1500.00, "Commissioned agents; see cac.agents"),
    ("pos-agents", "POS / agent-banking kiosks", BTL, "per referred signup", 1200.00, "Leverage existing POS footprint"),
    ("cooperatives", "Cooperative societies (ajo/esusu)", BTL, "per member signup", 800.00, "Trusted savings groups; agritech playbook channel"),
    ("churches", "Church partnerships", BTL, "per activation event", 300000.00, "Sunday announcements + registration desks"),
    ("mosques", "Mosque partnerships", BTL, "per activation event", 250000.00, "Jummah announcements; northern deployments"),
    ("market-associations", "Market association partnerships", BTL, "per market activation", 400000.00, "e.g. Balogun/Onitsha main market unions"),
    ("town-hall-meetings", "Town hall / community meetings", BTL, "per meeting", 150000.00, "Traditional ruler endorsement critical"),
    ("roadshows-activations", "Roadshows & street activations", BTL, "per activation day", 600000.00, "Branded bus + agents + instant signup"),
    ("referral-program", "Customer referral program", BTL, "per successful referral", 1000.00, "Bounty per pack growth.referral_bounty_ngn"),
    ("field-reps-door-to-door", "Door-to-door field reps", BTL, "per converted household", 2000.00, "Salaried reps; PAYG/utilities playbook"),
    ("community-events", "Community & town-union events", BTL, "per event sponsorship", 350000.00, "New-yam festivals, durbar, etc."),
    ("trade-fairs-expos", "Trade fairs & expos", BTL, "per booth per event", 900000.00, "B2B packs; Lagos Trade Fair"),
    ("campus-ambassadors", "Campus ambassador program", BTL, "per ambassador per month", 50000.00, "Edtech/youth packs"),
    ("keke-okada-networks", "Keke/okada rider networks", BTL, "per rider signup", 900.00, "Rider unions as distribution + audience"),
    ("telemarketing-outbound", "Outbound telemarketing", BTL, "per connected call", 120.00, "B2B-SME lists; DNC-scrubbed"),
    ("micro-influencers", "Community micro-influencers", BTL, "per sponsored post", 75000.00, "Pidgin/vernacular creators; strong Gen-Z reach"),
    ("nysc-cds-groups", "NYSC CDS group partnerships", BTL, "per CDS session", 100000.00, "Corps members as evangelists"),
]

EXPECTED_CHANNELS = 32


def build_rows(scale: float = 1.0) -> list[dict[str, object]]:
    """Pure + deterministic. scale ignored: reference cardinality is fixed."""
    return [
        {
            "id": _lib.deterministic_id(f"channel:{code}"),
            "channel_code": code,
            "name": name,
            "channel_class": klass,
            "unit_desc": unit,
            "base_cost_ngn": cost,
            "notes": notes,
        }
        for code, name, klass, unit, cost, notes in CHANNELS
    ]


def main(argv: list[str] | None = None) -> int:
    args = _lib.seed_argparser("Seed cac.channels (32 Nigerian acquisition channels)").parse_args(argv)
    scale = _lib.apply_scale_arg(args.scale)
    rows = build_rows(scale)
    print(f"[seed_channels] rows={len(rows)} (expected {EXPECTED_CHANNELS})")
    if args.dry_run:
        print("[seed_channels] dry-run: no DB writes")
        return 0
    try:
        conn = _lib.get_conn()
        _lib.delete_by_ids(conn, TABLE, [str(r["id"]) for r in rows])
        _lib.upsert_rows(conn, TABLE, COLUMNS, rows)
        _lib.log_seed_run(TABLE, len(rows), conn)
        _lib.commit(conn)
    except Exception as exc:  # noqa: BLE001 - fail loud, non-zero exit
        print(f"[seed_channels] FAILED: {exc}", file=sys.stderr)
        return 1
    _lib.emit_seed_report(TABLE, len(rows), _lib.runner_id(), _lib.git_sha())
    print(f"[seed_channels] seeded {len(rows)} rows into {TABLE}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
