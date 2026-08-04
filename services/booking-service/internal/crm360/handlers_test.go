package crm360

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
)

// Handler tests boot the same embedded-Postgres harness as the store tests
// and exercise the real HTTP request/response cycle through RegisterRoutes
// (the anti-collision contract entry point) with a fake tenant resolver —
// httpapi wiring itself is the integrator's gate.

type fakeResolver struct {
	tenant bookingops.TenantInfo
}

func (f fakeResolver) BySlug(_ context.Context, slug string) (bookingops.TenantInfo, error) {
	if slug != f.tenant.Slug {
		return bookingops.TenantInfo{}, ErrNotFound
	}
	return f.tenant, nil
}

// testRouter mounts RegisterRoutes exactly as the integrator will (tenant
// middleware via Deps.Resolver, no authz mw — the perms chain is the
// integrator's), with the events topic enabled so outbox emission is
// observable.
func testRouter(t *testing.T, tenant bookingops.TenantInfo) (http.Handler, *Store) {
	t.Helper()
	st := newTestStore(t)
	r := chi.NewRouter()
	RegisterRoutes(r, &Deps{
		Store:           st,
		Resolver:        fakeResolver{tenant: tenant},
		CRMEventsTopic:  DefaultEventsTopic,
		UserFromContext: func(ctx context.Context) string { return "agent-1" },
	})
	return r, st
}

func do(t *testing.T, r http.Handler, tenantSlug, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if tenantSlug != "" {
		req.Header.Set("X-Tenant-Slug", tenantSlug)
	}
	r.ServeHTTP(rec, req)
	return rec
}

func decodeEnv[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return v
}

// outboxTopics returns the (topic, event type) pairs queued in the outbox.
func outboxEvents(t *testing.T, st *Store) []string {
	t.Helper()
	rows, err := st.pool.Query(context.Background(),
		`SELECT topic, payload->>'type' FROM outbox ORDER BY created_at, id`)
	if err != nil {
		t.Fatalf("outbox query: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var topic, typ string
		if err := rows.Scan(&topic, &typ); err != nil {
			t.Fatalf("outbox scan: %v", err)
		}
		out = append(out, topic+" "+typ)
	}
	return out
}

// Full journey over HTTP: tag add/replay/remove, note create/patch, then
// the 360 + timeline + search reads, with CloudEvents observed in the
// outbox.
func TestCRMJourneyEndpoints(t *testing.T) {
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}
	r, st := testRouter(t, tenant)
	contactID := addContact(t, st, tenant.ID, "Ada Lovelace", "+2348011111111", "ada@example.com")
	base := "/v1/crm/contacts/" + contactID.String()

	// --- tags ---
	rec := do(t, r, tenant.Slug, http.MethodPost, base+"/tags", `{"tag":" VIP "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("add tag = %d (%s)", rec.Code, rec.Body.String())
	}
	tags := decodeEnv[struct {
		Tags []string `json:"tags"`
	}](t, rec)
	if len(tags.Tags) != 1 || tags.Tags[0] != "vip" {
		t.Fatalf("tags after add: %+v", tags)
	}

	// Invalid tag shape → 400.
	rec = do(t, r, tenant.Slug, http.MethodPost, base+"/tags", `{"tag":"bad tag!"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid tag = %d (%s)", rec.Code, rec.Body.String())
	}

	// Idempotent replay → 200, and NO second TagAdded event.
	rec = do(t, r, tenant.Slug, http.MethodPost, base+"/tags", `{"tag":"vip"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("replay tag = %d (%s)", rec.Code, rec.Body.String())
	}
	if ev := outboxEvents(t, st); len(ev) != 1 || ev[0] != DefaultEventsTopic+" "+EventTypeTagAdded {
		t.Fatalf("outbox after tag replay: %+v", ev)
	}

	rec = do(t, r, tenant.Slug, http.MethodDelete, base+"/tags/vip", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("remove tag = %d (%s)", rec.Code, rec.Body.String())
	}
	rec = do(t, r, tenant.Slug, http.MethodDelete, base+"/tags/vip", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("remove missing tag = %d, want 404", rec.Code)
	}

	// --- notes ---
	rec = do(t, r, tenant.Slug, http.MethodPost, base+"/notes", `{"body":"Customer prefers morning calls","pinned":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create note = %d (%s)", rec.Code, rec.Body.String())
	}
	created := decodeEnv[struct {
		Note Note `json:"note"`
	}](t, rec)
	if created.Note.Author != "agent-1" || !created.Note.Pinned {
		t.Fatalf("note author/pinned: %+v", created.Note)
	}

	// Empty body → 400.
	rec = do(t, r, tenant.Slug, http.MethodPost, base+"/notes", `{"body":"  "}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty note = %d, want 400", rec.Code)
	}

	// PATCH: edit body + unpin.
	rec = do(t, r, tenant.Slug, http.MethodPatch, "/v1/crm/notes/"+created.Note.ID.String(),
		`{"body":"Customer prefers afternoon calls","pinned":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch note = %d (%s)", rec.Code, rec.Body.String())
	}
	patched := decodeEnv[struct {
		Note Note `json:"note"`
	}](t, rec)
	if patched.Note.Body != "Customer prefers afternoon calls" || patched.Note.Pinned {
		t.Fatalf("patched note: %+v", patched.Note)
	}

	// PATCH with no fields → 400; unknown note → 404.
	rec = do(t, r, tenant.Slug, http.MethodPatch, "/v1/crm/notes/"+created.Note.ID.String(), `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty patch = %d, want 400", rec.Code)
	}
	rec = do(t, r, tenant.Slug, http.MethodPatch, "/v1/crm/notes/"+uuid.NewString(), `{"pinned":true}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("patch unknown note = %d, want 404", rec.Code)
	}

	rec = do(t, r, tenant.Slug, http.MethodGet, base+"/notes", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list notes = %d", rec.Code)
	}
	notes := decodeEnv[struct {
		Notes []Note `json:"notes"`
	}](t, rec)
	if len(notes.Notes) != 1 || notes.Notes[0].Body != "Customer prefers afternoon calls" {
		t.Fatalf("notes: %+v", notes)
	}

	// --- events: exactly TagAdded, TagRemoved, NoteCreated, NoteUpdated
	// (the outbox id is a random uuid, so assert as a set, not a sequence).
	ev := outboxEvents(t, st)
	want := map[string]int{
		DefaultEventsTopic + " " + EventTypeTagAdded:    1,
		DefaultEventsTopic + " " + EventTypeTagRemoved:  1,
		DefaultEventsTopic + " " + EventTypeNoteCreated: 1,
		DefaultEventsTopic + " " + EventTypeNoteUpdated: 1,
	}
	got := map[string]int{}
	for _, e := range ev {
		got[e]++
	}
	if len(ev) != len(want) {
		t.Fatalf("outbox events = %+v, want keys %+v", ev, want)
	}
	for k, n := range want {
		if got[k] != n {
			t.Fatalf("outbox event %q count = %d, want %d (all: %+v)", k, got[k], n, ev)
		}
	}

	// --- 360 + timeline + search reads ---
	rec = do(t, r, tenant.Slug, http.MethodGet, base+"/360", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("360 = %d (%s)", rec.Code, rec.Body.String())
	}
	profile := decodeEnv[struct {
		Profile Profile360 `json:"profile"`
	}](t, rec)
	if profile.Profile.Contact.Name != "Ada Lovelace" || len(profile.Profile.Tags) != 0 {
		t.Fatalf("profile: %+v", profile.Profile)
	}
	if profile.Profile.Consent != nil || profile.Profile.Wallet != nil {
		t.Fatalf("wallet/consent must be null: %+v", profile.Profile)
	}

	rec = do(t, r, tenant.Slug, http.MethodGet, base+"/timeline?limit=10", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("timeline = %d", rec.Code)
	}
	tl := decodeEnv[struct {
		Timeline []TimelineItem `json:"timeline"`
	}](t, rec)
	if len(tl.Timeline) != 1 || tl.Timeline[0].Kind != KindNote {
		t.Fatalf("timeline: %+v", tl.Timeline)
	}

	rec = do(t, r, tenant.Slug, http.MethodGet, "/v1/crm/contacts/search?q=ada", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("search = %d", rec.Code)
	}
	found := decodeEnv[struct {
		Contacts []ContactSearchResult `json:"contacts"`
	}](t, rec)
	if len(found.Contacts) != 1 || found.Contacts[0].ID != contactID {
		t.Fatalf("search: %+v", found.Contacts)
	}

	// Unknown contact → 404 on 360.
	rec = do(t, r, tenant.Slug, http.MethodGet, "/v1/crm/contacts/"+uuid.NewString()+"/360", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown contact 360 = %d, want 404", rec.Code)
	}
}

// Tenant middleware contract: missing slug → 400, unknown slug → 404.
func TestTenantMiddleware(t *testing.T) {
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}
	r, _ := testRouter(t, tenant)

	if rec := do(t, r, "", http.MethodGet, "/v1/crm/contacts/search", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing slug = %d, want 400", rec.Code)
	}
	if rec := do(t, r, "nope", http.MethodGet, "/v1/crm/contacts/search", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown slug = %d, want 404", rec.Code)
	}
}

// The consent hook: a wired resolver surfaces the status; a failing one
// degrades to null without failing the profile.
type stubConsent struct {
	status string
	err    error
}

func (s stubConsent) ConsentStatus(_ context.Context, _, _ uuid.UUID) (string, error) {
	return s.status, s.err
}

func TestConsentResolverHook(t *testing.T) {
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}
	st := newTestStore(t)
	contactID := addContact(t, st, tenant.ID, "Ada", "", "")

	r := chi.NewRouter()
	RegisterRoutes(r, &Deps{
		Store:           st,
		Resolver:        fakeResolver{tenant: tenant},
		ConsentResolver: stubConsent{status: "granted"},
	})
	rec := do(t, r, tenant.Slug, http.MethodGet, "/v1/crm/contacts/"+contactID.String()+"/360", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("360 with resolver = %d (%s)", rec.Code, rec.Body.String())
	}
	p := decodeEnv[struct {
		Profile Profile360 `json:"profile"`
	}](t, rec)
	if p.Profile.Consent == nil || *p.Profile.Consent != "granted" {
		t.Fatalf("consent = %+v, want granted", p.Profile.Consent)
	}

	r2 := chi.NewRouter()
	RegisterRoutes(r2, &Deps{
		Store:           st,
		Resolver:        fakeResolver{tenant: tenant},
		ConsentResolver: stubConsent{err: ErrNotFound},
	})
	rec = do(t, r2, tenant.Slug, http.MethodGet, "/v1/crm/contacts/"+contactID.String()+"/360", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("360 with failing resolver = %d, want 200", rec.Code)
	}
	p = decodeEnv[struct {
		Profile Profile360 `json:"profile"`
	}](t, rec)
	if p.Profile.Consent != nil {
		t.Fatalf("failing resolver must degrade to null: %+v", p.Profile.Consent)
	}
}
