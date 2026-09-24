// Package outbox implements the transactional-outbox dispatcher: it polls
// unsent rows and publishes them as CloudEvents to Kafka via the Dapr pubsub
// component `pubsub-kafka`, then marks them sent (SPEC §4/§6).
package outbox

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/daprc"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// Dispatcher polls and publishes outbox rows.
type Dispatcher struct {
	store    *store.Store
	dapr     *daprc.Client
	pubsub   string
	interval time.Duration
	log      *zap.Logger

	// health (SPEC-W44 W-B/F15-13) is the ConsumerHealth flag for
	// "outbox-dispatcher": three consecutive FAILED dispatch cycles clear it
	// (→ /healthz 503 — a wedged sidecar must page, not sit silently while
	// the outbox backs up); the next successful cycle restores it. Nil when
	// unwired (tests, legacy construction).
	health   *atomic.Bool
	failures int // consecutive failed cycles
}

// New builds the dispatcher.
func New(st *store.Store, d *daprc.Client, pubsub string, interval time.Duration, log *zap.Logger) *Dispatcher {
	return &Dispatcher{store: st, dapr: d, pubsub: pubsub, interval: interval, log: log}
}

// WithHealth wires the /healthz liveness flag (SPEC-W44 W-B/F15-13): see the
// health field. Returns the dispatcher for chaining.
func (d *Dispatcher) WithHealth(flag *atomic.Bool) *Dispatcher {
	d.health = flag
	return d
}

// healthFailThreshold consecutive failed cycles before the health flag
// clears (SPEC-W44 W-B/F15-13).
const healthFailThreshold = 3

// noteCycle applies one cycle outcome to the health flag.
func (d *Dispatcher) noteCycle(ok bool) {
	if d.health == nil {
		return
	}
	if ok {
		if d.failures > 0 {
			d.log.Info("outbox dispatcher recovered; restoring health", zap.Int("consecutive_failures", d.failures))
		}
		d.failures = 0
		d.health.Store(true)
		return
	}
	d.failures++
	if d.failures >= healthFailThreshold {
		d.log.Error("outbox dispatcher unhealthy: consecutive failed dispatch cycles",
			zap.Int("consecutive_failures", d.failures))
		d.health.Store(false)
	}
}

// Run loops until ctx is cancelled. Publish failures are retried next cycle
// (at-least-once delivery; consumers must tolerate duplicates).
//
// SPEC-W46 (W46-A item 6, K-03): next to the poll ticker the loop selects on
// the store's flush-on-commit signal (billing-engine pattern) — a write path
// that just committed outbox rows triggers an immediate dispatch cycle
// instead of waiting out the 2s poll. The ticker stays as the fallback
// (missed/coalesced signals, publishes that failed mid-cycle).
func (d *Dispatcher) Run(ctx context.Context) {
	tick := time.NewTicker(d.interval)
	defer tick.Stop()
	flush := d.store.OutboxFlushSignal()
	d.log.Info("outbox dispatcher started", zap.Duration("interval", d.interval))
	for {
		d.noteCycle(d.dispatchOnce(ctx))
		select {
		case <-ctx.Done():
			d.log.Info("outbox dispatcher stopped")
			return
		case <-tick.C:
		case <-flush:
		}
	}
}

// dispatchOnce runs one poll/publish cycle and reports whether it succeeded:
// a failed fetch, any publish failure, or any failed mark-sent counts as a
// failed cycle (an empty fetch with nothing to publish is a success).
//
// SPEC-W46 (W46-A item 6, PERF-07): successfully published (+ poisoned) rows
// are marked sent in ONE batched UPDATE ... WHERE id = ANY($1) at the end of
// the cycle instead of one UPDATE per row. On batch failure the per-row path
// is retried so a single bad id cannot silently un-mark the whole batch.
func (d *Dispatcher) dispatchOnce(ctx context.Context) bool {
	rows, err := d.store.FetchUnsentOutbox(ctx, 100)
	if err != nil {
		if ctx.Err() == nil {
			d.log.Error("fetch unsent outbox", zap.Error(err))
		}
		return false
	}
	ok := true
	sent := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		// payload is already a serialized CloudEvents envelope
		var evt map[string]any
		if err := json.Unmarshal(row.Payload, &evt); err != nil {
			// poison row: mark sent to avoid an infinite hot loop; it is
			// preserved in the table for inspection
			d.log.Error("undeliverable outbox payload, marking sent",
				zap.String("outbox_id", row.ID.String()), zap.Error(err))
			sent = append(sent, row.ID)
			continue
		}
		if err := d.dapr.PublishEvent(ctx, d.pubsub, row.Topic, evt); err != nil {
			d.log.Warn("publish failed, will retry",
				zap.String("outbox_id", row.ID.String()), zap.String("topic", row.Topic), zap.Error(err))
			ok = false
			continue
		}
		sent = append(sent, row.ID)
	}
	if len(sent) > 0 {
		if err := d.store.MarkOutboxSentBatch(ctx, sent); err != nil {
			d.log.Error("batch mark outbox sent; falling back to per-row", zap.Error(err))
			for _, id := range sent {
				if err := d.store.MarkOutboxSent(ctx, id); err != nil {
					d.log.Error("mark outbox sent", zap.String("outbox_id", id.String()), zap.Error(err))
					ok = false
				}
			}
		}
	}
	return ok
}
