package httpapi

// SPEC-W80 B4: GET /v1/bookings/{id}/crm-notes — the tenant-scoped read
// side of the reverse-sync crm_notes array, implemented in crm_notes.go
// WITHOUT touching the invariant bookings.go files (W57). Route wiring +
// handler behavior incl. the cross-tenant 404 posture.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// w80CRMNotesFixture boots the embedded store and a router with a stub
// identity service behind the tenant resolver (kyc_routes_test.go idiom).
func w80CRMNotesFixture(t *testing.T) (http.Handler, *store.Store, uuid.UUID) {
	t.Helper()
	st := newPortalStore(t)
	tenantID := uuid.New()
	identity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tenants/acme" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": tenantID.String(), "timezone": "UTC",
		})
	}))
	t.Cleanup(identity.Close)
	h := NewRouter(Deps{
		Store:         st,
		AuthzDisabled: true,
		Logger:        zap.NewNop(),
		Resolver: bookingops.NewTenantResolver(nil, "identity", time.Minute, zap.NewNop(),
			bookingops.WithIdentityBaseURL(identity.URL)),
	})
	return h, st, tenantID
}

func TestListBookingCRMNotes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping embedded-postgres crm-notes test in -short mode")
	}
	h, st, tenantID := w80CRMNotesFixture(t)
	ctx := context.Background()

	start := time.Now().UTC().Add(48 * time.Hour)
	b := store.Booking{
		TenantID: tenantID, OfferingID: uuid.New(), TeamMemberID: uuid.New(), ContactID: uuid.New(),
		StartsAt: start, EndsAt: start.Add(30 * time.Minute),
		Status: store.StatusConfirmed, Source: "api",
	}
	if err := st.CreateBookingTx(ctx, &b, store.SlotGuard{}, "test.events", []byte(`{}`)); err != nil {
		t.Fatalf("seed booking: %v", err)
	}
	// Two notes appended by the internal reverse-sync endpoint's store seam.
	note1 := store.CRMNote{At: time.Now().UTC(), Source: "twenty", Text: "Twenty task marked DONE"}
	if err := st.AppendBookingCRMNote(ctx, tenantID, b.ID, note1); err != nil {
		t.Fatalf("append note 1: %v", err)
	}
	note2 := store.CRMNote{At: time.Now().UTC(), Source: "crm-sync", Text: "Call summary synced"}
	if err := st.AppendBookingCRMNote(ctx, tenantID, b.ID, note2); err != nil {
		t.Fatalf("append note 2: %v", err)
	}

	// A second tenant's booking — cross-tenant reads must 404.
	otherTenant := uuid.New()
	ob := store.Booking{
		TenantID: otherTenant, OfferingID: uuid.New(), TeamMemberID: uuid.New(), ContactID: uuid.New(),
		StartsAt: start, EndsAt: start.Add(30 * time.Minute),
		Status: store.StatusConfirmed, Source: "api",
	}
	if err := st.CreateBookingTx(ctx, &ob, store.SlotGuard{}, "test.events", []byte(`{}`)); err != nil {
		t.Fatalf("seed other booking: %v", err)
	}
	if err := st.AppendBookingCRMNote(ctx, otherTenant, ob.ID, note1); err != nil {
		t.Fatalf("append other note: %v", err)
	}

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Tenant-Slug", "acme")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// Happy path: both notes back, tenant-scoped.
	rec := get("/v1/bookings/" + b.ID.String() + "/crm-notes")
	if rec.Code != http.StatusOK {
		t.Fatalf("crm-notes = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	var out struct {
		Notes []store.CRMNote `json:"notes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Notes) != 2 || out.Notes[0].Text != note1.Text || out.Notes[1].Text != note2.Text {
		t.Fatalf("notes = %+v, want [%q %q]", out.Notes, note1.Text, note2.Text)
	}

	// Cross-tenant booking id → 404 (indistinguishable from missing).
	rec = get("/v1/bookings/" + ob.ID.String() + "/crm-notes")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant crm-notes = %d, want 404", rec.Code)
	}

	// Unknown booking → 404.
	rec = get("/v1/bookings/" + uuid.NewString() + "/crm-notes")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown booking = %d, want 404", rec.Code)
	}

	// Malformed id → 400.
	rec = get("/v1/bookings/not-a-uuid/crm-notes")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed id = %d, want 400", rec.Code)
	}

	// No tenant header → 400 (the /v1 tenant middleware posture).
	req := httptest.NewRequest(http.MethodGet, "/v1/bookings/"+b.ID.String()+"/crm-notes", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no tenant header = %d, want 400", rec.Code)
	}
}
