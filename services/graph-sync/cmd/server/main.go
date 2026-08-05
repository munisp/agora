// graph-sync: Kafka consumer group "graph-sync" that materializes the
// per-tenant knowledge graph (SPEC-W28 §4 WS-A) into FalkorDB via
// Redis-protocol Cypher. Topics are env-configured (SPEC §2 documented
// defaults), upserts are idempotent by event_id, poison messages
// dead-letter to opendesk.dlq, erasure events propagate to the graph with
// an audit trail on opendesk.graph.erasure.done.v1. Layout mirrors
// crm-sync-service (multi-topic consumers + tiny metrics registry +
// /healthz|/metrics sidecar).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/opendesk/graph-sync/internal/config"
	"github.com/opendesk/graph-sync/internal/consumer"
	"github.com/opendesk/graph-sync/internal/embed"
	"github.com/opendesk/graph-sync/internal/events"
	"github.com/opendesk/graph-sync/internal/graph"
	"github.com/opendesk/graph-sync/internal/httpapi"
	"github.com/opendesk/graph-sync/internal/metrics"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

// kafkaAuditProducer emits the erasure-done audit CloudEvent via a direct
// Kafka writer (same posture as the DLQ writers — no Dapr dependency in
// this service).
type kafkaAuditProducer struct {
	w *kafka.Writer
}

func (p *kafkaAuditProducer) Publish(ctx context.Context, topic, key string, evt events.CloudEvent) error {
	payload, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return p.w.WriteMessages(writeCtx, kafka.Message{
		Topic: topic,
		Key:   []byte(key),
		Value: payload,
		Time:  time.Now(),
	})
}

func (p *kafkaAuditProducer) Close() error { return p.w.Close() }

func run() error {
	logger, err := zap.NewProduction()
	if err != nil {
		return err
	}
	defer logger.Sync() //nolint:errcheck

	cfg := config.Load()
	if cfg.PhoneHashSalt == "" {
		logger.Warn("PHONE_HASH_SALT is empty; phone hashes are unsalted (dev posture only)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// FalkorDB graph client (Redis-protocol Cypher; the Client seam is
	// faked in tests).
	gc := graph.NewFalkorDB(cfg.FalkorDBAddr, cfg.FalkorDBGraph)
	defer gc.Close() //nolint:errcheck

	reg := metrics.New()

	// Ollama embeddings (graceful degradation: unreachable → merge
	// proposals skipped, exact phone_hash merges unaffected).
	emb := embed.NewOllama(cfg.OllamaBaseURL, cfg.OllamaEmbedModel, nil)

	brokers := []string{cfg.KafkaBrokers}
	audit := &kafkaAuditProducer{w: &kafka.Writer{
		Addr:         kafka.TCP(brokers...),
		Balancer:     &kafka.Hash{},
		RequiredAcks: kafka.RequireOne,
	}}
	defer audit.Close() //nolint:errcheck

	syncer := &consumer.Syncer{
		Graph:            gc,
		Embed:            emb,
		Audit:            audit,
		Metrics:          reg,
		Log:              logger,
		Salt:             cfg.PhoneHashSalt,
		MergeThreshold:   cfg.MergeThreshold,
		ErasureDoneTopic: cfg.ErasureDoneTopic,
	}

	g, gctx := errgroup.WithContext(ctx)

	if cfg.ConsumerEnabled {
		consumers := []*consumer.Consumer{
			consumer.New(brokers, cfg.BookingTopic, cfg.ConsumerGroup, cfg.DLQTopic, syncer.HandleBooking, reg, logger),
			consumer.New(brokers, cfg.IdentityTopic, cfg.ConsumerGroup, cfg.DLQTopic, syncer.HandleIdentity, reg, logger),
			consumer.New(brokers, cfg.TranscriptsTopic, cfg.ConsumerGroup, cfg.DLQTopic, syncer.HandleTranscript, reg, logger),
			consumer.New(brokers, cfg.ErasureTopic, cfg.ConsumerGroup, cfg.DLQTopic, syncer.HandleErasure, reg, logger),
		}
		if cfg.CACTopic != "" {
			consumers = append(consumers,
				consumer.New(brokers, cfg.CACTopic, cfg.ConsumerGroup, cfg.DLQTopic, syncer.HandleCAC, reg, logger))
		} else {
			logger.Info("GRAPH_SYNC_CAC_TOPIC is empty; funnel/CAC consumer skipped")
		}
		// Nightly gold→graph enrichment (spark graph_enrichment.py). Same
		// group pattern; empty topic = skipped (logged).
		if cfg.EnrichmentTopic != "" {
			consumers = append(consumers,
				consumer.New(brokers, cfg.EnrichmentTopic, cfg.ConsumerGroup, cfg.DLQTopic, syncer.HandleEnrichment, reg, logger))
		} else {
			logger.Info("GRAPH_SYNC_ENRICHMENT_TOPIC is empty; enrichment consumer skipped")
		}
		for _, c := range consumers {
			c := c
			g.Go(func() error { return c.Run(gctx) })
			defer c.Close() //nolint:errcheck
		}
	} else {
		logger.Warn("CONSUMER_ENABLED=false; Kafka consumers disabled")
	}

	// HTTP sidecar: /healthz (FalkorDB ping) + /metrics.
	srv := &http.Server{
		Addr: fmt.Sprintf(":%d", cfg.Port),
		Handler: (&httpapi.Server{
			Graph:         gc,
			Metrics:       reg,
			EmbedDegraded: emb.Degraded,
		}).Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	g.Go(func() error {
		logger.Info("http listening", zap.Int("port", cfg.Port))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error {
		<-gctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		return srv.Shutdown(shCtx)
	})

	logger.Info("graph-sync started",
		zap.String("falkordb", cfg.FalkorDBAddr), zap.String("graph", cfg.FalkorDBGraph),
		zap.String("group", cfg.ConsumerGroup), zap.String("ollama", cfg.OllamaBaseURL))
	return g.Wait()
}
