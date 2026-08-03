package devices

import (
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
// and exercise the real HTTP request/response cycle (route adapter like
// httpapi's devicesHandler, but direct — httpapi wiring is covered by the
// httpapi route-conflict test).

func testRouter(t *testing.T, tenant bookingops.TenantInfo) (http.Handler, *Handlers) {
	t.Helper()
	h := &Handlers{Store: newTestStore(t)}
	r := chi.NewRouter()
	r.Post("/v1/devices", func(w http.ResponseWriter, r *http.Request) { h.Register(w, r, tenant) })
	r.Delete("/v1/devices/{token}", func(w http.ResponseWriter, r *http.Request) { h.Unregister(w, r, tenant) })
	r.Get("/v1/devices", func(w http.ResponseWriter, r *http.Request) { h.List(w, r, tenant) })
	r.Get("/internal/devices", func(w http.ResponseWriter, r *http.Request) { h.ListInternal(w, r, tenant) })
	return r, h
}

func TestDeviceEndpoints(t *testing.T) {
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}
	r, _ := testRouter(t, tenant)
	contactID := uuid.New()

	// Register (201) → refresh (200) → list → internal lookup → delete.
	body := `{"token":"fcm-abc","platform":"android","app":"field","contact_id":"` + contactID.String() + `"}`
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/devices", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	var reg struct {
		Device  DeviceToken `json:"device"`
		Created bool        `json:"created"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reg); err != nil || !reg.Created {
		t.Fatalf("register body: %v, %+v", err, reg)
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/devices", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("re-register = %d, want 200 (upsert refresh)", rec.Code)
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/devices?platform=android&app=field", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	var list struct {
		Devices []DeviceToken `json:"devices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Devices) != 1 {
		t.Fatalf("list body: %v, %+v", err, list)
	}

	// Contract §1 shape for Agent A: a BARE JSON array of device tokens.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/internal/devices?contact_id="+contactID.String(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("internal list = %d", rec.Code)
	}
	var arr []DeviceToken
	if err := json.Unmarshal(rec.Body.Bytes(), &arr); err != nil {
		t.Fatalf("internal response is not a bare JSON array: %v (%s)", err, rec.Body.String())
	}
	if len(arr) != 1 || arr[0].Token != "fcm-abc" {
		t.Fatalf("internal devices: %+v", arr)
	}
	// Unknown contact → 200 with [] (never null, never 404).
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/internal/devices?contact_id="+uuid.NewString(), nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("empty internal list = %d %q, want 200 []", rec.Code, rec.Body.String())
	}
	// Missing/invalid contact_id → 400.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/internal/devices", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("internal without contact_id = %d, want 400", rec.Code)
	}

	// Delete → 200; again → 404.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/devices/fcm-abc", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/devices/fcm-abc", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404", rec.Code)
	}
}

// Validation failures map to 400 (enums, required fields, bad filters).
func TestDeviceEndpointValidation(t *testing.T) {
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}
	r, _ := testRouter(t, tenant)

	for _, tc := range []struct {
		name, method, path, body string
	}{
		{"bad platform", http.MethodPost, "/v1/devices", `{"token":"x","platform":"windows","app":"field"}`},
		{"bad app", http.MethodPost, "/v1/devices", `{"token":"x","platform":"web","app":"crm"}`},
		{"missing token", http.MethodPost, "/v1/devices", `{"platform":"web","app":"field"}`},
		{"bad json", http.MethodPost, "/v1/devices", `{`},
		{"bad platform filter", http.MethodGet, "/v1/devices?platform=windows", ""},
		{"bad app filter", http.MethodGet, "/v1/devices?app=crm", ""},
	} {
		rec := httptest.NewRecorder()
		var req *http.Request
		if tc.body == "" {
			req = httptest.NewRequest(tc.method, tc.path, nil)
		} else {
			req = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		}
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d (%s), want 400", tc.name, rec.Code, rec.Body.String())
		}
	}
}
