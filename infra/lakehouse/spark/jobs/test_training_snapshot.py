"""test_training_snapshot — py_compile + pure-function unit tests (SPEC-W33 §2 A2).

No Spark session needed: training_snapshot.py guards its pyspark import so
the module imports on driver-less CI boxes, and the row shaping / feature
stats / manifest logic is exercised through the Spark-free reference
implementations that the Spark transforms mirror expression-for-expression.

Run either way:

  python3 -m pytest infra/lakehouse/spark/jobs/test_training_snapshot.py
  python3 infra/lakehouse/spark/jobs/test_training_snapshot.py   (unittest)
"""

import json
import os
import py_compile
import sys
import unittest

JOBS_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, JOBS_DIR)

import training_snapshot as ts  # noqa: E402


def node_row(person="p1", tenant="t1", label="Person", **overrides):
    row = {
        "snapshot_date": "2026-08-06",
        "tenant_id": tenant,
        "label": label,
        "node_id": person,
        "in_degree": 2,
        "out_degree": 3,
        "consent_marketing": True,
        "quarantine": False,
        "bookings_total": 4,
        "ltv_cents": 200000,
        "no_show_rate": 0.25,
        "propensity_show": 0.8,
        "propensity_convert": 0.1,
        "channel_of_first_touch": "field_pwa",
        "last_active_at": "2026-08-01T10:00:00Z",
    }
    row.update(overrides)
    return row


def edge_row(src="p1", dst="p2", tenant="t1", **overrides):
    row = {
        "snapshot_date": "2026-08-06",
        "tenant_id": tenant,
        "edge_type": "REFERRED",
        "src_label": "Person",
        "src_id": src,
        "dst_label": "Person",
        "dst_id": dst,
        "weight": 1.0,
        "edge_at": "2026-08-01T10:00:00Z",
    }
    row.update(overrides)
    return row


class TestPyCompile(unittest.TestCase):
    def test_job_compiles(self):
        py_compile.compile(os.path.join(JOBS_DIR, "training_snapshot.py"), doraise=True)

    def test_module_imports_without_pyspark(self):
        for fn in (
            "numeric_stats", "compute_feature_stats", "fraud_feature_rows",
            "credit_feature_rows", "gnn_node_rows", "gnn_edge_rows",
            "build_manifest", "manifest_json",
        ):
            self.assertTrue(callable(getattr(ts, fn)), fn)


class TestNumericStats(unittest.TestCase):
    def test_basic_stats(self):
        s = ts.numeric_stats([1, 2, 3, 4])
        self.assertEqual(s["count"], 4)
        self.assertAlmostEqual(s["mean"], 2.5)
        # sample stddev (ddof=1), matching Spark stddev
        self.assertAlmostEqual(s["std"], (5.0 / 3.0) ** 0.5)
        self.assertEqual(s["min"], 1.0)
        self.assertEqual(s["max"], 4.0)

    def test_none_and_bool_skipped(self):
        s = ts.numeric_stats([None, 5, None, True, 7])
        self.assertEqual(s["count"], 2)
        self.assertEqual(s["mean"], 6.0)

    def test_single_value_std_none(self):
        s = ts.numeric_stats([3])
        self.assertEqual(s["count"], 1)
        self.assertIsNone(s["std"])  # Spark stddev is NULL for n<2

    def test_empty_returns_none(self):
        self.assertIsNone(ts.numeric_stats([]))
        self.assertIsNone(ts.numeric_stats([None, None]))

    def test_compute_feature_stats_omits_empty(self):
        rows = [{"a": 1, "b": None}, {"a": 3, "b": None}]
        stats = ts.compute_feature_stats(rows, ("a", "b"))
        self.assertIn("a", stats)
        self.assertNotIn("b", stats)
        self.assertEqual(stats["a"]["mean"], 2.0)


class TestFraudFeatureRows(unittest.TestCase):
    def test_shape_and_label_join(self):
        labels = [
            {"entity_id": "p1", "scenario": "referral_ring", "fraud": True},
            {"entity_id": "p2", "scenario": "benign_family", "fraud": False},
        ]
        rows = ts.fraud_feature_rows(
            [node_row("p2"), node_row("p1"), node_row("p3")], labels
        )
        self.assertEqual([r["person_id"] for r in rows], ["p1", "p2", "p3"])
        by_id = {r["person_id"]: r for r in rows}
        self.assertTrue(by_id["p1"]["label_fraud"])
        self.assertEqual(by_id["p1"]["label_scenario"], "referral_ring")
        self.assertIs(by_id["p2"]["label_fraud"], False)  # hard negative kept
        self.assertIsNone(by_id["p3"]["label_fraud"])     # unlabeled != benign
        self.assertIsNone(by_id["p3"]["label_scenario"])
        # feature passthrough
        self.assertEqual(by_id["p1"]["ltv_cents"], 200000)
        self.assertEqual(by_id["p1"]["in_degree"], 2)

    def test_hygiene_and_label_filter(self):
        rows = ts.fraud_feature_rows([
            node_row("p1"),
            node_row("px", tenant=None),            # no tenant -> dropped
            node_row("o1", label="Offering"),        # non-Person -> dropped
        ])
        self.assertEqual([r["person_id"] for r in rows], ["p1"])

    def test_no_labels_extract_yields_null_labels(self):
        rows = ts.fraud_feature_rows([node_row("p1")], [])
        self.assertIsNone(rows[0]["label_fraud"])


class TestCreditFeatureRows(unittest.TestCase):
    def test_derived_avg_and_first_txn_join(self):
        cac = [
            {"event_name": "lead_created", "entity_id": "p1", "amount_ngn": None,
             "event_ts": "2026-01-01T00:00:00Z"},
            {"event_name": "first_txn", "entity_id": "p1", "amount_ngn": 5000.0,
             "event_ts": "2026-02-01T00:00:00Z"},
            {"event_name": "first_txn", "entity_id": "p1", "amount_ngn": 9000.0,
             "event_ts": "2026-01-15T00:00:00Z"},  # EARLIER first_txn wins
        ]
        rows = ts.credit_feature_rows([node_row("p1")], cac)
        self.assertEqual(len(rows), 1)
        row = rows[0]
        self.assertEqual(row["avg_booking_cents"], 50000.0)  # 200000/4
        self.assertEqual(row["first_txn_ngn"], 9000.0)
        self.assertEqual(row["first_txn_at"], "2026-01-15T00:00:00Z")

    def test_missing_join_keys_are_null_not_fabricated(self):
        rows = ts.credit_feature_rows([node_row("p9")], [])
        self.assertIsNone(rows[0]["first_txn_ngn"])
        zero_bookings = ts.credit_feature_rows(
            [node_row("p1", bookings_total=0, ltv_cents=None)], []
        )
        self.assertIsNone(zero_bookings[0]["avg_booking_cents"])

    def test_hygiene(self):
        rows = ts.credit_feature_rows([
            node_row("p1"),
            node_row("px", tenant=None),
            node_row("c1", label="Campaign"),
        ])
        self.assertEqual([r["person_id"] for r in rows], ["p1"])


class TestGnnRows(unittest.TestCase):
    def test_node_passthrough_hygiene_and_order(self):
        rows = ts.gnn_node_rows([
            node_row("p2"),
            node_row("p1"),
            node_row("px", tenant=None),
        ])
        self.assertEqual([r["node_id"] for r in rows], ["p1", "p2"])

    def test_edge_passthrough_hygiene(self):
        rows = ts.gnn_edge_rows([
            edge_row("p1", "p2"),
            edge_row("p1", None),             # dangling dst -> dropped
            edge_row("p1", "p3", tenant=None),
        ])
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]["dst_id"], "p2")


class TestManifest(unittest.TestCase):
    def test_schema_validation(self):
        stats = ts.compute_feature_stats([node_row()], ts.PERSON_NUMERIC_FEATURES)
        m = ts.build_manifest(
            "fraud_features", "2026-08-06",
            [{"kind": "table", "path": "iceberg.gold.graph_node_features"}],
            {"rows": 1}, stats, seed="42", created_at="2026-08-06T00:00:00+00:00",
        )
        self.assertEqual(m["schema_version"], "training-manifest-v1")
        self.assertEqual(m["family"], "fraud_features")
        self.assertEqual(m["snapshot_date"], "2026-08-06")
        self.assertEqual(m["seed"], "42")
        self.assertEqual(m["row_counts"], {"rows": 1})
        self.assertTrue(m["path"].endswith("fraud_features/2026-08-06/"))
        self.assertIn("synthetic", m["notes"].lower())  # I3 honesty
        for field, s in m["feature_stats"].items():
            self.assertEqual(
                sorted(s.keys()), ["count", "max", "mean", "min", "std"], field
            )
        # reference stats required by the W33-C drift PSI monitor are present
        self.assertIn("ltv_cents", m["feature_stats"])

    def test_all_families_accepted(self):
        for family in ts.FAMILIES:
            m = ts.build_manifest(family, "2026-08-06", [], {}, {})
            self.assertEqual(m["family"], family)
        self.assertEqual(
            ts.FAMILIES, ("fraud_features", "credit_features", "gnn_export")
        )

    def test_unknown_family_and_bad_date_rejected(self):
        with self.assertRaises(ValueError):
            ts.build_manifest("bogus", "2026-08-06", [], {}, {})
        with self.assertRaises(ValueError):
            ts.build_manifest("fraud_features", "06-08-2026", [], {}, {})

    def test_serialization_is_canonical(self):
        m1 = ts.build_manifest(
            "gnn_export", "2026-08-06", [{"kind": "table", "path": "x"}],
            {"nodes": 1}, {}, created_at="2026-08-06T00:00:00+00:00",
        )
        m2 = ts.build_manifest(
            "gnn_export", "2026-08-06", [{"kind": "table", "path": "x"}],
            {"nodes": 1}, {}, created_at="2026-08-06T00:00:00+00:00",
        )
        text = ts.manifest_json(m1)
        self.assertEqual(text, ts.manifest_json(m2))  # byte-stable
        parsed = json.loads(text)
        self.assertEqual(parsed["schema_version"], "training-manifest-v1")


if __name__ == "__main__":
    unittest.main()
