"""Rule-parity tests (SPEC-W33 §3 B2): the Python port must reproduce the
Go rule score EXACTLY — same signals -> same score, same (empty) reasons.

FIXTURE DERIVATION (documented per task contract): every expected value
below is hand-computed from the Go source
  services/booking-service/internal/lending/lending.go:405-426
    tenure  = min((max(tenure_days,0)//30)*3, 30)
    booking = min(completed_bookings*4, 40)
    repaid  = min(repaid_loans*10, 30)
    score   = min(tenure+booking+repaid, 100)
and the FIRST block mirrors the Go unit-test table verbatim
(services/booking-service/internal/lending/lending_test.go:68-88,
TestScore) so parity is checked against the same fixtures Go itself
asserts. The second block adds edge cases computed by hand from the same
formula (negative bookings/repaid flow through unclamped, exactly like
Go — only negative tenure is clamped, lending.go:406-408).

Reason codes: the Go rule emits none (verified: no reason-code constants
exist in the lending package), so the faithful port returns [] — asserted
here so a future "helpful" code invention fails loudly.
"""

from __future__ import annotations

import pytest

from credit_bureau.rules import (
    BUREAU_MAX,
    BUREAU_MIN,
    ScoreSignals,
    naive_score,
    naive_to_bureau,
    score,
)

# Mirrors lending_test.go TestScore verbatim (name, signals, want).
GO_TEST_TABLE = [
    ("zero", ScoreSignals(), 0),
    ("one month", ScoreSignals(tenure_days=30), 3),
    ("partial month floors", ScoreSignals(tenure_days=59), 3),
    ("tenure cap", ScoreSignals(tenure_days=3650), 30),
    ("bookings", ScoreSignals(completed_bookings=3), 12),
    ("bookings cap", ScoreSignals(completed_bookings=100), 40),
    ("repaid loans", ScoreSignals(repaid_loans=2), 20),
    ("repaid cap", ScoreSignals(repaid_loans=9), 30),
    ("negative tenure clamps", ScoreSignals(tenure_days=-5), 0),
    ("combined", ScoreSignals(tenure_days=120, completed_bookings=5, repaid_loans=1), 12 + 20 + 10),
    ("max 100", ScoreSignals(tenure_days=3650, completed_bookings=100, repaid_loans=100), 100),
]

# Extra hand-computed edge cases from lending.go:405-426.
HAND_COMPUTED = [
    # 29 days -> 0 months -> 0 tenure points (integer floor).
    ("29 days floor", ScoreSignals(tenure_days=29), 0),
    # 89 days -> 2 months -> 6; +1 booking (4) +0 repaid = 10.
    ("mixed below caps", ScoreSignals(tenure_days=89, completed_bookings=1), 10),
    # Negative bookings flow through (Go clamps ONLY negative tenure):
    # 0 tenure + (-1)*4 = -4.
    ("negative bookings flow", ScoreSignals(completed_bookings=-1), -4),
    # 60 days -> 6 tenure; -2 repaid -> -20; total -14.
    ("negative repaid flow", ScoreSignals(tenure_days=60, repaid_loans=-2), -14),
    # 300 days -> 30 tenure (cap); 10 bookings -> 40 (cap); total 70.
    ("caps without repaid", ScoreSignals(tenure_days=300, completed_bookings=10), 70),
    # Exactly at the total cap: 30 + 40 + 30 = 100.
    ("exact cap", ScoreSignals(tenure_days=300, completed_bookings=10, repaid_loans=3), 100),
]


@pytest.mark.parametrize("name,sig,want", GO_TEST_TABLE + HAND_COMPUTED)
def test_naive_score_parity(name: str, sig: ScoreSignals, want: int) -> None:
    assert naive_score(sig) == want, name


@pytest.mark.parametrize("name,sig,want", GO_TEST_TABLE + HAND_COMPUTED)
def test_score_returns_empty_reasons_like_go(name: str, sig: ScoreSignals, want: int) -> None:
    got_score, reasons = score(sig)
    assert got_score == want, name
    assert reasons == [], name  # the Go rule emits no reason codes


def test_bureau_mapping_is_affine_300_900() -> None:
    assert naive_to_bureau(0) == BUREAU_MIN == 300
    assert naive_to_bureau(50) == 600
    assert naive_to_bureau(100) == BUREAU_MAX == 900
