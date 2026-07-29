// Package pacer implements the outbound CPS pacer for notification-worker
// (docs/VOICE-SCALING.md §4 telephony plane, applied to SPEC-W3 §3
// innovation 7 waitlist backfill and the reminder sends).
//
// Telephony discipline from the article: the carrier sets two ceilings —
// channel count (hard cap) and CPS (start rate) — and pacing is ONE knob
// for both CPS compliance and spam-reputation: a smooth, low start rate is
// precisely what keeps sender numbers off carrier spam lists. Sender
// rotation spreads reputation across the number pool.
//
// Temporal safety: workflows must be deterministic, so pacing can never
// live in workflow code. It is implemented here as an activity-side rate
// limiter: the NotifyPaced activity acquires a token from this bucket
// BEFORE invoking the actual send activity.
//
// Backends (PACER_BACKEND):
//
//   - redis (default): a Lua token bucket in the shared redis:6379. This is
//     the ONLY correct choice with more than one worker replica: a
//     process-local limiter paces per replica, so N replicas silently
//     multiply the fleet-wide CPS by N. The bucket key is shared by all
//     notification-worker pods, giving a true fleet-wide start rate.
//   - local: golang.org/x/time/rate limiter shared by all activities in
//     this worker process. Correct only for a single-replica deployment
//     (dev / tests).
//
// Failure policy is FAIL-OPEN-TO-LOCAL: when redis is unreachable the
// pacer logs a warning and falls back to the in-process limiter. Rationale:
// notifications (waitlist claim windows, appointment reminders) are
// time-sensitive; dropping them because the pacing store is down is worse
// than briefly exceeding the CPS target on a single replica. Each replica
// still paces itself locally, so the worst case is replicas × CPS, not an
// unbounded burst. The fallback is re-attempted on every send, so the
// redis backend resumes automatically.
package pacer

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
)

// Redis keys shared by every notification-worker replica.
const (
	bucketKey   = "opendesk:pacer:outbound_bucket"
	rotationKey = "opendesk:pacer:rotation"
)

// bucketScript is an atomic token bucket: it refills at rate tokens/sec up
// to burst, consumes one token when available, and returns the number of
// milliseconds the caller must wait before retrying (0 = token granted).
//
// KEYS[1] = bucket key
// ARGV = rate (tokens/sec), burst, now (unix ms, client clock)
const bucketScript = `
local rate  = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local now   = tonumber(ARGV[3])
local data   = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(data[1]) or burst
local ts     = tonumber(data[2]) or now
tokens = math.min(burst, tokens + (now - ts) / 1000.0 * rate)
local wait_ms = 0
if tokens < 1 then
	wait_ms = math.ceil((1 - tokens) / rate * 1000)
else
	tokens = tokens - 1
end
redis.call('HMSET', KEYS[1], 'tokens', tokens, 'ts', now)
redis.call('PEXPIRE', KEYS[1], math.max(2000, math.ceil(burst / rate * 1000) * 2))
return wait_ms
`

// RedisClient is the minimal go-redis surface the pacer needs, so tests can
// inject fakes (e.g. a redis-down client). *redis.Client satisfies it.
type RedisClient interface {
	Eval(ctx context.Context, script string, keys []string, args ...any) *redis.Cmd
	Incr(ctx context.Context, key string) *redis.IntCmd
}

// Config holds the pacer configuration (loaded from env by config.Load).
type Config struct {
	// CPS is the outbound start rate in sends per second (OUTBOUND_CPS,
	// default 1.0). This is the carrier CPS ceiling AND the spam-reputation
	// knob — do not raise it without carrier agreement.
	CPS float64
	// Burst is the token-bucket capacity (OUTBOUND_BURST, default 3): the
	// maximum sends that may start instantly after an idle period.
	Burst int
	// Backend selects the limiter backend: "redis" (fleet-wide, default) or
	// "local" (this process only).
	Backend string
	// RedisAddr is the shared redis address (REDIS_ADDR, default redis:6379).
	RedisAddr string
	// FromNumbers is the sender rotation pool (OUTBOUND_FROM_NUMBERS,
	// comma-separated). Empty disables rotation (the binding's configured
	// sender is used).
	FromNumbers []string
}

// Pacer is the activity-side outbound rate limiter + sender rotator. It is
// safe for concurrent use by all activities in the worker process.
type Pacer struct {
	cfg   Config
	local *rate.Limiter
	rdb   RedisClient
	rot   atomic.Uint64
	log   *zap.Logger
	// redisWarned ensures the fail-open warning is logged once, not per send.
	redisWarned atomic.Bool
	// Metering (SPEC-W11 Part B §5): every granted start is counted, and
	// priority fast-lane bypasses are counted separately so emergency
	// traffic stays usage-visible despite skipping the token bucket.
	granted  atomic.Uint64
	prioLane atomic.Uint64
}

// New builds the pacer. With Backend "redis" it dials RedisAddr (go-redis
// connects lazily; outages surface at send time and fail open to local).
func New(cfg Config, log *zap.Logger) *Pacer {
	if cfg.CPS <= 0 {
		cfg.CPS = 1
	}
	if cfg.Burst <= 0 {
		cfg.Burst = 1
	}
	p := &Pacer{cfg: cfg, log: log}
	if p.log == nil {
		p.log = zap.NewNop()
	}
	// The local limiter always exists: it is the fail-open path when the
	// redis backend is unreachable.
	p.local = rate.NewLimiter(rate.Limit(cfg.CPS), cfg.Burst)
	if cfg.Backend != "local" {
		p.rdb = redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})
	}
	return p
}

// NewWithClient builds a redis-backed pacer on an injected client (tests).
func NewWithClient(cfg Config, rdb RedisClient, log *zap.Logger) *Pacer {
	p := New(cfg, log)
	p.rdb = rdb
	return p
}

// Numbers reports the configured sender rotation pool.
func (p *Pacer) Numbers() []string { return p.cfg.FromNumbers }

// Wait blocks until the token bucket grants one outbound start or ctx is
// cancelled. With the redis backend it acquires from the fleet-wide bucket;
// on any redis error it logs once and falls back to the local limiter
// (fail-open, see package doc).
func (p *Pacer) Wait(ctx context.Context) error {
	if p.rdb != nil {
		for {
			waitMs, err := p.acquireRedis(ctx)
			if err != nil {
				p.warnRedisDown(err)
				break // fall through to the local limiter
			}
			if waitMs <= 0 {
				p.granted.Add(1)
				return nil
			}
			timer := time.NewTimer(time.Duration(waitMs) * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	if err := p.local.Wait(ctx); err != nil {
		return err
	}
	p.granted.Add(1)
	return nil
}

// Priority grants an IMMEDIATE outbound start for emergency-grade traffic
// (SPEC-W11 Part B §5 incident_alert fast-lane), bypassing the token
// bucket. The bypass is still metered: the priority counter (exposed via
// Stats and logged on every use) keeps fast-lane sends usage-visible, which
// is the audit trail the carrier/billing conversation needs when the CPS
// ceiling is exceeded by design. It never blocks and only fails when ctx
// is already done.
func (p *Pacer) Priority(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.prioLane.Add(1)
	p.log.Info("pacer: priority fast-lane dispatch (token bucket bypassed, metered)",
		zap.Uint64("priority_total", p.prioLane.Load()))
	return nil
}

// Stats returns the metering counters: token-bucket grants and priority
// fast-lane bypasses since process start.
func (p *Pacer) Stats() (granted, priority uint64) {
	return p.granted.Load(), p.prioLane.Load()
}

// acquireRedis runs the Lua token bucket and returns the retry delay in ms.
func (p *Pacer) acquireRedis(ctx context.Context) (int64, error) {
	now := time.Now().UnixMilli()
	res, err := p.rdb.Eval(ctx, bucketScript, []string{bucketKey},
		p.cfg.CPS, p.cfg.Burst, now).Int64()
	if err != nil {
		return 0, fmt.Errorf("pacer bucket: %w", err)
	}
	return res, nil
}

// NextSender returns the next sender from the rotation pool, or "" when no
// pool is configured (caller keeps its default sender). With the redis
// backend the counter is a shared INCR key so rotation interleaves fairly
// across replicas; on redis error it falls back to a process-local atomic
// counter (fail-open, same rationale as Wait).
func (p *Pacer) NextSender(ctx context.Context) string {
	n := uint64(len(p.cfg.FromNumbers))
	if n == 0 {
		return ""
	}
	if p.rdb != nil {
		v, err := p.rdb.Incr(ctx, rotationKey).Result()
		if err == nil && v > 0 {
			return p.cfg.FromNumbers[uint64(v-1)%n]
		}
		if err != nil {
			p.warnRedisDown(fmt.Errorf("pacer rotation: %w", err))
		}
	}
	return p.cfg.FromNumbers[(p.rot.Add(1)-1)%n]
}

// warnRedisDown logs the fail-open fallback once per process.
func (p *Pacer) warnRedisDown(err error) {
	if p.redisWarned.CompareAndSwap(false, true) {
		p.log.Warn("pacer: redis backend unavailable, failing open to local rate limiter",
			zap.Error(err), zap.String("redis_addr", p.cfg.RedisAddr))
	}
}
