package consumer

// SPEC-W80 A10: consumer.go retry/DLQ unit tests (3-attempts-then-DLQ,
// permanent-error fast-track, commit-after-success, no-DLQ-on-cancel — A5).
// The Consumer's reader/writer surfaces are small interfaces (kafka.Reader
// / kafka.Writer satisfy them), so these tests run without a broker.

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/opendesk/crm-sync-service/internal/events"
	"github.com/opendesk/crm-sync-service/internal/metrics"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// fakeReader serves a fixed queue of messages, then reports the consumer
// context as done (which Run treats as a clean shutdown).
type fakeReader struct {
	msgs      []kafka.Message
	idx       int
	committed int
}

func (f *fakeReader) FetchMessage(ctx context.Context) (kafka.Message, error) {
	if f.idx >= len(f.msgs) {
		return kafka.Message{}, context.Canceled
	}
	m := f.msgs[f.idx]
	f.idx++
	return m, nil
}

func (f *fakeReader) CommitMessages(_ context.Context, msgs ...kafka.Message) error {
	f.committed += len(msgs)
	return nil
}

func (f *fakeReader) Close() error { return nil }

// fakeDLQ records dead-letter writes.
type fakeDLQ struct {
	written int
	lastErr error
}

func (f *fakeDLQ) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	f.written += len(msgs)
	return nil
}

func (f *fakeDLQ) Close() error { return nil }

func testMessage(t *testing.T) kafka.Message {
	t.Helper()
	ce := events.New("evt-1", "test", "com.opendesk.test.Thing", "acme", "", map[string]any{})
	b, err := json.Marshal(ce)
	if err != nil {
		t.Fatal(err)
	}
	return kafka.Message{Key: []byte("k-1"), Value: b}
}

func newTestConsumer(reader *fakeReader, dlq *fakeDLQ, h Handler) *Consumer {
	return &Consumer{
		topic:   "test-topic",
		reader:  reader,
		dlq:     dlq,
		handler: h,
		metrics: metrics.New(),
		log:     zap.NewNop(),
	}
}

// A10: a transiently failing handler is retried maxAttempts times, then the
// message is dead-lettered and the offset committed.
func TestRunDLQAfterMaxAttempts(t *testing.T) {
	reader := &fakeReader{msgs: []kafka.Message{testMessage(t)}}
	dlq := &fakeDLQ{}
	var attempts int32
	c := newTestConsumer(reader, dlq, func(context.Context, events.CloudEvent) error {
		atomic.AddInt32(&attempts, 1)
		return errors.New("transient boom")
	})
	if err := c.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != maxAttempts {
		t.Errorf("attempts = %d, want %d (retry budget exhausted before DLQ)", got, maxAttempts)
	}
	if dlq.written != 1 {
		t.Errorf("dlq writes = %d, want 1", dlq.written)
	}
	if reader.committed != 1 {
		t.Errorf("commits = %d, want 1 (offset committed after successful DLQ)", reader.committed)
	}
}

// A10: permanent (poison) errors dead-letter IMMEDIATELY — no retry budget
// is burned on a payload that cannot heal.
func TestRunPermanentErrorFastTrack(t *testing.T) {
	reader := &fakeReader{msgs: []kafka.Message{testMessage(t)}}
	dlq := &fakeDLQ{}
	var attempts int32
	c := newTestConsumer(reader, dlq, func(context.Context, events.CloudEvent) error {
		atomic.AddInt32(&attempts, 1)
		return permanent(errors.New("poison payload"))
	})
	if err := c.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1 (permanent errors fast-track to DLQ)", got)
	}
	if dlq.written != 1 {
		t.Errorf("dlq writes = %d, want 1", dlq.written)
	}
}

// A10: a successful message is committed and never touches the DLQ.
func TestRunCommitAfterSuccess(t *testing.T) {
	reader := &fakeReader{msgs: []kafka.Message{testMessage(t), testMessage(t)}}
	dlq := &fakeDLQ{}
	var handled int32
	c := newTestConsumer(reader, dlq, func(context.Context, events.CloudEvent) error {
		atomic.AddInt32(&handled, 1)
		return nil
	})
	if err := c.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt32(&handled); got != 2 {
		t.Errorf("handled = %d, want 2", got)
	}
	if dlq.written != 0 {
		t.Errorf("dlq writes = %d, want 0", dlq.written)
	}
	if reader.committed != 2 {
		t.Errorf("commits = %d, want 2 (commit after success)", reader.committed)
	}
}

// A10 + A5: when the run context is cancelled mid-processing (graceful
// shutdown), the in-flight message is NOT dead-lettered and NOT committed —
// it redelivers on the next boot instead of polluting the DLQ.
func TestRunNoDLQOnShutdownCancel(t *testing.T) {
	reader := &fakeReader{msgs: []kafka.Message{testMessage(t)}}
	dlq := &fakeDLQ{}
	ctx, cancel := context.WithCancel(context.Background())
	c := newTestConsumer(reader, dlq, func(context.Context, events.CloudEvent) error {
		cancel() // shutdown lands while the handler's error is being retried
		return errors.New("boom during shutdown")
	})
	if err := c.Run(ctx); err != nil {
		t.Fatalf("Run on shutdown must exit cleanly, got %v", err)
	}
	if dlq.written != 0 {
		t.Errorf("A5: dlq writes = %d, want 0 (no dead-lettering on shutdown)", dlq.written)
	}
	if reader.committed != 0 {
		t.Errorf("A5: commits = %d, want 0 (offset left for redelivery)", reader.committed)
	}
}
