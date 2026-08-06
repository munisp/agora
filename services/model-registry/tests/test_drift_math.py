"""C2: PSI / population-KS math against hand-computed values."""

from __future__ import annotations

import math

import pytest

from model_registry.drift import EPS, histogram_counts, ks_statistic, psi


def test_psi_identical_is_zero():
    assert psi([0.5, 0.5], [0.5, 0.5]) == pytest.approx(0.0, abs=1e-12)
    assert psi([40, 30, 20, 10], [40, 30, 20, 10]) == pytest.approx(0.0, abs=1e-12)


def test_psi_hand_computed_two_bin():
    # e=[0.5,0.5], a=[0.75,0.25]:
    #   (0.75-0.5)*ln(0.75/0.5) + (0.25-0.5)*ln(0.25/0.5)
    expected = (0.25 * math.log(1.5)) + (-0.25 * math.log(0.5))
    assert psi([50, 50], [75, 25]) == pytest.approx(expected, rel=1e-9)
    assert expected == pytest.approx(0.27465, abs=1e-4)  # above the 0.25 alert band


def test_psi_zero_cell_eps_smoothed():
    # e=[1.0, 0.0], a=[0.5, 0.5] with eps flooring of the zero cell
    expected = ((0.5 - 1.0) * math.log(0.5 / 1.0)
                + (0.5 - EPS) * math.log(0.5 / EPS))
    assert psi([100, 0], [50, 50]) == pytest.approx(expected, rel=1e-6)


def test_ks_hand_computed():
    assert ks_statistic([1, 2, 3, 4], [1, 2, 3, 4]) == pytest.approx(0.0)
    # fully separated samples: D = 1
    assert ks_statistic([1, 1, 1], [3, 3, 3]) == pytest.approx(1.0)
    # a=[1,2,3], b=[2,3,4]: max |Fa-Fb| = 1/3 (hand-verified at x=1,2,3,4)
    assert ks_statistic([1, 2, 3], [2, 3, 4]) == pytest.approx(1 / 3)
    # a=[1,2,2,3], b=[1,1,2,3]: ECDFs at x=1: 1/4 vs 1/2 -> D=1/4
    assert ks_statistic([1, 2, 2, 3], [1, 1, 2, 3]) == pytest.approx(0.25)


def test_histogram_counts_clamps_out_of_range():
    # edges [0,1,2] → bins (-inf..1]→bin0 semantics per documented clamp
    assert histogram_counts([-5.0, 0.5, 1.5, 99.0], [0.0, 1.0, 2.0]) == [2, 2]
    assert histogram_counts([0.25, 0.75], [0.0, 0.5, 1.0]) == [1, 1]
