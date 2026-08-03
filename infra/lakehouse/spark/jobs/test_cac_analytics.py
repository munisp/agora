"""test_cac_analytics — py_compile + pure-function unit tests (SPEC-W13 §Agent C).

No Spark session needed: cac_analytics.py guards its pyspark import so the
module imports on driver-less CI boxes, and the aggregation math is exercised
through the Spark-free reference implementations (aggregate_daily_by_channel /
aggregate_daily_by_lga + helpers) that the Spark transforms mirror
expression-for-expression.

Run either way:

  python3 -m pytest infra/lakehouse/spark/jobs/test_cac_analytics.py
  python3 infra/lakehouse/spark/jobs/test_cac_analytics.py   (unittest runner)
"""

import os
import py_compile
import sys
import unittest
from datetime import date, datetime

JOBS_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, JOBS_DIR)

import cac_analytics as cac  # noqa: E402


def ev(event_name, ts, tenant="t1", channel="web", lga_id=None,
       idem=None, event_id=None):
    """Build a bronze-contract funnel event row (dict)."""
    return {
        "event_id": event_id,
        "tenant_id": tenant,
        "entity_type": "lead",
        "entity_id": "lead-1",
        "event_name": event_name,
        "event_ts": ts,
        "channel": channel,
        "campaign_id": None,
        "lga_id": lga_id,
        "amount_ngn": None,
        "idempotency_key": idem,
    }


def spend(amount, day, tenant="t1", channel="web"):
    return {
        "tenant_id": tenant,
        "campaign_id": "camp-1",
        "channel": channel,
        "day": day,
        "amount_ngn": amount,
    }


class TestPyCompile(unittest.TestCase):
    def test_job_compiles(self):
        cac_path = os.path.join(JOBS_DIR, "cac_analytics.py")
        py_compile.compile(cac_path, doraise=True)

    def test_module_imports_without_pyspark(self):
        # If we got here the import at the top of this file already succeeded;
        # assert the guarded-import fallback left the pure API usable.
        self.assertTrue(callable(cac.aggregate_daily_by_channel))
        self.assertTrue(callable(cac.aggregate_daily_by_lga))
        self.assertTrue(callable(cac.compute_cac_ngn))


class TestHelpers(unittest.TestCase):
    def test_normalize_channel(self):
        self.assertEqual(cac.normalize_channel(" Web "), "web")
        self.assertEqual(cac.normalize_channel("WHATSAPP"), "whatsapp")
        self.assertEqual(cac.normalize_channel(None), "unknown")
        self.assertEqual(cac.normalize_channel("   "), "unknown")

    def test_classify_event(self):
        self.assertEqual(cac.classify_event("lead_created"), "lead")
        self.assertEqual(cac.classify_event(" Lead_Created "), "lead")
        self.assertEqual(cac.classify_event("converted"), "conversion")
        # Funnel stages that must NOT enter CAC math (SPEC-W13 §2 vocabulary):
        for ignored in ("contacted", "opted_in", "qualified", "first_txn", "lost", None, "bogus"):
            self.assertIsNone(cac.classify_event(ignored), ignored)

    def test_compute_cac_ngn(self):
        self.assertEqual(cac.compute_cac_ngn(1000.0, 4), 250.0)
        self.assertIsNone(cac.compute_cac_ngn(1000.0, 0))   # undefined, not inf
        self.assertIsNone(cac.compute_cac_ngn(1000.0, None))
        self.assertEqual(cac.compute_cac_ngn(None, 3), 0.0)  # organic conversions

    def test_allocate_spend(self):
        self.assertEqual(cac.allocate_spend(1000.0, 3, 10), 300.0)
        self.assertEqual(cac.allocate_spend(1000.0, 0, 10), 0.0)
        self.assertEqual(cac.allocate_spend(1000.0, 3, 0), 0.0)
        self.assertEqual(cac.allocate_spend(None, 3, 10), 0.0)


class TestAggregateDailyByChannel(unittest.TestCase):
    def test_basic_rollup_and_cac(self):
        day1, day2 = date(2026, 3, 1), date(2026, 3, 2)
        events = [
            ev("lead_created", datetime(2026, 3, 1, 9), idem="a"),
            ev("lead_created", datetime(2026, 3, 1, 10), idem="b"),
            ev("converted", datetime(2026, 3, 1, 11), idem="c"),
            ev("qualified", datetime(2026, 3, 1, 12), idem="d"),   # ignored
            ev("first_txn", datetime(2026, 3, 1, 13), idem="e"),   # ignored
            ev("lead_created", datetime(2026, 3, 2, 9), idem="f"),
        ]
        spend_rows = [spend(2000.0, day1), spend(500.0, day2)]
        out = cac.aggregate_daily_by_channel(events, spend_rows)
        self.assertEqual(len(out), 2)
        r1, r2 = out
        self.assertEqual((r1["day"], r1["tenant_id"], r1["channel"]), (day1, "t1", "web"))
        self.assertEqual((r1["leads"], r1["conversions"], r1["spend_ngn"]), (2, 1, 2000.0))
        self.assertEqual(r1["cac_ngn"], 2000.0)
        self.assertEqual((r2["leads"], r2["conversions"], r2["spend_ngn"]), (1, 0, 500.0))
        self.assertIsNone(r2["cac_ngn"])  # 0 conversions → NULL CAC

    def test_dedupe_by_idempotency_key_latest_ts_wins(self):
        day1 = date(2026, 3, 1)
        events = [
            # Same idempotency key delivered twice (at-least-once): count once.
            ev("lead_created", datetime(2026, 3, 1, 9), idem="dup"),
            ev("lead_created", datetime(2026, 3, 1, 10), idem="dup"),
            # Fallback to event_id when idempotency_key is NULL.
            ev("converted", datetime(2026, 3, 1, 11), event_id="e1"),
            ev("converted", datetime(2026, 3, 1, 12), event_id="e1"),
        ]
        out = cac.aggregate_daily_by_channel(events, [])
        self.assertEqual(len(out), 1)
        self.assertEqual((out[0]["leads"], out[0]["conversions"]), (1, 1))

    def test_channel_normalization_and_unknown_bucket(self):
        day1 = date(2026, 3, 1)
        events = [
            ev("lead_created", datetime(2026, 3, 1, 9), channel=" WhatsApp ", idem="a"),
            ev("lead_created", datetime(2026, 3, 1, 10), channel=None, idem="b"),
        ]
        spend_rows = [spend(100.0, day1, channel="WHATSAPP"), spend(50.0, day1, channel=None)]
        out = {r["channel"]: r for r in cac.aggregate_daily_by_channel(events, spend_rows)}
        self.assertEqual(set(out), {"whatsapp", "unknown"})
        self.assertEqual(out["whatsapp"]["spend_ngn"], 100.0)
        self.assertEqual(out["unknown"]["spend_ngn"], 50.0)

    def test_spend_without_events_still_produces_row(self):
        day1 = date(2026, 3, 1)
        out = cac.aggregate_daily_by_channel([], [spend(750.0, day1, channel="qr")])
        self.assertEqual(len(out), 1)
        self.assertEqual((out[0]["spend_ngn"], out[0]["leads"], out[0]["conversions"]), (750.0, 0, 0))
        self.assertIsNone(out[0]["cac_ngn"])

    def test_unkeyable_and_timeless_rows_dropped(self):
        events = [
            ev("lead_created", datetime(2026, 3, 1, 9), tenant=None, idem="a"),
            ev("lead_created", None, idem="b"),
            ev("lead_created", datetime(2026, 3, 1, 11), idem="c"),
        ]
        out = cac.aggregate_daily_by_channel(events, [])
        self.assertEqual(len(out), 1)
        self.assertEqual(out[0]["leads"], 1)

    def test_multiple_spend_rows_summed(self):
        day1 = date(2026, 3, 1)
        spend_rows = [spend(100.0, day1), spend(250.0, day1), spend(75.0, day1, channel="sms")]
        out = {r["channel"]: r for r in cac.aggregate_daily_by_channel([], spend_rows)}
        self.assertEqual(out["web"]["spend_ngn"], 350.0)
        self.assertEqual(out["sms"]["spend_ngn"], 75.0)


class TestAggregateDailyByLga(unittest.TestCase):
    def test_prorata_spend_allocation_sums_to_pool(self):
        day1 = date(2026, 3, 1)
        events = [
            ev("lead_created", datetime(2026, 3, 1, 9), lga_id=101, idem="a"),
            ev("lead_created", datetime(2026, 3, 1, 10), lga_id=101, idem="b"),
            ev("converted", datetime(2026, 3, 1, 11), lga_id=101, idem="c"),
            ev("lead_created", datetime(2026, 3, 1, 12), lga_id=202, idem="d"),
            ev("lead_created", datetime(2026, 3, 1, 13), lga_id=None, idem="e"),  # excluded
        ]
        # Tenant-day pool = 300 + 100 (across channels).
        spend_rows = [spend(300.0, day1, channel="web"), spend(100.0, day1, channel="qr")]
        out = {r["lga_id"]: r for r in cac.aggregate_daily_by_lga(events, spend_rows)}
        self.assertEqual(set(out), {101, 202})
        # Pool allocated over geolocated leads only: 2/3 vs 1/3 of 400.
        # cac = allocated / conversions; recover allocated = cac * conversions
        # for lga 101 (1 conversion) and check shares via leads weighting.
        self.assertEqual(out[101]["leads"], 2)
        self.assertEqual(out[101]["conversions"], 1)
        self.assertAlmostEqual(out[101]["cac_ngn"], 400.0 * 2 / 3)
        self.assertEqual(out[202]["leads"], 1)
        self.assertIsNone(out[202]["cac_ngn"])  # 0 conversions → NULL

    def test_no_spend_means_zero_cac_not_null(self):
        day1 = date(2026, 3, 1)
        events = [
            ev("lead_created", datetime(2026, 3, 1, 9), lga_id=101, idem="a"),
            ev("converted", datetime(2026, 3, 1, 10), lga_id=101, idem="b"),
        ]
        out = cac.aggregate_daily_by_lga(events, [])
        self.assertEqual(len(out), 1)
        self.assertEqual(out[0]["cac_ngn"], 0.0)  # organic conversions, no spend

    def test_tenant_isolation(self):
        day1 = date(2026, 3, 1)
        events = [
            ev("lead_created", datetime(2026, 3, 1, 9), tenant="t1", lga_id=101, idem="a"),
            ev("lead_created", datetime(2026, 3, 1, 10), tenant="t2", lga_id=101, idem="b"),
            ev("converted", datetime(2026, 3, 1, 11), tenant="t2", lga_id=101, idem="c"),
        ]
        spend_rows = [spend(900.0, day1, tenant="t2")]
        out = {(r["tenant_id"], r["lga_id"]): r for r in cac.aggregate_daily_by_lga(events, spend_rows)}
        self.assertEqual(set(out), {("t1", 101), ("t2", 101)})
        self.assertIsNone(out[("t1", 101)]["cac_ngn"])  # t1: 0 conversions → NULL
        self.assertEqual(out[("t2", 101)]["cac_ngn"], 900.0)  # full pool, 1/1 share


class TestContractColumns(unittest.TestCase):
    def test_channel_row_keys_match_spec_w13_s4(self):
        out = cac.aggregate_daily_by_channel(
            [ev("lead_created", datetime(2026, 3, 1, 9), idem="a")], []
        )
        self.assertEqual(
            set(out[0]),
            {"day", "tenant_id", "channel", "spend_ngn", "leads", "conversions", "cac_ngn"},
        )

    def test_lga_row_keys_match_spec_w13_s4(self):
        # geom/h3_cells are attached by the Spark transform (geometry needs
        # Sedona); the pure aggregator owns the metric columns of §4.
        out = cac.aggregate_daily_by_lga(
            [ev("lead_created", datetime(2026, 3, 1, 9), lga_id=101, idem="a")], []
        )
        self.assertEqual(
            set(out[0]),
            {"day", "tenant_id", "lga_id", "leads", "conversions", "cac_ngn"},
        )
        self.assertIn("PARTITIONED BY (day)", _job_source())
        self.assertIn("h3_cells", _job_source())


def _job_source():
    with open(os.path.join(JOBS_DIR, "cac_analytics.py")) as fh:
        return fh.read()


if __name__ == "__main__":
    unittest.main(verbosity=2)
