// Package provider implements the outbound messaging provider clients
// (Termii, Africa's Talking, WhatsApp Cloud API) with a shared HTTP
// machinery: 10s client timeout, up to 2 retries on 5xx/429/transport
// errors, no retry on 4xx, and structured logging that never includes the
// message body or full recipient (PII).
package provider

import (
	"math/rand"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/opendesk/messaging-gateway/internal/metrics"
	"go.uber.org/zap"
)

const (
	maxAttempts = 3 // 1 try + 2 retries
	maxBodyLog  = 512
)

// Error is a provider-side failure. StatusCode is the provider HTTP status
// (0 for transport errors); Body is the truncated provider response body.
type Error struct {
	StatusCode int
	Body       string
}

func (e *Error) Error() string {
	if e.StatusCode == 0 {
		return "provider unreachable: " + e.Body
	}
	return fmt.Sprintf("provider status %d: %s", e.StatusCode, e.Body)
}

// ClientError reports whether the error is a provider 4xx (caller fault —
// mapped to 400 by the API layer, never retried). 429 is excluded: it is
// rate limiting and is retried like a 5xx.
func ClientError(err error) bool {
	pe, ok := err.(*Error)
	return ok && pe.StatusCode >= 400 && pe.StatusCode < 500 && pe.StatusCode != http.StatusTooManyRequests
}

// requestBuilder creates a fresh *http.Request for one attempt (bodies
// cannot be replayed, so every attempt rebuilds the request).
type requestBuilder func(ctx context.Context) (*http.Request, error)

// Client is the shared per-provider HTTP machinery.
type Client struct {
	Provider string // "termii" | "africastalking" | "whatsapp"
	HC       *http.Client
	Metrics  *metrics.Registry
	Log      *zap.Logger

	// sleep is the retry backoff hook; overridden in tests.
	sleep func(ctx context.Context, attempt int)
}

// NewClient builds a Client with a 10s-timeout http.Client.
func NewClient(provider string, m *metrics.Registry, log *zap.Logger) *Client {
	return &Client{
		Provider: provider,
		HC:       &http.Client{Timeout: 10 * time.Second},
		Metrics:  m,
		Log:      log,
		sleep:    defaultSleep,
	}
}

// backoffDelay computes the N-06 jittered exponential backoff for one retry
// attempt (attempt starts at 1): base = 100ms << (attempt-1) capped at 1s,
// with ±25% jitter so simultaneous callers (failover chains across
// processes) never retry in lockstep. rnd is injectable for tests.
func backoffDelay(attempt int, rnd *rand.Rand) time.Duration {
	base := 100 * time.Millisecond
	for i := 1; i < attempt && base < time.Second; i++ {
		base *= 2
	}
	if base > time.Second {
		base = time.Second
	}
	lo := base * 3 / 4
	spread := base / 2
	return lo + time.Duration(rnd.Int63n(int64(spread)))
}

func defaultSleep(ctx context.Context, attempt int) {
	t := time.NewTimer(backoffDelay(attempt, rand.New(rand.NewSource(time.Now().UnixNano()))))
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// send executes the request with retry: retries (2x) on 5xx, 429 and
// transport errors; 4xx fails immediately. Returns the final provider
// status code and (truncated) response body on success.
func (c *Client) send(ctx context.Context, build requestBuilder) (int, []byte, error) {
	start := time.Now()
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			c.sleep(ctx, attempt-1)
		}
		req, err := build(ctx)
		if err != nil {
			return 0, nil, err // build failure: caller bug, no retry
		}
		status, body, perr := c.doOnce(req)
		if perr == nil {
			c.record("success", attempt, status, start)
			return status, body, nil
		}
		lastErr = perr
		if ClientError(perr) {
			c.record("client_error", attempt, status, start)
			return status, body, perr
		}
		if ctx.Err() != nil {
			break
		}
	}
	status := 0
	if pe, ok := lastErr.(*Error); ok {
		status = pe.StatusCode
	}
	result := "provider_error"
	if status == 0 {
		result = "transport_error"
	}
	c.record(result, maxAttempts, status, start)
	return status, nil, lastErr
}

// doOnce performs a single HTTP attempt and classifies the outcome.
func (c *Client) doOnce(req *http.Request) (int, []byte, *Error) {
	resp, err := c.HC.Do(req)
	if err != nil {
		return 0, nil, &Error{StatusCode: 0, Body: err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode >= 400 {
		return resp.StatusCode, body, &Error{
			StatusCode: resp.StatusCode,
			Body:       truncate(string(body)),
		}
	}
	return resp.StatusCode, body, nil
}

// record writes the counter and the structured log line. Never logs the
// message body (PII).
func (c *Client) record(result string, attempts, status int, start time.Time) {
	if c.Metrics != nil {
		c.Metrics.IncSend(c.Provider, result)
	}
	if c.Log != nil {
		c.Log.Info("provider send",
			zap.String("provider", c.Provider),
			zap.String("result", result),
			zap.Int("attempts", attempts),
			zap.Int("provider_status", status),
			zap.Int64("duration_ms", time.Since(start).Milliseconds()))
	}
}

func truncate(s string) string {
	if len(s) > maxBodyLog {
		return s[:maxBodyLog]
	}
	return s
}
