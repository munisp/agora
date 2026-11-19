package bookingops

// SPEC-W92 C6 (same cross-workstream contract shape as W80 B13): the
// pack-declared SLA anchor must ride CreateInput → SagaInput as
// sla_anchor, the booking row's created_at must ALWAYS ride as
// booking_created_at, and the resolver mirrors PackSummary.SLAAnchor into
// the nested BookingPolicy (the invariant staff path).
// Redirection: dropping the startSaga assignment fails the saga-input
// assertions; dropping the resolver mirror line fails the resolver test.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

func TestStartSagaCarriesSLAAnchor(t *testing.T) {
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
			name: "explicit anchor rides",
			in:   CreateInput{SLAAnchor: "ticket_created", Industry: "support-desk"},
			want: "ticket_created",
		},
		{
			name: "booking-policy mirror fallback",
			in: CreateInput{
				Industry:      "support-desk",
				BookingPolicy: &BookingPolicy{SLAAnchor: "ticket_created"},
			},
			want: "ticket_created",
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
			in.Contact = &ContactInput{Name: "Ada", Phone: "+234800000002"}
			in.StartsAt = w80NextNoonUTC().Add(time.Duration(i) * time.Hour)
			booking, err := svc.Create(ctx, in)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			calls := saga.started()
			if len(calls) == 0 {
				t.Fatal("no saga started")
			}
			last := calls[len(calls)-1]
			if last.SLAAnchor != tc.want {
				t.Fatalf("saga SLAAnchor = %q, want %q", last.SLAAnchor, tc.want)
			}
			// booking_created_at ALWAYS rides — the worker needs it the
			// moment any pack declares ticket_created.
			if !last.BookingCreatedAt.Equal(booking.CreatedAt) {
				t.Fatalf("BookingCreatedAt = %v, want the booking row created_at %v",
					last.BookingCreatedAt, booking.CreatedAt)
			}
			raw, err := json.Marshal(last)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]any
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if _, present := wire["sla_anchor"]; present {
					t.Fatalf("sla_anchor must be omitted when empty, wire: %s", raw)
				}
			} else if wire["sla_anchor"] != tc.want {
				t.Fatalf("wire sla_anchor = %v, want %q (%s)", wire["sla_anchor"], tc.want, raw)
			}
		})
	}
}

// The tenant resolver mirrors PackSummary.SLAAnchor into the nested
// BookingPolicy (C6), same decode-time mirror as TemporalWorkflow (B13).
func TestTenantResolverMirrorsSLAAnchorIntoBookingPolicy(t *testing.T) {
	tenantID := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":       tenantID.String(),
			"timezone": "Africa/Lagos",
			"industry": "support-desk",
			"pack": map[string]any{
				"id":               "support-desk",
				"displayName":      "Support Desk",
				"bookingPolicy":    map[string]any{"depositPercent": 0},
				"temporalWorkflow": "SupportEscalationWorkflow",
				"slaAnchor":        "ticket_created",
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
	if info.Pack.SLAAnchor != "ticket_created" {
		t.Fatalf("pack slaAnchor = %q", info.Pack.SLAAnchor)
	}
	if info.Pack.BookingPolicy.SLAAnchor != "ticket_created" {
		t.Fatalf("booking-policy mirror = %q, want ticket_created (staff-path carrier)",
			info.Pack.BookingPolicy.SLAAnchor)
	}
	info2, err := res.BySlug(context.Background(), "acme")
	if err != nil {
		t.Fatalf("BySlug cached: %v", err)
	}
	if info2.Pack.BookingPolicy.SLAAnchor != "ticket_created" {
		t.Fatalf("cached mirror = %q", info2.Pack.BookingPolicy.SLAAnchor)
	}
}
