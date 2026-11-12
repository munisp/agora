//! /metrics — hand-rolled Prometheus text exposition (no client dep; the
//! series set is fixed and tiny). Counters only, per house /metrics
//! convention.

use std::sync::atomic::{AtomicU64, Ordering};

#[derive(Default)]
pub struct Metrics {
    pub runs_ok: AtomicU64,
    pub runs_error: AtomicU64,
    pub triples_total: AtomicU64,
    pub nodes_total: AtomicU64,
    pub edges_total: AtomicU64,
    pub skipped_unknown_total: AtomicU64,
    pub last_run_duration_ms: AtomicU64,
}

impl Metrics {
    pub fn observe_run(&self, result: &crate::routes::RunOutcome) {
        self.nodes_total.fetch_add(result.nodes, Ordering::Relaxed);
        self.edges_total.fetch_add(result.edges, Ordering::Relaxed);
        self.triples_total
            .fetch_add(result.triples, Ordering::Relaxed);
        self.skipped_unknown_total
            .fetch_add(result.skipped_unknown, Ordering::Relaxed);
        self.last_run_duration_ms
            .store(result.duration_ms, Ordering::Relaxed);
    }

    pub fn render(&self) -> String {
        let g = |a: &AtomicU64| a.load(Ordering::Relaxed);
        format!(
            "# HELP rdf_export_runs_total Export runs by result.\n\
             # TYPE rdf_export_runs_total counter\n\
             rdf_export_runs_total{{result=\"ok\"}} {}\n\
             rdf_export_runs_total{{result=\"error\"}} {}\n\
             # HELP rdf_export_nodes_total Source node rows consumed.\n\
             # TYPE rdf_export_nodes_total counter\n\
             rdf_export_nodes_total {}\n\
             # HELP rdf_export_edges_total Source edge rows consumed.\n\
             # TYPE rdf_export_edges_total counter\n\
             rdf_export_edges_total {}\n\
             # HELP rdf_export_triples_total RDF quads produced.\n\
             # TYPE rdf_export_triples_total counter\n\
             rdf_export_triples_total {}\n\
             # HELP rdf_export_skipped_unknown_total Rows skipped (unknown label/predicate or unsafe id).\n\
             # TYPE rdf_export_skipped_unknown_total counter\n\
             rdf_export_skipped_unknown_total {}\n\
             # HELP rdf_export_last_run_duration_ms Duration of the last export run.\n\
             # TYPE rdf_export_last_run_duration_ms gauge\n\
             rdf_export_last_run_duration_ms {}\n",
            g(&self.runs_ok),
            g(&self.runs_error),
            g(&self.nodes_total),
            g(&self.edges_total),
            g(&self.triples_total),
            g(&self.skipped_unknown_total),
            g(&self.last_run_duration_ms),
        )
    }
}
