package fieldcapture

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/civic"
	"github.com/opendesk/booking-service/internal/store"
)

// SPEC-W32 WS-A: kind=civic_report — a field agent files a civic report on
// behalf of a citizen; the payload is the public report body and the civic
// module creates the case with channel=pwa.

// fakeCivicSubmitter records civic submissions (civic.Service satisfies the
// interface in production).
type fakeCivicSubmitter struct {
	calls       int
	lastIn      civic.ReportInput
	lastChannel string
	lastTenant  uuid.UUID
	err         error
}

func (f *fakeCivicSubmitter) Submit(_ context.Context, tenantID uuid.UUID, _, channel string, in civic.ReportInput) (store.CivicCase, error) {
	f.calls++
	f.lastIn = in
	f.lastChannel = channel
	f.lastTenant = tenantID
	if f.err != nil {
		return store.CivicCase{}, f.err
	}
	return store.CivicCase{ID: uuid.New(), TenantID: tenantID, Ref: "GOV-IKEJA-WARD3-2026-000001", Channel: channel}, nil
}

var testTenant = bookingops.TenantInfo{ID: uuid.New(), Slug: "ikeja-lga"}

// The kind enum accepts civic_report (validation matrix addition).
func TestCivicReportKindAccepted(t *testing.T) {
	payload, _ := json.Marshal(CivicReportPayload{
		CategorySlug: "roads",
		Description:  "Deep pothole at the junction blocking one lane",
	})
	it := CaptureItem{ClientID: uuid.NewString(), Kind: KindCivicReport, Payload: payload}
	if err := it.Validate(); err != nil {
		t.Fatalf("civic_report must validate: %v", err)
	}
}

// applyCivicReport builds the civic input from the payload, attaches the
// item GPS fix as the case location and returns the case ref/id.
func TestApplyCivicReport(t *testing.T) {
	fake := &fakeCivicSubmitter{}
	svc := &Service{Civic: fake}
	payload, _ := json.Marshal(CivicReportPayload{
		CategorySlug:      "roads",
		Description:       "Deep pothole at the junction blocking one lane",
		Ward:              "Ward 3",
		LGA:               "Ikeja",
		ReporterPhoneE164: "+2348012345678",
	})
	it := CaptureItem{
		ClientID: uuid.NewString(), Kind: KindCivicReport, Payload: payload,
		GPS: &GPS{Lat: 6.5244, Lng: 3.3792, Accuracy: 12},
	}
	res := &ItemResult{}
	if err := svc.applyCivicReport(context.Background(), testTenant, it, res); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if fake.calls != 1 || fake.lastChannel != civic.ChannelPWA {
		t.Fatalf("submit calls=%d channel=%q", fake.calls, fake.lastChannel)
	}
	if fake.lastTenant != testTenant.ID {
		t.Fatalf("tenant = %v", fake.lastTenant)
	}
	if fake.lastIn.Lat == nil || *fake.lastIn.Lat != 6.5244 || fake.lastIn.Lon == nil || *fake.lastIn.Lon != 3.3792 {
		t.Fatalf("gps not attached: %+v", fake.lastIn)
	}
	if fake.lastIn.Ward != "Ward 3" || fake.lastIn.ReporterPhoneE164 != "+2348012345678" {
		t.Fatalf("payload fields lost: %+v", fake.lastIn)
	}
	if res.CaseRef != "GOV-IKEJA-WARD3-2026-000001" || res.CaseID == nil {
		t.Fatalf("result = %+v", res)
	}
}

// Validation failures of the payload surface as deterministic errors
// (recorded on the anchor; replays dedupe to the same outcome).
func TestApplyCivicReportInvalidPayload(t *testing.T) {
	fake := &fakeCivicSubmitter{}
	svc := &Service{Civic: fake}
	it := CaptureItem{ClientID: uuid.NewString(), Kind: KindCivicReport, Payload: json.RawMessage(`{"category_slug":`)}
	res := &ItemResult{}
	if err := svc.applyCivicReport(context.Background(), testTenant, it, res); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad JSON err = %v", err)
	}
	// Civic-side validation errors propagate as deterministic too.
	fake.err = civic.ErrInvalidInput
	it.Payload = json.RawMessage(`{"category_slug":"roads","description":"short"}`)
	if err := svc.applyCivicReport(context.Background(), testTenant, it, res); !errors.Is(err, civic.ErrInvalidInput) {
		t.Fatalf("civic validation err = %v", err)
	}
}

// Nil Civic wiring → deterministic error (same posture as nil Leads).
func TestApplyCivicReportUnavailable(t *testing.T) {
	svc := &Service{}
	it := CaptureItem{ClientID: uuid.NewString(), Kind: KindCivicReport, Payload: json.RawMessage(`{}`)}
	if err := svc.applyCivicReport(context.Background(), testTenant, it, &ItemResult{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unavailable err = %v", err)
	}
}

// Full capture pipe (embedded Postgres; -short skips): a civic_report item
// applies once, and a client_id replay dedupes to the ORIGINAL case_ref
// without re-submitting.
func TestCivicReportCaptureAndDedupe(t *testing.T) {
	rig := newTestRig(t)
	fake := &fakeCivicSubmitter{}
	rig.svc.Civic = fake

	payload, _ := json.Marshal(CivicReportPayload{
		CategorySlug: "roads",
		Description:  "Deep pothole at the junction blocking one lane",
	})
	item := CaptureItem{ClientID: uuid.NewString(), Kind: KindCivicReport, Payload: payload}

	results := rig.svc.Capture(context.Background(), rig.tenant, []CaptureItem{item})
	if len(results) != 1 || results[0].Status != StatusApplied {
		t.Fatalf("first capture = %+v", results)
	}
	if results[0].CaseRef == "" || results[0].CaseID == nil {
		t.Fatalf("case outcome missing: %+v", results[0])
	}
	if fake.calls != 1 {
		t.Fatalf("civic calls = %d", fake.calls)
	}

	// Replay: deduped, original case_ref returned, no re-submission.
	results = rig.svc.Capture(context.Background(), rig.tenant, []CaptureItem{item})
	if len(results) != 1 || results[0].Status != StatusDeduped {
		t.Fatalf("replay = %+v", results)
	}
	if results[0].CaseRef != "GOV-IKEJA-WARD3-2026-000001" {
		t.Fatalf("replay case_ref = %q", results[0].CaseRef)
	}
	if fake.calls != 1 {
		t.Fatalf("replay re-submitted: calls = %d", fake.calls)
	}
}
