package httpapi

// SPEC-W88: ?contact_phone= resolves the needle through the
// digits-canonical contact match (GetContactByChannel → ContactID), so a
// WhatsApp digits-only verified pin and a '+'-form stored contact — the
// SAME subscriber — find their bookings. The raw ContactPhone filter in
// store/bookings.go is HANDS OFF; the handler rewires around it with a
// byte-identical empty-list fallback when no contact matches.
// Redirection: removing the GetContactByChannel resolution fails the
// cross-format cases (the raw filter misses).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opendesk/booking-service/internal/store"
)

func TestW88ContactPhoneCrossFormat(t *testing.T) {
	f := newW47FixtureOnPort(t, "w88-secret", 5585)
	ctx := context.Background()

	mk := func(name, storedPhone string) (store.Contact, store.Booking) {
		contact := store.Contact{TenantID: f.tenant.ID, Name: name, Phone: storedPhone}
		if err := f.st.CreateContact(ctx, &contact); err != nil {
			t.Fatal(err)
		}
		start := time.Now().UTC().Add(72 * time.Hour)
		b := store.Booking{TenantID: f.tenant.ID, OfferingID: uuid.New(), ContactID: contact.ID,
			TeamMemberID: uuid.New(), StartsAt: start, EndsAt: start.Add(30 * time.Minute),
			Status: store.StatusConfirmed, Source: "voice"}
		if err := f.st.CreateBookingTx(ctx, &b, store.SlotGuard{}, "test.events", []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
		return contact, b
	}

	// Direction 1: contact stored digits-only (WhatsApp wa_id write form),
	// needle in the SIP/OTP '+'-form.
	_, b1 := mk("DigitsStored", "2348033344444")
	// Direction 2: contact stored '+'-form, needle digits-only (wa_id pin).
	_, b2 := mk("PlusStored", "+2348055566666")

	hdr := map[string]string{"X-Internal-Token": "w88-secret"}
	get := func(phone string) map[string]any {
		rec := f.do(http.MethodGet, "/v1/bookings?contact_phone="+url.QueryEscape(phone), ``, hdr)
		if rec.Code != http.StatusOK {
			t.Fatalf("contact_phone=%s -> %d: %s", phone, rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	contains := func(body map[string]any, id string) bool {
		list, _ := body["bookings"].([]any)
		for _, it := range list {
			if m, _ := it.(map[string]any); m["id"] == id {
				return true
			}
		}
		return false
	}

	if body := get("+234 803 334 4444"); !contains(body, b1.ID.String()) {
		t.Fatalf("'+'-form needle missed digits-only contact: %v", body)
	}
	if body := get("2348055566666"); !contains(body, b2.ID.String()) {
		t.Fatalf("digits-only needle missed '+'-form contact: %v", body)
	}
	// Legacy behavior preserved: no-contact needle -> empty list (the
	// ErrNotFound fallback to the raw filter), and the internauth gate is
	// untouched (W47 covers 401/503).
	if body := get("+2348000000000"); contains(body, b1.ID.String()) || contains(body, b2.ID.String()) {
		t.Fatalf("unknown phone leaked bookings: %v", body)
	}
}
