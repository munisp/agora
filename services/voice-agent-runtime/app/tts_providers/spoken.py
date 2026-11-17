"""Numbers, times and dates written out as words before the YarnGPT English voice speaks them.

YarnGPT reads words well but garbles digits: "9:00, 9:30 or 10:00" came out as "my 28th or 9th thoughts… 10, 10, 10
thoughts". Phone numbers are read digit by digit ("0806" -> "zero eight zero six"), clock times as a person says them
("9:30 AM" -> "nine thirty a m"), ordinals and plain numbers as words. Text without digits is returned unchanged.
"""
from __future__ import annotations

import re

_ONES = ["zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten", "eleven", "twelve",
         "thirteen", "fourteen", "fifteen", "sixteen", "seventeen", "eighteen", "nineteen"]
_TENS = ["", "", "twenty", "thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety"]
_ORD = {"one": "first", "two": "second", "three": "third", "five": "fifth", "eight": "eighth", "nine": "ninth",
        "twelve": "twelfth"}


def number_words(n: int) -> str:
    """0 .. 999,999,999 as English words ("one hundred and five", "twenty-five thousand")."""
    if n < 20:
        return _ONES[n]
    if n < 100:
        return _TENS[n // 10] + ("-" + _ONES[n % 10] if n % 10 else "")
    if n < 1000:
        rest = n % 100
        return _ONES[n // 100] + " hundred" + (" and " + number_words(rest) if rest else "")
    for size, name in ((1_000_000, "million"), (1000, "thousand")):
        if n >= size:
            rest = n % size
            head = number_words(n // size) + " " + name
            if not rest:
                return head
            return head + (" and " if rest < 100 else " ") + number_words(rest)
    return str(n)


def ordinal_words(n: int) -> str:
    words = number_words(n)
    last = re.split(r"([ -])", words)
    tail = last[-1]
    if tail in _ORD:
        last[-1] = _ORD[tail]
    elif tail.endswith("y"):
        last[-1] = tail[:-1] + "ieth"
    else:
        last[-1] = tail + "th"
    return "".join(last)


def _digits(s: str) -> str:
    return " ".join(_ONES[int(d)] for d in s)


def _time(m: re.Match) -> str:
    h, mins, ampm = int(m.group(1)), m.group(2), (m.group(3) or "").lower().replace(".", "")
    words = number_words(h) if h <= 24 else str(h)
    if mins and mins != "00":
        mm = int(mins)
        words += " " + ("oh " + _ONES[mm] if mm < 10 else number_words(mm))
    elif not ampm:
        words += " o'clock"
    if ampm:
        words += " " + " ".join(ampm)  # "a m" / "p m": spoken letter by letter
    return words


_PHONE = re.compile(r"(?<!\w)\+?\d[\d\s-]{6,}\d(?!\w)")
_TIME = re.compile(r"\b(\d{1,2}):(\d{2})\s*([AaPp]\.?[Mm])?(?![\w])|\b(\d{1,2})\s*([AaPp]\.?[Mm])(?![\w])")
_ORDINAL = re.compile(r"\b(\d{1,3})(st|nd|rd|th)\b", re.I)
_MONEY = re.compile(r"(₦|NGN\s?|N(?=\d))(\d[\d,]*)(?:\.(\d{2}))?")
_NUMBER = re.compile(r"\b\d[\d,]*\b")


def spoken(text: str) -> str:
    """Rewrite digits in `text` as words for a voice that cannot read digits."""
    if not text or not re.search(r"\d", text):
        return text
    # Phone and account numbers (7+ digits, spaces/dashes allowed): digit by digit.
    text = _PHONE.sub(lambda m: _digits(re.sub(r"\D", "", m.group(0))), text)

    def time_sub(m: re.Match) -> str:
        if m.group(4):  # "9am"
            return _time(re.match(r"(\d{1,2})()(.*)", m.group(4) + m.group(5)))
        return _time(m)

    text = _TIME.sub(time_sub, text)
    text = _MONEY.sub(lambda m: number_words(int(m.group(2).replace(",", ""))) + " naira", text)
    text = _ORDINAL.sub(lambda m: ordinal_words(int(m.group(1))), text)

    def num_sub(m: re.Match) -> str:
        raw = m.group(0).replace(",", "")
        if len(raw) > 1 and raw.startswith("0"):
            return _digits(raw)  # codes like "0803" are read digit by digit
        n = int(raw)
        return number_words(n) if n < 1_000_000_000 else _digits(raw)

    return _NUMBER.sub(num_sub, text)
