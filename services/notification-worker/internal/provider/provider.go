// Package provider implements the outbound push-notification provider
// clients (SPEC-W16 Agent A): FCM (HTTP v1 / legacy server key, FCM_MOCK=1
// deterministic mock default) and APNs (stub only — interface + config +
// documented TODO, no fake implementation claims).
//
// The shared HTTP machinery mirrors messaging-gateway/internal/provider:
// 10s client timeout, up to 2 retries on 5xx/429/transport errors, no retry
// on 4xx, and structured logging that never includes the notification body
// or the full device token (PII). notification-worker has no metrics
// registry, so the Client only logs (the messaging-gateway Client also
// counts; the call pattern is otherwise identical).
package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"
)

const (
	maxAttempts = 3 // 1 try + 2 retries
	maxBodyLog  = 512
)

// Error is a provider-side failure. StatusCode is the provider HTTP status
// (0 for transport errors and for local failures such as "not configured");
// Body is the truncated provider response body (or the local reason).
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
// never retried). 429 is excluded: it is rate limiting and is retried like
// a 5xx.
func ClientError(err error) bool {
	pe, ok := err.(*Error)
	return ok && pe.StatusCode >= 400 && pe.StatusCode < 500 && pe.StatusCode != http.StatusTooManyRequests
}

// PushMessage is one push notification to one device token.
type PushMessage struct {
	Token string // FCM registration token / APNs device token
	Title string
	Body  string
	// Data is the custom key/value payload. Values are strings only: the
	// FCM data message contract requires string values, and the activity
	// layer enforces the same so mock and live modes behave identically.
	Data map[string]string
}

// PushProvider is the contract every outbound push provider implements
// (mirrors the SMSProvider interface of messaging-gateway's failover chain).
type PushProvider interface {
	// Name is the provider id ("fcm" | "apns").
	Name() string
	// Configured reports whether the provider has the credentials it needs
	// (mock mode counts as configured: it needs no credentials).
	Configured() bool
	// SendPush delivers one notification to msg.Token, returning the
	// provider HTTP status and (truncated) response body on success.
	SendPush(ctx context.Context, msg PushMessage) (int, []byte, error)
}

// requestBuilder creates a fresh *http.Request for one attempt (bodies
// cannot be replayed, so every attempt rebuilds the request).
type requestBuilder func(ctx context.Context) (*http.Request, error)

// Client is the shared per-provider HTTP machinery.
type Client struct {
	Provider string // "fcm" | "apns"
	HC       *http.Client
	Log      *zap.Logger

	// sleep is the retry backoff hook; overridden in tests.
	sleep func(ctx context.Context, attempt int)
}

// NewClient builds a Client with a 10s-timeout http.Client. A nil logger
// becomes a no-op logger.
func NewClient(provider string, log *zap.Logger) *Client {
	if log == nil {
		log = zap.NewNop()
	}
	return &Client{
		Provider: provider,
		HC:       &http.Client{Timeout: 10 * time.Second},
		Log:      log,
		sleep:    defaultSleep,
	}
}

func defaultSleep(ctx context.Context, attempt int) {
	t := time.NewTimer(time.Duration(attempt) * 100 * time.Millisecond)
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
			return 0, nil, err // build failure: caller bug / auth failure, no retry
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

// record writes the structured log line. Never logs the notification body
// or the device token (PII).
func (c *Client) record(result string, attempts, status int, start time.Time) {
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
