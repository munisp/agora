// Package availability is the pure slot-computation engine of
// booking-service. It derives open appointment slots from weekly
// availability rules, existing bookings and offering buffers.
// It has no I/O dependencies and is exhaustively unit-tested.
package availability

import (
	"sort"
	"time"
)

// Rule is a weekly recurring availability window for a team member.
// Minutes are offsets from midnight in the tenant's timezone.
type Rule struct {
	Weekday  time.Weekday
	StartMin int // inclusive
	EndMin   int // exclusive

	// EffectiveFrom/EffectiveTo bound the rule to a date range.
	// Nil means unbounded. EffectiveTo is exclusive.
	EffectiveFrom *time.Time
	EffectiveTo   *time.Time
}

// Booking is an existing reservation occupying [StartsAt, EndsAt).
type Booking struct {
	StartsAt time.Time
	EndsAt   time.Time
}

// Slot is one bookable opening.
type Slot struct {
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
}

// Params are the inputs to Slots.
type Params struct {
	// From/To bound the search range (typically [now, now+N weeks]).
	From time.Time
	To   time.Time

	// Duration of the offering appointment.
	Duration time.Duration
	// Buffer enforced between distinct appointments on both sides.
	Buffer time.Duration
	// Capacity is how many overlapping appointments are allowed per slot
	// (1 = exclusive, >1 = class/group style). 0 is treated as 1.
	Capacity int
	// Step is the grid distance between candidate start times.
	// 0 defaults to Duration.
	Step time.Duration

	Rules    []Rule
	Bookings []Booking

	// Location is the tenant timezone used to expand weekly rules.
	// Nil defaults to UTC.
	Location *time.Location
}

// Slots computes all open slots in [From, To). The result is sorted by
// start time and deduplicated across overlapping rules.
func Slots(p Params) []Slot {
	if p.Duration <= 0 || !p.To.After(p.From) {
		return nil
	}
	if p.Capacity <= 0 {
		p.Capacity = 1
	}
	if p.Step <= 0 {
		p.Step = p.Duration
	}
	loc := p.Location
	if loc == nil {
		loc = time.UTC
	}

	// Iterate tenant-local calendar days covering [From, To].
	day := time.Date(p.From.In(loc).Year(), p.From.In(loc).Month(), p.From.In(loc).Day(), 0, 0, 0, 0, loc)
	endDay := time.Date(p.To.In(loc).Year(), p.To.In(loc).Month(), p.To.In(loc).Day(), 0, 0, 0, 0, loc)

	seen := make(map[time.Time]struct{})
	var out []Slot

	for d := day; !d.After(endDay); d = d.AddDate(0, 0, 1) {
		for _, rule := range p.Rules {
			if rule.Weekday != d.Weekday() || rule.EndMin <= rule.StartMin {
				continue
			}
			if !effectiveOn(rule, d) {
				continue
			}
			winStart, winEnd := ruleWindow(d, rule.StartMin, rule.EndMin)
			for s := winStart; !s.Add(p.Duration).After(winEnd); s = s.Add(p.Step) {
				e := s.Add(p.Duration)
				if s.Before(p.From) || e.After(p.To) {
					continue
				}
				if !fits(s, e, p.Buffer, p.Capacity, p.Bookings) {
					continue
				}
				if _, dup := seen[s]; dup {
					continue
				}
				seen[s] = struct{}{}
				out = append(out, Slot{StartsAt: s, EndsAt: e})
			}
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].StartsAt.Before(out[j].StartsAt) })
	return out
}

// effectiveOn reports whether the rule applies to the given calendar day.
func effectiveOn(r Rule, day time.Time) bool {
	if r.EffectiveFrom != nil && day.Before(midnightIn(*r.EffectiveFrom, day.Location())) {
		return false
	}
	if r.EffectiveTo != nil && !day.Before(midnightIn(*r.EffectiveTo, day.Location())) {
		return false
	}
	return true
}

func midnightIn(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

// Covers reports whether at least one rule window fully contains [s,e).
// Used to validate an explicitly requested appointment time (as opposed to
// grid-generated candidates).
func Covers(rules []Rule, loc *time.Location, s, e time.Time) bool {
	if loc == nil {
		loc = time.UTC
	}
	sl := s.In(loc)
	day := time.Date(sl.Year(), sl.Month(), sl.Day(), 0, 0, 0, 0, loc)
	for _, r := range rules {
		if r.Weekday != day.Weekday() || r.EndMin <= r.StartMin {
			continue
		}
		if !effectiveOn(r, day) {
			continue
		}
		winStart, winEnd := ruleWindow(day, r.StartMin, r.EndMin)
		if !s.Before(winStart) && !e.After(winEnd) {
			return true
		}
	}
	return false
}

// ruleWindow builds the [start,end) wall-clock window of a rule on the
// given calendar day (SPEC-W43 K-04). time.Date arithmetic — NOT
// midnight.Add(duration) — so a DST transition inside the day keeps
// "09:00" at 09:00 local wall-clock time (midnight.Add(9h) would land at
// 10:00 on a spring-forward day). EndMin=1440 (midnight end-of-day) spills
// to 00:00 of the next day, which time.Date normalizes correctly.
func ruleWindow(day time.Time, startMin, endMin int) (time.Time, time.Time) {
	loc := day.Location()
	y, m, d := day.Date()
	winStart := time.Date(y, m, d, startMin/60, startMin%60, 0, 0, loc)
	winEnd := time.Date(y, m, d, endMin/60, endMin%60, 0, 0, loc)
	return winStart, winEnd
}

// Fits reports whether candidate [s,e) can be booked given existing bookings,
// the required buffer and the capacity. Exported for single-candidate
// validation in the booking write path.
func Fits(s, e time.Time, buffer time.Duration, capacity int, bookings []Booking) bool {
	if capacity <= 0 {
		capacity = 1
	}
	return fits(s, e, buffer, capacity, bookings)
}

// fits reports whether candidate [s,e) can be booked given existing bookings,
// the required buffer and the capacity.
func fits(s, e time.Time, buffer time.Duration, capacity int, bookings []Booking) bool {
	concurrent := 0
	for _, b := range bookings {
		if overlaps(s, e, b.StartsAt, b.EndsAt) {
			concurrent++
			if concurrent >= capacity {
				return false
			}
			continue
		}
		// Non-overlapping bookings must still respect the buffer: no other
		// appointment may sit within `buffer` of the candidate on either side.
		if buffer > 0 && overlaps(s.Add(-buffer), e.Add(buffer), b.StartsAt, b.EndsAt) {
			return false
		}
	}
	return true
}

// overlaps reports whether half-open intervals [s1,e1) and [s2,e2) intersect.
func overlaps(s1, e1, s2, e2 time.Time) bool {
	return s1.Before(e2) && s2.Before(e1)
}
