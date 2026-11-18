"""SPEC-W82 A4.3: Naija spoken-entity normalization pre-TTS — unit tests."""

from __future__ import annotations

import pytest

from app.tts_normalize import normalize_for_tts, number_to_words


# ---------------------------------------------------------------------------
# number_to_words
# ---------------------------------------------------------------------------
@pytest.mark.parametrize(
    ("n", "words"),
    [
        (0, "zero"),
        (7, "seven"),
        (15, "fifteen"),
        (21, "twenty-one"),
        (100, "one hundred"),
        (115, "one hundred and fifteen"),
        (1000, "one thousand"),
        (5000, "five thousand"),
        (1234, "one thousand two hundred and thirty-four"),
        (1_000_000, "one million"),
        (2_500_000, "two million five hundred thousand"),
    ],
)
def test_number_to_words(n, words):
    assert number_to_words(n) == words


# ---------------------------------------------------------------------------
# Naira amounts
# ---------------------------------------------------------------------------
def test_naira_symbol_amount():
    assert normalize_for_tts("₦5,000") == "five thousand naira"


def test_naira_n_prefix_amount():
    assert normalize_for_tts("N5000") == "five thousand naira"


def test_naira_ngn_prefix_and_kobo():
    assert normalize_for_tts("NGN 2500.50") == "two thousand five hundred naira, fifty kobo"


def test_naira_amount_inside_sentence():
    out = normalize_for_tts("The braids cost ₦15,000 per session.")
    assert out == "The braids cost fifteen thousand naira per session."


def test_naira_not_triggered_by_bare_words():
    assert normalize_for_tts("No booking now") == "No booking now"


# ---------------------------------------------------------------------------
# Phone numbers: +234 / 080 digit read-back
# ---------------------------------------------------------------------------
def test_plus_234_number_read_back():
    out = normalize_for_tts("+2348035550101")
    assert out == "plus two three four eight zero three five five five zero one zero one"


def test_080_number_read_back():
    out = normalize_for_tts("08035550101")
    assert out == "zero eight zero three five five five zero one zero one"


def test_number_with_spaces_and_dashes():
    out = normalize_for_tts("call 0803 555-0101 now")
    assert "zero eight zero three five five five zero one zero one" in out
    assert out.startswith("call ")
    assert out.endswith(" now")


def test_short_numbers_untouched():
    assert normalize_for_tts("we open at 9 and close at 5") == (
        "we open at 9 and close at 5"
    )


# ---------------------------------------------------------------------------
# Dates (day-first)
# ---------------------------------------------------------------------------
def test_date_spoken_form():
    assert normalize_for_tts("15/03/2026") == (
        "the fifteenth of March, twenty twenty-six"
    )


def test_date_first_of_month():
    assert normalize_for_tts("01/01/2030") == "the first of January, twenty thirty"


def test_date_in_sentence():
    out = normalize_for_tts("Your appointment is on 02/05/2026 at 10am.")
    assert "the second of May, twenty twenty-six" in out


def test_implausible_date_untouched():
    assert normalize_for_tts("score 99/99/9999") == "score 99/99/9999"


# ---------------------------------------------------------------------------
# Abbreviations
# ---------------------------------------------------------------------------
def test_abbreviation_expansion():
    out = normalize_for_tts("Dr. Adaeze from Acme Ltd sent an SMS.")
    assert "Doctor Adaeze" in out
    assert "Acme Limited" in out
    assert "an S M S." in out


def test_abbreviation_case_and_boundary_safe():
    # "Ltd" inside a longer word must not expand.
    assert normalize_for_tts("Ltdco Ltd") == "Ltdco Limited"


# ---------------------------------------------------------------------------
# Idempotence + misc
# ---------------------------------------------------------------------------
@pytest.mark.parametrize(
    "text",
    [
        "₦5,000 for the session on 15/03/2026, call +2348035550101.",
        "N5000 only, Dr. Musa (Acme Ltd) — SMS 08035550101.",
        "NGN 2500.50 deposit before 01/01/2030.",
        "Hello, no entities here.",
        "",
    ],
)
def test_idempotent(text):
    once = normalize_for_tts(text)
    assert normalize_for_tts(once) == once


def test_mixed_entities_full_sentence():
    out = normalize_for_tts("Pay ₦5,000 before 15/03/2026; call 08035550101.")
    assert "five thousand naira" in out
    assert "the fifteenth of March, twenty twenty-six" in out
    assert "zero eight zero three five five five zero one zero one" in out
    assert "₦" not in out and "2026" not in out
