package workforce

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Package unit tests (no database): validation, the shift status machine,
// error wrapping and the CloudEvents marshalling.

func TestShiftTransitionTable(t *testing.T) {
	legal := [][2]string{
		{ShiftScheduled, ShiftConfirmed},
		{ShiftScheduled, ShiftCompleted},
		{ShiftScheduled, ShiftNoShow},
		{ShiftScheduled, ShiftCancelled},
		{ShiftConfirmed, ShiftCompleted},
		{ShiftConfirmed, ShiftNoShow},
		{ShiftConfirmed, ShiftCancelled},
		{ShiftScheduled, ShiftScheduled}, // no-op
		{ShiftCompleted, ShiftCompleted}, // no-op
	}
	for _, tr := range legal {
		if err := ValidateShiftTransition(tr[0], tr[1]); err != nil {
			t.Errorf("%s → %s rejected: %v", tr[0], tr[1], err)
		}
	}
	illegal := [][2]string{
		{ShiftConfirmed, ShiftScheduled},
		{ShiftCompleted, ShiftScheduled},
		{ShiftCancelled, ShiftScheduled},
		{ShiftNoShow, ShiftConfirmed},
		{ShiftCompleted, ShiftCancelled},
	}
	for _, tr := range illegal {
		if err := ValidateShiftTransition(tr[0], tr[1]); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s → %s = %v, want ErrInvalidTransition", tr[0], tr[1], err)
		}
	}
	if err := ValidateShiftTransition("bogus", ShiftScheduled); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("unknown status = %v, want ErrInvalidTransition", err)
	}
}

func TestShiftValidate(t *testing.T) {
	base := Shift{
		TenantID: uuid.New(), AgentID: uuid.New(),
		StartsAt: time.Date(2030, 1, 1, 9, 0, 0, 0, time.UTC),
		EndsAt:   time.Date(2030, 1, 1, 17, 0, 0, 0, time.UTC),
		Status:   ShiftScheduled,
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid shift rejected: %v", err)
	}

	cases := []struct {
		name  string
		mut   func(*Shift)
		isErr error
	}{
		{"no tenant", func(s *Shift) { s.TenantID = uuid.Nil }, ErrInvalidInput},
		{"no agent", func(s *Shift) { s.AgentID = uuid.Nil }, ErrInvalidInput},
		{"zero window", func(s *Shift) { s.StartsAt = time.Time{} }, ErrInvalidInput},
		{"ends before starts", func(s *Shift) { s.EndsAt = s.StartsAt.Add(-time.Hour) }, ErrInvalidInput},
		{"zero-length window", func(s *Shift) { s.EndsAt = s.StartsAt }, ErrInvalidInput},
		{"bad status", func(s *Shift) { s.Status = "bogus" }, ErrInvalidInput},
	}
	for _, tc := range cases {
		s := base
		tc.mut(&s)
		if err := s.Validate(); !errors.Is(err, tc.isErr) {
			t.Errorf("%s = %v, want %v", tc.name, err, tc.isErr)
		}
	}

	// Role is trimmed; overlong role rejected.
	s := base
	s.Role = "  front desk  "
	if err := s.Validate(); err != nil || s.Role != "front desk" {
		t.Fatalf("role trim: %q, %v", s.Role, err)
	}
}

func TestLeaveValidate(t *testing.T) {
	base := LeaveRequest{
		TenantID: uuid.New(), AgentID: uuid.New(), Kind: LeaveAnnual,
		StartsOn: time.Date(2030, 5, 4, 0, 0, 0, 0, time.UTC),
		EndsOn:   time.Date(2030, 5, 4, 0, 0, 0, 0, time.UTC), // same-day leave is legal
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("same-day leave rejected: %v", err)
	}
	for _, kind := range []string{"annual", "sick", "unpaid"} {
		if err := ValidateKind(kind); err != nil {
			t.Errorf("kind %s rejected: %v", kind, err)
		}
	}
	if err := ValidateKind("sabbatical"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("bad kind = %v, want ErrInvalidInput", err)
	}
	l := base
	l.EndsOn = l.StartsOn.AddDate(0, 0, -1)
	if err := l.Validate(); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("inverted range = %v, want ErrInvalidInput", err)
	}
}

func TestTimeEntryGPSAndMethod(t *testing.T) {
	lat := 6.5244
	e := TimeEntry{GPSLat: &lat} // lng missing
	if err := e.ValidateGPS(); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("half gps = %v, want ErrInvalidInput", err)
	}
	lat = 91
	lng := 3.0
	e = TimeEntry{GPSLat: &lat, GPSLng: &lng}
	if err := e.ValidateGPS(); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("lat 91 = %v, want ErrInvalidInput", err)
	}
	lat = 6.5244
	e = TimeEntry{GPSLat: &lat, GPSLng: &lng}
	if err := e.ValidateGPS(); err != nil {
		t.Errorf("valid gps rejected: %v", err)
	}
	if err := ValidateMethod(MethodWeb); err != nil {
		t.Errorf("web rejected: %v", err)
	}
	if err := ValidateMethod(MethodFieldPWA); err != nil {
		t.Errorf("field_pwa rejected: %v", err)
	}
	if err := ValidateMethod("smoke-signal"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("bad method = %v, want ErrInvalidInput", err)
	}
}

func TestErrorWrapping(t *testing.T) {
	var overlap error = OverlapError{ConflictShiftID: uuid.New()}
	if !errors.Is(overlap, ErrShiftOverlap) {
		t.Error("OverlapError does not unwrap to ErrShiftOverlap")
	}
	var open error = OpenEntryError{EntryID: uuid.New()}
	if !errors.Is(open, ErrOpenEntry) {
		t.Error("OpenEntryError does not unwrap to ErrOpenEntry")
	}
}

func TestEventMarshalling(t *testing.T) {
	tenantID, agentID := uuid.New(), uuid.New()
	sh := Shift{
		ID: uuid.New(), TenantID: tenantID, AgentID: agentID,
		StartsAt: time.Date(2030, 3, 10, 9, 0, 0, 0, time.UTC),
		EndsAt:   time.Date(2030, 3, 10, 17, 0, 0, 0, time.UTC),
		Role:     "front desk", Status: ShiftScheduled,
	}
	payload, err := MarshalShiftAssignedEvent("acme", sh)
	if err != nil {
		t.Fatalf("marshal assigned: %v", err)
	}
	var evt struct {
		SpecVersion string         `json:"specversion"`
		Type        string         `json:"type"`
		Subject     string         `json:"subject"`
		TenantID    string         `json:"tenantid"`
		Data        map[string]any `json:"data"`
	}
	if err := json.Unmarshal(payload, &evt); err != nil {
		t.Fatalf("assigned envelope: %v", err)
	}
	if evt.SpecVersion != "1.0" || evt.Type != EventTypeShiftAssigned || evt.Subject != "acme" || evt.TenantID != tenantID.String() {
		t.Fatalf("assigned envelope wrong: %+v", evt)
	}
	if evt.Data["shift_id"] != sh.ID.String() || evt.Data["agent_id"] != agentID.String() || evt.Data["role"] != "front desk" {
		t.Fatalf("assigned data wrong: %+v", evt.Data)
	}

	decidedAt := time.Now().UTC()
	l := LeaveRequest{
		ID: uuid.New(), TenantID: tenantID, AgentID: agentID, Kind: LeaveSick,
		StartsOn: time.Date(2030, 5, 4, 0, 0, 0, 0, time.UTC),
		EndsOn:   time.Date(2030, 5, 6, 0, 0, 0, 0, time.UTC),
		Status:   LeaveApproved, DecidedBy: "manager-7", DecidedAt: &decidedAt,
	}
	payload, err = MarshalLeaveDecidedEvent("acme", l)
	if err != nil {
		t.Fatalf("marshal decided: %v", err)
	}
	if err := json.Unmarshal(payload, &evt); err != nil {
		t.Fatalf("decided envelope: %v", err)
	}
	if evt.Type != EventTypeLeaveDecided {
		t.Fatalf("decided type = %s", evt.Type)
	}
	if evt.Data["decision"] != LeaveApproved || evt.Data["decided_by"] != "manager-7" ||
		evt.Data["starts_on"] != "2030-05-04" || evt.Data["kind"] != LeaveSick {
		t.Fatalf("decided data wrong: %+v", evt.Data)
	}
}
