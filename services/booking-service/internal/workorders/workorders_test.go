package workorders

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Transition matrix (SPEC-W19 Agent B):
// created→assigned→en_route→on_site→completed, any→cancelled, plus the
// documented assigned→assigned re-dispatch edge.
func TestTransitionMatrix(t *testing.T) {
	legal := map[string][]string{
		StatusCreated:  {StatusAssigned, StatusCancelled},
		StatusAssigned: {StatusAssigned, StatusEnRoute, StatusCancelled}, // assigned→assigned = re-dispatch
		StatusEnRoute:  {StatusOnSite, StatusCancelled},
		StatusOnSite:   {StatusCompleted, StatusCancelled},
	}
	for _, from := range Statuses {
		for _, to := range Statuses {
			want := false
			for _, ok := range legal[from] {
				if ok == to {
					want = true
				}
			}
			if got := CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
			err := ValidateTransition(from, to)
			if want && err != nil {
				t.Errorf("ValidateTransition(%s, %s) = %v, want nil", from, to, err)
			}
			if !want && !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("ValidateTransition(%s, %s) = %v, want ErrInvalidTransition", from, to, err)
			}
		}
	}
	// Unknown statuses are rejected as transitions AND as enum input.
	if err := ValidateTransition("bogus", StatusCreated); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("unknown from = %v", err)
	}
	if err := ValidateStatus("bogus"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("ValidateStatus(bogus) = %v", err)
	}
}

// Completion gate: all checklist items done + non-empty proof.notes.
func TestCompletionGate(t *testing.T) {
	mk := func() *WorkOrder {
		return &WorkOrder{
			Status: StatusOnSite,
			Checklist: []ChecklistItem{
				{Label: "inspect", Done: true},
				{Label: "repair", Done: true},
			},
			Proof: Proof{Notes: "fixed the compressor"},
		}
	}
	if err := mk().checkCompletionGate(); err != nil {
		t.Fatalf("complete order rejected: %v", err)
	}

	undone := mk()
	undone.Checklist[1].Done = false
	if err := undone.checkCompletionGate(); !errors.Is(err, ErrCompletionGate) {
		t.Fatalf("undone checklist = %v, want ErrCompletionGate", err)
	}

	noNotes := mk()
	noNotes.Proof.Notes = "   "
	if err := noNotes.checkCompletionGate(); !errors.Is(err, ErrCompletionGate) {
		t.Fatalf("empty notes = %v, want ErrCompletionGate", err)
	}

	// Empty checklist passes vacuously (nothing left undone) — notes still required.
	empty := mk()
	empty.Checklist = nil
	if err := empty.checkCompletionGate(); err != nil {
		t.Fatalf("empty checklist with notes rejected: %v", err)
	}
	empty.Proof.Notes = ""
	if err := empty.checkCompletionGate(); !errors.Is(err, ErrCompletionGate) {
		t.Fatalf("empty checklist without notes = %v, want ErrCompletionGate", err)
	}
}

// Field validation: required fields, enums, GPS bounds, checklist/proof bounds.
func TestValidate(t *testing.T) {
	lat, lng, acc := 6.5244, 3.3792, 12.0
	good := WorkOrder{
		TenantID:    uuid.New(),
		Title:       "Fix AC unit",
		Status:      StatusCreated,
		GPSLat:      &lat,
		GPSLng:      &lng,
		GPSAccuracy: &acc,
		Checklist:   []ChecklistItem{{Label: "inspect"}},
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid order rejected: %v", err)
	}

	cases := map[string]func(*WorkOrder){
		"missing tenant":   func(w *WorkOrder) { w.TenantID = uuid.Nil },
		"empty title":      func(w *WorkOrder) { w.Title = "  " },
		"oversized title":  func(w *WorkOrder) { w.Title = strings.Repeat("x", maxTitleLen+1) },
		"bad status":       func(w *WorkOrder) { w.Status = "doing" },
		"lat without lng":  func(w *WorkOrder) { w.GPSLng = nil },
		"lat out of range": func(w *WorkOrder) { *w.GPSLat = 91 },
		"lng out of range": func(w *WorkOrder) { *w.GPSLng = -181 },
		"negative accuracy": func(w *WorkOrder) {
			neg := -1.0
			w.GPSAccuracy = &neg
		},
		"blank checklist label": func(w *WorkOrder) { w.Checklist = []ChecklistItem{{Label: " "}} },
		"end before start": func(w *WorkOrder) {
			start := time.Now()
			end := start.Add(-time.Hour)
			w.ScheduledStart = &start
			w.ScheduledEnd = &end
		},
		"oversized notes": func(w *WorkOrder) { w.Proof.Notes = strings.Repeat("n", maxProofNotesLen+1) },
		"bad capture id": func(w *WorkOrder) {
			v := " "
			w.FieldCaptureID = &v
		},
	}
	for name, mutate := range cases {
		w := good
		w.Checklist = []ChecklistItem{{Label: "inspect"}}
		mutate(&w)
		if err := w.Validate(); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: = %v, want ErrInvalidInput", name, err)
		}
	}
}

// Checklist bound: >100 items rejected.
func TestChecklistBound(t *testing.T) {
	items := make([]ChecklistItem, maxChecklistItems+1)
	for i := range items {
		items[i] = ChecklistItem{Label: "x"}
	}
	if err := ValidateChecklist(items); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("oversized checklist = %v", err)
	}
}
