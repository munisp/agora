package channel

// SPEC-W87: DaprTgBindStore against a fake Dapr state endpoint (httptest).
// REDIRECTION PROOF: fails on the base tree (DaprTgBindStore undefined).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDaprState records SaveTgBind payloads and serves canned GetTgBind
// responses.
type fakeDaprState struct {
	mu        sync.Mutex
	saves     []map[string]any
	getStatus int
	getBody   string
}

func (f *fakeDaprState) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			var body []map[string]any
			raw, _ := io.ReadAll(r.Body)
			json.Unmarshal(raw, &body) //nolint:errcheck
			f.saves = append(f.saves, body...)
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			if f.getStatus != 0 && f.getStatus != http.StatusOK {
				w.WriteHeader(f.getStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(f.getBody)) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
}

// srvPort parses the port out of an httptest URL (http://127.0.0.1:PORT).
func srvPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	i := strings.LastIndex(srv.URL, ":")
	if i < 0 {
		t.Fatalf("no port in %q", srv.URL)
	}
	p, err := strconv.Atoi(srv.URL[i+1:])
	if err != nil {
		t.Fatalf("port parse: %v", err)
	}
	return p
}

func TestW87DaprTgBindStoreSaveShape(t *testing.T) {
	fake := &fakeDaprState{}
	srv := fake.server()
	defer srv.Close()
	st := NewDaprTgBindStore("statestore", srvPort(t, srv))

	if err := st.SaveTgBind(context.Background(), "42424242", "+2348012345678", 30*24*time.Hour); err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(fake.saves) != 1 {
		t.Fatalf("expected 1 save, got %d", len(fake.saves))
	}
	sv := fake.saves[0]
	if sv["key"] != "tgbind:42424242" {
		t.Fatalf("key = %v", sv["key"])
	}
	val, _ := sv["value"].(map[string]any)
	if val["phone"] != "+2348012345678" {
		t.Fatalf("value = %v", sv["value"])
	}
	meta, _ := sv["metadata"].(map[string]any)
	if meta["ttlInSeconds"] != "2592000" {
		t.Fatalf("ttlInSeconds = %v, want 2592000", meta["ttlInSeconds"])
	}
}

func TestW87DaprTgBindStoreGetHitMissError(t *testing.T) {
	fake := &fakeDaprState{getBody: `{"phone":"+2348012345678"}`}
	srv := fake.server()
	defer srv.Close()
	st := NewDaprTgBindStore("statestore", srvPort(t, srv))

	phone, err := st.GetTgBind(context.Background(), "42424242")
	if err != nil || phone != "+2348012345678" {
		t.Fatalf("hit: phone=%q err=%v", phone, err)
	}

	// Miss: 204 No Content -> ("", nil).
	fake.mu.Lock()
	fake.getStatus = http.StatusNoContent
	fake.mu.Unlock()
	phone, err = st.GetTgBind(context.Background(), "42424242")
	if err != nil || phone != "" {
		t.Fatalf("miss: phone=%q err=%v, want empty/nil", phone, err)
	}

	// Error: 500 -> non-nil error.
	fake.mu.Lock()
	fake.getStatus = http.StatusInternalServerError
	fake.mu.Unlock()
	if _, err = st.GetTgBind(context.Background(), "42424242"); err == nil {
		t.Fatal("500 must surface an error")
	}
}

func TestW87DaprTgBindStoreDefaultComponent(t *testing.T) {
	st := NewDaprTgBindStore("", 3500)
	if st.store != "statestore" {
		t.Fatalf("default store = %q, want statestore", st.store)
	}
	if st.base != "http://127.0.0.1:3500" {
		t.Fatalf("base = %q", st.base)
	}
}
