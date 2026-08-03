// SMS provider failover chain (SPEC-W12 Agent A): an ordered chain parsed
// from SMS_PROVIDER_CHAIN (default "africastalking,termii,ebulksms"), a
// per-provider circuit breaker mirroring the voice runtime's CircuitBreaker
// concept (app/pipeline/llm.py — after N consecutive failures open for a
// cooldown, then allow one probe; a failed probe re-opens), and per-provider
// relative price-tier annotations used only for reporting.
//
// Failover policy: a provider 5xx or transport/timeout failure (after the
// shared Client's own 2 retries) moves to the next provider in the chain. A
// provider 4xx is a caller fault (bad recipient/sender) and is returned
// immediately — failing over would just have the next provider reject the
// same request.
package provider

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// SMSProvider is the contract every outbound SMS provider in the chain
// implements (the existing *AfricasTalking / *Termii / *EBulkSMS structs all
// satisfy it).
type SMSProvider interface {
	// Configured reports whether the provider has credentials. Unconfigured
	// providers are skipped by the chain.
	Configured() bool
	// SendSMS delivers one SMS; from/sender-id override semantics are
	// provider-specific.
	SendSMS(ctx context.Context, to, message, from string) (int, []byte, error)
}

// priceTiers are RELATIVE per-provider SMS cost annotations (ASSUMPTION —
// no live rate cards this wave): 1.0 = baseline, 0.85 = ~15% cheaper.
// Used for reporting only (startup log + Entries()); never for routing.
var priceTiers = map[string]float64{
	"africastalking": 1.0,
	"termii":         1.0,
	"ebulksms":       0.85,
}

// PriceTier returns the relative price-tier annotation for a provider name
// (1.0 when unknown).
func PriceTier(name string) float64 {
	if t, ok := priceTiers[name]; ok {
		return t
	}
	return 1.0
}

// DefaultChain is used when SMS_PROVIDER_CHAIN is empty.
const DefaultChain = "africastalking,termii,ebulksms"

// CircuitBreaker mirrors the voice runtime's CircuitBreaker (Go idiom):
// after threshold consecutive failures the breaker opens for cooldown, then
// allows one probe; a failed probe re-opens for another cooldown window.
type CircuitBreaker struct {
	mu        sync.Mutex
	threshold int
	cooldown  time.Duration
	now       func() time.Time // injectable for tests

	failures int
	open     bool
	openedAt time.Time
}

// NewCircuitBreaker builds a breaker with the voice-runtime defaults when
// given non-positive values (threshold 3, cooldown 60s).
func NewCircuitBreaker(threshold int, cooldown time.Duration) *CircuitBreaker {
	if threshold <= 0 {
		threshold = 3
	}
	if cooldown <= 0 {
		cooldown = 60 * time.Second
	}
	return &CircuitBreaker{threshold: threshold, cooldown: cooldown, now: time.Now}
}

// Allow reports whether a call may proceed: closed → always; open → only
// once the cooldown elapsed (the call is the probe).
func (cb *CircuitBreaker) Allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if !cb.open {
		return true
	}
	return cb.now().Sub(cb.openedAt) >= cb.cooldown
}

// Open reports whether the breaker is currently open (reporting only).
func (cb *CircuitBreaker) Open() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.open && cb.now().Sub(cb.openedAt) < cb.cooldown
}

// RecordSuccess closes the breaker and resets the failure streak.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures = 0
	cb.open = false
}

// RecordFailure counts one failure; reaching the threshold opens the
// breaker (or re-opens it after a failed probe).
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures++
	if cb.failures >= cb.threshold {
		cb.failures = 0
		cb.open = true
		cb.openedAt = cb.now()
	}
}

// ChainEntry is one provider in the failover chain with its breaker and
// reporting annotation.
type ChainEntry struct {
	Name      string
	Provider  SMSProvider
	Breaker   *CircuitBreaker
	PriceTier float64 // relative cost annotation, reporting only
}

// Failover is the ordered SMS provider chain.
type Failover struct {
	chain []ChainEntry
	log   *zap.Logger
}

// NewFailover parses the SMS_PROVIDER_CHAIN csv and links each name to an
// SMSProvider from providers. Unknown names and nil providers are skipped
// with a warning; duplicates are collapsed (first occurrence wins).
func NewFailover(providers map[string]SMSProvider, chainCSV string, log *zap.Logger) *Failover {
	if strings.TrimSpace(chainCSV) == "" {
		chainCSV = DefaultChain
	}
	f := &Failover{log: log}
	seen := map[string]bool{}
	for _, name := range strings.Split(chainCSV, ",") {
		name = strings.TrimSpace(strings.ToLower(name))
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		p, ok := providers[name]
		if !ok || p == nil {
			log.Warn("sms provider chain: unknown provider, skipping", zap.String("provider", name))
			continue
		}
		f.chain = append(f.chain, ChainEntry{
			Name:      name,
			Provider:  p,
			Breaker:   NewCircuitBreaker(0, 0),
			PriceTier: PriceTier(name),
		})
	}
	return f
}

// Entries returns the chain for reporting (price tiers, breaker state).
func (f *Failover) Entries() []ChainEntry {
	out := make([]ChainEntry, len(f.chain))
	copy(out, f.chain)
	return out
}

// SendSMS tries each configured provider in chain order. Returns the name of
// the provider that accepted the send alongside the provider status/body.
func (f *Failover) SendSMS(ctx context.Context, to, message, from string) (string, int, []byte, error) {
	if len(f.chain) == 0 {
		return "", 0, nil, fmt.Errorf("sms provider chain is empty (SMS_PROVIDER_CHAIN)")
	}
	var failed []string
	for i := range f.chain {
		e := &f.chain[i]
		if !e.Provider.Configured() {
			continue
		}
		if !e.Breaker.Allow() {
			f.log.Info("sms provider circuit open, skipping",
				zap.String("provider", e.Name))
			continue
		}
		status, body, err := e.Provider.SendSMS(ctx, to, message, from)
		if err == nil {
			e.Breaker.RecordSuccess()
			return e.Name, status, body, nil
		}
		if ClientError(err) {
			// Provider 4xx: caller fault — the next provider would reject the
			// same request. Surface immediately (maps to 400 upstream).
			return e.Name, status, body, err
		}
		e.Breaker.RecordFailure()
		failed = append(failed, fmt.Sprintf("%s: %v", e.Name, err))
		f.log.Warn("sms provider failed, failing over",
			zap.String("provider", e.Name), zap.Error(err))
	}
	if len(failed) == 0 {
		return "", 0, nil, fmt.Errorf("no sms provider available (all unconfigured or circuit-open)")
	}
	return "", 0, nil, fmt.Errorf("all sms providers failed: %s", strings.Join(failed, "; "))
}
