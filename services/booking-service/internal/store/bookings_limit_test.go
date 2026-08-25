package store

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// SPEC-W44 W-B/F15-10: an over-large BookingFilter.Limit is CLAMPED to 500,
// not silently reset to the 100 default. 150 rows distinguish the two
// behaviors (clamp 500 → 150 rows; reset 100 → 100 rows).
func TestListBookingsClampsLimit(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	// 150 cheap rows via generate_series (bookingCols slice only).
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO bookings (tenant_id, offering_id, starts_at, ends_at)
		SELECT $1, $2, now() + (g || ' hours')::interval, now() + ((g+1) || ' hours')::interval
		FROM generate_series(1, 150) g`, tenantID, uuid.New()); err != nil {
		t.Fatalf("seed bookings: %v", err)
	}

	rows, err := st.ListBookings(ctx, tenantID, BookingFilter{Limit: 9999})
	if err != nil {
		t.Fatalf("ListBookings: %v", err)
	}
	if len(rows) != 150 {
		t.Fatalf("limit 9999 returned %d rows, want 150 (clamp to 500, NOT reset to 100)", len(rows))
	}

	// An explicit small limit still wins.
	rows, err = st.ListBookings(ctx, tenantID, BookingFilter{Limit: 7})
	if err != nil {
		t.Fatalf("ListBookings: %v", err)
	}
	if len(rows) != 7 {
		t.Fatalf("limit 7 returned %d rows", len(rows))
	}

	// The default stays 100.
	rows, err = st.ListBookings(ctx, tenantID, BookingFilter{})
	if err != nil {
		t.Fatalf("ListBookings: %v", err)
	}
	if len(rows) != 100 {
		t.Fatalf("default limit returned %d rows, want 100", len(rows))
	}
}
