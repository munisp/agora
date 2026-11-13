package consumer

// SPEC-W61 G2: the media byte-erasure hook on the privacy consumer —
// after a successful subject anonymize, the ocr document_ids bound to the
// subject via media_inbox.sender_phone_digits are erased through the
// internauth ocr endpoint. The ocr HTTP hop is stubbed AT THE CONSUMER
// BOUNDARY ONLY (the ocrDocumentEraser seam; no production-path mocks).
//
// Covered:
//   - happy: both media-linked document ids are erased; an unrelated
//     tenant/phone row is NOT; the contact row is tombstoned (real store).
//   - erase failure on one document is logged + skipped; process still
//     returns nil (erasure NEVER fails on ocr unavailability).
//   - nil eraser (OCR_SERVICE_URL unset) leaves the flow untouched.
//   - mediaPhoneDigits matches the pinned G3 canonical form (conformance
//     fixtures duplicated from the contract — no shared Go module).
//
// Harness: embedded-postgres on dedicated port 5592 (consumer-package
// idiom; -short skips). The media_inbox DDL mirrors the SPEC-W61 pinned
// table (B's ensure* hook converges it at merge — CREATE IF NOT EXISTS).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/opendesk/booking-service/internal/store"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

const w61ConsumerDDL = `
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE TABLE IF NOT EXISTS contacts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL, name TEXT NOT NULL, phone TEXT, email TEXT, notes TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS media_inbox (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL,
    channel              TEXT NOT NULL,
    provider_message_id  TEXT NOT NULL,
    document_id          UUID NOT NULL,
    sender_phone         TEXT NOT NULL,
    sender_phone_digits  TEXT NOT NULL,
    mime                 TEXT NOT NULL,
    bytes                BIGINT NOT NULL,
    sha256_prefix        TEXT NOT NULL,
    state                TEXT NOT NULL DEFAULT 'parked'
        CHECK (state IN ('parked','anchored','linked','discarded')),
    anchor_method        TEXT,
    booking_id           UUID,
    contact_id           UUID,
    linked_by            TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, channel, provider_message_id)
);
ALTER TABLE media_inbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE media_inbox FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON media_inbox;
CREATE POLICY tenant_isolation ON media_inbox
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
`

// stubOCREraser is the consumer-boundary stub (G2): records erase calls,
// fails on demand.
type stubOCREraser struct {
	erased []string
	failOn map[string]bool
	calls  int
}

func (s *stubOCREraser) EraseDocument(_ context.Context, documentID string) error {
	s.calls++
	if s.failOn[documentID] {
		return errors.New("ocr down")
	}
	s.erased = append(s.erased, documentID)
	return nil
}

type w61PrivacyFixture struct {
	t      *testing.T
	st     *store.Store
	conn   *pgx.Conn
	tenant uuid.UUID
}

func newW61PrivacyFixture(t *testing.T) *w61PrivacyFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres W61 privacy hook test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_w61_priv").
		Port(5592).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	dsn := "postgres://postgres:postgres@localhost:5592/booking_w61_priv?sslmode=disable"
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(ctx) }) //nolint:errcheck
	if _, err := conn.Exec(ctx, w61ConsumerDDL); err != nil {
		t.Fatalf("schema: %v", err)
	}
	st, err := store.New(ctx, dsn, 0)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(st.Close)
	return &w61PrivacyFixture{t: t, st: st, conn: conn, tenant: uuid.New()}
}

func (f *w61PrivacyFixture) seed(tenant uuid.UUID, phone, digits string, docIDs ...uuid.UUID) {
	f.t.Helper()
	ctx := context.Background()
	if _, err := f.conn.Exec(ctx,
		`INSERT INTO contacts (tenant_id, name, phone) VALUES ($1,'Ada Lovelace',$2)`,
		tenant, phone); err != nil {
		f.t.Fatalf("seed contact: %v", err)
	}
	for i, docID := range docIDs {
		if _, err := f.conn.Exec(ctx,
			`INSERT INTO media_inbox (tenant_id, channel, provider_message_id, document_id,
			     sender_phone, sender_phone_digits, mime, bytes, sha256_prefix)
			 VALUES ($1,'whatsapp',$2,$3,$4,$5,'image/jpeg',10,'0123456789abcdef')`,
			tenant, fmt.Sprintf("m-%d-%s", i, docID), docID, phone, digits); err != nil {
			f.t.Fatalf("seed media_inbox: %v", err)
		}
	}
}

func (f *w61PrivacyFixture) process(eraser ocrDocumentEraser, phone, tenantID string) error {
	f.t.Helper()
	c := &PrivacyConsumer{store: f.st, log: zap.NewNop(), ocr: eraser}
	body := fmt.Sprintf(`{"id":"evt-w61-1","type":"PrivacyEraseRequested",`+
		`"data":{"phone":%q,"email":"","tenant_id":%q}}`, phone, tenantID)
	return c.process(context.Background(), kafka.Message{Value: []byte(body)})
}

func TestPrivacyEraseErasesMediaDocuments(t *testing.T) {
	f := newW61PrivacyFixture(t)
	doc1, doc2, docOther := uuid.New(), uuid.New(), uuid.New()
	// Two rows bound to the subject (E.164 phone; digits stored canonical);
	// one row for a DIFFERENT subject in the same tenant.
	f.seed(f.tenant, "+234 801 234 5678", "2348012345678", doc1, doc2)
	f.seed(uuid.New(), "+234 801 234 5678", "2348012345678", docOther) // other tenant

	eraser := &stubOCREraser{}
	if err := f.process(eraser, "+234 801 234 5678", f.tenant.String()); err != nil {
		t.Fatalf("process: %v", err)
	}
	if eraser.calls != 2 || len(eraser.erased) != 2 {
		t.Fatalf("want exactly 2 erase calls, got %d (%v)", eraser.calls, eraser.erased)
	}
	got := map[string]bool{eraser.erased[0]: true, eraser.erased[1]: true}
	if !got[doc1.String()] || !got[doc2.String()] || got[docOther.String()] {
		t.Fatalf("erased wrong documents: %v", eraser.erased)
	}
	// The contact row is tombstoned by the real store path.
	var name, phone string
	if err := f.conn.QueryRow(context.Background(),
		`SELECT name, phone FROM contacts WHERE tenant_id=$1`, f.tenant).Scan(&name, &phone); err != nil {
		t.Fatalf("read contact: %v", err)
	}
	if name != "erased" || !strings.HasPrefix(phone, "sha256:") {
		t.Fatalf("contact not tombstoned: name=%q phone=%q", name, phone)
	}
}

func TestPrivacyEraseContinuesOnOCRFailure(t *testing.T) {
	f := newW61PrivacyFixture(t)
	doc1, doc2 := uuid.New(), uuid.New()
	f.seed(f.tenant, "+2348012345678", "2348012345678", doc1, doc2)

	eraser := &stubOCREraser{failOn: map[string]bool{doc2.String(): true}}
	// ocr down for one document: the erasure itself MUST still succeed
	// (rows tombstoned; TTL sweep is the byte backstop).
	if err := f.process(eraser, "+2348012345678", f.tenant.String()); err != nil {
		t.Fatalf("erase failure must never fail the erasure, got %v", err)
	}
	if eraser.calls != 2 || len(eraser.erased) != 1 {
		t.Fatalf("want 2 attempts / 1 success, got %d/%v", eraser.calls, eraser.erased)
	}
}

func TestPrivacyEraseNilEraserIsInert(t *testing.T) {
	f := newW61PrivacyFixture(t)
	f.seed(f.tenant, "+2348012345678", "2348012345678", uuid.New())
	// nil eraser (OCR_SERVICE_URL unset) — the hook is a no-op, the
	// anonymize still runs.
	if err := f.process(nil, "+2348012345678", f.tenant.String()); err != nil {
		t.Fatalf("nil eraser must not fail the erasure, got %v", err)
	}
	var name string
	if err := f.conn.QueryRow(context.Background(),
		`SELECT name FROM contacts WHERE tenant_id=$1`, f.tenant).Scan(&name); err != nil {
		t.Fatalf("read contact: %v", err)
	}
	if name != "erased" {
		t.Fatalf("anonymize must still run with a nil eraser (name=%q)", name)
	}
}

// G3 conformance fixtures (pinned contract, duplicated — no shared Go
// module): digits-only, single leading "00" stripped once, no national
// folding.
func TestMediaPhoneDigitsConformance(t *testing.T) {
	for in, want := range map[string]string{
		"2348012345678":     "2348012345678",
		"+234 801 234 5678": "2348012345678",
		"002348012345678":   "2348012345678",
		"08012345678":       "08012345678",
		"+1 (415) 555-0132": "14155550132",
	} {
		if got := mediaPhoneDigits(in); got != want {
			t.Fatalf("mediaPhoneDigits(%q) = %q, want %q", in, got, want)
		}
	}
}
