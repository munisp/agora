package bookingops

// SPEC-W80 B13 (cross-workstream contract with Coder C): the pack-declared
// TemporalWorkflow must ride CreateInput → SagaInput as workflow_name.
//   - explicit CreateInput.WorkflowName (public/waitlist/consumer paths) →
//     carried verbatim;
//   - the BookingPolicy.TemporalWorkflow mirror (the INVARIANT staff path
//     flows it through tenant.Pack.BookingPolicy untouched) → fallback;
//   - no pack at all → EMPTY string (legacy behavior — the worker owns
//     the fallback; never invent a default).
// Struct-plumbing seam only — a recordingSaga (sweeper_test.go) stands in
// for Temporal.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// w80CreateFixture seeds offering + member + Mon–Sun 08:00–20:00 rules so
// Service.Create's slot check passes, and returns the ids.
func w80CreateFixture(t *testing.T, st *store.Store, ctx context.Context, tenantID uuid.UUID) (offeringID, memberID uuid.UUID) {
	t.Helper()
	offering := store.Offering{TenantID: tenantID, Name: "Cut", DurationMin: 30, BufferMin: 0, Capacity: 1, Bookable: true}
	if err := st.CreateOffering(ctx, &offering); err != nil {
		t.Fatalf("offering: %v", err)
	}
	member := store.TeamMember{TenantID: tenantID, Name: "Ana", Active: true}
	if err := st.CreateTeamMember(ctx, &member); err != nil {
		t.Fatalf("member: %v", err)
	}
	rules := make([]store.AvailabilityRule, 0, 7)
	for wd := 0; wd < 7; wd++ {
		rules = append(rules, store.AvailabilityRule{TenantID: tenantID, TeamMemberID: member.ID, Weekday: wd, StartMin: 8 * 60, EndMin: 20 * 60})
	}
	if err := st.SetAvailability(ctx, tenantID, member.ID, rules); err != nil {
		t.Fatalf("rules: %v", err)
	}
	return offering.ID, member.ID
}

func TestStartSagaCarriesPackWorkflowName(t *testing.T) {
	st, ctx := newW80ClaimStore(t)
	tenantID := uuid.New()
	offeringID, memberID := w80CreateFixture(t, st, ctx, tenantID)

	saga := &recordingSaga{}
	svc := &Service{Store: st, Saga: saga, EventsTopic: "test.events", Logger: zap.NewNop()}

	cases := []struct {
		name string
		in   CreateInput
		want string
	}{
		{
			// healthcare pack declares ClinicIntakeWorkflow — the explicit
			// CreateInput.WorkflowName path (public/waitlist/consumer).
			name: "pack-declared workflow rides explicitly",
			in: CreateInput{
				WorkflowName: "ClinicIntakeWorkflow",
				Industry:     "healthcare",
			},
			want: "ClinicIntakeWorkflow",
		},
		{
			// The invariant staff path: only BookingPolicy flows (carrying
			// the resolver's TemporalWorkflow mirror).
			name: "booking-policy mirror fallback",
			in: CreateInput{
				Industry:      "healthcare",
				BookingPolicy: &BookingPolicy{DepositPercent: 30, TemporalWorkflow: "ClinicIntakeWorkflow"},
			},
			want: "ClinicIntakeWorkflow",
		},
		{
			name: "no pack keeps legacy empty",
			in:   CreateInput{},
			want: "",
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			in.TenantID = tenantID
			in.TenantSlug = "acme"
			in.Timezone = "UTC"
			in.OfferingID = offeringID
			in.TeamMemberID = memberID
			in.Contact = &ContactInput{Name: "Ada", Phone: "+234800000001"}
			in.StartsAt = w80NextNoonUTC().Add(time.Duration(i) * time.Hour)
			if _, err := svc.Create(ctx, in); err != nil {
				t.Fatalf("create: %v", err)
			}
			calls := saga.started()
			if len(calls) == 0 {
				t.Fatal("no saga started")
			}
			got := calls[len(calls)-1].WorkflowName
			if got != tc.want {
				t.Fatalf("saga WorkflowName = %q, want %q", got, tc.want)
			}
			// The contract wire name is exactly "workflow_name".
			raw, err := json.Marshal(calls[len(calls)-1])
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]any
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if _, present := wire["workflow_name"]; present {
					t.Fatalf("workflow_name must be omitted when empty, wire: %s", raw)
				}
			} else if wire["workflow_name"] != tc.want {
				t.Fatalf("wire workflow_name = %v, want %q (%s)", wire["workflow_name"], tc.want, raw)
			}
		})
	}
}

// The tenant resolver (BySlug) mirrors PackSummary.TemporalWorkflow into
// the nested BookingPolicy so the invariant staff create path carries it
// (B13). Exercised end-to-end through the IDENTITY_BASE_URL direct-HTTP
// fallback — no Dapr sidecar needed.
func TestTenantResolverMirrorsWorkflowIntoBookingPolicy(t *testing.T) {
	tenantID := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":       tenantID.String(),
			"timezone": "Africa/Lagos",
			"industry": "healthcare",
			"pack": map[string]any{
				"id":               "healthcare-ng",
				"displayName":      "Healthcare",
				"bookingPolicy":    map[string]any{"depositPercent": 25},
				"temporalWorkflow": "ClinicIntakeWorkflow",
			},
		})
	}))
	t.Cleanup(srv.Close)

	res := NewTenantResolver(nil, "", 0, nil, WithIdentityBaseURL(srv.URL))
	info, err := res.BySlug(context.Background(), "acme")
	if err != nil {
		t.Fatalf("BySlug: %v", err)
	}
	if info.Pack == nil {
		t.Fatal("pack must resolve")
	}
	if info.Pack.TemporalWorkflow != "ClinicIntakeWorkflow" {
		t.Fatalf("pack workflow = %q", info.Pack.TemporalWorkflow)
	}
	if info.Pack.BookingPolicy.TemporalWorkflow != "ClinicIntakeWorkflow" {
		t.Fatalf("booking-policy mirror = %q, want ClinicIntakeWorkflow (staff-path carrier)",
			info.Pack.BookingPolicy.TemporalWorkflow)
	}
	// A cached read returns the same mirrored form.
	info2, err := res.BySlug(context.Background(), "acme")
	if err != nil {
		t.Fatalf("BySlug cached: %v", err)
	}
	if info2.Pack.BookingPolicy.TemporalWorkflow != "ClinicIntakeWorkflow" {
		t.Fatalf("cached mirror = %q", info2.Pack.BookingPolicy.TemporalWorkflow)
	}
}
