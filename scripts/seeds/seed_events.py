#!/usr/bin/env python3
"""seed_events.py — synthetic funnel events onto ``cac.events`` (SPEC-W17, Agent B, contract E).

Emits W13-shaped FunnelEvents (CloudEvents 1.0, type
``com.opendesk.cac.FunnelEvent`` — the exact envelope consumed by
services/analytics-pipeline/analytics_pipeline/cac_events.py, whose real
parser also validates this wave's tests) so CAC dashboards/analytics fill.

Volume: ``max(1, int(50 * SEED_SCALE))`` events per seeded customer
("50 events/customer × SEED_SCALE", spec §Agent B).

Funnel model (spec: impression→click→contact→qualified→converted with
realistic drop-off), mapped onto the W13 event_name enum:

    impression  ⇒ lead_created      (every customer, possibly repeated)
    click       ⇒ contacted         P = 0.62
    contact     ⇒ opted_in          P = 0.45
    qualified   ⇒ qualified         P = 0.55
    converted   ⇒ converted         P = 0.42   (amount_ngn = deal size)
    (post-sale) ⇒ first_txn         P = 0.35 of converted (amount_ngn = txn)
    stalled     ⇒ lost              P = 0.25 of stalled paths (terminal)

Events beyond the stage path are repeat top-of-funnel touches (extra
lead_created/contacted) — 50 events/customer means many repeat impressions.
Stage order is non-decreasing in time by construction.

Idempotent producer semantics (documented, spec §Agent B): every event id and
idempotency_key is ``deterministic_id("funnel-event:<customer_id>:<seq>")`` —
fully deterministic, so a re-run re-publishes byte-identical envelopes. The
W13 consumer dedupes on idempotency_key; the JSONL outbox is rewritten ('w')
each run, so file-level replays converge. Delivery is at-least-once; identity
of replays is the idempotency mechanism (kafka-python has no
enable_idempotence — the deterministic-key replay contract is the documented
substitute; same posture as the W14 outbox).

Sink: Kafka topic ``cac.events`` via kafka-python when ``SEED_KAFKA=on``
(guarded optional import per contract F; bootstrap ``KAFKA_BOOTSTRAP``);
default (CI) is a JSONL outbox at ``SEED_EVENTS_OUTBOX`` (default
/var/tmp/cac_events_outbox.jsonl) — same payload shape either way.

Envelope fields: ``tenant_id`` = documented synthetic tenant
``cac-seed-tenant``; ``channel`` = cac.channels.channel_code (joined at read
time); ``lga_id`` = W13 int shape: the 1-based ordinal of the customer's LGA
in ``cac.lgas ORDER BY id`` (stable across runs), NULL when unresolvable;
``campaign_id`` = one of 8 synthetic ``seed-campaign-N`` slugs on ~30% of
customers, else NULL.

``--dry-run`` prints counts + one sample envelope; no DB/Kafka/file writes.
"""

from __future__ import annotations

import json
import os
import sys
from datetime import UTC, date, datetime, timedelta
from pathlib import Path
from typing import Iterable, Iterator, Mapping

sys.path.insert(0, str(Path(__file__).resolve().parent))
import _lib  # noqa: E402  (contract A lib, owned by Agent A)
import seed_agents  # noqa: E402
import seed_customers  # noqa: E402

TOPIC = "cac.events"
EVENT_TYPE = "com.opendesk.cac.FunnelEvent"  # == cac_events.FUNNEL_EVENT_TYPE (W13)
SOURCE = "opendesk.seeds//scripts/seeds/seed_events.py"
SEED_TENANT_ID = "cac-seed-tenant"
EVENTS_PER_CUSTOMER = 50
DEFAULT_OUTBOX = "/var/tmp/cac_events_outbox.jsonl"
CHUNK = 5_000

# Stage machine: (event_name, P(reach | previous reached)). Order fixed.
STAGES: tuple[tuple[str, float], ...] = (
    ("lead_created", 1.00),
    ("contacted", 0.62),
    ("opted_in", 0.45),
    ("qualified", 0.55),
    ("converted", 0.42),
    ("first_txn", 0.35),
)
P_LOST_WHEN_STALLED = 0.25


def _iso(dt: datetime) -> str:
    return dt.astimezone(UTC).isoformat().replace("+00:00", "Z")


def build_customer_path(rng) -> list[str]:
    """One customer's stage path per the drop-off model (deterministic via rng)."""
    path: list[str] = []
    for name, prob in STAGES:
        if rng.random() <= prob:
            path.append(name)
        else:
            if name != "lead_created" and rng.random() < P_LOST_WHEN_STALLED:
                path.append("lost")
            break
    return path or ["lead_created"]


def iter_customer_envelopes(
    customer: Mapping[str, object],
    events_per_customer: int,
    lga_ordinals: Mapping[str, int] | None = None,
) -> Iterator[dict[str, object]]:
    """Yield CloudEvents envelopes for one customer (pure, deterministic).

    customer: {id, channel (channel_code), lga_id (text), acquired_on (date)}.
    lga_ordinals: cac.lgas id -> 1-based int ordinal (W13 lga_id shape).
    """
    cid = str(customer["id"])
    rng = seed_agents.seeded_rng("funnel", cid)
    path = build_customer_path(rng)
    # Pad remaining budget with repeat top-of-funnel touches; stage order
    # preserved (stable sort by stage rank; pads sort before same-rank path).
    pads = ["lead_created" if rng.random() < 0.7 else "contacted" for _ in range(max(0, events_per_customer - len(path)))]
    stage_rank = {name: i for i, (name, _) in enumerate(STAGES)}
    events = sorted((stage_rank.get(n, len(STAGES)), n) for n in pads + path)
    base = customer.get("acquired_on")
    if isinstance(base, datetime):
        base_dt = base if base.tzinfo else base.replace(tzinfo=UTC)
    elif isinstance(base, date):
        base_dt = datetime(base.year, base.month, base.day, tzinfo=UTC)
    else:
        base_dt = datetime(2023, 8, 1, tzinfo=UTC)  # deterministic dry-run base
    ts = base_dt + timedelta(hours=float(rng.randrange(0, 48)))
    channel = str(customer.get("channel") or "") or None
    campaign = f"seed-campaign-{1 + rng.randrange(8)}" if rng.random() < 0.3 else None
    lga_key = None
    if lga_ordinals is not None:
        lga_key = lga_ordinals.get(str(customer.get("lga_id")))
    for seq, (_, name) in enumerate(events):
        ts = ts + timedelta(hours=float(rng.randrange(1, 72)))
        amount = None
        if name == "converted":
            amount = 15_000 + rng.randrange(46) * 1_000  # ₦15k–₦60k deal size
        elif name == "first_txn":
            amount = 2_000 + rng.randrange(24) * 1_000  # ₦2k–₦25k first txn
        event_id = _lib.deterministic_id(f"funnel-event:{cid}:{seq:04d}")
        data = {
            "event_id": event_id,
            "tenant_id": SEED_TENANT_ID,
            "entity_type": "customer",
            "entity_id": cid,
            "event_name": name,
            "event_ts": _iso(ts),
            "channel": channel,
            "campaign_id": campaign,
            "lga_id": lga_key,
            "amount_ngn": amount,
            "idempotency_key": event_id,
        }
        yield {
            "specversion": "1.0",
            "id": event_id,
            "source": SOURCE,
            "type": EVENT_TYPE,
            "time": data["event_ts"],
            "datacontenttype": "application/json",
            "data": data,
        }


def iter_db_customers(conn) -> Iterator[dict[str, object]]:
    """Chunked scan of cac.customers joined to channel codes (memory-safe)."""
    cur = conn.cursor()
    offset = 0
    while True:
        cur.execute(
            "SELECT c.id, ch.channel_code, c.lga_id, c.acquired_on "
            "FROM cac.customers c LEFT JOIN cac.channels ch ON ch.id = c.channel_id "
            "ORDER BY c.id LIMIT %s OFFSET %s",
            (CHUNK, offset),
        )
        rows = cur.fetchall()
        if not rows:
            return
        for r in rows:
            yield {"id": str(r[0]), "channel": r[1], "lga_id": r[2], "acquired_on": r[3]}
        offset += len(rows)


def load_lga_ordinals(conn) -> dict[str, int]:
    cur = conn.cursor()
    cur.execute("SELECT id FROM cac.lgas ORDER BY id")
    return {str(r[0]): i + 1 for i, r in enumerate(cur.fetchall())}


def publish_kafka(envelopes: Iterable[Mapping[str, object]], topic: str = TOPIC) -> int:
    """Batch publish via kafka-python (guarded optional import, contract F)."""
    try:
        from kafka import KafkaProducer  # type: ignore
    except ImportError as exc:  # fail loud — SEED_KAFKA=on demands the dep
        raise RuntimeError("SEED_KAFKA=on but kafka-python is not installed") from exc
    producer = KafkaProducer(
        bootstrap_servers=os.environ.get("KAFKA_BOOTSTRAP", "localhost:9092"),
        value_serializer=lambda v: json.dumps(v).encode("utf-8"),
        key_serializer=lambda v: v.encode("utf-8") if v else None,
        retries=5,
        acks="all",
    )
    sent = 0
    try:
        for env in envelopes:
            producer.send(topic, key=str(env["id"]), value=env)
            sent += 1
            if sent % CHUNK == 0:
                producer.flush()
        producer.flush()
    finally:
        producer.close()
    return sent


def write_outbox(envelopes: Iterable[Mapping[str, object]], path: str) -> int:
    """JSONL outbox — rewritten ('w') each run so replays converge."""
    written = 0
    with open(path, "w", encoding="utf-8") as fh:
        for env in envelopes:
            fh.write(json.dumps(env, separators=(",", ":")) + "\n")
            written += 1
    return written


def main(argv: list[str] | None = None) -> int:
    args = _lib.seed_argparser("Emit synthetic FunnelEvents to cac.events (or JSONL outbox)").parse_args(argv)
    scale = _lib.apply_scale_arg(args.scale)
    per_customer = max(1, int(EVENTS_PER_CUSTOMER * scale))
    use_kafka = os.environ.get("SEED_KAFKA", "off").lower() == "on"
    outbox = os.environ.get("SEED_EVENTS_OUTBOX", DEFAULT_OUTBOX)
    sink = f"kafka://{TOPIC}" if use_kafka else outbox

    if args.dry_run:
        customers = int(seed_customers.CARDINALITY * scale)
        sample = next(
            iter_customer_envelopes(
                {
                    "id": _lib.deterministic_id(seed_customers.natural_key(0)),
                    "channel": "agent-network",
                    "lga_id": "dry-run-lga",
                    "acquired_on": date(2023, 8, 1),
                },
                per_customer,
            )
        )
        print(f"[seed_events] would emit {customers * per_customer} envelopes ({customers} customers × {per_customer}) -> {sink}")
        print("[seed_events] sample: " + json.dumps(sample, separators=(",", ":")))
        print("[seed_events] dry-run: no DB/Kafka/file writes")
        return 0

    try:
        conn = _lib.get_conn()
        lga_ordinals = load_lga_ordinals(conn)
        envelopes = (
            env
            for cust in iter_db_customers(conn)
            for env in iter_customer_envelopes(cust, per_customer, lga_ordinals)
        )
        count = publish_kafka(envelopes) if use_kafka else write_outbox(envelopes, outbox)
        _lib.log_seed_run(TOPIC, count, conn)
        _lib.commit(conn)
    except Exception as exc:  # noqa: BLE001 - fail loud, non-zero exit
        print(f"[seed_events] FAILED: {exc}", file=sys.stderr)
        return 1
    _lib.emit_seed_report(TOPIC, count, _lib.runner_id(), _lib.git_sha())
    print(f"[seed_events] emitted {count} funnel events -> {sink}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
