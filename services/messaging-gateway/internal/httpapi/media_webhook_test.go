// SPEC-W60 (Scope A) tests: the WhatsApp inbound media -> OCR funnel. The
// recon S4 10-item matrix + the P4 throttle trip, all in the existing
// httptest idiom with fake Meta + OCR servers.
package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/opendesk/messaging-gateway/internal/channel"
	"github.com/opendesk/messaging-gateway/internal/media"
	"github.com/opendesk/messaging-gateway/internal/metrics"
	"github.com/opendesk/messaging-gateway/internal/provider"
	"go.uber.org/zap"
)

// waMediaTenant is the CHANNEL_SITE_MAP tenant used by the media tests
// (uuid form — the ocr contract requires it).
const waMediaTenant = "9f1c2a3b-0000-4000-8000-0000000000aa"

// waMediaPayload builds a signed media webhook body (document when
// kind=="document", image otherwise).
func waMediaPayload(kind, msgID, mediaID, mime, sha, filename string) string {
	block := fmt.Sprintf(`{"id":%q,"mime_type":%q,"sha256":%q,"caption":"do not log me","filename":%q}`,
		mediaID, mime, sha, filename)
	return fmt.Sprintf(`{"entry":[{"changes":[{"value":{
		"metadata":{"phone_number_id":"10987654321"},
		"messages":[{"from":"2348012345678","id":%q,"timestamp":"1733000000","type":%q,%q:%s}]
	}}]}]}`, msgID, kind, kind, block)
}

// fakeMeta is a two-endpoint Meta Cloud API stand-in: GET /{mediaID}
// returns the metadata JSON; GET /lookaside/{mediaID} returns the bytes.
// Counters let tests assert the cost gates (unmapped/dedupe/throttle =>
// ZERO Meta hits).
type fakeMeta struct {
	srv        *httptest.Server
	metaHits   atomic.Int32
	byteHits   atomic.Int32
	payload    []byte
	mime       string
	sha        string // hex sha256 of payload
	metaStatus int    // non-zero overrides the metadata status
	byteStatus int    // non-zero overrides the byte status
}

func newFakeMeta(t *testing.T, payload []byte, mime string) *fakeMeta {
	t.Helper()
	fm := &fakeMeta{payload: payload, mime: mime}
	sum := sha256.Sum256(payload)
	fm.sha = hex.EncodeToString(sum[:])
	fm.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer wa-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/lookaside/") {
			fm.byteHits.Add(1)
			if fm.byteStatus != 0 {
				w.WriteHeader(fm.byteStatus)
				return
			}
			w.Header().Set("Content-Type", fm.mime)
			w.Write(fm.payload) //nolint:errcheck
			return
		}
		fm.metaHits.Add(1)
		if fm.metaStatus != 0 {
			w.WriteHeader(fm.metaStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"url":%q,"mime_type":%q,"sha256":%q,"file_size":%d}`,
			fm.srv.URL+"/lookaside/m1", fm.mime, fm.sha, len(fm.payload)) //nolint:errcheck
	}))
	t.Cleanup(fm.srv.Close)
	return fm
}

// fakeOCR is an ocr-service stand-in asserting the A↔B extract contract.
type fakeOCR struct {
	srv    *httptest.Server
	hits   atomic.Int32
	status int // non-2xx override
	body   string
	last   struct {
		kind, tenantID, token, slug, filename string
	}
}

func newFakeOCR(t *testing.T) *fakeOCR {
	t.Helper()
	fo := &fakeOCR{}
	fo.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fo.hits.Add(1)
		if r.URL.Path != "/v1/ocr/extract" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fo.last.token = r.Header.Get("X-Internal-Token")
		fo.last.slug = r.Header.Get("X-Tenant-Slug")
		mr, err := r.MultipartReader()
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			switch p.FormName() {
			case "kind":
				b, _ := io.ReadAll(p)
				fo.last.kind = string(b)
			case "tenant_id":
				b, _ := io.ReadAll(p)
				fo.last.tenantID = string(b)
			case "file":
				fo.last.filename = p.FileName()
				_, _ = io.Copy(io.Discard, p)
			}
		}
		if fo.status != 0 {
			w.WriteHeader(fo.status)
			w.Write([]byte(fo.body)) //nolint:errcheck
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"document_id":"11111111-2222-3333-4444-555555555555","status":"uploaded","extracted_fields":{},"page_count":1}`)) //nolint:errcheck
	}))
	t.Cleanup(fo.srv.Close)
	return fo
}

// newMediaServer builds a Server with the media funnel ENABLED and wired to
// the fake Meta/OCR servers (signed-post posture, like newWebhookServer).
func newMediaServer(fb Bridger, fm *fakeMeta, fo *fakeOCR) *Server {
	s := newWebhookServer(fb)
	s.WhatsAppMediaEnabled = true
	s.Sites = map[string]channel.Site{
		"whatsapp:10987654321": {SiteSlug: "acme-ng", TenantID: waMediaTenant},
	}
	s.WhatsApp = &provider.WhatsApp{
		Client:        provider.NewClient("whatsapp", metrics.New(), zap.NewNop()),
		BaseURL:       fm.srv.URL,
		Token:         "wa-token",
		PhoneNumberID: "10987654321",
	}
	s.Media = media.NewOCRClient(fo.srv.URL, "ocr-token", zap.NewNop())
	return s
}

// jpegBytes is a minimal jpeg-magic payload (magic-byte validation only
// needs the header).
var jpegBytes = append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte("fake-jpeg-payload")...)

func postMedia(t *testing.T, s *Server, kind, msgID, mediaID, mime, sha string) *httptest.ResponseRecorder {
	t.Helper()
	body := waMediaPayload(kind, msgID, mediaID, mime, sha, "receipt.jpg")
	return do(t, s.Router(), http.MethodPost, "/webhooks/whatsapp", body, waSignedHeaders(waTestAppSecret, body))
}

// S4-1: the waWebhook struct parses image AND document blocks.
func TestWAMediaParsesBlocks(t *testing.T) {
	var payload waWebhook
	img := waMediaPayload("image", "wamid.Img", "img-1", "image/jpeg", "abc123", "photo.jpg")
	if err := json.Unmarshal([]byte(img), &payload); err != nil {
		t.Fatalf("image payload: %v", err)
	}
	m := payload.Entry[0].Changes[0].Value.Messages[0]
	if m.Image == nil || m.Image.ID != "img-1" || m.Image.MimeType != "image/jpeg" ||
		m.Image.SHA256 == "" || m.Image.Caption == "" || m.Image.Filename != "photo.jpg" {
		t.Fatalf("image block not parsed: %+v", m.Image)
	}
	var payload2 waWebhook
	doc := waMediaPayload("document", "wamid.Doc", "doc-1", "application/pdf", "def456", "invoice.pdf")
	if err := json.Unmarshal([]byte(doc), &payload2); err != nil {
		t.Fatalf("document payload: %v", err)
	}
	d := payload2.Entry[0].Changes[0].Value.Messages[0].Document
	if d == nil || d.ID != "doc-1" || d.MimeType != "application/pdf" || d.Filename != "invoice.pdf" {
		t.Fatalf("document block not parsed: %+v", d)
	}
}

// S4-2: media on an UNMAPPED phone_number_id is dropped 200 with ZERO Meta
// fetches (P3 cost gate: resolve before fetch).
func TestWAMediaUnmappedRouteNoFetch(t *testing.T) {
	fm := newFakeMeta(t, jpegBytes, "image/jpeg")
	fo := newFakeOCR(t)
	s := newMediaServer(&fakeBridge{}, fm, fo)
	s.Sites = map[string]channel.Site{} // nothing mapped
	rec := postMedia(t, s, "image", "wamid.Unmapped", "m1", "image/jpeg", fm.sha)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if n := fm.metaHits.Load() + fm.byteHits.Load(); n != 0 {
		t.Fatalf("unmapped route must never fetch from Meta, got %d hits", n)
	}
	if n := fo.hits.Load(); n != 0 {
		t.Fatalf("unmapped route must never call OCR, got %d hits", n)
	}
}

// S4 happy path + contract: image -> kind=receipt, pdf -> kind=document
// (P2 hard-coded mapping), tenant uuid on form field AND header, token
// header set, caption never sent (no such form field).
func TestWAMediaIngestsImageAndDocument(t *testing.T) {
	fm := newFakeMeta(t, jpegBytes, "image/jpeg")
	fo := newFakeOCR(t)
	s := newMediaServer(&fakeBridge{}, fm, fo)
	rec := postMedia(t, s, "image", "wamid.Img1", "m1", "image/jpeg", fm.sha)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if fo.last.kind != "receipt" {
		t.Fatalf("image must map to kind=receipt, got %q", fo.last.kind)
	}
	if fo.last.tenantID != waMediaTenant || fo.last.slug != waMediaTenant {
		t.Fatalf("tenant uuid must ride form field and header, got %q / %q", fo.last.tenantID, fo.last.slug)
	}
	if fo.last.token != "ocr-token" {
		t.Fatalf("X-Internal-Token missing, got %q", fo.last.token)
	}
	if fo.last.filename != "receipt.jpg" {
		t.Fatalf("filename must pass through for extension validation, got %q", fo.last.filename)
	}

	// Document block: pdf -> kind=document.
	fm2 := newFakeMeta(t, append([]byte("%PDF-1.7 "), []byte("fake-pdf")...), "application/pdf")
	fo2 := newFakeOCR(t)
	s2 := newMediaServer(&fakeBridge{}, fm2, fo2)
	rec = postMedia(t, s2, "document", "wamid.Doc1", "m1", "application/pdf", fm2.sha)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if fo2.last.kind != "document" {
		t.Fatalf("pdf must map to kind=document, got %q", fo2.last.kind)
	}
}

// S4-3: redelivery of an already-ingested message id is a no-op — no Meta
// refetch, no OCR repost (claim-on-success dedupe, P3).
func TestWAMediaRedeliveryNoRefetch(t *testing.T) {
	fm := newFakeMeta(t, jpegBytes, "image/jpeg")
	fo := newFakeOCR(t)
	s := newMediaServer(&fakeBridge{}, fm, fo)
	h := s.Router() // one router => one shared deduper
	body := waMediaPayload("image", "wamid.Dup", "m1", "image/jpeg", fm.sha, "receipt.jpg")
	for i := 0; i < 2; i++ {
		rec := do(t, h, http.MethodPost, "/webhooks/whatsapp", body, waSignedHeaders(waTestAppSecret, body))
		if rec.Code != http.StatusOK {
			t.Fatalf("post %d: expected 200, got %d", i, rec.Code)
		}
	}
	if n := fm.metaHits.Load(); n != 1 {
		t.Fatalf("redelivery must not refetch metadata, got %d hits", n)
	}
	if n := fo.hits.Load(); n != 1 {
		t.Fatalf("redelivery must not repost to OCR, got %d hits", n)
	}
}

// S4-4: Meta metadata 500 => webhook 500 (N-05); the redelivery succeeds
// and the success claims the dedupe exactly once.
func TestWAMediaFetchFailure500ThenRedeliverySucceeds(t *testing.T) {
	fm := newFakeMeta(t, jpegBytes, "image/jpeg")
	fo := newFakeOCR(t)
	s := newMediaServer(&fakeBridge{}, fm, fo)
	fm.metaStatus = http.StatusInternalServerError
	rec := postMedia(t, s, "image", "wamid.Retry", "m1", "image/jpeg", fm.sha)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("fetch failure must answer 500, got %d", rec.Code)
	}
	if n := fo.hits.Load(); n != 0 {
		t.Fatalf("failed fetch must not reach OCR, got %d hits", n)
	}
	fm.metaStatus = 0 // Meta recovers
	rec = postMedia(t, s, "image", "wamid.Retry", "m1", "image/jpeg", fm.sha)
	if rec.Code != http.StatusOK {
		t.Fatalf("redelivery must succeed, got %d", rec.Code)
	}
	if n := fo.hits.Load(); n != 1 {
		t.Fatalf("redelivery must post to OCR exactly once, got %d", n)
	}
	// A THIRD post (redelivery after success) is a no-op.
	rec = postMedia(t, s, "image", "wamid.Retry", "m1", "image/jpeg", fm.sha)
	if rec.Code != http.StatusOK || fo.hits.Load() != 1 {
		t.Fatalf("post-success redelivery must be a 200 no-op, got %d / %d OCR hits", rec.Code, fo.hits.Load())
	}
}

// S4-5: disallowed declared mimes drop 200 BEFORE any fetch; audio/video
// message types stay on the plain-ignore path.
func TestWAMediaMimeRejectedNoFetch(t *testing.T) {
	fm := newFakeMeta(t, jpegBytes, "image/jpeg")
	fo := newFakeOCR(t)
	s := newMediaServer(&fakeBridge{}, fm, fo)
	for i, mime := range []string{"image/gif", "video/mp4", "audio/ogg", "application/zip"} {
		rec := postMedia(t, s, "image", fmt.Sprintf("wamid.Mime%d", i), "m1", mime, fm.sha)
		if rec.Code != http.StatusOK {
			t.Fatalf("mime %s: expected 200, got %d", mime, rec.Code)
		}
	}
	if n := fm.metaHits.Load() + fm.byteHits.Load(); n != 0 {
		t.Fatalf("mime rejects must never fetch, got %d Meta hits", n)
	}
	if n := fo.hits.Load(); n != 0 {
		t.Fatalf("mime rejects must never reach OCR, got %d hits", n)
	}
}

// S4-6: an oversized byte stream (>25MB) aborts before OCR and answers 200
// (P6 deterministic reject — Meta must not redeliver-storm it).
func TestWAMediaOversizeAborts(t *testing.T) {
	big := make([]byte, (25<<20)+64)
	copy(big, []byte{0xFF, 0xD8, 0xFF, 0xE0})
	fm := newFakeMeta(t, big, "image/jpeg")
	fo := newFakeOCR(t)
	s := newMediaServer(&fakeBridge{}, fm, fo)
	rec := postMedia(t, s, "image", "wamid.Big", "m1", "image/jpeg", fm.sha)
	if rec.Code != http.StatusOK {
		t.Fatalf("oversize must be a deterministic 200 drop, got %d", rec.Code)
	}
	if n := fo.hits.Load(); n != 0 {
		t.Fatalf("oversize must abort before OCR, got %d hits", n)
	}
	// And the drop is not claimed: a redelivery retries the fetch (which
	// keeps deterministically rejecting without OCR).
	rec = postMedia(t, s, "image", "wamid.Big", "m1", "image/jpeg", fm.sha)
	if rec.Code != http.StatusOK || fo.hits.Load() != 0 {
		t.Fatalf("oversize redelivery must stay a 200 no-OCR drop, got %d / %d", rec.Code, fo.hits.Load())
	}
}

// S4-7: OCR 500 => webhook 500 (transient, N-05 redelivery); OCR 415
// (AV-infected) => webhook 200 + drop (P6 deterministic, no storm).
func TestWAMediaOCR500vs415(t *testing.T) {
	fm := newFakeMeta(t, jpegBytes, "image/jpeg")
	fo := newFakeOCR(t)
	s := newMediaServer(&fakeBridge{}, fm, fo)
	fo.status = http.StatusInternalServerError
	rec := postMedia(t, s, "image", "wamid.Ocr500", "m1", "image/jpeg", fm.sha)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("OCR 500 must map to webhook 500, got %d", rec.Code)
	}
	fo.status = http.StatusUnsupportedMediaType
	rec = postMedia(t, s, "image", "wamid.Ocr415", "m1", "image/jpeg", fm.sha)
	if rec.Code != http.StatusOK {
		t.Fatalf("OCR 415 must map to webhook 200 (deterministic), got %d", rec.Code)
	}
}

// S4-8a: flag off (default) => media drops + logs exactly like pre-W60 —
// zero fetches, zero OCR calls, text path untouched.
func TestWAMediaFlagOffDrops(t *testing.T) {
	fm := newFakeMeta(t, jpegBytes, "image/jpeg")
	fo := newFakeOCR(t)
	s := newMediaServer(&fakeBridge{}, fm, fo)
	s.WhatsAppMediaEnabled = false
	rec := postMedia(t, s, "image", "wamid.Off", "m1", "image/jpeg", fm.sha)
	if rec.Code != http.StatusOK {
		t.Fatalf("flag-off drop must answer 200, got %d", rec.Code)
	}
	if n := fm.metaHits.Load() + fm.byteHits.Load() + fo.hits.Load(); n != 0 {
		t.Fatalf("flag-off must not fetch/OCR, got %d hits", n)
	}
	// Text path untouched with the flag off.
	fb := s.Bridge.(*fakeBridge) //nolint:errcheck
	rec = do(t, s.Router(), http.MethodPost, "/webhooks/whatsapp", waTextPayload, waSignedHeaders(waTestAppSecret, waTextPayload))
	if rec.Code != http.StatusOK || len(fb.captured()) != 1 {
		t.Fatalf("text path must be untouched with the flag off, got %d / %d bridge calls", rec.Code, len(fb.captured()))
	}
}

// S4-8b: flag on but OCR token unset => fail-closed per-message drop, no
// Meta fetch (the cost gate runs before any byte is spent).
func TestWAMediaOCRTokenUnsetFailClosed(t *testing.T) {
	fm := newFakeMeta(t, jpegBytes, "image/jpeg")
	fo := newFakeOCR(t)
	s := newMediaServer(&fakeBridge{}, fm, fo)
	s.Media = media.NewOCRClient(fo.srv.URL, "", zap.NewNop()) // URL set, token unset
	rec := postMedia(t, s, "image", "wamid.NoTok", "m1", "image/jpeg", fm.sha)
	if rec.Code != http.StatusOK {
		t.Fatalf("fail-closed drop must answer 200, got %d", rec.Code)
	}
	if n := fm.metaHits.Load() + fm.byteHits.Load(); n != 0 {
		t.Fatalf("unconfigured OCR must drop before any Meta fetch, got %d hits", n)
	}
	if n := fo.hits.Load(); n != 0 {
		t.Fatalf("unconfigured OCR must never be called, got %d hits", n)
	}
}

// S4-9: mock posture — WHATSAPP_MOCK=1 unsigned media posts still work end
// to end (dev journeys).
func TestWAMediaMockModeUnsigned(t *testing.T) {
	fm := newFakeMeta(t, jpegBytes, "image/jpeg")
	fo := newFakeOCR(t)
	s := newMediaServer(&fakeBridge{}, fm, fo)
	s.WhatsAppMock = true
	body := waMediaPayload("image", "wamid.Mock", "m1", "image/jpeg", fm.sha, "receipt.jpg")
	rec := do(t, s.Router(), http.MethodPost, "/webhooks/whatsapp", body, nil) // unsigned
	if rec.Code != http.StatusOK {
		t.Fatalf("mock-mode unsigned media post must succeed, got %d", rec.Code)
	}
	if n := fo.hits.Load(); n != 1 {
		t.Fatalf("mock-mode media must reach OCR, got %d hits", n)
	}
}

// S4-10: signature regression — a media post with a bad
// X-Hub-Signature-256 is 401 and NOTHING is fetched (the existing verify
// path covers it).
func TestWAMediaBadSignature401NoFetch(t *testing.T) {
	fm := newFakeMeta(t, jpegBytes, "image/jpeg")
	fo := newFakeOCR(t)
	s := newMediaServer(&fakeBridge{}, fm, fo)
	body := waMediaPayload("image", "wamid.BadSig", "m1", "image/jpeg", fm.sha, "receipt.jpg")
	rec := do(t, s.Router(), http.MethodPost, "/webhooks/whatsapp", body,
		map[string]string{"X-Hub-Signature-256": "sha256=deadbeef"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature must be 401, got %d", rec.Code)
	}
	if n := fm.metaHits.Load() + fm.byteHits.Load() + fo.hits.Load(); n != 0 {
		t.Fatalf("rejected post must not fetch/OCR, got %d hits", n)
	}
}

// P4: the per-sender throttle trips => 200 drop + no fetch/OCR; OTHER
// senders are unaffected (per-key window).
func TestWAMediaThrottleTrip(t *testing.T) {
	fm := newFakeMeta(t, jpegBytes, "image/jpeg")
	fo := newFakeOCR(t)
	s := newMediaServer(&fakeBridge{}, fm, fo)
	s.MediaRate = NewPhoneRateLimiter(2, 0) // 2 per window
	h := s.Router()
	mk := func(id string) string {
		return waMediaPayload("image", id, "m1", "image/jpeg", fm.sha, "receipt.jpg")
	}
	for i, id := range []string{"wamid.T1", "wamid.T2"} {
		rec := do(t, h, http.MethodPost, "/webhooks/whatsapp", mk(id), waSignedHeaders(waTestAppSecret, mk(id)))
		if rec.Code != http.StatusOK {
			t.Fatalf("post %d within limit must be 200, got %d", i, rec.Code)
		}
	}
	if n := fo.hits.Load(); n != 2 {
		t.Fatalf("expected 2 OCR posts within the limit, got %d", n)
	}
	// Third media message from the SAME sender trips the throttle.
	rec := do(t, h, http.MethodPost, "/webhooks/whatsapp", mk("wamid.T3"), waSignedHeaders(waTestAppSecret, mk("wamid.T3")))
	if rec.Code != http.StatusOK {
		t.Fatalf("throttle trip must still answer 200, got %d", rec.Code)
	}
	if n := fm.metaHits.Load(); n != 2 {
		t.Fatalf("throttled message must not fetch, got %d Meta hits", n)
	}
	if n := fo.hits.Load(); n != 2 {
		t.Fatalf("throttled message must not reach OCR, got %d hits", n)
	}
	// A different sender is unaffected (per-key window).
	other := waMediaPayload("image", "wamid.T4", "m1", "image/jpeg", fm.sha, "receipt.jpg")
	other = strings.Replace(other, "2348012345678", "2348099999999", 1)
	rec = do(t, h, http.MethodPost, "/webhooks/whatsapp", other, waSignedHeaders(waTestAppSecret, other))
	if rec.Code != http.StatusOK || fo.hits.Load() != 3 {
		t.Fatalf("other sender must be unaffected, got %d / %d OCR hits", rec.Code, fo.hits.Load())
	}
}

// sha256 mismatch (Meta declared vs bytes) is a deterministic reject.
func TestWAMediaChecksumMismatch(t *testing.T) {
	fm := newFakeMeta(t, jpegBytes, "image/jpeg")
	fm.sha = "00000000000000000000000000000000000000000000000000000000000000ff"
	fo := newFakeOCR(t)
	s := newMediaServer(&fakeBridge{}, fm, fo)
	rec := postMedia(t, s, "image", "wamid.Sha", "m1", "image/jpeg", fm.sha)
	if rec.Code != http.StatusOK {
		t.Fatalf("checksum mismatch must be a deterministic 200 drop, got %d", rec.Code)
	}
	if n := fo.hits.Load(); n != 0 {
		t.Fatalf("checksum mismatch must not reach OCR, got %d hits", n)
	}
}

// OCR 2xx without document_id is a contract violation => transient 500.
func TestWAMediaOCRContractViolation(t *testing.T) {
	fm := newFakeMeta(t, jpegBytes, "image/jpeg")
	fo := newFakeOCR(t)
	fo.status = http.StatusOK
	fo.body = `{"status":"uploaded"}`
	s := newMediaServer(&fakeBridge{}, fm, fo)
	rec := postMedia(t, s, "image", "wamid.NoDoc", "m1", "image/jpeg", fm.sha)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("2xx without document_id must map to 500, got %d", rec.Code)
	}
}

// MediaDeduper unit: claim-on-success semantics + empty ids never dedupe.
func TestMediaDeduperUnit(t *testing.T) {
	d := NewMediaDeduper()
	if d.seen("a") {
		t.Fatal("unseen id reported seen")
	}
	d.mark("a")
	if !d.seen("a") {
		t.Fatal("marked id not seen")
	}
	if d.seen("") {
		t.Fatal("empty id must never dedupe")
	}
	d.mark("")
	if d.seen("") {
		t.Fatal("empty id must never be claimed")
	}
}
