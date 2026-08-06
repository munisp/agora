"""A1 labeled-dataset loader (SPEC-W33 §3 B3): joins the
``scripts/seeds/naija_transactions.py`` outputs into a
:class:`extract.TenantGraph`-compatible labeled graph for the supervised
node-classification head (``gnn_head.py``).

Files joined (all under one dataset dir, e.g. ``.../naija_txn/42/``):
  * ``persons.jsonl``     -> ``PersonRec`` rows (one per person)
  * ``graph_edges.jsonl`` -> REFERRED -> ``ReferralRec``, BOOKED -> ``BookingRec``
                             (+ distinct ``OfferingRec``), CAPTURED -> ``ContactRec``
  * ``labels.json``       -> per-person ground-truth labels
  * ``events.jsonl``      -> OPTIONAL, only to fill booking/offering
                             ``price_cents`` from ``amount_ngn`` (booking events
                             keyed by ``reference_id`` == ``booking_id``); absent
                             -> documented honest 0, same convention as the
                             zero-padded offering features in ``gnn_data``.

CRITICAL A1 QUIRK — labels.json is the ONLY ground truth. The A1 generator
stamps injected fraud persons created via ``make_person`` (sybil clusters)
with ``fraud: true`` on their person row, but persons SAMPLED FROM THE
BENIGN POPULATION and then injected into fraud scenarios (referral_ring
members, ghost_booking staff, structuring persons — 704 of them on the
default seed-42 generation) keep ``fraud: false`` on their
``persons.jsonl`` row. The person-row ``fraud``/``scenario`` fields are
therefore NEVER read here; supervision comes exclusively from
``labels.json`` entries joined on ``entity_id == person_id``.

Supervision semantics (SPEC-W33 §3 B3):
  * fraud label with scenario in :data:`POSITIVE_SCENARIOS`
    (``sybil_cluster``, ``referral_ring``) -> label 1 (positive)
  * fraud label with any OTHER scenario (ghost_booking, structuring, ...)
    -> MASKED OUT of both the loss and the val metrics (neither positive
    nor trusted negative)
  * benign label (``fraud: false``, ``benign_*`` scenarios) -> label 0
    (hard negative)
  * persons with no labels.json entry at all -> label 0 (benign population)

A person carrying BOTH a positive-scenario label and another fraud-scenario
label is a positive (positive wins — they are genuinely part of a ring or
cluster); the precedence order above is applied per person after collecting
all of their labels.json entries.

Everything is deterministic: persons/offerings are emitted sorted by id and
the label map is built from sorted entries, independent of file ordering.
"""

from __future__ import annotations

import hashlib
import json
import logging
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterator, Mapping

from .extract import (
    BookingRec,
    ContactRec,
    OfferingRec,
    PersonRec,
    ReferralRec,
    TenantGraph,
)

log = logging.getLogger(__name__)

#: A1 fraud scenarios whose person-level labels are positives for the
#: supervised head (SPEC-W33 §3 B3: sybil cluster + referral ring nodes).
POSITIVE_SCENARIOS = frozenset({"sybil_cluster", "referral_ring"})

#: Files that make up the labeled dataset (hashed in this fixed order for
#: the dataset fingerprint; events.jsonl is optional and hashed when used).
REQUIRED_FILES = ("persons.jsonl", "graph_edges.jsonl", "labels.json")
EVENTS_FILE = "events.jsonl"


@dataclass(frozen=True)
class LabeledGraph:
    """TenantGraph + per-person supervision payload for one dataset dir."""

    graph: TenantGraph
    labels: dict[str, int]  # person_id -> 0/1 (the supervised node set)
    masked_out: tuple[str, ...]  # sorted person ids excluded from supervision
    num_positives: int
    dataset_sha256: str  # sha256 over the joined files (I2 provenance)
    dataset_seed: int | None  # seed recorded in labels.json (A1 manifest)
    dataset_generated_at: str | None  # deterministic A1 generation stamp
    source_dir: str

    @property
    def num_supervised(self) -> int:
        return len(self.labels)


def _iter_jsonl(path: Path) -> Iterator[dict[str, Any]]:
    with open(path, encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if line:
                yield json.loads(line)


def dataset_fingerprint(dataset_dir: str | Path, use_events: bool = True) -> str:
    """sha256 over the dataset files actually joined (I2 dataset hash)."""
    d = Path(dataset_dir)
    names = list(REQUIRED_FILES)
    if use_events and (d / EVENTS_FILE).is_file():
        names.append(EVENTS_FILE)
    digest = hashlib.sha256()
    for name in sorted(names):
        digest.update(name.encode("utf-8"))
        with open(d / name, "rb") as fh:
            for chunk in iter(lambda: fh.read(1 << 20), b""):
                digest.update(chunk)
    return digest.hexdigest()


def _load_person_labels(
    labels_doc: Mapping[str, Any], person_ids: set[str]
) -> tuple[dict[str, int], list[str]]:
    """Join labels.json onto persons (labels.json is the ONLY ground truth)."""
    by_person: dict[str, list[Mapping[str, Any]]] = {}
    for entry in labels_doc.get("entries", []):
        entity_id = str(entry.get("entity_id", ""))
        if entity_id in person_ids:
            by_person.setdefault(entity_id, []).append(entry)

    labels: dict[str, int] = {}
    masked: list[str] = []
    for pid in sorted(person_ids):
        entries = by_person.get(pid, [])
        fraud_scenarios = {
            str(e.get("scenario"))
            for e in entries
            if bool(e.get("fraud")) and e.get("scenario")
        }
        if fraud_scenarios & POSITIVE_SCENARIOS:
            labels[pid] = 1  # sybil_cluster / referral_ring member
        elif fraud_scenarios:
            masked.append(pid)  # other fraud scenario: neither class
        else:
            # benign_* hard negative or unlabeled benign population
            labels[pid] = 0
    return labels, masked


def load_labeled_graph(
    dataset_dir: str | Path,
    tenant_id: str = "naija-txn",
    *,
    use_events: bool = True,
) -> LabeledGraph:
    """Build a labeled TenantGraph from one A1 dataset directory.

    ``use_events`` joins ``events.jsonl`` for booking/offering prices when
    present; pass False to force the documented zero-price fallback.
    """
    d = Path(dataset_dir)
    for name in REQUIRED_FILES:
        if not (d / name).is_file():
            raise FileNotFoundError(f"A1 dataset file missing: {d / name}")

    persons_raw = sorted(_iter_jsonl(d / "persons.jsonl"), key=lambda p: p["person_id"])
    person_ids = {str(p["person_id"]) for p in persons_raw}
    # NOTE: person-row fraud/scenario fields are deliberately IGNORED (the
    # A1 quirk documented in the module docstring).
    persons = [
        PersonRec(person_id=str(p["person_id"]), name=str(p.get("name_hash") or ""))
        for p in persons_raw
    ]

    with open(d / "labels.json", encoding="utf-8") as fh:
        labels_doc = json.load(fh)
    labels, masked = _load_person_labels(labels_doc, person_ids)

    # manifest.json carries the deterministic A1 generation stamp (GA1).
    generated_at: str | None = None
    manifest_path = d / "manifest.json"
    if manifest_path.is_file():
        with open(manifest_path, encoding="utf-8") as fh:
            manifest_doc = json.load(fh)
        if manifest_doc.get("generated_at"):
            generated_at = str(manifest_doc["generated_at"])

    referrals: list[ReferralRec] = []
    bookings: list[BookingRec] = []
    contacts: list[ContactRec] = []
    offering_ids: set[str] = set()
    for edge in _iter_jsonl(d / "graph_edges.jsonl"):
        edge_type = edge.get("edge_type")
        if edge_type == "REFERRED":
            referrals.append(
                ReferralRec(
                    from_person_id=str(edge["from_person_id"]),
                    to_person_id=str(edge["to_person_id"]),
                    at=edge.get("at"),
                    program=str(edge.get("program") or ""),
                )
            )
        elif edge_type == "BOOKED":
            offering_ids.add(str(edge["offering_id"]))
            bookings.append(
                BookingRec(
                    person_id=str(edge["person_id"]),
                    booking_id=str(edge.get("booking_id") or ""),
                    offering_id=str(edge["offering_id"]),
                    at=edge.get("at"),
                    status=str(edge.get("status") or ""),
                    showed=edge.get("showed"),
                    price_cents=0,  # filled from events.jsonl when available
                )
            )
        elif edge_type == "CAPTURED":
            contacts.append(
                ContactRec(
                    person_id=str(edge["agent_id"]),
                    lead_id=str(edge.get("contact_id") or ""),
                    captured_at=edge.get("captured_at") or edge.get("at"),
                    channel=str(edge.get("channel") or ""),
                    source="naija_txn",
                )
            )
        else:  # forward-compatible: unknown edge types are ignored
            log.debug("ignoring unknown A1 edge type %r", edge_type)

    # Optional price join: booking amount_ngn -> price_cents (x100, rounded).
    price_by_booking: dict[str, int] = {}
    price_by_offering: dict[str, int] = {}
    events_path = d / EVENTS_FILE
    if use_events and events_path.is_file():
        for event in _iter_jsonl(events_path):
            if event.get("event_type") != "booking":
                continue
            cents = int(round(float(event.get("amount_ngn") or 0.0) * 100.0))
            reference = event.get("reference_id")
            if reference:
                price_by_booking[str(reference)] = cents
            counterparty = event.get("counterparty")
            if counterparty:
                price_by_offering[str(counterparty)] = cents
        if price_by_booking:
            bookings = [
                BookingRec(
                    person_id=b.person_id,
                    booking_id=b.booking_id,
                    offering_id=b.offering_id,
                    offering_name=b.offering_name,
                    at=b.at,
                    status=b.status,
                    showed=b.showed,
                    price_cents=price_by_booking.get(b.booking_id, 0),
                )
                for b in bookings
            ]

    offerings = [
        OfferingRec(
            offering_id=oid,
            name="",
            price_cents=price_by_offering.get(oid, 0),
        )
        for oid in sorted(offering_ids)
    ]

    graph = TenantGraph(
        tenant_id=tenant_id,
        persons=persons,
        offerings=offerings,
        bookings=bookings,
        referrals=referrals,
        contacts=contacts,
    )
    return LabeledGraph(
        graph=graph,
        labels=labels,
        masked_out=tuple(masked),
        num_positives=sum(labels.values()),
        dataset_sha256=dataset_fingerprint(d, use_events=use_events),
        dataset_seed=(
            int(labels_doc["seed"]) if labels_doc.get("seed") is not None else None
        ),
        dataset_generated_at=generated_at,
        source_dir=str(d),
    )
