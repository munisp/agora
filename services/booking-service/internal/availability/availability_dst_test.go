package availability

import (
	"testing"
	"time"
)

// SPEC-W43 K-04: rule windows are wall-clock (time.Date) arithmetic, not
// midnight.Add(duration). Across a DST transition a "09:00" window start
// must remain 09:00 local — midnight.Add(9h) lands at 10:00 on a
// spring-forward day (offset increased by an hour during the night).

// findDSTTransitionDay scans the zone for a day whose UTC offset at
// midnight differs from the next midnight (the day the clocks change).
func findDSTTransitionDay(t *testing.T, loc *time.Location, year int) time.Time {
	t.Helper()
	day := time.Date(year, 1, 1, 0, 0, 0, 0, loc)
	for i := 0; i < 370; i++ {
		next := day.AddDate(0, 0, 1)
		_, off1 := day.Zone()
		_, off2 := next.Zone()
		if off1 != off2 {
			return day
		}
		day = next
	}
	t.Skipf("no DST transition in %s during %d (tzdata changed?)", loc, year)
	return time.Time{}
}

func TestSlotsWallClockAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("Africa/Casablanca")
	if err != nil {
		t.Skipf("zoneinfo unavailable: %v", err)
	}
	trans := findDSTTransitionDay(t, loc, 2025)

	rule := Rule{Weekday: trans.Weekday(), StartMin: 9 * 60, EndMin: 12 * 60}
	from := trans.Add(-time.Hour)
	to := trans.Add(30 * time.Hour)
	slots := Slots(Params{
		From: from, To: to,
		Duration: 30 * time.Minute,
		Rules:    []Rule{rule},
		Location: loc,
	})
	if len(slots) == 0 {
		t.Fatalf("no slots on transition day %s", trans)
	}
	for _, s := range slots {
		ls := s.StartsAt.In(loc)
		le := s.EndsAt.In(loc)
		startMin := ls.Hour()*60 + ls.Minute()
		endMin := le.Hour()*60 + le.Minute()
		if startMin < rule.StartMin || endMin > rule.EndMin {
			t.Fatalf("slot %s..%s local escapes the 09:00-12:00 wall-clock window (DST drift)",
				ls.Format(time.RFC3339), le.Format(time.RFC3339))
		}
		if startMin%30 != 0 {
			t.Fatalf("slot %s local is off the 30min wall-clock grid", ls.Format(time.RFC3339))
		}
	}
	// The 09:00 slot must exist at wall-clock 09:00 exactly (midnight.Add
	// would have produced 10:00 on a spring-forward day).
	first := slots[0].StartsAt.In(loc)
	if first.Hour() != 9 || first.Minute() != 0 {
		t.Fatalf("first slot = %s local, want 09:00 wall-clock", first.Format(time.RFC3339))
	}
	if first.YearDay() != trans.In(loc).YearDay() {
		t.Fatalf("first slot day %s, want the transition day %s", first, trans)
	}
}

// Covers must honour wall-clock windows on a transition day as well: the
// 09:00-09:30 local candidate is inside the rule, and midnight.Add-based
// windows would have rejected it (window starting 10:00).
func TestCoversWallClockAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("Africa/Casablanca")
	if err != nil {
		t.Skipf("zoneinfo unavailable: %v", err)
	}
	trans := findDSTTransitionDay(t, loc, 2025)
	rule := Rule{Weekday: trans.Weekday(), StartMin: 9 * 60, EndMin: 12 * 60}

	s := time.Date(trans.Year(), trans.Month(), trans.Day(), 9, 0, 0, 0, loc)
	e := s.Add(30 * time.Minute)
	if !Covers([]Rule{rule}, loc, s, e) {
		t.Fatalf("09:00 wall-clock candidate on %s must be covered by the 09:00-12:00 rule", trans.Format("2006-01-02"))
	}
	// 08:30-09:00 local is BEFORE the wall-clock window — must not be covered.
	s2 := time.Date(trans.Year(), trans.Month(), trans.Day(), 8, 30, 0, 0, loc)
	if Covers([]Rule{rule}, loc, s2, s2.Add(30*time.Minute)) {
		t.Fatal("08:30 wall-clock candidate must NOT be covered (window starts 09:00)")
	}
}

// EndMin=1440 (midnight end-of-day) spills to 00:00 of the next day —
// time.Date normalization keeps the window open through the evening.
func TestRuleWindowEndMin1440(t *testing.T) {
	loc := time.UTC
	day := time.Date(2025, 3, 9, 0, 0, 0, 0, loc)
	ws, we := ruleWindow(day, 20*60, 1440)
	if ws.Hour() != 20 || we.Hour() != 0 || we.Day() != 10 {
		t.Fatalf("window = %s..%s, want 20:00..00:00(+1d)", ws, we)
	}
	if !we.After(ws) {
		t.Fatal("window end must be after start")
	}
}
