package lending

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// SPEC-W44 W-B/F16-1: a sidecar fallback is never silent — every fallback
// logs DEBUG (and the first unreachable per boot logs WARN; the once-per-boot
// WARN may already be consumed by another test in this package, so this
// asserts the per-fallback DEBUG floor).
func TestSidecarFallbackLogs(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	SetSidecarLogger(zap.New(core))
	t.Cleanup(func() { SetSidecarLogger(nil) })

	t.Setenv("CREDIT_BUREAU_URL", "http://127.0.0.1:1") // unreachable
	d := ScoreDecisionWithSidecar(context.Background(), ScoreSignals{})
	if d.Source != SidecarSourceLocal {
		t.Fatalf("source = %q, want local-rules fallback", d.Source)
	}
	entries := logs.FilterMessage("credit-bureau fallback: scoring with local rules").All()
	if len(entries) == 0 {
		t.Fatal("fallback produced no log entry — F16-1 regression (silent fallback)")
	}
}

// LogCreditBureauBootStatus: WARN when CREDIT_BUREAU_URL is unset, INFO when
// configured (F16-1 boot posture).
func TestLogCreditBureauBootStatus(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	SetSidecarLogger(zap.New(core))
	t.Cleanup(func() { SetSidecarLogger(nil) })

	t.Setenv("CREDIT_BUREAU_URL", "")
	LogCreditBureauBootStatus()
	if logs.FilterLevelExact(zap.WarnLevel).Len() == 0 {
		t.Fatal("unset CREDIT_BUREAU_URL must WARN at boot")
	}

	t.Setenv("CREDIT_BUREAU_URL", "http://bureau:8090")
	LogCreditBureauBootStatus()
	if logs.FilterMessage("credit-bureau sidecar configured").Len() == 0 {
		t.Fatal("configured CREDIT_BUREAU_URL must INFO at boot")
	}
}
