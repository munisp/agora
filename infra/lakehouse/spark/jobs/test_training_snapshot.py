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
import tempfile
import unittest

JOBS_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, JOBS_DIR)

import training_snapshot as ts  # noqa: E402

# The REAL drift manifest consumer (SPEC-W34 GF2 roundtrip proof):
# services/model-registry/model_registry/drift.py — pure stdlib, imports
# cleanly on driver-less CI boxes.
MODEL_REGISTRY_DIR = os.path.abspath(
    os.path.join(JOBS_DIR, "..", "..", "..", "..",
                 "services", "model-registry", "model_registry"))
sys.path.insert(0, MODEL_REGISTRY_DIR)

import drift  # noqa: E402  model_registry/drift.py


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
            "feature_histogram", "compute_feature_histograms",
            "histogram_edges", "histogram_counts_on_edges",
            "empty_score_baseline", "manifest_hash", "verify_manifest_hash",
            "registry_manifest", "sync_registry_manifests",
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


class TestHistograms(unittest.TestCase):
    """Drift-contract histograms (SPEC-W34 GF2): fixed-bin, deterministic,
    binned with exactly the drift.py histogram_counts semantics."""

    def test_edges_and_counts_cover_all_samples(self):
        values = [float(i) for i in range(100)]
        h = ts.feature_histogram(values)
        edges, counts = h["edges"], h["counts"]
        self.assertEqual(len(edges), ts.HISTOGRAM_BINS + 1)
        self.assertEqual(len(counts), ts.HISTOGRAM_BINS)
        self.assertEqual(sum(counts), 100)
        self.assertEqual(edges[0], 0.0)
        self.assertEqual(edges[-1], 99.0)
        self.assertTrue(all(b > a for a, b in zip(edges, edges[1:])))
        # uniform 0..99 over 10 equal-width bins -> 10 per bin
        self.assertEqual(counts, [10] * 10)

    def test_binning_matches_drift_semantics(self):
        # boundary values land exactly where drift.py's histogram_counts
        # (bisect_right, edge-clamped) puts them
        edges = [0.0, 1.0, 2.0]
        self.assertEqual(ts.histogram_counts_on_edges([0.5, 1.0, 5.0], edges),
                         [1, 2])  # 1.0 -> upper bin; 5.0 clamps to last

    def test_degenerate_single_value_range(self):
        h = ts.feature_histogram([7, 7, 7])
        edges, counts = h["edges"], h["counts"]
        self.assertTrue(all(b > a for a, b in zip(edges, edges[1:])))
        self.assertAlmostEqual(edges[0], 6.5)
        self.assertAlmostEqual(edges[-1], 7.5)
        self.assertEqual(sum(counts), 3)
        # all values land in ONE central bin (7.0 is an exact edge midpoint)
        self.assertEqual(max(counts), 3)

    def test_none_bool_skipped_and_empty_omitted(self):
        self.assertIsNone(ts.feature_histogram([None, True]))
        hists = ts.compute_feature_histograms(
            [{"a": 1, "b": None}, {"a": 3, "b": None}], ("a", "b"))
        self.assertIn("a", hists)
        self.assertNotIn("b", hists)
        self.assertEqual(
            sorted(hists["a"].keys()), ["histogram"])

    def test_deterministic(self):
        rows = [node_row(f"p{i}", ltv_cents=1000 * i) for i in range(50)]
        h1 = ts.compute_feature_histograms(rows, ts.PERSON_NUMERIC_FEATURES)
        h2 = ts.compute_feature_histograms(rows, ts.PERSON_NUMERIC_FEATURES)
        self.assertEqual(json.dumps(h1, sort_keys=True),
                         json.dumps(h2, sort_keys=True))


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

    def test_drift_contract_fields(self):
        """SPEC-W34 GF2: every manifest ALSO carries the exact contract
        drift.py's DirectoryManifestProvider/DriftJob consume."""
        rows = [node_row(f"p{i}", ltv_cents=1000 * (i + 1)) for i in range(30)]
        stats = ts.compute_feature_stats(rows, ts.PERSON_NUMERIC_FEATURES)
        hists = ts.compute_feature_histograms(rows, ts.PERSON_NUMERIC_FEATURES)
        m = ts.build_manifest(
            "fraud_features", "2026-08-06",
            [{"kind": "table", "path": "iceberg.gold.graph_node_features"}],
            {"rows": 30}, stats, seed="42",
            created_at="2026-08-06T00:00:00+00:00", feature_histograms=hists,
        )
        self.assertEqual(m["schema"], "opendesk/training-manifest/v1")
        # features.<name>.histogram.{edges,counts} — the drift PSI input
        self.assertEqual(sorted(m["features"]), sorted(hists.keys()))
        for name, entry in m["features"].items():
            hist = entry["histogram"]
            self.assertEqual(len(hist["edges"]), ts.HISTOGRAM_BINS + 1, name)
            self.assertEqual(len(hist["counts"]), ts.HISTOGRAM_BINS, name)
            self.assertEqual(sum(hist["counts"]), 30, name)
            self.assertTrue(
                all(b > a for a, b in zip(hist["edges"], hist["edges"][1:])),
                name)
        # score baseline is the documented-empty variant (labels, not scores)
        self.assertEqual(m["score_baseline"]["histogram"],
                         {"edges": [], "counts": []})
        self.assertIn("snapshot", m["score_baseline"]["note"])
        # manifest_hash: sha256:<hex>, verifies over the hash-less body
        self.assertRegex(m["manifest_hash"], r"^sha256:[0-9a-f]{64}$")
        self.assertTrue(ts.verify_manifest_hash(m))
        tampered = dict(m)
        tampered["row_counts"] = {"rows": 31}
        self.assertFalse(ts.verify_manifest_hash(tampered))

    def test_legacy_and_drift_histograms_agree_on_range(self):
        rows = [node_row(f"p{i}", ltv_cents=100 * i) for i in range(20)]
        stats = ts.compute_feature_stats(rows, ts.PERSON_NUMERIC_FEATURES)
        hists = ts.compute_feature_histograms(rows, ts.PERSON_NUMERIC_FEATURES)
        edges = hists["ltv_cents"]["histogram"]["edges"]
        self.assertEqual(edges[0], stats["ltv_cents"]["min"])
        self.assertEqual(edges[-1], stats["ltv_cents"]["max"])

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


class TestRegistrySync(unittest.TestCase):
    """SPEC-W34 GF2: snapshot manifests -> $DRIFT_MANIFEST_DIR registry
    manifests via the explicit family mapping, loaded through the REAL
    drift.py DirectoryManifestProvider."""

    def _write_snapshot_tree(self, base, snapshot_date="2026-08-06"):
        """Small synthetic snapshot: one family dir + manifest.json per
        family, built through the pure reference implementations."""
        manifests = {}
        builders = {
            "fraud_features": (
                ts.fraud_feature_rows(
                    [node_row(f"p{i}", ltv_cents=1000 * (i + 1))
                     for i in range(30)]),
                ts.PERSON_NUMERIC_FEATURES,
            ),
            "credit_features": (
                ts.credit_feature_rows(
                    [node_row(f"p{i}", ltv_cents=2000 * (i + 1))
                     for i in range(30)]),
                ts.CREDIT_NUMERIC_FEATURES,
            ),
            "gnn_export": (
                ts.gnn_node_rows([node_row(f"p{i}") for i in range(30)]),
                ts.PERSON_NUMERIC_FEATURES,
            ),
        }
        for family, (rows, numeric_fields) in builders.items():
            m = ts.build_manifest(
                family, snapshot_date,
                [{"kind": "table", "path": "synthetic"}],
                {"rows": len(rows)},
                ts.compute_feature_stats(rows, numeric_fields),
                seed="42", created_at="2026-08-06T00:00:00+00:00",
                feature_histograms=ts.compute_feature_histograms(
                    rows, numeric_fields),
            )
            d = os.path.join(base, family, snapshot_date)
            os.makedirs(d, exist_ok=True)
            with open(os.path.join(d, "manifest.json"), "w",
                      encoding="utf-8") as fh:
                fh.write(ts.manifest_json(m))
            manifests[family] = m
        return manifests

    def test_mapping_constant(self):
        self.assertEqual(
            ts.FAMILY_REGISTRY_MAPPING,
            {"fraud_features": "fraud-ml",
             "credit_features": "credit-ml",
             "gnn_export": "graphsage"},
        )

    def test_sync_then_load_through_real_provider(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = os.path.join(tmp, "training")
            registry_dir = os.path.join(tmp, "drift_manifests")
            snapshot_manifests = self._write_snapshot_tree(base)

            written = ts.sync_registry_manifests(
                registry_dir, base_path=base, snapshot_date="2026-08-06")
            self.assertEqual(sorted(written), sorted(ts.FAMILIES))

            provider = drift.DirectoryManifestProvider(registry_dir)
            for snap_family, reg_family in ts.FAMILY_REGISTRY_MAPPING.items():
                path = os.path.join(registry_dir, f"{reg_family}.json")
                self.assertTrue(os.path.isfile(path), path)
                m = provider.get(reg_family)
                self.assertIsNotNone(m, reg_family)
                # registry family name, drift contract schema
                self.assertEqual(m["family"], reg_family)
                self.assertEqual(m["schema"],
                                 "opendesk/training-manifest/v1")
                # histograms survived the round trip byte-intact
                src = snapshot_manifests[snap_family]
                self.assertEqual(m["features"], src["features"])
                for name, entry in m["features"].items():
                    hist = entry["histogram"]
                    self.assertEqual(len(hist["edges"]),
                                     ts.HISTOGRAM_BINS + 1, name)
                    self.assertEqual(len(hist["counts"]),
                                     ts.HISTOGRAM_BINS, name)
                    self.assertGreater(sum(hist["counts"]), 0, name)
                # score baseline + integrity hash (recomputed over the
                # translated body) intact
                self.assertEqual(m["score_baseline"]["histogram"],
                                 {"edges": [], "counts": []})
                self.assertTrue(ts.verify_manifest_hash(m), reg_family)
                # the histograms are directly usable by drift.py's PSI path:
                # re-binning the reference counts' own edges is a no-op PSI
                ltv = m["features"].get("ltv_cents")
                if ltv is not None:
                    hist = ltv["histogram"]
                    same = drift.psi(hist["counts"], hist["counts"])
                    self.assertAlmostEqual(same, 0.0)
            # unknown family -> honest None (I1)
            self.assertIsNone(provider.get("no-such-family"))

    def test_sync_skips_missing_family_manifest(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = os.path.join(tmp, "training")
            registry_dir = os.path.join(tmp, "drift_manifests")
            manifests = self._write_snapshot_tree(base)
            os.remove(os.path.join(base, "gnn_export", "2026-08-06",
                                   "manifest.json"))
            written = ts.sync_registry_manifests(
                registry_dir, base_path=base, snapshot_date="2026-08-06")
            self.assertEqual(sorted(written),
                             ["credit_features", "fraud_features"])
            provider = drift.DirectoryManifestProvider(registry_dir)
            self.assertIsNotNone(provider.get("fraud-ml"))
            self.assertIsNone(provider.get("graphsage"))
            self.assertTrue(manifests)  # tree was built

    def test_registry_manifest_rejects_unmapped_family(self):
        with self.assertRaises(ValueError):
            ts.registry_manifest({"family": "nope"})

    def test_sync_rejects_non_local_paths(self):
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(ValueError):
                ts.sync_registry_manifests(
                    tmp, base_path="s3://lake/training/")


if __name__ == "__main__":
    unittest.main()
