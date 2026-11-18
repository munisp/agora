package recon

// SPEC-W78 A2: count-level observability for CLEAN receipt↔payment pairs
// (the W64 honest-register gap — clean pairs emit no finding, so they were
// invisible). The counter registers on the DEFAULT Prometheus registry,
// which the existing /metrics promhttp handler already serves
// (httpapi/server.go) — no wiring change. Per-pair events stay rejected
// (W64: volume vs value); this is a single monotonic counter.

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// cleanPairsTotal counts receipt↔payment pairs judged clean by
// classifyCapture's default branch (matched, amounts comparable AND agree,
// no finding emitted). Incremented in sweepTenant at the same site where
// the per-tenant clean count for Summary.CleanPairs accrues, so the counter
// delta over a successful run equals Summary.CleanPairs. Never incremented
// for not-comparable heuristic pairs or when any finding fires.
var cleanPairsTotal = promauto.NewCounter(prometheus.CounterOpts{
	Name: "booking_recon_clean_pairs_total",
	Help: "Receipt/payment pairs judged clean (matched, amounts comparable and equal, no finding) by the reconciliation sweep.",
})
