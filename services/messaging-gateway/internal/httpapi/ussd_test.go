package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opendesk/messaging-gateway/internal/channel"
	"github.com/opendesk/messaging-gateway/internal/metrics"
	"go.uber.org/zap"
)

// --- fakes ---

type fakeMenus struct {
	menu []channel.USSDMenuItem
	err  error
}

func (f *fakeMenus) USSDMenu(_ context.Context, tenantSlug string) ([]channel.USSDMenuItem, error) {
	return f.menu, f.err
}

type fakeConversation struct {
	mu    sync.Mutex
	turns []channel.USSDTurnRequest
	resp  channel.USSDTurnResponse
	err   error
}

func (f *fakeConversation) Turn(_ context.Context, req channel.USSDTurnRequest) (channel.USSDTurnResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.turns = append(f.turns, req)
	if f.err != nil {
		return channel.USSDTurnResponse{}, f.err
	}
	return f.resp, nil
}

func (f *fakeConversation) captured() []channel.USSDTurnRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]channel.USSDTurnRequest, len(f.turns))
	copy(out, f.turns)
	return out
}

// testMenu mirrors a tenant pack ussd.menu (numeric keys, as Agent D's
// menu renderer displays "{key}. {label}").
var testMenu = []channel.USSDMenuItem{
	{Key: "1", Label: "Book appointment", Action: "book"},
	{Key: "2", Label: "Talk to an agent", Action: "handoff"},
}

func newUSSDServer(store channel.USSDSessionStore, menus channel.USSDMenuFetcher, conv channel.USSDConversation) *Server {
	return &Server{
		USSD: &USSDConfig{
			Sites: map[string]channel.Site{
				"ussd:*384*123#": {SiteSlug: "acme-ng", TenantID: "9f1c2a4e-0000-4000-8000-000000000001"},
			},
			Store:        store,
			Menus:        menus,
			Conversation: conv,
			SessionTTL:   channel.USSDSessionTTL,
		},
		Metrics: metrics.New(),
		Log:     zap.NewNop(),
	}
}

// ussdPost issues one aggregator callback (form fields per SPEC-W12 §1).
func ussdPost(t *testing.T, h http.Handler, sessionID, serviceCode, phone, text string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	if sessionID != "" {
		form.Set("sessionId", sessionID)
	}
	if serviceCode != "" {
		form.Set("serviceCode", serviceCode)
	}
	if phone != "" {
		form.Set("phoneNumber", phone)
	}
	if text != "" {
		form.Set("text", text)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/ussd", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func mustPlain(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("expected text/plain, got %q", ct)
	}
	return rec.Body.String()
}

// --- happy path: new session → CON menu → selection → END (SPEC §1) ---
// conversation-service (Agent D) drives the menu; the gateway renders
// CON/END from the continue flag.

func TestUSSDMenuHappyPath(t *testing.T) {
	store := channel.NewMemoryUSSDStore()
	conv := &fakeConversation{resp: channel.USSDTurnResponse{
		ConversationID: "conv-1",
		Reply:          "1. Book appointment\n2. Talk to an agent\n0. Back\n00. Main menu",
		Continue:       true, Mode: "menu",
	}}
	s := newUSSDServer(store, &fakeMenus{menu: testMenu}, conv)

	// 1. First request (empty text): forwarded with the resolved menu;
	// continue=true → CON + the menu text.
	rec := ussdPost(t, s.Router(), "sess-1", "*384*123#", "+2348012345678", "")
	body := mustPlain(t, rec)
	if !strings.HasPrefix(body, "CON ") || !strings.Contains(body, "1. Book appointment") {
		t.Fatalf("first response mismatch: %q", body)
	}

	// 2. Selection "1": continue=false → END, session closed.
	conv.resp = channel.USSDTurnResponse{
		Reply: "Book appointment. Thank you — your request has been received.", Continue: false,
		Mode: "menu", Selection: "1", Action: "book",
	}
	rec = ussdPost(t, s.Router(), "sess-1", "*384*123#", "+2348012345678", "1")
	body = mustPlain(t, rec)
	if !strings.HasPrefix(body, "END Book appointment.") {
		t.Fatalf("selection response mismatch: %q", body)
	}

	turns := conv.captured()
	if len(turns) != 2 {
		t.Fatalf("expected 2 conversation turns, got %d", len(turns))
	}
	for i, turn := range turns {
		if turn.TenantID != "9f1c2a4e-0000-4000-8000-000000000001" || turn.SiteSlug != "acme-ng" ||
			turn.SessionID != "sess-1" || turn.ServiceCode != "*384*123#" ||
			turn.PhoneNumber != "+2348012345678" {
			t.Fatalf("turn %d envelope mismatch: %+v", i, turn)
		}
		if len(turn.Menu) != 2 || turn.Menu[0].Key != "1" || turn.Menu[1].Action != "handoff" {
			t.Fatalf("turn %d must carry the resolved menu: %+v", i, turn.Menu)
		}
	}
	if turns[0].Text != "" || turns[1].Text != "1" {
		t.Fatalf("cumulative text mismatch: %+v", turns)
	}

	// 3. Session was deleted on END: the next callback starts a new session
	// (fresh menu fetch + turn).
	conv.resp = channel.USSDTurnResponse{Reply: "1. Book appointment\n2. Talk to an agent", Continue: true, Mode: "menu"}
	rec = ussdPost(t, s.Router(), "sess-1", "*384*123#", "+2348012345678", "")
	if body := mustPlain(t, rec); !strings.HasPrefix(body, "CON ") {
		t.Fatalf("post-END session must restart, got %q", body)
	}
	if n := len(conv.captured()); n != 3 {
		t.Fatalf("restarted session must forward a fresh turn, got %d turns", n)
	}
}

// --- pass-through text mode (no pack menu) ---

func TestUSSDPassThroughMode(t *testing.T) {
	store := channel.NewMemoryUSSDStore()
	conv := &fakeConversation{resp: channel.USSDTurnResponse{Reply: "What is your name?", Continue: true, Mode: "text"}}
	s := newUSSDServer(store, &fakeMenus{menu: nil}, conv)

	rec := ussdPost(t, s.Router(), "sess-4", "*384*123#", "+2348012345678", "")
	if body := mustPlain(t, rec); body != "CON What is your name?" {
		t.Fatalf("pass-through first reply mismatch: %q", body)
	}
	conv.resp = channel.USSDTurnResponse{Reply: "Thanks, Ada.", Continue: false, Mode: "text"}
	rec = ussdPost(t, s.Router(), "sess-4", "*384*123#", "+2348012345678", "Ada")
	if body := mustPlain(t, rec); body != "END Thanks, Ada." {
		t.Fatalf("pass-through second reply mismatch: %q", body)
	}
	turns := conv.captured()
	if len(turns) != 2 || turns[0].Text != "" || turns[1].Text != "Ada" {
		t.Fatalf("pass-through turns mismatch: %+v", turns)
	}
	if turns[0].Menu != nil || turns[1].Menu != nil {
		t.Fatalf("pass-through must not attach a menu: %+v", turns)
	}
}

// --- wire shape against Agent D's delivered contract (POST /v1/ussd/turns) ---

func TestUSSDTurnWireShape(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &gotBody) //nolint:errcheck
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"conversation_id": "conv-x", "reply": "hi", "continue": true,
			"mode": "text", "selection": "", "action": "",
		})
	}))
	defer srv.Close()

	client := channel.NewUSSDConversation(srv.URL)
	resp, err := client.Turn(context.Background(), channel.USSDTurnRequest{
		TenantID: "9f1c2a4e-0000-4000-8000-000000000001", SiteSlug: "acme-ng",
		SessionID: "sess-w", ServiceCode: "*384*123#", PhoneNumber: "+2348012345678",
		Text: "1*2", Menu: testMenu,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/ussd/turns" {
		t.Fatalf("contract endpoint mismatch: %q", gotPath)
	}
	// Exactly the fields Agent D's UssdTurnRequest accepts.
	want := map[string]any{
		"tenant_id": "9f1c2a4e-0000-4000-8000-000000000001", "site_slug": "acme-ng",
		"session_id": "sess-w", "service_code": "*384*123#",
		"phone_number": "+2348012345678", "text": "1*2",
	}
	for k, v := range want {
		if gotBody[k] != v {
			t.Fatalf("body[%s] = %v, want %v (full: %v)", k, gotBody[k], v, gotBody)
		}
	}
	menu, ok := gotBody["menu"].([]any)
	if !ok || len(menu) != 2 {
		t.Fatalf("menu not sent: %v", gotBody)
	}
	first, _ := menu[0].(map[string]any)
	if first["key"] != "1" || first["label"] != "Book appointment" || first["action"] != "book" {
		t.Fatalf("menu item shape mismatch: %v", first)
	}
	for _, banned := range []string{"channel", "input", "menu_item", "phone"} {
		if _, present := gotBody[banned]; present {
			t.Fatalf("body must not contain %q (not in D's contract): %v", banned, gotBody)
		}
	}
	if !resp.Continue || resp.Reply != "hi" || resp.Mode != "text" || resp.ConversationID != "conv-x" {
		t.Fatalf("response decode mismatch: %+v", resp)
	}
}

// --- TTL expiry (SPEC §1: 180s) ---

func TestUSSDSessionTTLExpiry(t *testing.T) {
	store := channel.NewMemoryUSSDStore()
	conv := &fakeConversation{resp: channel.USSDTurnResponse{Reply: "ok", Continue: true}}
	s := newUSSDServer(store, &fakeMenus{menu: testMenu}, conv)

	// Session starts.
	ussdPost(t, s.Router(), "sess-5", "*384*123#", "+2348012345678", "")
	sess, err := store.Get(context.Background(), "sess-5")
	if err != nil || sess == nil {
		t.Fatalf("session must be stored after the first callback (sess=%v, err=%v)", sess, err)
	}
	// Expire it: re-save with a negative TTL (avoids sleeping 180s).
	if err := store.Save(context.Background(), sess, -time.Second); err != nil {
		t.Fatal(err)
	}
	if sess, _ := store.Get(context.Background(), "sess-5"); sess != nil {
		t.Fatal("session must expire after the TTL")
	}
	// Next callback is a NEW session: site re-resolved, menu re-fetched,
	// turn forwarded as a fresh session.
	callsBefore := len(conv.captured())
	rec := ussdPost(t, s.Router(), "sess-5", "*384*123#", "+2348012345678", "1")
	if body := mustPlain(t, rec); body != "CON ok" {
		t.Fatalf("expired session restart mismatch: %q", body)
	}
	if len(conv.captured()) != callsBefore+1 {
		t.Fatal("restarted session must forward a fresh turn")
	}
}

// --- garbage form → 400 ---

func TestUSSDGarbageForm400(t *testing.T) {
	s := newUSSDServer(channel.NewMemoryUSSDStore(), &fakeMenus{menu: testMenu}, &fakeConversation{})
	// Missing phoneNumber.
	rec := ussdPost(t, s.Router(), "sess-6", "*384*123#", "", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing phoneNumber must be 400, got %d", rec.Code)
	}
	// Missing sessionId.
	rec = ussdPost(t, s.Router(), "", "*384*123#", "+2348012345678", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing sessionId must be 400, got %d", rec.Code)
	}
	// Missing serviceCode.
	rec = ussdPost(t, s.Router(), "sess-6", "", "+2348012345678", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing serviceCode must be 400, got %d", rec.Code)
	}
	// Not a form at all.
	req := httptest.NewRequest(http.MethodPost, "/webhooks/ussd", strings.NewReader("\xff\xfe garbage"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("garbage body must be 400, got %d", rec.Code)
	}
}

// --- edge cases ---

func TestUSSDUnknownServiceCode(t *testing.T) {
	conv := &fakeConversation{}
	s := newUSSDServer(channel.NewMemoryUSSDStore(), &fakeMenus{menu: testMenu}, conv)
	rec := ussdPost(t, s.Router(), "sess-7", "*384*999#", "+2348012345678", "")
	if body := mustPlain(t, rec); body != "END Unknown service code." {
		t.Fatalf("unmapped service code must end the session, got %q", body)
	}
	if n := len(conv.captured()); n != 0 {
		t.Fatalf("unmapped service code must not reach conversation-service, got %d calls", n)
	}
}

func TestUSSDConversationFailureEndsSession(t *testing.T) {
	conv := &fakeConversation{err: context.DeadlineExceeded}
	s := newUSSDServer(channel.NewMemoryUSSDStore(), &fakeMenus{menu: nil}, conv)
	rec := ussdPost(t, s.Router(), "sess-8", "*384*123#", "+2348012345678", "help")
	body := mustPlain(t, rec)
	if !strings.HasPrefix(body, "END ") {
		t.Fatalf("conversation failure must END the session, got %q", body)
	}
	if !strings.Contains(body, "try again") {
		t.Fatalf("fallback line must ask the subscriber to retry, got %q", body)
	}
}

func TestUSSDEmptyReplyEndsSession(t *testing.T) {
	conv := &fakeConversation{resp: channel.USSDTurnResponse{Reply: "", Continue: true}}
	s := newUSSDServer(channel.NewMemoryUSSDStore(), &fakeMenus{menu: nil}, conv)
	rec := ussdPost(t, s.Router(), "sess-8b", "*384*123#", "+2348012345678", "help")
	if body := mustPlain(t, rec); !strings.HasPrefix(body, "END ") {
		t.Fatalf("empty reply must END the session with the fallback line, got %q", body)
	}
}

func TestUSSDNotConfigured(t *testing.T) {
	s := &Server{Metrics: metrics.New(), Log: zap.NewNop()} // no USSD config
	rec := ussdPost(t, s.Router(), "sess-9", "*384*123#", "+2348012345678", "")
	if body := mustPlain(t, rec); !strings.HasPrefix(body, "END ") {
		t.Fatalf("unconfigured USSD must answer END, got %q", body)
	}
}

// --- memory store unit behaviour ---

func TestMemoryUSSDStoreTTL(t *testing.T) {
	store := channel.NewMemoryUSSDStore()
	ctx := context.Background()
	sess := &channel.USSDSession{ID: "s1", Phone: "+2348"}
	if err := store.Save(ctx, sess, channel.USSDSessionTTL); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, "s1")
	if err != nil || got == nil || got.Phone != "+2348" {
		t.Fatalf("Get after Save: %+v, %v", got, err)
	}
	// Mutating the returned copy must not corrupt the stored session.
	got.Phone = "mutated"
	again, _ := store.Get(ctx, "s1")
	if again.Phone != "+2348" {
		t.Fatalf("store returned its internal session (aliasing): %q", again.Phone)
	}
	// Expired entry reads as absent.
	if err := store.Save(ctx, sess, -time.Second); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Get(ctx, "s1"); got != nil {
		t.Fatal("expired session must read as absent")
	}
	// Delete.
	_ = store.Save(ctx, sess, channel.USSDSessionTTL)
	if err := store.Delete(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Get(ctx, "s1"); got != nil {
		t.Fatal("deleted session must read as absent")
	}
}

// --- menu parsing (identity pack summary shape) ---

func TestParseUSSDMenu(t *testing.T) {
	payload := map[string]any{
		"id": "9f1c2a4e-0000-4000-8000-000000000001", "slug": "acme-ng",
		"pack": map[string]any{
			"id": "salon",
			"ussd": map[string]any{
				"menu": []any{
					map[string]any{"key": "1", "label": "Book appointment", "action": "book"},
					map[string]any{"key": "2", "label": "Talk to an agent", "action": "handoff"},
					map[string]any{"key": "", "label": "broken"}, // skipped: no key
				},
			},
		},
	}
	menu := channel.ParseUSSDMenu(payload)
	if len(menu) != 2 || menu[0].Key != "1" || menu[1].Action != "handoff" {
		t.Fatalf("menu parse mismatch: %+v", menu)
	}
	// No pack / no ussd block → nil (pass-through mode).
	for _, p := range []map[string]any{
		{},
		{"pack": map[string]any{"id": "salon"}},
		{"pack": map[string]any{"ussd": map[string]any{}}},
	} {
		if m := channel.ParseUSSDMenu(p); m != nil {
			t.Fatalf("expected nil menu for %+v, got %+v", p, m)
		}
	}
}
