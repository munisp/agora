package socialpub

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
	"github.com/opendesk/booking-service/internal/socialpub/provider"
)

// Handler tests boot the same embedded-Postgres harness as the store tests
// and exercise the real HTTP request/response cycle through RegisterRoutes
// (the anti-collision contract entry point) with a test tenant accessor —
// httpapi wiring itself is the integrator's gate. Providers are the
// deterministic mocks via the explicit SOCIAL_MOCK=1 opt-in (W39 SIM-005:
// the mock is no longer the silent default, so tests opt in explicitly).

func testRouter(t *testing.T, tenant bookingops.TenantInfo) (http.Handler, *Store) {
	t.Helper()
	t.Setenv("SOCIAL_MOCK", "1") // explicit mock opt-in (dev/test posture)
	st := newTestStore(t)
	r := chi.NewRouter()
	RegisterRoutes(r, &Deps{
		Store: st,
		TenantFromContext: func(ctx context.Context) (bookingops.TenantInfo, bool) {
			return tenant, true
		},
		EventsTopic: "opendesk.social.events.v1",
		UsageTopic:  "opendesk.usage.events",
	})
	return r, st
}

func do(t *testing.T, r http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
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

func qstr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func createAccountAPI(t *testing.T, r http.Handler, providerID string, political bool) Account {
	t.Helper()
	rec := do(t, r, http.MethodPost, "/v1/social/accounts",
		`{"provider":`+qstr(providerID)+`,"account_ref":"acct-1","display_name":"Page One","political_ads_authorized":`+boolStr(political)+`}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create account = %d (%s)", rec.Code, rec.Body.String())
	}
	return decodeEnv[struct {
		Account Account `json:"account"`
	}](t, rec).Account
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func createCreativeAPI(t *testing.T, r http.Handler, disclaimer string) Creative {
	t.Helper()
	body := `{"name":"Creative","kind":"text","body":"Town hall Saturday 10am."}`
	if disclaimer != "" {
		body = `{"name":"Creative","kind":"text","body":"Town hall Saturday 10am.","disclaimer_text":` + qstr(disclaimer) + `}`
	}
	rec := do(t, r, http.MethodPost, "/v1/social/creatives", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create creative = %d (%s)", rec.Code, rec.Body.String())
	}
	return decodeEnv[struct {
		Creative Creative `json:"creative"`
	}](t, rec).Creative
}

func createAdAPI(t *testing.T, r http.Handler, accountID, creativeID uuid.UUID, name string, political bool, disclaimer string) Ad {
	t.Helper()
	body := `{"account_id":"` + accountID.String() + `","creative_id":"` + creativeID.String() + `",` +
		`"name":` + qstr(name) + `,"objective":"awareness","budget_kobo":500000,"daily_budget_kobo":100000,` +
		`"targeting":{"lgas":["Ikeja"],"age_min":18,"age_max":65,"interests":["politics"]},` +
		`"political":` + boolStr(political)
	if disclaimer != "" {
		body += `,"disclaimer_text":` + qstr(disclaimer)
	}
	body += `}`
	rec := do(t, r, http.MethodPost, "/v1/social/ads", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create ad = %d (%s)", rec.Code, rec.Body.String())
	}
	return decodeEnv[struct {
		Ad Ad `json:"ad"`
	}](t, rec).Ad
}

func outboxTypes(t *testing.T, st *Store, topic string) []string {
	t.Helper()
	rows, err := st.pool.Query(context.Background(),
		`SELECT payload->>'type' FROM outbox WHERE topic=$1 ORDER BY created_at`, topic)
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		out = append(out, s)
	}
	return out
}

// Post lifecycle through the API: account + creative → queued post →
// publish (mock) → published with a deterministic mock-post-* id +
// PostPublished event whose payload carries NO creative body.
func TestPostPublishLifecycle(t *testing.T) {
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}
	r, st := testRouter(t, tenant)

	acct := createAccountAPI(t, r, ProviderMeta, false)
	cr := createCreativeAPI(t, r, "")

	rec := do(t, r, http.MethodPost, "/v1/social/posts",
		`{"account_id":"`+acct.ID.String()+`","creative_id":"`+cr.ID.String()+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create post = %d (%s)", rec.Code, rec.Body.String())
	}
	post := decodeEnv[struct {
		Post Post `json:"post"`
	}](t, rec).Post
	if post.Status != PostQueued {
		t.Fatalf("new post status = %s, want queued", post.Status)
	}

	rec = do(t, r, http.MethodPost, "/v1/social/posts/"+post.ID.String()+"/publish", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("publish = %d (%s)", rec.Code, rec.Body.String())
	}
	post = decodeEnv[struct {
		Post Post `json:"post"`
	}](t, rec).Post
	if post.Status != PostPublished || post.ProviderPostID == nil ||
		!strings.HasPrefix(*post.ProviderPostID, "mock-post-meta-") || post.PublishedAt == nil {
		t.Fatalf("published post mismatch: %+v", post)
	}

	// Re-publish → 409.
	rec = do(t, r, http.MethodPost, "/v1/social/posts/"+post.ID.String()+"/publish", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("re-publish = %d, want 409", rec.Code)
	}

	// PostPublished event on the events topic; NO creative body in payload.
	types := outboxTypes(t, st, "opendesk.social.events.v1")
	if len(types) != 1 || types[0] != EventTypePostPublished {
		t.Fatalf("events = %v, want [PostPublished]", types)
	}
	var payload map[string]any
	if err := st.pool.QueryRow(context.Background(),
		`SELECT payload FROM outbox WHERE topic='opendesk.social.events.v1'`).Scan(&payload); err != nil {
		t.Fatalf("scan payload: %v", err)
	}
	data, _ := payload["data"].(map[string]any)
	if _, leaked := data["body"]; leaked {
		t.Fatalf("creative body leaked into event payload: %v", data)
	}

	// Publish on an expired account → 409 (hard gate).
	expired := createAccountAPI(t, r, ProviderX, false)
	rec = do(t, r, http.MethodPatch, "/v1/social/accounts/"+expired.ID.String(), `{"status":"expired"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expire account = %d (%s)", rec.Code, rec.Body.String())
	}
	rec = do(t, r, http.MethodPost, "/v1/social/posts",
		`{"account_id":"`+expired.ID.String()+`","creative_id":"`+cr.ID.String()+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create post (expired acct) = %d (%s)", rec.Code, rec.Body.String())
	}
	post2 := decodeEnv[struct {
		Post Post `json:"post"`
	}](t, rec).Post
	rec = do(t, r, http.MethodPost, "/v1/social/posts/"+post2.ID.String()+"/publish", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("publish on expired account = %d (%s), want 409", rec.Code, rec.Body.String())
	}
}

// Ad input gates: budget > 0, daily ≤ total, age 18..100 min ≤ max (400s).
func TestAdInputGates(t *testing.T) {
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}
	r, _ := testRouter(t, tenant)
	acct := createAccountAPI(t, r, ProviderMeta, false)
	cr := createCreativeAPI(t, r, "")
	base := `"account_id":"` + acct.ID.String() + `","creative_id":"` + cr.ID.String() + `","name":"A","objective":"traffic"`

	cases := []struct {
		name string
		body string
	}{
		{"zero budget", `{` + base + `,"budget_kobo":0,"daily_budget_kobo":100,"targeting":{"age_min":18,"age_max":65}}`},
		{"daily exceeds total", `{` + base + `,"budget_kobo":100,"daily_budget_kobo":101,"targeting":{"age_min":18,"age_max":65}}`},
		{"age below 18", `{` + base + `,"budget_kobo":1000,"daily_budget_kobo":100,"targeting":{"age_min":17,"age_max":65}}`},
		{"age above 100", `{` + base + `,"budget_kobo":1000,"daily_budget_kobo":100,"targeting":{"age_min":18,"age_max":101}}`},
		{"min above max", `{` + base + `,"budget_kobo":1000,"daily_budget_kobo":100,"targeting":{"age_min":60,"age_max":40}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, r, http.MethodPost, "/v1/social/ads", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s = %d (%s), want 400", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

// Political-ads launch gates (SPEC-W21 hard gates, each asserted):
// political without account authorization → 422; authorized but no
// effective disclaimer → 422; authorized + creative disclaimer → launch;
// launch on expired account → 409.
func TestLaunchGates(t *testing.T) {
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}
	r, st := testRouter(t, tenant)

	// 1) political=true on an UNAUTHORIZED account → 422.
	unauth := createAccountAPI(t, r, ProviderMeta, false)
	crDisc := createCreativeAPI(t, r, "Paid for by the Progress Committee")
	ad1 := createAdAPI(t, r, unauth.ID, crDisc.ID, "GOTV", true, "")
	rec := do(t, r, http.MethodPost, "/v1/social/ads/"+ad1.ID.String()+"/launch", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("political launch unauthorized = %d (%s), want 422", rec.Code, rec.Body.String())
	}

	// 2) AUTHORIZED account but NO effective disclaimer → 422.
	auth := createAccountAPI(t, r, ProviderMeta, true)
	crPlain := createCreativeAPI(t, r, "")
	ad2 := createAdAPI(t, r, auth.ID, crPlain.ID, "GOTV 2", true, "")
	rec = do(t, r, http.MethodPost, "/v1/social/ads/"+ad2.ID.String()+"/launch", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("political launch no disclaimer = %d (%s), want 422", rec.Code, rec.Body.String())
	}

	// 3) authorized + creative disclaimer → 200, status review, mock-ad-*
	// provider id, AdLaunched event. W39 SIM-006: a MOCK launch is NOT
	// metered as real usage (no social_ad_launched usage row).
	ad3 := createAdAPI(t, r, auth.ID, crDisc.ID, "GOTV 3", true, "")
	rec = do(t, r, http.MethodPost, "/v1/social/ads/"+ad3.ID.String()+"/launch", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("gated launch = %d (%s)", rec.Code, rec.Body.String())
	}
	launched := decodeEnv[struct {
		Ad Ad `json:"ad"`
	}](t, rec).Ad
	if launched.Status != AdReview || launched.ProviderAdID == nil ||
		!strings.HasPrefix(*launched.ProviderAdID, "mock-ad-meta-") {
		t.Fatalf("launched ad mismatch: %+v", launched)
	}
	types := outboxTypes(t, st, "opendesk.social.events.v1")
	if len(types) != 1 || types[0] != EventTypeAdLaunched {
		t.Fatalf("events = %v, want [AdLaunched]", types)
	}
	var usageRows int
	if err := st.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox WHERE topic='opendesk.usage.events'`).Scan(&usageRows); err != nil {
		t.Fatalf("count usage rows: %v", err)
	}
	if usageRows != 0 {
		t.Fatalf("mock launch must NOT be metered as real usage, found %d rows", usageRows)
	}

	// 4) launch on an EXPIRED account → 409.
	expired := createAccountAPI(t, r, ProviderTikTok, false)
	rec = do(t, r, http.MethodPatch, "/v1/social/accounts/"+expired.ID.String(), `{"status":"expired"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expire = %d (%s)", rec.Code, rec.Body.String())
	}
	ad4 := createAdAPI(t, r, expired.ID, crPlain.ID, "Plain", false, "")
	rec = do(t, r, http.MethodPost, "/v1/social/ads/"+ad4.ID.String()+"/launch", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("launch on expired account = %d (%s), want 409", rec.Code, rec.Body.String())
	}

	// 5) launch a non-draft ad → 409.
	rec = do(t, r, http.MethodPost, "/v1/social/ads/"+ad3.ID.String()+"/launch", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("re-launch = %d, want 409", rec.Code)
	}
}

// Provider rejection (mock sentinel "mock-reject" in the ad name) lands
// the ad in rejected with an AdRejected event.
func TestLaunchRejected(t *testing.T) {
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}
	r, st := testRouter(t, tenant)
	acct := createAccountAPI(t, r, ProviderX, false)
	cr := createCreativeAPI(t, r, "")
	ad := createAdAPI(t, r, acct.ID, cr.ID, "promo mock-reject", false, "")

	rec := do(t, r, http.MethodPost, "/v1/social/ads/"+ad.ID.String()+"/launch", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("rejecting launch = %d (%s)", rec.Code, rec.Body.String())
	}
	resp := decodeEnv[struct {
		Ad       Ad     `json:"ad"`
		Rejected bool   `json:"rejected"`
		Reason   string `json:"reason"`
	}](t, rec)
	if !resp.Rejected || resp.Ad.Status != AdRejected || resp.Reason == "" {
		t.Fatalf("rejection mismatch: %+v", resp)
	}
	types := outboxTypes(t, st, "opendesk.social.events.v1")
	if len(types) != 1 || types[0] != EventTypeAdRejected {
		t.Fatalf("events = %v, want [AdRejected]", types)
	}
}

// Operator status machine + stats: launch → review → active → paused;
// illegal edge → 409; stats 409 pre-launch, deterministic post-launch.
func TestAdStatusMachineAndStats(t *testing.T) {
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}
	r, _ := testRouter(t, tenant)
	acct := createAccountAPI(t, r, ProviderMeta, false)
	cr := createCreativeAPI(t, r, "")
	ad := createAdAPI(t, r, acct.ID, cr.ID, "Status machine", false, "")

	// Stats before launch → 409.
	rec := do(t, r, http.MethodGet, "/v1/social/ads/"+ad.ID.String()+"/stats", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("pre-launch stats = %d, want 409", rec.Code)
	}

	// Illegal operator edge: draft → active → 409.
	rec = do(t, r, http.MethodPatch, "/v1/social/ads/"+ad.ID.String(), `{"status":"active"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("draft→active = %d (%s), want 409", rec.Code, rec.Body.String())
	}

	// Launch (non-political) → review.
	rec = do(t, r, http.MethodPost, "/v1/social/ads/"+ad.ID.String()+"/launch", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("launch = %d (%s)", rec.Code, rec.Body.String())
	}

	// review → active → paused.
	for _, to := range []string{"active", "paused"} {
		rec = do(t, r, http.MethodPatch, "/v1/social/ads/"+ad.ID.String(), `{"status":"`+to+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("→%s = %d (%s)", to, rec.Code, rec.Body.String())
		}
	}

	// Stats post-launch: deterministic + mock-disclosed.
	rec = do(t, r, http.MethodGet, "/v1/social/ads/"+ad.ID.String()+"/stats", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("stats = %d (%s)", rec.Code, rec.Body.String())
	}
	stats1 := decodeEnv[struct {
		Mock  bool `json:"mock"`
		Stats struct {
			Impressions int64 `json:"impressions"`
			Reach       int64 `json:"reach"`
			Clicks      int64 `json:"clicks"`
			SpendKobo   int64 `json:"spend_kobo"`
		} `json:"stats"`
	}](t, rec)
	if !stats1.Mock {
		t.Fatalf("stats should disclose mock=true")
	}
	if stats1.Stats.Impressions <= 0 || stats1.Stats.Reach > stats1.Stats.Impressions ||
		stats1.Stats.Clicks > stats1.Stats.Reach {
		t.Fatalf("implausible stats: %+v", stats1.Stats)
	}
	rec = do(t, r, http.MethodGet, "/v1/social/ads/"+ad.ID.String()+"/stats", "")
	stats2 := decodeEnv[struct {
		Stats struct {
			Impressions int64 `json:"impressions"`
			SpendKobo   int64 `json:"spend_kobo"`
		} `json:"stats"`
	}](t, rec)
	if stats2.Stats.Impressions != stats1.Stats.Impressions || stats2.Stats.SpendKobo != stats1.Stats.SpendKobo {
		t.Fatalf("stats not deterministic: %+v vs %+v", stats1.Stats, stats2.Stats)
	}

	// Field edit on an active ad → 404 (store guards draft|review|rejected).
	rec = do(t, r, http.MethodPatch, "/v1/social/ads/"+ad.ID.String(), `{"name":"rename while paused"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("edit paused ad = %d (%s), want 404", rec.Code, rec.Body.String())
	}
}

// Account/creative PATCH + list filters through the API.
func TestAccountCreativePatchAndFilters(t *testing.T) {
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}
	r, _ := testRouter(t, tenant)

	m := createAccountAPI(t, r, ProviderMeta, false)
	x := createAccountAPI(t, r, ProviderX, false)
	rec := do(t, r, http.MethodPatch, "/v1/social/accounts/"+x.ID.String(), `{"status":"revoked","display_name":"Old X"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch account = %d (%s)", rec.Code, rec.Body.String())
	}
	patched := decodeEnv[struct {
		Account Account `json:"account"`
	}](t, rec).Account
	if patched.Status != AccountRevoked || patched.DisplayName != "Old X" {
		t.Fatalf("patched account mismatch: %+v", patched)
	}

	rec = do(t, r, http.MethodGet, "/v1/social/accounts?status=revoked", "")
	revoked := decodeEnv[struct {
		Accounts []Account `json:"accounts"`
	}](t, rec).Accounts
	if len(revoked) != 1 || revoked[0].ID != x.ID {
		t.Fatalf("revoked filter = %+v", revoked)
	}
	rec = do(t, r, http.MethodGet, "/v1/social/accounts?provider=meta", "")
	metas := decodeEnv[struct {
		Accounts []Account `json:"accounts"`
	}](t, rec).Accounts
	if len(metas) != 1 || metas[0].ID != m.ID {
		t.Fatalf("meta filter = %+v", metas)
	}
	// Invalid filters → 400.
	if rec := do(t, r, http.MethodGet, "/v1/social/accounts?provider=myspace", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad provider filter = %d, want 400", rec.Code)
	}

	cr := createCreativeAPI(t, r, "")
	rec = do(t, r, http.MethodPatch, "/v1/social/creatives/"+cr.ID.String(),
		`{"body":"Updated body copy.","disclaimer_text":"Paid for by Z"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch creative = %d (%s)", rec.Code, rec.Body.String())
	}
	pc := decodeEnv[struct {
		Creative Creative `json:"creative"`
	}](t, rec).Creative
	if pc.Body != "Updated body copy." || pc.DisclaimerText == nil || *pc.DisclaimerText != "Paid for by Z" {
		t.Fatalf("patched creative mismatch: %+v", pc)
	}
	rec = do(t, r, http.MethodGet, "/v1/social/creatives?kind=text", "")
	list := decodeEnv[struct {
		Creatives []Creative `json:"creatives"`
	}](t, rec).Creatives
	if len(list) != 1 {
		t.Fatalf("creative kind filter = %+v", list)
	}
}

// Tenant context is required (integrator wires the middleware; the
// accessor contract is asserted here).
func TestTenantContextRequired(t *testing.T) {
	st := newTestStore(t)
	r := chi.NewRouter()
	RegisterRoutes(r, &Deps{
		Store: st,
		TenantFromContext: func(ctx context.Context) (bookingops.TenantInfo, bool) {
			return bookingops.TenantInfo{}, false
		},
	})
	rec := do(t, r, http.MethodGet, "/v1/social/accounts", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no tenant = %d, want 400", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// W39 SIM-005 / SIM-006 regressions
// ---------------------------------------------------------------------------

// fakeRealPublisher is a NON-mock Publisher test double (a "real rail"
// that succeeds): metering must count its launches as real usage.
type fakeRealPublisher struct{}

func (fakeRealPublisher) Name() string { return "meta" }
func (fakeRealPublisher) PublishPost(_ context.Context, _ provider.PostRequest) (string, error) {
	return "real-post-1", nil
}
func (fakeRealPublisher) LaunchAd(_ context.Context, _ provider.AdRequest) (string, bool, string, error) {
	return "real-ad-1", false, "", nil
}
func (fakeRealPublisher) AdStats(_ context.Context, _ string) (provider.Stats, error) {
	return provider.Stats{Impressions: 1}, nil
}

// W39 SIM-005: with NO mock opt-in (SOCIAL_MOCK off) and no wired
// publisher, publish fails closed — 502 "not configured", the post is NOT
// marked published, and NO PostPublished event is emitted.
func TestPublishFailsClosedWithoutMockOptIn(t *testing.T) {
	t.Setenv("SOCIAL_MOCK", "")
	t.Setenv("META_MOCK", "")
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}
	st := newTestStore(t)
	r := chi.NewRouter()
	RegisterRoutes(r, &Deps{
		Store: st,
		TenantFromContext: func(ctx context.Context) (bookingops.TenantInfo, bool) {
			return tenant, true
		},
		EventsTopic: "opendesk.social.events.v1",
		UsageTopic:  "opendesk.usage.events",
	})

	acct := createAccountAPI(t, r, ProviderMeta, false)
	cr := createCreativeAPI(t, r, "")
	rec := do(t, r, http.MethodPost, "/v1/social/posts",
		`{"account_id":"`+acct.ID.String()+`","creative_id":"`+cr.ID.String()+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create post = %d (%s)", rec.Code, rec.Body.String())
	}
	post := decodeEnv[struct {
		Post Post `json:"post"`
	}](t, rec).Post

	rec = do(t, r, http.MethodPost, "/v1/social/posts/"+post.ID.String()+"/publish", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("fail-closed publish = %d (%s), want 502", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not configured") {
		t.Fatalf("error must name the missing configuration: %s", rec.Body.String())
	}

	// No fake success: the post is not published, no event emitted.
	rec = do(t, r, http.MethodGet, "/v1/social/posts/"+post.ID.String(), "")
	got := decodeEnv[struct {
		Post Post `json:"post"`
	}](t, rec).Post
	if got.Status == PostPublished || got.ProviderPostID != nil {
		t.Fatalf("post must not be marked published on the fail-closed rail: %+v", got)
	}
	if types := outboxTypes(t, st, "opendesk.social.events.v1"); len(types) != 0 {
		t.Fatalf("no events may be emitted on the fail-closed rail, got %v", types)
	}
}

// W39 SIM-006: a launch through a REAL (non-mock) provider rail IS
// metered exactly once as social_ad_launched.
func TestLaunchMetersRealProviderRail(t *testing.T) {
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}
	st := newTestStore(t)
	r := chi.NewRouter()
	RegisterRoutes(r, &Deps{
		Store: st,
		TenantFromContext: func(ctx context.Context) (bookingops.TenantInfo, bool) {
			return tenant, true
		},
		EventsTopic: "opendesk.social.events.v1",
		UsageTopic:  "opendesk.usage.events",
		Publishers:  map[string]provider.Publisher{"meta": fakeRealPublisher{}},
	})

	acct := createAccountAPI(t, r, ProviderMeta, false)
	cr := createCreativeAPI(t, r, "")
	ad := createAdAPI(t, r, acct.ID, cr.ID, "Awareness", false, "")
	rec := do(t, r, http.MethodPost, "/v1/social/ads/"+ad.ID.String()+"/launch", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("real-rail launch = %d (%s)", rec.Code, rec.Body.String())
	}
	launched := decodeEnv[struct {
		Ad Ad `json:"ad"`
	}](t, rec).Ad
	if launched.ProviderAdID == nil || *launched.ProviderAdID != "real-ad-1" {
		t.Fatalf("real-rail provider ad id mismatch: %+v", launched)
	}
	var metric string
	if err := st.pool.QueryRow(context.Background(),
		`SELECT payload->'data'->>'metric' FROM outbox WHERE topic='opendesk.usage.events'`).Scan(&metric); err != nil {
		t.Fatalf("usage metering row missing for a real-rail launch: %v", err)
	}
	if metric != UsageMetricAdLaunched {
		t.Fatalf("usage metric = %s, want %s", metric, UsageMetricAdLaunched)
	}
}
