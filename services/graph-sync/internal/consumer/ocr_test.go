package consumer

// OCR hash-only Document stub projection tests (SPEC-W55 Scope A): Document
// nodes carry ids + hashes + metadata ONLY (tenant_id, document_id, kind,
// status, sha256, phash, page_count, model_version, created_at) — the
// payload's extracted_fields/storage_* junk decodes harmlessly and is NEVER
// persisted; the ProcessedEvent marker makes redelivery a no-op; non-
// "extracted" statuses are ack-skipped; missing ids are poison.

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/opendesk/graph-sync/internal/events"
	"github.com/stretchr/testify/require"
)

func ocrEvent(id, tenantID, typ string, data map[string]any) events.CloudEvent {
	return events.CloudEvent{
		SpecVersion: "1.0",
		ID:          id,
		Source:      "ocr-service",
		Type:        typ,
		Subject:     "doc-00000000-0000-0000-0000-0000000000a1",
		Time:        time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC),
		TenantID:    tenantID,
		Data:        data,
	}
}

// ocrData mirrors the ocr-service DocumentExtracted payload (app/events.py
// document_extracted_event) — INCLUDING the extracted_fields / storage_*
// junk the hash-only projection must ignore (never decode/persist/log).
func ocrData() map[string]any {
	return map[string]any{
		"document_id":   "doc-00000000-0000-0000-0000-0000000000a1",
		"tenant_id":     "tenant-1",
		"kind":          "receipt",
		"status":        "extracted",
		"page_count":    2,
		"sha256":        "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		"phash":         "a1b2c3d4e5f60708",
		"model_version": "ocr-v1.3",
		// PII/storage junk the projection must NEVER touch (SPEC-W55).
		"storage_bucket":   "tenant-1-ocr",
		"storage_key":      "tenant-1/doc-...a1/receipt.jpg",
		"extracted_fields": map[string]any{"vendor": "ACME Stores", "amount": 12500.5},
	}
}

func TestOCRDocumentExtracted_Projected_TenantTagged_AllFields(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()

	require.NoError(t, s.HandleOCR(ctx, ocrEvent("ocr-1", "tenant-1", events.TypeOCRDocumentExtracted, ocrData())))

	doc := g.documents[key("tenant-1", "doc-00000000-0000-0000-0000-0000000000a1")]
	require.NotNil(t, doc, "Document stub projected")
	require.Equal(t, "tenant-1", doc.TenantID, "tenant_id mandatory on the Document node")
	require.Equal(t, "doc-00000000-0000-0000-0000-0000000000a1", doc.DocumentID)
	require.Equal(t, "receipt", doc.Kind)
	require.Equal(t, "extracted", doc.Status)
	require.Equal(t, "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", doc.SHA256)
	require.Equal(t, "a1b2c3d4e5f60708", doc.PHash)
	require.Equal(t, 2, doc.PageCount)
	require.Equal(t, "ocr-v1.3", doc.ModelVersion)
	require.Equal(t, time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC), doc.CreatedAt,
		"created_at comes from the CloudEvent time")
	_, ok := g.tenants["tenant-1"]
	require.True(t, ok, "tenant anchor exists")
	require.Contains(t, s.Metrics.Render(), `graph_sync_counter{name="ocr_documents_projected"} 1`)
}

func TestOCRDocumentExtracted_HashOnly_JunkNeverProjected(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()

	// The payload junk (extracted_fields/storage_bucket/storage_key) decodes
	// fine — OCRDocumentData simply does not declare those fields.
	require.NoError(t, s.HandleOCR(ctx, ocrEvent("ocr-1", "tenant-1", events.TypeOCRDocumentExtracted, ocrData())))

	doc := g.documents[key("tenant-1", "doc-00000000-0000-0000-0000-0000000000a1")]
	require.NotNil(t, doc)
	// Pin the EXACT projected field set (SPEC-W55 + SPEC-W67): nothing
	// PII-shaped can even exist on the stub. SeenElsewhereCount (W67) is
	// the ONLY cross-tenant-derived datum — a count, never an identity —
	// and it is set ONLY via SetDocumentSeenElsewhere from the pinned
	// match event, never from DocumentExtracted.
	var names []string
	for i := 0; i < reflect.TypeOf(*doc).NumField(); i++ {
		names = append(names, reflect.TypeOf(*doc).Field(i).Name)
	}
	require.ElementsMatch(t, []string{
		"DocumentID", "TenantID", "Kind", "Status", "SHA256", "PHash",
		"ModelVersion", "PageCount", "CreatedAt", "SeenElsewhereCount",
	}, names, "Document stub carries exactly the pinned hash-only field set (+ W67 count)")
	require.Zero(t, doc.SeenElsewhereCount,
		"DocumentExtracted never sets the W67 cross-tenant count")
	// The junk values must not leak into any projected property.
	for _, v := range []string{doc.DocumentID, doc.TenantID, doc.Kind, doc.Status, doc.SHA256, doc.PHash, doc.ModelVersion} {
		require.NotContains(t, v, "ACME Stores")
		require.NotContains(t, v, "tenant-1-ocr")
	}
	require.Empty(t, g.persons, "no person linkage on Document stubs")
}

func TestOCR_IdempotentReplay(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()
	evt := ocrEvent("ocr-dup", "tenant-1", events.TypeOCRDocumentExtracted, ocrData())
	require.NoError(t, s.HandleOCR(ctx, evt))
	require.NoError(t, s.HandleOCR(ctx, evt)) // redelivery
	require.Len(t, g.documents, 1, "redelivery is a no-op (processed marker)")
	require.Contains(t, s.Metrics.Render(), `graph_sync_counter{name="events_duplicate"} 1`)
}

func TestOCR_UnknownType_Acked(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	require.NoError(t, s.HandleOCR(context.Background(),
		ocrEvent("ocr-x", "tenant-1", "com.opendesk.ocr.FutureThing", ocrData())))
	require.Empty(t, g.documents, "unknown types are acknowledged and skipped")
	require.Empty(t, g.processed, "unknown types never consume the idempotency marker")
}

func TestOCR_MissingDocumentID_IsPoison(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	data := ocrData()
	delete(data, "document_id")
	err := s.HandleOCR(context.Background(), ocrEvent("ocr-1", "tenant-1", events.TypeOCRDocumentExtracted, data))
	require.Error(t, err)
	require.ErrorIs(t, err, errPermanent)
	require.Empty(t, g.documents)
}

func TestOCR_MissingSHA256_IsPoison(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	data := ocrData()
	delete(data, "sha256")
	err := s.HandleOCR(context.Background(), ocrEvent("ocr-1", "tenant-1", events.TypeOCRDocumentExtracted, data))
	require.Error(t, err)
	require.ErrorIs(t, err, errPermanent, "sha256 is the D9 exact-duplicate key — blank is poison")
	require.Empty(t, g.documents)
}

func TestOCR_StatusNotExtracted_AckSkipped(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	data := ocrData()
	data["status"] = "failed"
	require.NoError(t, s.HandleOCR(context.Background(), ocrEvent("ocr-1", "tenant-1", events.TypeOCRDocumentExtracted, data)))
	require.Empty(t, g.documents, "non-extracted statuses are ack-skipped, never projected")
}

func TestOCR_TenantFallback_DataTenantID(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	// No tenantid extension on the envelope — the event payload's
	// data.tenant_id carries the tenant (producer sets both).
	evt := ocrEvent("ocr-1", "", events.TypeOCRDocumentExtracted, ocrData())
	require.NoError(t, s.HandleOCR(context.Background(), evt))
	doc := g.documents[key("tenant-1", "doc-00000000-0000-0000-0000-0000000000a1")]
	require.NotNil(t, doc, "tenant falls back to data.tenant_id")
	require.Equal(t, "tenant-1", doc.TenantID)
}

func TestOCR_MissingTenant_IsPoison(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	data := ocrData()
	delete(data, "tenant_id")
	err := s.HandleOCR(context.Background(), ocrEvent("ocr-1", "", events.TypeOCRDocumentExtracted, data))
	require.Error(t, err)
	require.ErrorIs(t, err, errPermanent, "tenant-less OCR events are poison (gate 1)")
	require.Empty(t, g.documents)
}

func TestOCR_TenantWipeDropsDocuments(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()
	require.NoError(t, s.HandleOCR(ctx, ocrEvent("ocr-1", "tenant-1", events.TypeOCRDocumentExtracted, ocrData())))
	found, err := g.DeleteTenantSubgraph(ctx, "tenant-1")
	require.NoError(t, err)
	require.True(t, found)
	require.Empty(t, g.documents, "Document stubs die with the tenant (RECON-W55 §5.4)")
}

func TestOCR_TenantIsolation_SameDocumentIDDifferentTenants(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()
	require.NoError(t, s.HandleOCR(ctx, ocrEvent("ocr-a", "tenant-1", events.TypeOCRDocumentExtracted, ocrData())))
	data := ocrData()
	data["tenant_id"] = "tenant-2"
	require.NoError(t, s.HandleOCR(ctx, ocrEvent("ocr-b", "tenant-2", events.TypeOCRDocumentExtracted, data)))
	require.NotNil(t, g.documents[key("tenant-1", "doc-00000000-0000-0000-0000-0000000000a1")])
	require.NotNil(t, g.documents[key("tenant-2", "doc-00000000-0000-0000-0000-0000000000a1")])
	require.Len(t, g.documents, 2, "Document stubs are MERGEd per (tenant_id, document_id)")
}

// ---------------------------------------------------------------------------
// SPEC-W67 G6: com.opendesk.ocr.CrossTenantDocumentMatch projection.
// The PINNED payload is EXACTLY {document_id, sha256, kind, tenant_count} —
// the LATER tenant's own identifiers plus ONE integer. The consumer decodes
// it under a STRICT allowlist (extra keys => dead-letter), projects the
// additive seen_elsewhere_count with MAX semantics onto the tenant's OWN
// Document node, and never crashes on malformed/unknown events.
// ---------------------------------------------------------------------------

// matchData mirrors the PINNED SPEC-W67 payload EXACTLY (4 keys, nothing
// else). ocr-service emits it post-commit after a registry match.
func matchData() map[string]any {
	return map[string]any{
		"document_id":  "doc-00000000-0000-0000-0000-0000000000a1",
		"sha256":       "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		"kind":         "receipt",
		"tenant_count": 3,
	}
}

func TestCrossTenantMatch_ProjectedOnOwnDocument(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()
	// The tenant's own Document stub already exists (DocumentExtracted ran).
	require.NoError(t, s.HandleOCR(ctx, ocrEvent("ocr-1", "tenant-1", events.TypeOCRDocumentExtracted, ocrData())))

	require.NoError(t, s.HandleOCR(ctx, ocrEvent("ocr-match-1", "tenant-1", events.TypeOCRCrossTenantMatch, matchData())))

	doc := g.documents[key("tenant-1", "doc-00000000-0000-0000-0000-0000000000a1")]
	require.NotNil(t, doc)
	require.Equal(t, 3, doc.SeenElsewhereCount, "tenant_count projected onto the tenant's OWN Document node")
	// The match projection is additive: the hash-only fields are untouched.
	require.Equal(t, "receipt", doc.Kind)
	require.Equal(t, "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", doc.SHA256)
	require.Contains(t, s.Metrics.Render(), `graph_sync_counter{name="ocr_cross_tenant_matches_projected"} 1`)
}

func TestCrossTenantMatch_OutOfOrderBeforeExtract_StillLands(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()
	// Match event arrives BEFORE DocumentExtracted (redelivery/backfill
	// ordering): the stub is MERGEd so the count is not lost, and the later
	// extract projection does NOT clear it (additive property).
	require.NoError(t, s.HandleOCR(ctx, ocrEvent("ocr-match-1", "tenant-1", events.TypeOCRCrossTenantMatch, matchData())))
	doc := g.documents[key("tenant-1", "doc-00000000-0000-0000-0000-0000000000a1")]
	require.NotNil(t, doc, "MERGE creates the stub when the match precedes the extract")
	require.Equal(t, 3, doc.SeenElsewhereCount)

	require.NoError(t, s.HandleOCR(ctx, ocrEvent("ocr-1", "tenant-1", events.TypeOCRDocumentExtracted, ocrData())))
	doc = g.documents[key("tenant-1", "doc-00000000-0000-0000-0000-0000000000a1")]
	require.Equal(t, "receipt", doc.Kind, "extract projection fills the hash-only fields")
	require.Equal(t, 3, doc.SeenElsewhereCount, "extract projection never clears the W67 count")
}

func TestCrossTenantMatch_IdempotentRedelivery_AndMaxSemantics(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()
	require.NoError(t, s.HandleOCR(ctx, ocrEvent("ocr-1", "tenant-1", events.TypeOCRDocumentExtracted, ocrData())))
	k := key("tenant-1", "doc-00000000-0000-0000-0000-0000000000a1")

	evt := ocrEvent("ocr-match-1", "tenant-1", events.TypeOCRCrossTenantMatch, matchData())
	require.NoError(t, s.HandleOCR(ctx, evt))
	require.NoError(t, s.HandleOCR(ctx, evt)) // exact redelivery (same event id)
	require.Equal(t, 3, g.documents[k].SeenElsewhereCount, "same tenant_count re-SET is a no-op")
	require.Contains(t, s.Metrics.Render(), `graph_sync_counter{name="events_duplicate"} 1`)

	higher := matchData()
	higher["tenant_count"] = 5
	require.NoError(t, s.HandleOCR(ctx, ocrEvent("ocr-match-2", "tenant-1", events.TypeOCRCrossTenantMatch, higher)))
	require.Equal(t, 5, g.documents[k].SeenElsewhereCount, "a HIGHER tenant_count updates")

	lower := matchData()
	lower["tenant_count"] = 2
	require.NoError(t, s.HandleOCR(ctx, ocrEvent("ocr-match-3", "tenant-1", events.TypeOCRCrossTenantMatch, lower)))
	require.Equal(t, 5, g.documents[k].SeenElsewhereCount, "a LOWER (stale) tenant_count never regresses (MAX semantics)")
}

func TestCrossTenantMatch_StrictAllowlist_ExtraKeyIsPoison(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	for _, extra := range []string{"phash", "storage_key", "extracted_fields", "other_tenant_id", "tenant_ids", "first_seen"} {
		data := matchData()
		data[extra] = "disallowed"
		err := s.HandleOCR(context.Background(), ocrEvent("ocr-match-x-"+extra, "tenant-1", events.TypeOCRCrossTenantMatch, data))
		require.Error(t, err, "extra key %q rejects the event", extra)
		require.ErrorIs(t, err, errPermanent, "extra key %q => dead-letter posture", extra)
	}
	require.Empty(t, g.documents, "rejected payloads are never projected")
}

func TestCrossTenantMatch_MissingRequiredKey_IsPoison(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	for _, missing := range []string{"document_id", "sha256", "kind", "tenant_count"} {
		data := matchData()
		delete(data, missing)
		err := s.HandleOCR(context.Background(), ocrEvent("ocr-match-m-"+missing, "tenant-1", events.TypeOCRCrossTenantMatch, data))
		require.Error(t, err, "missing key %q rejects the event", missing)
		require.ErrorIs(t, err, errPermanent)
	}
	require.Empty(t, g.documents)
}

func TestCrossTenantMatch_BadValues_ArePoison(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()

	blank := matchData()
	blank["document_id"] = "  "
	err := s.HandleOCR(ctx, ocrEvent("ocr-match-x1", "tenant-1", events.TypeOCRCrossTenantMatch, blank))
	require.ErrorIs(t, err, errPermanent, "blank document_id is poison")

	noHash := matchData()
	noHash["sha256"] = ""
	err = s.HandleOCR(ctx, ocrEvent("ocr-match-x2", "tenant-1", events.TypeOCRCrossTenantMatch, noHash))
	require.ErrorIs(t, err, errPermanent, "blank sha256 is poison")

	zeroCount := matchData()
	zeroCount["tenant_count"] = 0
	err = s.HandleOCR(ctx, ocrEvent("ocr-match-x3", "tenant-1", events.TypeOCRCrossTenantMatch, zeroCount))
	require.ErrorIs(t, err, errPermanent, "tenant_count must be a positive integer")

	floatCount := matchData()
	floatCount["tenant_count"] = 2.5
	err = s.HandleOCR(ctx, ocrEvent("ocr-match-x4", "tenant-1", events.TypeOCRCrossTenantMatch, floatCount))
	require.ErrorIs(t, err, errPermanent, "non-integer tenant_count is poison")

	strCount := matchData()
	strCount["tenant_count"] = "3"
	err = s.HandleOCR(ctx, ocrEvent("ocr-match-x5", "tenant-1", events.TypeOCRCrossTenantMatch, strCount))
	require.ErrorIs(t, err, errPermanent, "stringly tenant_count is poison")
	require.Empty(t, g.documents, "no poison payload projected anything")
}

func TestCrossTenantMatch_MissingTenant_IsPoison(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	// The pinned payload has NO tenant_id key — the CloudEvents tenantid
	// extension is the only tenant carrier. Without it the event can never
	// be written (SPEC §5 gate 1).
	err := s.HandleOCR(context.Background(), ocrEvent("ocr-match-x", "", events.TypeOCRCrossTenantMatch, matchData()))
	require.Error(t, err)
	require.ErrorIs(t, err, errPermanent)
	require.Empty(t, g.documents)
}

func TestCrossTenantMatch_TenantIsolation_CountNeverCrosses(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()
	// Both tenants hold the same document bytes; the match event addressed
	// to tenant-2 (the LATER tenant) touches ONLY tenant-2's node.
	require.NoError(t, s.HandleOCR(ctx, ocrEvent("ocr-a", "tenant-1", events.TypeOCRDocumentExtracted, ocrData())))
	data := ocrData()
	data["tenant_id"] = "tenant-2"
	require.NoError(t, s.HandleOCR(ctx, ocrEvent("ocr-b", "tenant-2", events.TypeOCRDocumentExtracted, data)))

	require.NoError(t, s.HandleOCR(ctx, ocrEvent("ocr-match-1", "tenant-2", events.TypeOCRCrossTenantMatch, matchData())))

	earlier := g.documents[key("tenant-1", "doc-00000000-0000-0000-0000-0000000000a1")]
	later := g.documents[key("tenant-2", "doc-00000000-0000-0000-0000-0000000000a1")]
	require.Zero(t, earlier.SeenElsewhereCount, "the EARLIER tenant's node is unchanged (first-use-wins; it learns NOTHING)")
	require.Equal(t, 3, later.SeenElsewhereCount, "only the LATER tenant's own node carries the count")
}
