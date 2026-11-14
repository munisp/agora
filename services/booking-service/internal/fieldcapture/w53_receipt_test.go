package fieldcapture

// SPEC-W53 Scope B receipt-OCR tests — embedded Postgres (newTestRig, port
// 5562, -short skips) + a FAKE ocr-service httptest server (TESTS ONLY — no
// production mock; the production path calls the real sidecar). Covers:
// happy path upload→forward→persist, ocr-down 502 fail-open with the
// capture row intact, mime/magic rejection, the 25 MB cap, fail-closed 503
// postures, additive-column backward compatibility and RLS isolation.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/opendesk/booking-service/internal/bookingops"
)

// w53PNG is a payload with a valid PNG magic prefix (content beyond the
// signature is irrelevant to validation).
var w53PNG = append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, bytes.Repeat([]byte{0x42}, 64)...)

// w53FakeOCR is a scripted ocr-service double recording what it received.
type w53FakeOCR struct {
	mu     sync.Mutex
	calls  int
	token  string
	tenant string // X-Tenant-Slug header
	kind   string // multipart field
	tid    string // tenant_id multipart field
	fmime  string // file part Content-Type
	fbytes []byte

	status int    // HTTP status to answer (default 200)
	docID  uuid.UUID
	fields string // raw extracted_fields JSON ("" → null)
}

func (f *w53FakeOCR) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls++
		f.token = r.Header.Get("X-Internal-Token")
		f.tenant = r.Header.Get("X-Tenant-Slug")
		f.mu.Unlock()
		if err := r.ParseMultipartForm(MaxReceiptBytes); err != nil {
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		file, fh, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "missing file", http.StatusBadRequest)
			return
		}
		data, _ := io.ReadAll(file)
		_ = file.Close()
		f.mu.Lock()
		f.kind = r.FormValue("kind")
		f.tid = r.FormValue("tenant_id")
		f.fmime = fh.Header.Get("Content-Type")
		f.fbytes = data
		f.mu.Unlock()

		if f.status != 0 && f.status != 200 {
			http.Error(w, "ocr exploded", f.status)
			return
		}
		fields := f.fields
		if fields == "" {
			fields = "null"
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"document_id":%q,"status":"extracted","extracted_fields":%s,"page_count":1}`, f.docID, fields)
	})
}

func (f *w53FakeOCR) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// w53ReceiptHTTP invokes Handlers.Receipt with the chi route context set as
// httpapi would ({id} path param), tenant injected explicitly.
func w53ReceiptHTTP(t *testing.T, h *Handlers, tenant bookingops.TenantInfo, clientID, mime string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/field/captures/"+clientID+"/receipt", bytes.NewReader(body))
	if mime != "" {
		req.Header.Set("Content-Type", mime)
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", clientID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.Receipt(rec, req, tenant)
	return rec
}

// w53AnchorCapture applies one checkin item so the field_captures anchor row
// exists, returning its client_id.
func w53AnchorCapture(t *testing.T, rig testRig) string {
	t.Helper()
	item := mkItem(uuid.NewString(), KindCheckin, CheckinPayload{Note: "site visit"}, nil)
	res := rig.svc.Capture(context.Background(), rig.tenant, []CaptureItem{item})
	if len(res) != 1 || res[0].Status != StatusApplied {
		t.Fatalf("anchor capture: %+v", res)
	}
	return item.ClientID
}

// w53ReceiptCols reads the additive W53 columns of one capture row.
func w53ReceiptCols(t *testing.T, fs *Store, tenantID uuid.UUID, clientID string) (docID *uuid.UUID, fields json.RawMessage) {
	t.Helper()
	err := fs.withTenant(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT receipt_document_id, receipt_fields FROM field_captures WHERE tenant_id=$1 AND client_id=$2`,
			tenantID, clientID).Scan(&docID, &fields)
	})
	if err != nil {
		t.Fatalf("read receipt cols: %v", err)
	}
	return docID, fields
}

func TestW53ReceiptHappyPath(t *testing.T) {
	rig := newTestRig(t)
	clientID := w53AnchorCapture(t, rig)

	fake := &w53FakeOCR{
		docID:  uuid.New(),
		fields: `{"vendor":"Chop Life","amount":1250.5,"date":"2025-01-14","items":[{"name":"Rice","quantity":1,"price":1250.5}]}`,
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	h := &Handlers{
		Svc:        rig.svc,
		BatchLimit: 10,
		OCR:        NewOCRClient(srv.URL, "w53-token", nil),
	}
	rec := w53ReceiptHTTP(t, h, rig.tenant, clientID, "image/png", w53PNG)
	if rec.Code != http.StatusOK {
		t.Fatalf("receipt upload = %d: %s", rec.Code, rec.Body.String())
	}

	// Contract shape: the ocr-service response is passed through verbatim.
	var res OCRExtractResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if res.DocumentID != fake.docID || res.Status != "extracted" || res.PageCount != 1 {
		t.Fatalf("contract response: %+v", res)
	}
	var fields ReceiptFields
	if err := json.Unmarshal(res.ExtractedFields, &fields); err != nil {
		t.Fatalf("extracted_fields shape: %v", err)
	}
	if fields.Vendor == nil || *fields.Vendor != "Chop Life" || fields.Amount == nil || *fields.Amount != 1250.5 || len(fields.Items) != 1 {
		t.Fatalf("extracted_fields: %+v", fields)
	}

	// Forwarding shape: internal token + tenant uuid (header AND form field)
	// + kind=receipt + byte-exact file with the declared mime.
	if fake.callCount() != 1 {
		t.Fatalf("ocr calls = %d, want 1", fake.callCount())
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.token != "w53-token" {
		t.Fatalf("X-Internal-Token = %q", fake.token)
	}
	if fake.tenant != rig.tenant.ID.String() || fake.tid != rig.tenant.ID.String() {
		t.Fatalf("tenant forwarding: header %q, field %q", fake.tenant, fake.tid)
	}
	if fake.kind != "receipt" || fake.fmime != "image/png" || !bytes.Equal(fake.fbytes, w53PNG) {
		t.Fatalf("multipart forward: kind=%q mime=%q bytes=%d", fake.kind, fake.fmime, len(fake.fbytes))
	}

	// Persisted: document id + raw fields jsonb on the capture anchor.
	docID, stored := w53ReceiptCols(t, rig.fstore, rig.tenant.ID, clientID)
	if docID == nil || *docID != fake.docID {
		t.Fatalf("receipt_document_id = %v, want %s", docID, fake.docID)
	}
	var storedFields map[string]any
	if err := json.Unmarshal(stored, &storedFields); err != nil || storedFields["vendor"] != "Chop Life" {
		t.Fatalf("receipt_fields = %s, %v", stored, err)
	}
}

func TestW53ReceiptOCRDownFailOpen(t *testing.T) {
	rig := newTestRig(t)
	clientID := w53AnchorCapture(t, rig)

	fake := &w53FakeOCR{status: http.StatusInternalServerError, docID: uuid.New()}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	h := &Handlers{Svc: rig.svc, OCR: NewOCRClient(srv.URL, "w53-token", nil)}
	rec := w53ReceiptHTTP(t, h, rig.tenant, clientID, "image/png", w53PNG)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("ocr-down = %d, want 502: %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["code"] != "OCR_UNAVAILABLE" {
		t.Fatalf("ocr-down body = %s, want code OCR_UNAVAILABLE", rec.Body.String())
	}

	// Fail-open for the capture: the anchor row stands, terminal status
	// intact, receipt columns untouched.
	docID, fields := w53ReceiptCols(t, rig.fstore, rig.tenant.ID, clientID)
	if docID != nil || fields != nil {
		t.Fatalf("capture row mutated by failed attach: doc=%v fields=%s", docID, fields)
	}
	var status string
	err := rig.fstore.withTenant(context.Background(), rig.tenant.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT status FROM field_captures WHERE tenant_id=$1 AND client_id=$2`,
			rig.tenant.ID, clientID).Scan(&status)
	})
	if err != nil || status != "applied" {
		t.Fatalf("capture row lost or mutated: status=%q err=%v", status, err)
	}

	// Transport-level failure (unreachable host) is the same 502 contract.
	dead := httptest.NewServer(fake.handler())
	deadURL := dead.URL
	dead.Close()
	h2 := &Handlers{Svc: rig.svc, OCR: NewOCRClient(deadURL, "w53-token", nil)}
	if rec := w53ReceiptHTTP(t, h2, rig.tenant, clientID, "image/png", w53PNG); rec.Code != http.StatusBadGateway {
		t.Fatalf("ocr-unreachable = %d, want 502", rec.Code)
	}
}

func TestW53ReceiptMimeAndMagicRejection(t *testing.T) {
	rig := newTestRig(t)
	clientID := w53AnchorCapture(t, rig)
	fake := &w53FakeOCR{docID: uuid.New()}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	h := &Handlers{Svc: rig.svc, OCR: NewOCRClient(srv.URL, "w53-token", nil)}

	// Disallowed declared mime.
	if rec := w53ReceiptHTTP(t, h, rig.tenant, clientID, "text/plain", []byte("hello")); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain = %d, want 415", rec.Code)
	}
	// Allow-listed mime but magic bytes do not match.
	if rec := w53ReceiptHTTP(t, h, rig.tenant, clientID, "image/png", []byte("not a png")); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("bad-magic png = %d, want 415", rec.Code)
	}
	// Content-Type parameters are tolerated (image/jpeg; charset=binary).
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 1, 2, 3}
	if rec := w53ReceiptHTTP(t, h, rig.tenant, clientID, "image/jpeg; charset=binary", jpeg); rec.Code != http.StatusOK {
		t.Fatalf("jpeg with params = %d: %s", rec.Code, rec.Body.String())
	}
	if fake.callCount() != 1 {
		t.Fatalf("rejected uploads reached ocr: calls = %d, want 1", fake.callCount())
	}
	docID, _ := w53ReceiptCols(t, rig.fstore, rig.tenant.ID, clientID)
	if docID == nil {
		t.Fatal("jpeg attach did not persist")
	}
}

func TestW53ReceiptSizeCap(t *testing.T) {
	rig := newTestRig(t)
	clientID := w53AnchorCapture(t, rig)
	fake := &w53FakeOCR{docID: uuid.New()}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	h := &Handlers{Svc: rig.svc, OCR: NewOCRClient(srv.URL, "w53-token", nil)}

	oversize := make([]byte, MaxReceiptBytes+1)
	copy(oversize, []byte("%PDF-"))
	if rec := w53ReceiptHTTP(t, h, rig.tenant, clientID, "application/pdf", oversize); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize = %d, want 413", rec.Code)
	}
	if fake.callCount() != 0 {
		t.Fatalf("oversize reached ocr: calls = %d", fake.callCount())
	}
	docID, _ := w53ReceiptCols(t, rig.fstore, rig.tenant.ID, clientID)
	if docID != nil {
		t.Fatal("oversize attach persisted")
	}
}

func TestW53ReceiptFailClosedAndNotFound(t *testing.T) {
	rig := newTestRig(t)
	clientID := w53AnchorCapture(t, rig)
	fake := &w53FakeOCR{docID: uuid.New()}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	// OCR_INTERNAL_TOKEN unset → 503 (kyc internauth mirror).
	h := &Handlers{Svc: rig.svc, OCR: NewOCRClient(srv.URL, "", nil)}
	if rec := w53ReceiptHTTP(t, h, rig.tenant, clientID, "image/png", w53PNG); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no token = %d, want 503", rec.Code)
	}
	// OCR_SERVICE_URL unset → 503.
	h = &Handlers{Svc: rig.svc, OCR: NewOCRClient("", "w53-token", nil)}
	if rec := w53ReceiptHTTP(t, h, rig.tenant, clientID, "image/png", w53PNG); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no url = %d, want 503", rec.Code)
	}
	// No client at all → 503.
	h = &Handlers{Svc: rig.svc}
	if rec := w53ReceiptHTTP(t, h, rig.tenant, clientID, "image/png", w53PNG); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil client = %d, want 503", rec.Code)
	}
	// Unknown capture id → 404 BEFORE any OCR spend.
	h = &Handlers{Svc: rig.svc, OCR: NewOCRClient(srv.URL, "w53-token", nil)}
	if rec := w53ReceiptHTTP(t, h, rig.tenant, uuid.NewString(), "image/png", w53PNG); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown capture = %d, want 404", rec.Code)
	}
	if fake.callCount() != 0 {
		t.Fatalf("fail-closed/404 paths reached ocr: calls = %d", fake.callCount())
	}
}

func TestW53AdditiveColumnsBackwardCompat(t *testing.T) {
	rig := newTestRig(t)

	// ensureSchema is idempotent: re-running it on the live database (the
	// upgrade path onto a pre-W53 cluster) must succeed.
	if _, err := NewStore(context.Background(), rig.fstore.pool); err != nil {
		t.Fatalf("re-run ensureSchema: %v", err)
	}

	// The pre-W53 insert path (Anchor never names the receipt columns) works
	// unchanged and leaves both columns NULL.
	clientID := w53AnchorCapture(t, rig)
	docID, fields := w53ReceiptCols(t, rig.fstore, rig.tenant.ID, clientID)
	if docID != nil || fields != nil {
		t.Fatalf("fresh anchor has receipt data: doc=%v fields=%s", docID, fields)
	}

	// Replay dedupe + resolve semantics are unaffected by the new columns.
	item := mkItem(clientID, KindCheckin, CheckinPayload{Note: "site visit"}, nil)
	res := rig.svc.Capture(context.Background(), rig.tenant, []CaptureItem{item})
	if len(res) != 1 || res[0].Status != StatusDeduped {
		t.Fatalf("replay after additive columns: %+v", res)
	}

	// A null extracted_fields from A stores SQL NULL (not the literal).
	fake := &w53FakeOCR{docID: uuid.New(), fields: ""}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	h := &Handlers{Svc: rig.svc, OCR: NewOCRClient(srv.URL, "w53-token", nil)}
	if rec := w53ReceiptHTTP(t, h, rig.tenant, clientID, "application/pdf", []byte("%PDF-1.4 fake")); rec.Code != http.StatusOK {
		t.Fatalf("null-fields attach = %d: %s", rec.Code, rec.Body.String())
	}
	docID, fields = w53ReceiptCols(t, rig.fstore, rig.tenant.ID, clientID)
	if docID == nil || *docID != fake.docID || fields != nil {
		t.Fatalf("null-fields attach: doc=%v fields=%s", docID, fields)
	}
}

func TestW53ReceiptRLSIsolation(t *testing.T) {
	rig := newTestRig(t)
	clientID := w53AnchorCapture(t, rig)
	fake := &w53FakeOCR{docID: uuid.New(), fields: `{"vendor":"V","amount":1,"date":"2025-01-01","items":[]}`}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	h := &Handlers{Svc: rig.svc, OCR: NewOCRClient(srv.URL, "w53-token", nil)}

	if rec := w53ReceiptHTTP(t, h, rig.tenant, clientID, "image/png", w53PNG); rec.Code != http.StatusOK {
		t.Fatalf("tenant-A attach = %d", rec.Code)
	}

	other := bookingops.TenantInfo{ID: uuid.New(), Slug: "other"}
	// Another tenant cannot see the anchor (404) — and no OCR call is spent.
	if rec := w53ReceiptHTTP(t, h, other, clientID, "image/png", w53PNG); rec.Code != http.StatusNotFound {
		t.Fatalf("tenant-B attach on A's capture = %d, want 404 (RLS)", rec.Code)
	}
	if fake.callCount() != 1 {
		t.Fatalf("tenant-B attach reached ocr: calls = %d, want 1", fake.callCount())
	}
	// Store-level: CaptureExists / AttachReceipt are RLS-scoped.
	exists, err := rig.fstore.CaptureExists(context.Background(), other.ID, clientID)
	if err != nil || exists {
		t.Fatalf("CaptureExists as tenant B = %v, %v — RLS broken", exists, err)
	}
	if err := rig.fstore.AttachReceipt(context.Background(), other.ID, clientID, uuid.New(), nil, ""); err != ErrNotFound {
		t.Fatalf("AttachReceipt as tenant B = %v, want ErrNotFound", err)
	}
	// Genuine RLS assertion: the harness connects as the postgres SUPERUSER,
	// which bypasses RLS entirely (FORCE applies to the table owner, not
	// superusers) — so probe through a non-privileged role where the forced
	// tenant_isolation policy actually evaluates.
	ctx := context.Background()
	if _, err := rig.fstore.pool.Exec(ctx,
		`CREATE ROLE w53probe LOGIN PASSWORD 'w53probe'`); err != nil {
		t.Fatalf("create probe role: %v", err)
	}
	if _, err := rig.fstore.pool.Exec(ctx,
		`GRANT SELECT ON field_captures TO w53probe`); err != nil {
		t.Fatalf("grant probe role: %v", err)
	}
	probe, err := pgxpool.New(ctx,
		"postgres://w53probe:w53probe@localhost:5562/booking_fieldcapture_test?sslmode=disable")
	if err != nil {
		t.Fatalf("probe pool: %v", err)
	}
	defer probe.Close()
	countWithTenant := func(tenantID uuid.UUID) int {
		tx, err := probe.Begin(ctx)
		if err != nil {
			t.Fatalf("probe begin: %v", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID.String()); err != nil {
			t.Fatalf("probe set tenant: %v", err)
		}
		var n int
		// No tenant filter in the SQL — the RLS policy alone decides.
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM field_captures WHERE receipt_document_id IS NOT NULL`).Scan(&n); err != nil {
			t.Fatalf("probe count: %v", err)
		}
		return n
	}
	if n := countWithTenant(other.ID); n != 0 {
		t.Fatalf("tenant-B sees %d receipt rows via forced RLS — isolation broken", n)
	}
	if n := countWithTenant(rig.tenant.ID); n != 1 {
		t.Fatalf("tenant-A sees %d receipt rows via forced RLS, want 1", n)
	}
	// Tenant A's row is still intact with its receipt.
	docID, _ := w53ReceiptCols(t, rig.fstore, rig.tenant.ID, clientID)
	if docID == nil || *docID != fake.docID {
		t.Fatalf("tenant-A receipt lost after tenant-B attempts: %v", docID)
	}
}
