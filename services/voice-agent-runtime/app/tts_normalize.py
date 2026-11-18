"""SPEC-W82 A4.3: Naija spoken-entity normalization pre-TTS.

TTS engines mangle written entities that Nigerian callers expect to HEAR
properly: "₦5,000" should come out as "five thousand naira", a phone number
as a digit read-back ("zero eight zero three ..."), "15/03/2026" as
"the fifteenth of March, twenty twenty-six". :func:`normalize_for_tts`
rewrites those surface forms into speakable words before provider dispatch.

Pure functions, stdlib only, idempotent
(``normalize_for_tts(normalize_for_tts(x)) == normalize_for_tts(x)``).
"""

from __future__ import annotations

import re

_ONES = (
    "zero one two three four five six seven eight nine ten eleven twelve "
    "thirteen fourteen fifteen sixteen seventeen eighteen nineteen"
).split()
_TENS = (
    "", "", "twenty", "thirty", "forty", "fifty", "sixty", "seventy",
    "eighty", "ninety",
)
_MONTHS = (
    "", "January", "February", "March", "April", "May", "June", "July",
    "August", "September", "October", "November", "December",
)
_ORDINALS = (
    "first second third fourth fifth sixth seventh eighth ninth tenth "
    "eleventh twelfth thirteenth fourteenth fifteenth sixteenth seventeenth "
    "eighteenth nineteenth twentieth twenty-first twenty-second "
    "twenty-third twenty-fourth twenty-fifth twenty-sixth twenty-seventh "
    "twenty-eighth twenty-ninth thirtieth thirty-first"
).split()


def number_to_words(n: int) -> str:
    """English words for 0 <= n < 1,000,000,000,000 (stdlib, no deps)."""
    if n < 0:
        return f"minus {number_to_words(-n)}"
    if n < 20:
        return _ONES[n]
    if n < 100:
        tens, rem = divmod(n, 10)
        return _TENS[tens] + (f"-{_ONES[rem]}" if rem else "")
    if n < 1000:
        hundreds, rem = divmod(n, 100)
        return f"{_ONES[hundreds]} hundred" + (
            f" and {number_to_words(rem)}" if rem else ""
        )
    for scale, name in ((1_000_000_000, "billion"), (1_000_000, "million"), (1000, "thousand")):
        if n >= scale:
            head, rem = divmod(n, scale)
            return f"{number_to_words(head)} {name}" + (
                f" {number_to_words(rem)}" if rem else ""
            )
    return str(n)  # pragma: no cover - unreachable below 1e12


def _digits_to_words(digits: str) -> str:
    return " ".join(_ONES[int(d)] for d in digits)


# ---------------------------------------------------------------------------
# Naira amounts: "₦5,000" / "N5000" / "NGN 5000.50" -> words + "naira"
# ---------------------------------------------------------------------------
_NAIRA_RE = re.compile(
    r"(?<![\w.])(?:₦\s?|NGN\s?|N(?=\d))(\d[\d,]*(?:\.\d{1,2})?)"
)


def _naira_words(match: re.Match) -> str:
    raw = match.group(1).replace(",", "")
    if "." in raw:
        whole, frac = raw.split(".", 1)
        kobo = int(frac.ljust(2, "0")[:2])
    else:
        whole, kobo = raw, 0
    naira = int(whole or "0")
    words = f"{number_to_words(naira)} naira"
    if kobo:
        words += f", {number_to_words(kobo)} kobo"
    return words


# ---------------------------------------------------------------------------
# Dates (day-first, Nigerian convention): "15/03/2026" ->
# "the fifteenth of March, twenty twenty-six"
# ---------------------------------------------------------------------------
_DATE_RE = re.compile(r"\b(\d{1,2})/(\d{1,2})/(\d{4})\b")


def _spoken_year(year: int) -> str:
    """2026 -> "twenty twenty-six"; 2005 -> "two thousand and five";
    1900 -> "nineteen hundred"; 1999 -> "nineteen ninety-nine"."""
    if 2000 <= year <= 2009:
        return "two thousand" + (
            f" and {number_to_words(year - 2000)}" if year > 2000 else ""
        )
    century, rem = divmod(year, 100)
    head = number_to_words(century)
    if rem == 0:
        return f"{head} hundred"
    return f"{head} {number_to_words(rem)}"


def _date_words(match: re.Match) -> str:
    day, month, year = int(match.group(1)), int(match.group(2)), int(match.group(3))
    if not (1 <= day <= 31 and 1 <= month <= 12):
        return match.group(0)  # not a plausible day-first date; leave as-is
    return f"the {_ORDINALS[day - 1]} of {_MONTHS[month]}, {_spoken_year(year)}"


# ---------------------------------------------------------------------------
# Phone-style numbers: "+234 803 555 0101" / "08035550101" -> digit read-back
# ---------------------------------------------------------------------------
_PHONE_RE = re.compile(r"(?<![\w/])\+?\d[\d\s\-]{5,}\d(?![\w/])")


def _phone_words(match: re.Match) -> str:
    raw = match.group(0)
    digits = re.sub(r"\D", "", raw)
    words = _digits_to_words(digits)
    return f"plus {words}" if raw.startswith("+") else words


# ---------------------------------------------------------------------------
# Common abbreviations callers hear expanded.
# ---------------------------------------------------------------------------
_ABBREVIATIONS = (
    (re.compile(r"\bDr\."), "Doctor"),
    (re.compile(r"\bMr\."), "Mister"),
    (re.compile(r"\bMrs\."), "Missus"),
    (re.compile(r"\bMs\."), "Miss"),
    (re.compile(r"\bSt\."), "Street"),
    (re.compile(r"\bNo\."), "Number"),
    (re.compile(r"\bLtd\b"), "Limited"),
    (re.compile(r"\bPLC\b"), "P L C"),
    (re.compile(r"\bSMS\b"), "S M S"),
    (re.compile(r"\betc\b"), "et cetera"),
)


def normalize_for_tts(text: str) -> str:
    """Rewrite TTS-bound text into speakable Naija surface forms.

    Order matters: naira amounts first (their digits must not reach the
    phone read-back), then dates (slashes), then phone-style digit runs,
    then abbreviation expansion. The output contains no digits or currency
    symbols, so the function is idempotent.
    """
    if not text:
        return text
    out = _NAIRA_RE.sub(_naira_words, text)
    out = _DATE_RE.sub(_date_words, out)
    out = _PHONE_RE.sub(_phone_words, out)
    for pattern, expansion in _ABBREVIATIONS:
        out = pattern.sub(expansion, out)
    return out
