package httpapi

// SPEC-W66 WS2 route tests: the additive booking_ref_hint on the media
// inbox ingest + staff list surface.
//
// Coverage:
//   - a VALID hint on ingest is normalized, recorded, and surfaced by the
//     staff list (booking_ref_hint in the item JSON).
//   - a MALFORMED hint is treated as ABSENT — the ingest still answers
//     200 (NEVER 400; the hint is best-effort) and the row stores no hint.
//   - the hint contradicting the phone match parks with
//     anchor_method='ref_hint_mismatch' (visible in the staff list).
//
// Harness: the shared w61Fixture (embedded postgres, port 5591).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// w66IngestBody is validIngestBody plus a booking_ref_hint.
func (f *w61Fixture) w66IngestBody(messageID, hint string) string {
	return fmt.Sprintf(`{"tenant_id":%q,"channel":"whatsapp","message_id":%q,`+
		`"document_id":%q,"sender_phone":"2348012345678","sender_phone_digits":"2348012345678",`+
		`"mime":"image/jpeg","bytes":1234,"sha256_prefix":"0123456789abcdef","booking_ref_hint":%q}`,
		f.tenant.ID.String(), messageID, uuid.NewString(), hint)
}

// w66ListItem decodes the staff list page.
type w66ListPage struct {
	Items []struct {
		ID             string `json:"id"`
		State          string `json:"state"`
		AnchorMethod   string `json:"anchor_method"`
		BookingRefHint string `json:"booking_ref_hint"`
	} `json:"items"`
}

func TestMediaInboxIngestHintAcceptedAndListed(t *testing.T) {
	f := newW61Fixture(t)

	rec := f.ingest(f.router, w61InternalToken, f.w66IngestBody("w66-h1", "bk-7c3x9q2m"))
	if rec.Code != http.StatusOK {
		t.Fatalf("hint ingest must 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.State != "parked" {
		t.Fatalf("no contact seeded => parked, got %s", rec.Body.String())
	}

	// Staff list surfaces the normalized hint.
	rec = f.staffReq(f.router, http.MethodGet, "/v1/media/inbox?state=parked", "", "owner")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var page w66ListPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].BookingRefHint != "BK-7C3X9Q2M" {
		t.Fatalf("list must surface the normalized hint, got %s", rec.Body.String())
	}
}

func TestMediaInboxIngestHintMalformedNever400(t *testing.T) {
	f := newW61Fixture(t)

	for i, bad := range []string{"BK-7C3X9Q2U", "BK-SHORT", "not a ref", "BK-"} {
		rec := f.ingest(f.router, w61InternalToken, f.w66IngestBody(fmt.Sprintf("w66-bad-%d", i), bad))
		if rec.Code != http.StatusOK {
			t.Fatalf("malformed hint %q must never 400, got %d (%s)", bad, rec.Code, rec.Body.String())
		}
		var resp struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.State != "parked" {
			t.Fatalf("malformed hint %q => absent => parked, got %s", bad, rec.Body.String())
		}
	}

	// The malformed hints were treated as absent: nothing stored.
	rec := f.staffReq(f.router, http.MethodGet, "/v1/media/inbox", "", "owner")
	var page w66ListPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	for _, it := range page.Items {
		if it.BookingRefHint != "" {
			t.Fatalf("malformed hint must not be stored, row %s has %q", it.ID, it.BookingRefHint)
		}
	}
}

func TestMediaInboxIngestHintMismatchParksThroughHTTP(t *testing.T) {
	f := newW61Fixture(t)
	_, bookingID := f.seedContactBooking("+234 801 234 5678", true)
	// Give the booking a real public_ref the hint will NOT match.
	if _, err := f.conn.Exec(context.Background(),
		`UPDATE bookings SET public_ref='BK-THR0HTTP' WHERE id=$1`, bookingID); err != nil {
		t.Fatalf("set public_ref: %v", err)
	}

	rec := f.ingest(f.router, w61InternalToken, f.w66IngestBody("w66-mm1", "BK-0THERREF"))
	if rec.Code != http.StatusOK {
		t.Fatalf("mismatch ingest must 200 (parked), got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.State != "parked" {
		t.Fatalf("contradicting hint must park, got %s", rec.Body.String())
	}

	rec = f.staffReq(f.router, http.MethodGet, "/v1/media/inbox?state=parked", "", "owner")
	var page w66ListPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || len(page.Items) != 1 {
		t.Fatalf("decode list: %v / %s", err, rec.Body.String())
	}
	if page.Items[0].AnchorMethod != "ref_hint_mismatch" || page.Items[0].BookingRefHint != "BK-0THERREF" {
		t.Fatalf("list must show the mismatch flag + hint, got %s", rec.Body.String())
	}
}
