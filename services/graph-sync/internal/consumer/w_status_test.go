package consumer

import (
	"testing"

	"github.com/opendesk/graph-sync/internal/events"
)

// 2026-11-21: booking-service sends the pre-change row (a BookingConfirmed
// says "pending"), so the status comes from the event type; Created only
// seeds the status of a new node.
func TestBookingStatusFor(t *testing.T) {
	cases := []struct {
		typ, payload, want string
		initial            bool
	}{
		{events.TypeBookingCreated, "pending", "pending", true},
		{events.TypeBookingCreated, "", "pending", true},
		{events.TypeBookingConfirmed, "pending", "confirmed", false},
		{events.TypeBookingCompleted, "confirmed", "completed", false},
		{events.TypeBookingCancelled, "confirmed", "cancelled", false},
		{events.TypeBookingNoShow, "confirmed", "no_show", false},
		{events.TypeBookingRescheduled, "confirmed", "confirmed", true},
	}
	for _, c := range cases {
		got, initial := bookingStatusFor(c.typ, c.payload)
		if got != c.want || initial != c.initial {
			t.Errorf("%s/%q: got (%q,%v) want (%q,%v)", c.typ, c.payload, got, initial, c.want, c.initial)
		}
	}
}
