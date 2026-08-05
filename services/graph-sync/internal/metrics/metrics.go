// Package metrics is a tiny dependency-free Prometheus text-format registry:
// counters per event type/topic and latency summaries (mirrors
// crm-sync-service/internal/metrics).
package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Registry collects counters and latency summaries.
type Registry struct {
	mu       sync.Mutex
	counters map[string]int64
	latency  map[string]*latStat
}

type latStat struct {
	count int64
	sum   float64 // seconds
	max   float64
}

// New builds an empty registry.
func New() *Registry {
	return &Registry{
		counters: map[string]int64{},
		latency:  map[string]*latStat{},
	}
}

// Inc increments counter `name` by 1.
func (r *Registry) Inc(name string) { r.Add(name, 1) }

// Add increments counter `name` by delta.
func (r *Registry) Add(name string, delta int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters[name] += delta
}

// Observe records one latency observation under `name`.
func (r *Registry) Observe(name string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.latency[name]
	if s == nil {
		s = &latStat{}
		r.latency[name] = s
	}
	sec := d.Seconds()
	s.count++
	s.sum += sec
	if sec > s.max {
		s.max = sec
	}
}

// Render emits the Prometheus text exposition format.
func (r *Registry) Render() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	keys := make([]string, 0, len(r.counters))
	for k := range r.counters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b.WriteString("# HELP graph_sync_counter Counters by name (events processed/failed/DLQ, merges, erasures, embedding degradation).\n")
	b.WriteString("# TYPE graph_sync_counter counter\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "graph_sync_counter{name=%q} %d\n", k, r.counters[k])
	}
	lkeys := make([]string, 0, len(r.latency))
	for k := range r.latency {
		lkeys = append(lkeys, k)
	}
	sort.Strings(lkeys)
	b.WriteString("# HELP graph_sync_latency_seconds Latency summary in seconds by operation.\n")
	b.WriteString("# TYPE graph_sync_latency_seconds summary\n")
	for _, k := range lkeys {
		s := r.latency[k]
		fmt.Fprintf(&b, "graph_sync_latency_seconds_count{op=%q} %d\n", k, s.count)
		fmt.Fprintf(&b, "graph_sync_latency_seconds_sum{op=%q} %.6f\n", k, s.sum)
		fmt.Fprintf(&b, "graph_sync_latency_seconds_max{op=%q} %.6f\n", k, s.max)
	}
	return b.String()
}
