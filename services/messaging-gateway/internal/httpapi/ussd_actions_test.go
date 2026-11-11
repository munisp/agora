package httpapi

// SPEC-W47-C (UIMOB-06) tests: USSD menu-action dispatch end-to-end —
// booking-service effects (helpdesk ticket / status lookup), the promised
// confirmation SMS via notification-worker, session+action idempotency and
// fail-closed machine tokens.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/opendesk/messaging-gateway/internal/channel"
	"go.uber.org/zap"
)

// recordedCall captures one downstream HTTP call (method/path/headers/body).
type recordedCall struct {
	method      string
	path        string
	internalTok string
	tenantSlug  string
	body        map[string]any
}

// fakeDownstream records booking-service / notification-worker calls.
type fakeDownstream struct {
	mu       sync.Mutex
	calls    []recordedCall
	statusLn int // status for /v1/bookings lookups
	bookings []map[string]any
}

func (f *fakeDownstream) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/helpdesk/tickets", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"ticket-1"}`)) //nolint:errcheck
	})
	mux.HandleFunc("/v1/bookings", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		st := f.statusLn
		if st == 0 {
			st = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st)
		if st == http.StatusOK {
			json.NewEncoder(w).Encode(map[string]any{"bookings": f.bookings}) //nolint:errcheck
		}
	})
	mux.HandleFunc("/v1/notify/ussd-action", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"workflow_id":"wf-1"}`)) //nolint:errcheck
	})
	return mux
}

func (f *fakeDownstream) record(r *http.Request) {
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, recordedCall{
		method:      r.Method,
		path:        r.URL.Path,
		internalTok: r.Header.Get("X-Internal-Token"),
		tenantSlug:  r.Header.Get("X-Tenant-Slug"),
		body:        body,
	})
}

func (f *fakeDownstream) callsFor(path string) []recordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedCall
	for _, c := range f.calls {
		if c.path == path {
			out = append(out, c)
		}
	}
	return out
}

func newTestDispatcher(t *testing.T) (*fakeDownstream, *httptest.Server, USSDActionDispatcher) {
	t.Helper()
	fd := &fakeDownstream{}
	ts := httptest.NewServer(fd.handler())
	t.Cleanup(ts.Close)
	return fd, ts, NewUSSDActionDispatcher(ts.URL, "booking-tok", ts.URL, "notify-tok", zap.NewNop())
}

func baseActionReq(action string) USSDActionRequest {
	return USSDActionRequest{
		TenantSlug: "acme-ng", SessionID: "sess-1", Phone: "+2348012345678",
		Action: action, Label: "Book appointment", ConversationID: "conv-1",
	}
}

func TestUSSDActionHandoffCreatesTicketAndSMS(t *testing.T) {
	fd, _, d := newTestDispatcher(t)
	if err := d.Dispatch(context.Background(), baseActionReq("handoff")); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	tickets := fd.callsFor("/v1/helpdesk/tickets")
	if len(tickets) != 1 {
		t.Fatalf("want 1 helpdesk ticket, got %d", len(tickets))
	}
	tk := tickets[0]
	if tk.internalTok != "booking-tok" || tk.tenantSlug != "acme-ng" {
		t.Fatalf("ticket missing machine auth/tenant binding: %+v", tk)
	}
	if tk.body["priority"] != "high" || tk.body["channel"] != "ussd" {
		t.Fatalf("handoff ticket priority/channel wrong: %+v", tk.body)
	}
	if tk.body["conversation_id"] != "conv-1" {
		t.Fatalf("conversation linkage missing: %+v", tk.body)
	}
	subj, _ := tk.body["subject"].(string)
	if !strings.Contains(subj, "Book appointment") || !strings.Contains(subj, "+2348012345678") {
		t.Fatalf("subject must carry label + callback phone: %q", subj)
	}
	sms := fd.callsFor("/v1/notify/ussd-action")
	if len(sms) != 1 {
		t.Fatalf("want 1 confirmation SMS, got %d", len(sms))
	}
	if sms[0].internalTok != "notify-tok" || sms[0].body["phone"] != "+2348012345678" || sms[0].body["session_id"] != "sess-1" {
		t.Fatalf("confirmation SMS shape wrong: %+v", sms[0])
	}
}

func TestUSSDActionBookNormalPriority(t *testing.T) {
	fd, _, d := newTestDispatcher(t)
	if err := d.Dispatch(context.Background(), baseActionReq("book")); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	tickets := fd.callsFor("/v1/helpdesk/tickets")
	if len(tickets) != 1 || tickets[0].body["priority"] != "normal" {
		t.Fatalf("book ticket wrong: %+v", tickets)
	}
}

func TestUSSDActionStatusLookup(t *testing.T) {
	fd, _, d := newTestDispatcher(t)
	fd.bookings = []map[string]any{{"id": "bk-9", "status": "confirmed"}}
	if err := d.Dispatch(context.Background(), baseActionReq("status")); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(fd.callsFor("/v1/bookings")) != 1 {
		t.Fatalf("booking lookup by phone not called")
	}
	sms := fd.callsFor("/v1/notify/ussd-action")
	if len(sms) != 1 || !strings.Contains(sms[0].body["text"].(string), "confirmed") {
		t.Fatalf("status SMS must carry the live status: %+v", sms)
	}
}

func TestUSSDActionStatusFallbacks(t *testing.T) {
	// No bookings → honest "not found" SMS.
	fd, _, d := newTestDispatcher(t)
	if err := d.Dispatch(context.Background(), baseActionReq("status")); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	sms := fd.callsFor("/v1/notify/ussd-action")
	if len(sms) != 1 || !strings.Contains(sms[0].body["text"].(string), "No booking") {
		t.Fatalf("empty lookup must say so: %+v", sms)
	}
	// Lookup failure → generic text, SMS still goes out.
	fd2, _, d2 := newTestDispatcher(t)
	fd2.statusLn = http.StatusInternalServerError
	if err := d2.Dispatch(context.Background(), baseActionReq("status")); err != nil {
		t.Fatalf("lookup failure must not break the confirmation: %v", err)
	}
	sms = fd2.callsFor("/v1/notify/ussd-action")
	if len(sms) != 1 || !strings.Contains(sms[0].body["text"].(string), "could not retrieve") {
		t.Fatalf("failed lookup must degrade honestly: %+v", sms)
	}
}

func TestUSSDActionInfoTenantText(t *testing.T) {
	fd, _, d := newTestDispatcher(t)
	req := baseActionReq("info")
	req.Text = "Float top-up: dial *123# from your POS line."
	if err := d.Dispatch(context.Background(), req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(fd.callsFor("/v1/helpdesk/tickets")) != 0 || len(fd.callsFor("/v1/bookings")) != 0 {
		t.Fatalf("info must not touch booking-service")
	}
	sms := fd.callsFor("/v1/notify/ussd-action")
	if len(sms) != 1 || sms[0].body["text"] != req.Text {
		t.Fatalf("info SMS must carry the tenant-configured text: %+v", sms)
	}
	// No configured text → label fallback.
	fd2, _, d2 := newTestDispatcher(t)
	req.Text = ""
	if err := d2.Dispatch(context.Background(), req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if sms := fd2.callsFor("/v1/notify/ussd-action"); len(sms) != 1 || sms[0].body["text"] != "Book appointment" {
		t.Fatalf("info without text falls back to the label: %+v", sms)
	}
}

func TestUSSDActionFailClosedTokens(t *testing.T) {
	fd := &fakeDownstream{}
	ts := httptest.NewServer(fd.handler())
	defer ts.Close()
	d := NewUSSDActionDispatcher(ts.URL, "", ts.URL, "", zap.NewNop())
	err := d.Dispatch(context.Background(), baseActionReq("handoff"))
	if err == nil {
		t.Fatal("unset tokens must fail closed")
	}
	if len(fd.callsFor("/v1/notify/ussd-action")) != 0 {
		t.Fatal("no confirmation SMS may be attempted without the K2 token")
	}
	if len(fd.callsFor("/v1/helpdesk/tickets")) != 0 {
		t.Fatal("no ticket may be attempted without the K2 token")
	}
}

// --- ussdTurn integration: dispatch + session+action idempotency ---

type fakeDispatcher struct {
	mu   sync.Mutex
	reqs []USSDActionRequest
}

func (f *fakeDispatcher) Dispatch(_ context.Context, req USSDActionRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	return nil
}

func (f *fakeDispatcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func TestUSSDMenuActionDispatchesOnce(t *testing.T) {
	store := channel.NewMemoryUSSDStore()
	conv := &fakeConversation{resp: channel.USSDTurnResponse{
		ConversationID: "conv-1", Reply: "Talk to an agent. You will get a confirmation SMS shortly.",
		Continue: false, Mode: "menu", Selection: "2", Action: "handoff",
	}}
	disp := &fakeDispatcher{}
	srv := newUSSDServer(store, &fakeMenus{menu: testMenu}, conv)
	srv.USSD.Actions = disp

	// First callback opens the session (menu render, CON).
	menus := &fakeMenus{menu: testMenu}
	srv.USSD.Menus = menus
	conv.resp.Continue = true
	conv.resp.Action = ""
	mustPlainBody := ussdPost(t, srv.Router(), "sess-42", "*384*123#", "+2348012345678", "")
	if !strings.HasPrefix(mustPlain(t, mustPlainBody), "CON ") {
		t.Fatalf("first turn should continue the session: %q", mustPlainBody.Body.String())
	}

	// Selection "2" (Talk to an agent) ends the session and dispatches.
	conv.resp.Continue = false
	conv.resp.Action = "handoff"
	rec := ussdPost(t, srv.Router(), "sess-42", "*384*123#", "+2348012345678", "2")
	if body := mustPlain(t, rec); !strings.HasPrefix(body, "END ") {
		t.Fatalf("selection should end the session: %q", body)
	}
	if disp.count() != 1 {
		t.Fatalf("want exactly 1 dispatch, got %d", disp.count())
	}
	got := disp.reqs[0]
	if got.Action != "handoff" || got.Label != "Talk to an agent" || got.Phone != "+2348012345678" ||
		got.TenantSlug != "acme-ng" || got.SessionID != "sess-42" || got.ConversationID != "conv-1" {
		t.Fatalf("dispatch request wrong: %+v", got)
	}

	// AT redelivers the FINAL callback after the session row is gone: the
	// session+action claim marker suppresses the duplicate dispatch.
	rec = ussdPost(t, srv.Router(), "sess-42", "*384*123#", "+2348012345678", "2")
	mustPlain(t, rec)
	if disp.count() != 1 {
		t.Fatalf("redelivery must not re-dispatch (idempotent per session+action), got %d", disp.count())
	}
}

func TestUSSDMenuActionNilDispatcherStillAnswers(t *testing.T) {
	store := channel.NewMemoryUSSDStore()
	conv := &fakeConversation{resp: channel.USSDTurnResponse{
		Reply: "Talk to an agent. You will get a confirmation SMS shortly.",
		Mode:  "menu", Selection: "2", Action: "handoff",
	}}
	srv := newUSSDServer(store, &fakeMenus{menu: testMenu}, conv) // Actions nil
	rec := ussdPost(t, srv.Router(), "sess-7", "*384*123#", "+2348012345678", "2")
	if body := mustPlain(t, rec); !strings.HasPrefix(body, "END ") {
		t.Fatalf("nil dispatcher must never break the reply: %q", body)
	}
}
