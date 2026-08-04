package consent

import (
	"regexp"
	"strings"
	"time"
)

// Erasure eligibility + the is_synthetic fast-path (SPEC-W17 §8.8 / Agent D —
// ADDITIVE to the W12 consent registry).
//
// Platform seed data (schema cac.*, SPEC-W17 contract C) marks every row
// is_synthetic=true and keys entities by deterministic ids
// (scripts/seeds/_lib.py contract A: sha256(SEED_SALT + "|" + natural_key)
// hex). No real person's rights attach to such subjects, so an erasure
// request targeting one is IMMEDIATE-ELIGIBLE and must skip any waiting
// period. No waiting period exists in the current code (erasure is
// tombstone-only and immediate for everyone); this file is the eligibility
// check where the NDPA waiting period will plug in, and the synthetic
// short-circuit already sits at the top of it so seeded subjects bypass any
// future delay.
//
// Detection is deliberately conservative — seed-tagged id patterns only:
//
//   - 64-char lowercase hex: the deterministic_id output shape. Seeded
//     customers/agents are referenced by these ids everywhere downstream.
//   - "seed:" / "seed-" prefix: explicitly seed-tagged natural keys.
//
// The W17 synthetic PHONE band (+234 80XX XXX XXXX) is NOT used for
// auto-classification: it overlaps real Nigerian allocations (0803/0806 etc.
// normalize into it), and fast-pathing a real person's erasure is a
// compliance bug, not a feature. Synthetic consent subjects are keyed by id,
// never by raw phone.
const (
	// ErasureWaitingPeriod is the NDPA waiting period applied to erasure
	// requests. Zero today (tombstone-only immediate erasure, W12 §4); when a
	// non-zero period is introduced, synthetic subjects skip it via
	// EvaluateErasureEligibility.
	ErasureWaitingPeriod = 0 * time.Second

	// SeedIDPrefix tags natural keys as seed-owned ("seed:" canonical,
	// "seed-" tolerated).
	SeedIDPrefix = "seed:"
)

// seedDeterministicIDPattern matches the deterministic-id output shape of
// scripts/seeds/_lib.py (sha256 hex digest, lowercase).
var seedDeterministicIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ErasureEligibility is the outcome of the erasure eligibility check.
type ErasureEligibility struct {
	// Immediate: the request may be tombstoned now (no waiting period left).
	Immediate bool
	// Synthetic: the subject matched a seed-tagged id pattern (fast-path).
	Synthetic bool
	// WaitingPeriod: the delay that applies before tombstoning (0 = none).
	WaitingPeriod time.Duration
	// Reason: human/audit-readable basis for the decision.
	Reason string
}

// IsSyntheticSubject reports whether a data-subject id carries a seed tag
// (deterministic seed id shape or explicit seed prefix).
func IsSyntheticSubject(subject string) bool {
	s := strings.TrimSpace(subject)
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, SeedIDPrefix) || strings.HasPrefix(s, "seed-") {
		return true
	}
	return seedDeterministicIDPattern.MatchString(s)
}

// EvaluateErasureEligibility applies the eligibility rules for an erasure
// request. The synthetic short-circuit is FIRST: a seeded data subject is
// immediate-eligible regardless of ErasureWaitingPeriod, because the record
// describes no real person (SPEC-W17 §8.8). Real subjects wait out
// ErasureWaitingPeriod (currently zero — behaviour unchanged from W12).
func EvaluateErasureEligibility(subject string) ErasureEligibility {
	if IsSyntheticSubject(subject) {
		return ErasureEligibility{
			Immediate:     true,
			Synthetic:     true,
			WaitingPeriod: 0,
			Reason:        "is_synthetic fast-path: seed-tagged data subject (SPEC-W17 §8.8) — no waiting period applies",
		}
	}
	return ErasureEligibility{
		Immediate:     ErasureWaitingPeriod <= 0,
		Synthetic:     false,
		WaitingPeriod: ErasureWaitingPeriod,
		Reason:        "standard erasure: waiting period applies before tombstoning",
	}
}
