package geo

import (
	"testing"
	"time"
)

func TestSummaryBoundsAcceptDates(t *testing.T) {
	from, err := parseSummaryBound("2026-08-17", false)
	if err != nil || !from.Equal(time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("from: %v %v", from, err)
	}
	to, err := parseSummaryBound("2026-11-15", true)
	if err != nil || to.Before(time.Date(2026, 11, 15, 23, 59, 0, 0, time.UTC)) || !to.Before(time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("to covers the whole day: %v %v", to, err)
	}
	if _, err := parseSummaryBound("2026-11-15T10:00:00Z", true); err != nil {
		t.Fatal("RFC3339 still accepted")
	}
	if _, err := parseSummaryBound("15/11/2026", false); err == nil {
		t.Fatal("other formats refused")
	}
}
