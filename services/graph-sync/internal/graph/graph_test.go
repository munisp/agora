package graph

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPhoneHash_DeterministicAndNormalized(t *testing.T) {
	h1 := PhoneHash("salt", "t1", "+234 803 555 0101")
	h2 := PhoneHash("salt", "t1", "+2348035550101")
	require.Equal(t, h1, h2, "formatting-insensitive normalization")
	require.Len(t, h1, 64, "sha256 hex")
	require.NotContains(t, h1, "234", "raw digits never leak into the hash")

	require.NotEqual(t, h1, PhoneHash("salt", "t2", "+2348035550101"),
		"tenant is part of the hash input (not joinable across tenants)")
	require.NotEqual(t, h1, PhoneHash("other-salt", "t1", "+2348035550101"),
		"salt changes the hash")
}

func TestCosine(t *testing.T) {
	require.InDelta(t, 1.0, Cosine([]float32{1, 0}, []float32{1, 0}), 1e-9)
	require.InDelta(t, 0.0, Cosine([]float32{1, 0}, []float32{0, 1}), 1e-9)
	require.InDelta(t, 1/math.Sqrt2, Cosine([]float32{1, 1}, []float32{1, 0}), 1e-9)
	require.InDelta(t, 0.9999, Cosine([]float32{0.99, 0.01}, []float32{0.98, 0.02}), 1e-3,
		"near-identical embeddings clear the 0.92 merge threshold")
	require.Equal(t, 0.0, Cosine(nil, []float32{1}), "empty vectors score 0")
	require.Equal(t, 0.0, Cosine([]float32{1}, []float32{1, 2}), "length mismatch scores 0")
	require.Equal(t, 0.0, Cosine([]float32{0, 0}, []float32{1, 1}), "zero norm scores 0")
}

func TestFormatTime_AlwaysUTC(t *testing.T) {
	withOffset := time.Date(2026, 8, 5, 10, 0, 0, 0, time.FixedZone("WAT", 3600))
	require.Equal(t, "2026-08-05T09:00:00Z", FormatTime(withOffset))
	require.Equal(t, "", FormatTime(time.Time{}))
}

// TestQueries_TenantIDOnEveryStatement is the static side of SPEC-W28 §5
// gate 1: every Cypher statement this service can emit is tenant-scoped.
func TestQueries_TenantIDOnEveryStatement(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	showed := true
	revoked := now
	stmts := []q{
		markProcessedQuery("e1", "t1", now),
		upsertTenantQuery("t1", "acme", now),
		matchPersonByPhoneHashQuery("t1", "h"),
		personChannelsQuery("t1", "p1"),
		upsertPersonQuery(Person{PersonID: "p1", TenantID: "t1", PhoneHash: "h", Name: "n", UpdatedAt: now}, []string{"web"}),
		upsertContactQuery(Contact{LeadID: "l1", TenantID: "t1", CapturedAt: now}, "p1", "acme"),
		captureLocationQuery(Contact{LeadID: "l1", TenantID: "t1", LGA: "Ikeja"}),
		linkConsentQuery(Consent{ConsentID: "c1", TenantID: "t1", Purpose: "marketing", GrantedAt: now, RevokedAt: &revoked}, "p1"),
		upsertBookingQuery(Booking{BookingID: "b1", TenantID: "t1", CreatedAt: now, Showed: &showed}, "p1"),
		linkReferralQuery("t1", "p1", "p2", "prog", now),
		personCandidatesQuery("t1", "p1", 100),
		setPersonEmbeddingQuery("t1", "p1", []float32{0.1, 0.2}, now),
		addMergeCandidateQuery("t1", "p1", "p2", 0.95, now),
		personExistsQuery("t1", "p1"),
		erasePersonQuery("t1", "p1"),
		applyEnrichmentQuery("t1", "p1", map[string]any{"ltv_cents": 1000}, "2026-08-06", now),
	}
	for i, stmt := range stmts {
		require.Contains(t, stmt.text, "tenant_id", "statement %d must be tenant-scoped", i)
		require.Equal(t, "t1", stmt.params["tenant_id"], "statement %d tenant param", i)
	}
}

// TestEraseQuery_DetachDelete pins the erasure shape (SPEC §4: DETACH
// DELETE the Person subgraph).
func TestEraseQuery_DetachDelete(t *testing.T) {
	stmt := erasePersonQuery("t1", "p1")
	require.Contains(t, stmt.text, "DETACH DELETE")
	require.Contains(t, stmt.text, "Consent")
	require.Contains(t, stmt.text, "Contact")
}

// TestMarkProcessedQuery_OnCreateTimestamp pins the idempotency mechanism:
// the marker keeps its original timestamp on re-MERGE, so returning
// processed_at == $now identifies first processing.
func TestMarkProcessedQuery_OnCreateTimestamp(t *testing.T) {
	stmt := markProcessedQuery("e1", "t1", time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC))
	require.Contains(t, stmt.text, "ON CREATE SET")
	require.Contains(t, stmt.text, "RETURN m.processed_at")
	require.Equal(t, "2026-08-05T12:00:00Z", stmt.params["now"])
}

func TestCypherLiteral_Escaping(t *testing.T) {
	require.Equal(t, "'plain'", cypherLiteral("plain"))
	require.Equal(t, `'o\'brien'`, cypherLiteral("o'brien"))
	require.Equal(t, `'a\\b'`, cypherLiteral(`a\b`))
	require.Equal(t, "true", cypherLiteral(true))
	require.Equal(t, "42", cypherLiteral(42))
	require.Equal(t, "0.92", cypherLiteral(0.92))
	require.Equal(t, "['a','b']", cypherLiteral([]string{"a", "b"}))
	require.Equal(t, "[0.1,0.2]", cypherLiteral([]float32{0.1, 0.2}))
	require.Equal(t, "null", cypherLiteral(nil))
	require.Equal(t, "{ltv_cents: 1000, no_show_rate: 0.14}",
		cypherLiteral(map[string]any{"ltv_cents": 1000, "no_show_rate": 0.14}),
		"map literals sorted by key")
	require.Equal(t, "{}", cypherLiteral(map[string]any{"bad key!": 1, "1x": 2}),
		"non-identifier map keys are skipped defensively")
}

// TestApplyEnrichmentQuery_MatchNeverMerge pins the no-resurrect contract
// (docs/graph.md §4): enrichment applies via MATCH, so unknown/erased
// persons match nothing and are dropped by the caller.
func TestApplyEnrichmentQuery_MatchNeverMerge(t *testing.T) {
	stmt := applyEnrichmentQuery("t1", "p1", map[string]any{"ltv_cents": 1000}, "2026-08-06",
		time.Date(2026, 8, 6, 2, 0, 0, 0, time.UTC))
	require.NotContains(t, stmt.text, "MERGE")
	require.Contains(t, stmt.text, "MATCH (p:Person {tenant_id: $tenant_id, person_id: $person_id})")
	require.Contains(t, stmt.text, "SET p += $props")
	require.Equal(t, "2026-08-06", stmt.params["snapshot_day"])
}

func TestCypherParamsPrefix_Deterministic(t *testing.T) {
	prefix := cypherParamsPrefix(map[string]any{"b": 1, "a": "x"})
	require.Equal(t, "CYPHER a='x' b=1 ", prefix)
	require.True(t, strings.HasPrefix(prefix, "CYPHER "))
}

func TestParseQueryResult(t *testing.T) {
	// non-compact GRAPH.QUERY reply: [header, records, stats]
	reply := []any{
		[]any{[]any{int64(1), "person_id"}, []any{int64(1), "processed_at"}},
		[]any{[]any{"p1", "2026-08-05T12:00:00Z"}},
		[]any{"Cached execution: 0", "Query internal execution time: 0.5 milliseconds"},
	}
	rows, err := parseQueryResult(reply)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "p1", rows[0]["person_id"])
	require.Equal(t, "2026-08-05T12:00:00Z", rows[0]["processed_at"])
}
