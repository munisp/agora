"""test_receipt_recon_gold — py_compile + pure-function unit tests (SPEC-W64 §6).

No Spark session needed: receipt_recon_gold.py guards its pyspark import so
the module imports on driver-less CI boxes, and the W64 aggregation changes
are exercised through the Spark-free reference implementations
(aggregate_recon_daily / aggregate_ingest_daily) that the Spark transforms
mirror expression-for-expression:
  * resolved_count = REAL count of bronze.recon_resolutions events joined to
    findings on finding_id (degrades to 0 when resolutions are unavailable);
  * receipt_ingest_daily gains `channel` attributed from bronze.receipt_ingest
    via document_id (degrades to NULL when the ingest table is unavailable).

Run either way:

  python3 -m pytest infra/lakehouse/spark/jobs/test_receipt_recon_gold.py
  python3 infra/lakehouse/spark/jobs/test_receipt_recon_gold.py   (unittest runner)
"""

import os
import py_compile
import sys
import unittest
from datetime import date, datetime

JOBS_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, JOBS_DIR)

import receipt_recon_gold as gold  # noqa: E402

DAY = date(2026, 2, 5)
TS = datetime(2026, 2, 5, 10, 0, 0)


def finding(event_id, finding_id, kind="amount_mismatch", confidence="exact",
            tenant="t1", expected=5000, captured=4500, occurred_at=TS):
    return {
        "event_id": event_id,
        "tenant_id": tenant,
        "run_id": "run-1",
        "finding_kind": kind,
        "confidence": confidence,
        "finding_id": finding_id,
        "expected_amount_minor": expected,
        "captured_amount_minor": captured,
        "occurred_at": occurred_at,
    }


def resolution(event_id, finding_id, tenant="t1", occurred_at=None):
    return {
        "event_id": event_id,
        "tenant_id": tenant,
        "finding_id": finding_id,
        "run_id": "run-1",
        "kind": "amount_mismatch",
        "resolved_by": "ops-user-7",
        "occurred_at": occurred_at or datetime(2026, 2, 6, 9, 0, 0),
    }


def document(event_id, document_id, tenant="t1", pages=2, amount=1250000,
             occurred_at=TS):
    return {
        "event_id": event_id,
        "tenant_id": tenant,
        "document_id": document_id,
        "kind": "receipt",
        "page_count": pages,
        "amount_minor": amount,
        "occurred_at": occurred_at,
    }


def ingest(event_id, document_id, channel, tenant="t1", occurred_at=TS):
    return {
        "event_id": event_id,
        "tenant_id": tenant,
        "document_id": document_id,
        "channel": channel,
        "occurred_at": occurred_at,
    }


class TestPyCompile(unittest.TestCase):
    def test_job_compiles(self):
        py_compile.compile(os.path.join(JOBS_DIR, "receipt_recon_gold.py"), doraise=True)

    def test_module_imports_without_pyspark(self):
        self.assertTrue(callable(gold.aggregate_recon_daily))
        self.assertTrue(callable(gold.aggregate_ingest_daily))

    def test_new_bronze_table_env_defaults(self):
        self.assertEqual(gold.RESOLUTIONS_TABLE, "iceberg.bronze.recon_resolutions")
        self.assertEqual(gold.INGEST_TABLE, "iceberg.bronze.receipt_ingest")
        self.assertEqual(gold.CHANNELS, ("field_pwa", "whatsapp"))


class TestResolvedCount(unittest.TestCase):
    def test_resolutions_none_degrades_to_zero(self):
        # Pre-W64 deployment posture (no bronze.recon_resolutions table).
        out = gold.aggregate_recon_daily([finding("e1", "f1")])
        self.assertEqual(len(out), 1)
        self.assertEqual(out[0]["findings_count"], 1)
        self.assertEqual(out[0]["resolved_count"], 0)

    def test_resolutions_empty_list_is_real_zero(self):
        out = gold.aggregate_recon_daily([finding("e1", "f1")], [])
        self.assertEqual(out[0]["resolved_count"], 0)

    def test_real_resolved_count_via_finding_id(self):
        findings = [
            finding("e1", "f1"),
            finding("e2", "f2", confidence="heuristic"),
            finding("e3", "f3"),
        ]
        resolutions = [
            resolution("r1", "f1"),
            resolution("r2", "f2"),
        ]
        out = gold.aggregate_recon_daily(findings, resolutions)
        by_conf = {row["confidence"]: row for row in out}
        # f1 and f3 share (kind, confidence=exact) => findings 2, resolved 1.
        self.assertEqual(by_conf["exact"]["findings_count"], 2)
        self.assertEqual(by_conf["exact"]["resolved_count"], 1)
        self.assertEqual(by_conf["heuristic"]["findings_count"], 1)
        self.assertEqual(by_conf["heuristic"]["resolved_count"], 1)
        # Money math unchanged.
        self.assertEqual(by_conf["exact"]["expected_sum_minor"], 10000)
        self.assertEqual(by_conf["exact"]["mismatch_abs_sum_minor"], 1000)

    def test_resolution_deduped_on_event_id(self):
        findings = [finding("e1", "f1")]
        dup = resolution("r1", "f1")
        resolutions = [dup, dict(dup)]  # at-least-once redelivery
        out = gold.aggregate_recon_daily(findings, resolutions)
        self.assertEqual(out[0]["resolved_count"], 1)

    def test_unknown_finding_id_unattributable_not_counted(self):
        findings = [finding("e1", "f1")]
        resolutions = [resolution("r1", "f-unknown")]
        out = gold.aggregate_recon_daily(findings, resolutions)
        self.assertEqual(out[0]["resolved_count"], 0)

    def test_resolution_attributed_to_finding_day_not_resolution_day(self):
        findings = [finding("e1", "f1")]  # occurred DAY 2026-02-05
        resolutions = [resolution("r1", "f1")]  # resolved 2026-02-06
        out = gold.aggregate_recon_daily(findings, resolutions)
        self.assertEqual(len(out), 1)
        self.assertEqual(out[0]["day"], DAY)
        self.assertEqual(out[0]["resolved_count"], 1)


class TestIngestChannel(unittest.TestCase):
    def test_channels_none_degrades_to_null_channel(self):
        # Pre-W64 deployment posture: the column exists, values are NULL.
        out = gold.aggregate_ingest_daily([document("e1", "d1")])
        self.assertEqual(len(out), 1)
        self.assertIsNone(out[0]["channel"])
        self.assertEqual(out[0]["documents"], 1)
        self.assertEqual(out[0]["receipts"], 1)
        self.assertEqual(out[0]["receipts_with_amount"], 1)

    def test_channel_attribution_splits_grain(self):
        documents = [
            document("e1", "d1"),
            document("e2", "d2"),
            document("e3", "d3", amount=None),
        ]
        channels = [
            ingest("i1", "d1", "field_pwa"),
            ingest("i2", "d2", "whatsapp"),
            # d3 has no ingest event -> NULL channel bucket.
        ]
        out = gold.aggregate_ingest_daily(documents, channels)
        self.assertEqual(len(out), 3)
        by_channel = {row["channel"]: row for row in out}
        self.assertEqual(by_channel["field_pwa"]["documents"], 1)
        self.assertEqual(by_channel["whatsapp"]["documents"], 1)
        self.assertIsNone(by_channel[None]["channel"])
        self.assertEqual(by_channel[None]["receipts_with_amount"], 0)

    def test_channel_latest_ingest_event_wins(self):
        documents = [document("e1", "d1")]
        channels = [
            ingest("i1", "d1", "whatsapp", occurred_at=datetime(2026, 2, 5, 8, 0, 0)),
            ingest("i2", "d1", "field_pwa", occurred_at=datetime(2026, 2, 5, 9, 0, 0)),
        ]
        out = gold.aggregate_ingest_daily(documents, channels)
        self.assertEqual(len(out), 1)
        self.assertEqual(out[0]["channel"], "field_pwa")

    def test_channel_rows_deduped_on_event_id(self):
        documents = [document("e1", "d1")]
        channels = [ingest("i1", "d1", "whatsapp"), ingest("i1", "d1", "field_pwa")]
        out = gold.aggregate_ingest_daily(documents, channels)
        self.assertEqual(len(out), 1)  # one deduped channel row, one bucket


if __name__ == "__main__":
    unittest.main()
