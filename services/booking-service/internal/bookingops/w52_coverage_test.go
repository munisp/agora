package bookingops

// SPEC-W52 Scope B: coverage-zone enforcement matrix. The decision logic is
// exercised through the CoverageCounter seam (a fake — the embedded Postgres
// of the store/httpapi tests has no PostGIS, so the ST_Covers queries cannot
// run there; the SQL text is asserted separately and the httpapi-level test
// proves the 422/503 mappings and the public-path marker wiring).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/store"
)

// w52FakeCounter answers the three coverage counts (areas, contact points,
// covered) in query order; err short-circuits every call (the PostGIS-absent
// case).
type w52FakeCounter struct {
	areas, points, covered int
	err                    error
	calls                  int
}

func (f *w52FakeCounter) AudienceCount(_ context.Context, _ uuid.UUID, q string, _ []any) (int, error) {
	f.calls++
	if f.err != nil {
		return 0, f.err
	}
	switch {
	case strings.Contains(q, "ST_Covers"):
		return f.covered, nil
	case strings.Contains(q, "FROM contact_locations"):
		return f.points, nil
	default: // service_areas count
		return f.areas, nil
	}
}

func TestW52CoverageMatrix(t *testing.T) {
	tenantID, contactID := uuid.New(), uuid.New()
	public := MarkPublicBookingCreate(context.Background())

	cases := []struct {
		name      string
		enforce   bool
		ctx       context.Context
		counter   *w52FakeCounter
		wantErr   error // nil = pass
		wantCalls int   // 0 proves the gate never touched the DB
	}{
		{"enforce disabled → pass, no DB", false, public, &w52FakeCounter{areas: 5, covered: 0}, nil, 0},
		{"enabled but tenant API path (no marker) → pass, no DB", true, context.Background(), &w52FakeCounter{areas: 5, covered: 0}, nil, 0},
		{"enabled + no zones → pass (documented semantics)", true, public, &w52FakeCounter{areas: 0}, nil, 1},
		{"enabled + zones + contact without fix → pass", true, public, &w52FakeCounter{areas: 2, points: 0}, nil, 2},
		{"enabled + inside coverage → pass", true, public, &w52FakeCounter{areas: 2, points: 1, covered: 1}, nil, 3},
		{"enabled + outside coverage → 422 sentinel", true, public, &w52FakeCounter{areas: 2, points: 1, covered: 0}, ErrGeoOutsideCoverage, 3},
		{"enabled + PostGIS absent → ErrPostGISUnavailable (503 upstream)", true, public,
			&w52FakeCounter{err: store.ErrPostGISUnavailable}, store.ErrPostGISUnavailable, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{GeoEnforce: tc.enforce, Coverage: tc.counter} // Store nil: seam must be the only path
			err := s.checkGeoCoverage(tc.ctx, tenantID, contactID)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.counter.calls != tc.wantCalls {
				t.Fatalf("counter calls = %d, want %d", tc.counter.calls, tc.wantCalls)
			}
		})
	}
}

// TestW52CoverageSQLBuilders pins the parameterized query text (house rule:
// geometry only via bound parameters; tenant scoping by $1 under withTenant
// RLS). The builders stay pure so this runs without a database.
func TestW52CoverageSQLBuilders(t *testing.T) {
	contactID := uuid.New()

	areasQ, areasArgs := buildServiceAreaCountQuery()
	if areasQ != `SELECT count(*) FROM service_areas WHERE tenant_id=$1` || len(areasArgs) != 0 {
		t.Fatalf("areas query drifted: %q %v", areasQ, areasArgs)
	}
	pointQ, pointArgs := buildContactPointCountQuery(contactID)
	if !strings.Contains(pointQ, "contact_id=$2") || len(pointArgs) != 1 || pointArgs[0] != contactID {
		t.Fatalf("point query drifted: %q %v", pointQ, pointArgs)
	}
	covQ, covArgs := buildCoveredCountQuery(contactID)
	if !strings.Contains(covQ, "ST_Covers(a.geom, cl.geom)") || !strings.Contains(covQ, "a.tenant_id=$1") ||
		len(covArgs) != 1 || covArgs[0] != contactID {
		t.Fatalf("coverage query drifted: %q %v", covQ, covArgs)
	}
	// No string interpolation of coordinates ever (the point comes from
	// contact_locations, not request input).
	for _, q := range []string{areasQ, pointQ, covQ} {
		if strings.Contains(q, "ST_MakePoint") || strings.Contains(q, "$3") {
			t.Fatalf("query takes raw geometry input: %q", q)
		}
	}
}

// TestW52PublicMarkerRoundTrip pins the marker helpers.
func TestW52PublicMarkerRoundTrip(t *testing.T) {
	if isPublicBookingCreate(context.Background()) {
		t.Fatal("unmarked context reported as public create")
	}
	if !isPublicBookingCreate(MarkPublicBookingCreate(context.Background())) {
		t.Fatal("marked context not reported as public create")
	}
}
