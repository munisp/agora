"""Faithful Python port of the booking-service Go lending rule score.

PORT SOURCE (verified line-by-line):
  services/booking-service/internal/lending/lending.go:388-426
    - weight constants ``ScoreMaxTenure=30``, ``ScoreTenurePerMonth=3``,
      ``ScoreMaxBookings=40``, ``ScorePerCompletedBooking=4``,
      ``ScoreMaxRepaidLoans=30``, ``ScorePerRepaidLoan=10`` (lending.go:388-395)
    - ``func Score(sig ScoreSignals) int`` (lending.go:405-426):
      tenure = min((tenure_days//30)*3, 30) with negative tenure clamped
      to 0 first; bookings = min(completed_bookings*4, 40); repaid =
      min(repaid_loans*10, 30); total = min(tenure+bookings+repaid, 100).
      Go ``/`` on non-negative ints truncates toward zero, which matches
      Python ``//`` here (tenure_days >= 0 after the clamp).
  Signals struct ``ScoreSignals{TenureDays, CompletedBookings,
  RepaidLoans}`` at lending.go:398-402.

DOCUMENTED DEVIATIONS FROM THE W33 TASK BRIEF (repo reality, verified):
  1. The Go rule returns a NAIVE 0..100 score, NOT a 300-900 bureau
     score (lending.go:37-41 package doc: "a NAIVE rule-based score
     0..100 (NOT a credit-bureau score)"). The bureau-scale mapping
     lives in :func:`naive_to_bureau` and is additive — the ported math
     itself is bit-identical to Go.
  2. The Go rule emits NO reason codes anywhere (no reason-code
     constants exist in the lending package; "W24 reason codes" do not
     exist in repo — SPEC-W24 is an ops-backlog spec that never touched
     scoring). To remain a faithful port, :func:`score` returns an empty
     reason list rather than inventing codes. The API therefore always
     carries ``reasons: []`` and the ML blend can never suppress a rule
     reason (there are none to suppress); provenance rides in the
     dedicated ``model_version`` / ``ml_contribution`` fields (I2).
"""

from __future__ import annotations

from dataclasses import dataclass

# Weight constants — identical values to lending.go:388-395.
SCORE_MAX_TENURE = 30
SCORE_TENURE_PER_MONTH = 3
SCORE_MAX_BOOKINGS = 40
SCORE_PER_COMPLETED_BOOKING = 4
SCORE_MAX_REPAID_LOANS = 30
SCORE_PER_REPAID_LOAN = 10

# Bureau-scale mapping (documented, additive): the naive 0..100 rule
# score is affinely mapped onto the 300..900 bureau band so the ML blend
# (0.6*ml + 0.4*rule) compares like-with-like. 0 -> 300, 100 -> 900.
BUREAU_MIN = 300
BUREAU_MAX = 900


@dataclass(frozen=True)
class ScoreSignals:
    """Raw inputs of the naive score (mirror of lending.go:398-402)."""

    tenure_days: int = 0
    completed_bookings: int = 0
    repaid_loans: int = 0


def naive_score(sig: ScoreSignals) -> int:
    """The ported Go ``Score()`` — identical math, identical edge cases.

    Like Go, only negative tenure is clamped; negative
    bookings/repaid counts flow through exactly as the Go code does.
    """
    tenure_days = sig.tenure_days if sig.tenure_days >= 0 else 0
    tenure = (tenure_days // 30) * SCORE_TENURE_PER_MONTH
    if tenure > SCORE_MAX_TENURE:
        tenure = SCORE_MAX_TENURE
    bookings = sig.completed_bookings * SCORE_PER_COMPLETED_BOOKING
    if bookings > SCORE_MAX_BOOKINGS:
        bookings = SCORE_MAX_BOOKINGS
    repaid = sig.repaid_loans * SCORE_PER_REPAID_LOAN
    if repaid > SCORE_MAX_REPAID_LOANS:
        repaid = SCORE_MAX_REPAID_LOANS
    total = tenure + bookings + repaid
    if total > 100:
        total = 100
    return total


def score(sig: ScoreSignals) -> tuple[int, list[str]]:
    """Score + reason codes, mirroring the Go rule's observable contract.

    The Go rule returns no reason codes (see module docstring, deviation
    2), so the faithful port returns an empty list — never None — and
    never invents codes the Go rule does not emit.
    """
    return naive_score(sig), []


def naive_to_bureau(naive: int) -> int:
    """Affine 0..100 -> 300..900 mapping used by the blend (additive)."""
    return BUREAU_MIN + 6 * naive
