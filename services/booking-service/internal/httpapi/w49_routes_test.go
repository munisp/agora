package httpapi

// SPEC-W49 (W49-B items B2/B4) route tests: the public pay leg's
// redirect_url allow-list — allowed/exact, allowed/subdomain-wildcard,
// rejected/other-host, rejected/http-scheme, rejected when the env is unset
// (fail closed), empty-ok. Reuses the W48 embedded-postgres fixture
// (port 5573; -short skips) with a stubbed payments rail.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// w49Pay issues the pay call and returns (status, error-code). The stub
// records the last initialize body in f.paymentsReq (nil = not called).
func w49Pay(f *w48Fixture, bookingID, redirect string) (int, string) {
	f.t.Helper()
	body := `{"email":"ada@example.com"`
	if redirect != "" {
		body += `,"redirect_url":` + strconvQuote(redirect)
	}
	body += "}"
	rec := f.do(http.MethodPost, "/public/sites/"+f.siteSlug+"/bookings/"+bookingID+"/pay", body, nil)
	if rec.Code != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(rec.Body).Decode(&e)
		return rec.Code, e.Error
	}
	return rec.Code, ""
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestW49PayRedirectAllowList(t *testing.T) {
	f := newW48Fixture(t, "")
	f.startPaymentsStub()
	allow := []string{"pay.acme.test", "*.acme.ng"}
	f.router = f.buildRouter(Deps{AuthzDisabled: true, PayRedirectAllowHosts: allow},
		bookingops.NewPaymentsClient(f.payments.URL, "tok", zap.NewNop()), f.newResolver())

	// Exact host allowed; the rail sees the redirect_url.
	f.paymentsReq = nil
	b1 := f.seedBooking(store.StatusPending)
	if code, e := w49Pay(f, b1.String(), "https://pay.acme.test/done"); code != http.StatusOK {
		t.Fatalf("exact allowed: code=%d err=%q", code, e)
	}
	if f.paymentsReq["redirect_url"] != "https://pay.acme.test/done" {
		t.Fatalf("stub saw redirect_url = %v", f.paymentsReq["redirect_url"])
	}

	// Wildcard parent-domain entry allows subdomains (and the parent).
	b2 := f.seedBooking(store.StatusPending)
	if code, e := w49Pay(f, b2.String(), "https://tenant.acme.ng/thanks"); code != http.StatusOK {
		t.Fatalf("wildcard subdomain allowed: code=%d err=%q", code, e)
	}
	if code, e := w49Pay(f, b2.String(), "https://acme.ng/thanks"); code != http.StatusOK {
		t.Fatalf("wildcard parent allowed: code=%d err=%q", code, e)
	}

	// Other host rejected 400 redirect_url_not_allowed; the rail is NOT hit.
	f.paymentsReq = nil
	if code, e := w49Pay(f, b2.String(), "https://evil.example.com/"); code != http.StatusBadRequest || e != "redirect_url_not_allowed" {
		t.Fatalf("other host: code=%d err=%q, want 400 redirect_url_not_allowed", code, e)
	}
	if f.paymentsReq != nil {
		t.Fatalf("rejected redirect must not reach the rail; stub saw %v", f.paymentsReq)
	}

	// http scheme on an allowed host is still rejected.
	if code, e := w49Pay(f, b2.String(), "http://pay.acme.test/done"); code != http.StatusBadRequest || e != "redirect_url_not_allowed" {
		t.Fatalf("http scheme: code=%d err=%q, want 400 redirect_url_not_allowed", code, e)
	}

	// Empty redirect stays allowed (provider default applies).
	b3 := f.seedBooking(store.StatusPending)
	f.paymentsReq = nil
	if code, e := w49Pay(f, b3.String(), ""); code != http.StatusOK {
		t.Fatalf("empty redirect: code=%d err=%q", code, e)
	}
	if _, present := f.paymentsReq["redirect_url"]; present {
		t.Fatalf("empty redirect must not be forwarded; stub saw %v", f.paymentsReq)
	}
}

func TestW49PayRedirectFailClosedWhenUnset(t *testing.T) {
	f := newW48Fixture(t, "")
	f.startPaymentsStub()
	// No PayRedirectAllowHosts in Deps — the fail-closed posture.
	f.router = f.buildRouter(Deps{AuthzDisabled: true},
		bookingops.NewPaymentsClient(f.payments.URL, "tok", zap.NewNop()), f.newResolver())

	b := f.seedBooking(store.StatusPending)
	f.paymentsReq = nil
	if code, e := w49Pay(f, b.String(), "https://pay.acme.test/done"); code != http.StatusBadRequest || e != "redirect_url_not_allowed" {
		t.Fatalf("unset allow-list: code=%d err=%q, want 400 redirect_url_not_allowed", code, e)
	}
	if f.paymentsReq != nil {
		t.Fatalf("rejected redirect must not reach the rail; stub saw %v", f.paymentsReq)
	}
	// Empty redirect remains allowed even fail-closed.
	if code, e := w49Pay(f, b.String(), ""); code != http.StatusOK {
		t.Fatalf("empty redirect fail-closed: code=%d err=%q", code, e)
	}
}
