"""The receptionist knows today's date (it asked for availability in 2025 and said nothing was free)."""
from datetime import datetime, timezone

from app.prompts import today_line
from app.tools import _past_window_note

NOW = datetime(2026, 11, 16, 9, 30, tzinfo=timezone.utc)


def test_today_line_in_the_business_time_zone():
    line = today_line("Africa/Lagos", now=NOW)
    assert line.startswith("Today is Monday, 16 November 2026; the local time is 10:30 (Africa/Lagos).")
    assert "never check or book a date in the past" in line
    assert today_line("Not/AZone", now=NOW).startswith("Today is Monday, 16 November 2026")  # bad zone -> UTC


def test_past_window_is_named_not_reported_as_fully_booked():
    note = _past_window_note("2025-11-18T23:59:59+01:00", "Africa/Lagos", now=NOW)
    assert note and "2025-11-18" in note and "Today is Monday 2026-11-16" in note
    assert _past_window_note("2026-11-16T23:59:59+01:00", "Africa/Lagos", now=NOW) is None  # today is fine
    assert _past_window_note("2026-11-18T23:59:59+01:00", "Africa/Lagos", now=NOW) is None
    assert _past_window_note("not a date", "Africa/Lagos", now=NOW) is None


def test_bare_times_become_rfc3339_in_the_business_zone():
    from app.tools import _rfc3339

    assert _rfc3339("2026-11-18T08:00:00", "Africa/Lagos") == "2026-11-18T08:00:00+01:00"
    assert _rfc3339("2026-11-18", "Africa/Lagos") == "2026-11-18T00:00:00+01:00"
    assert _rfc3339("2026-11-18T08:00:00Z", "Africa/Lagos") == "2026-11-18T08:00:00+00:00"
    assert _rfc3339("2026-11-18T08:00:00+01:00", "Africa/Lagos") == "2026-11-18T08:00:00+01:00"
    assert _rfc3339("next wednesday", "Africa/Lagos") == "next wednesday"
