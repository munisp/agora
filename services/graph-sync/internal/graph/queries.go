// Cypher builders for the graph-sync write model. These are PURE functions
// (query string + parameters) so unit tests can assert the schema invariants
// — tenant_id on every node/edge, hashed phones only — without a live
// FalkorDB. The FalkorDB client (falkordb.go) executes them over the Redis
// protocol.
package graph

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// q is a parameterized Cypher statement.
type q struct {
	text   string
	params map[string]any
}

// markProcessedQuery: MERGE the idempotency marker; the returned
// processed_at equals $now IFF the marker was newly created (an existing
// marker keeps its original timestamp) — that distinguishes first
// processing from a duplicate without a separate read.
func markProcessedQuery(eventID, tenantID string, at time.Time) q {
	return q{
		text: `MERGE (m:ProcessedEvent {event_id: $event_id})
ON CREATE SET m.tenant_id = $tenant_id, m.processed_at = $now
RETURN m.processed_at AS processed_at`,
		params: map[string]any{
			"event_id":  eventID,
			"tenant_id": tenantID,
			"now":       FormatTime(at),
		},
	}
}

func upsertTenantQuery(tenantID, slug string, at time.Time) q {
	return q{
		text: `MERGE (t:Tenant {tenant_id: $tenant_id})
SET t.slug = CASE WHEN $slug <> '' THEN $slug ELSE t.slug END,
    t.updated_at = $now`,
		params: map[string]any{"tenant_id": tenantID, "slug": slug, "now": FormatTime(at)},
	}
}

// matchPersonByPhoneHashQuery finds the auto-merge target: exact phone_hash
// match within the SAME tenant (SPEC §4 — the ONLY auto-merge rule).
func matchPersonByPhoneHashQuery(tenantID, phoneHash string) q {
	return q{
		text: `MATCH (p:Person {tenant_id: $tenant_id, phone_hash: $phone_hash})
RETURN p.person_id AS person_id`,
		params: map[string]any{"tenant_id": tenantID, "phone_hash": phoneHash},
	}
}

// personChannelsQuery reads the current channels list for the Go-side union
// (kept out of Cypher to avoid list-comprehension dialect risk).
func personChannelsQuery(tenantID, personID string) q {
	return q{
		text: `MATCH (p:Person {tenant_id: $tenant_id, person_id: $person_id})
RETURN p.channels AS channels`,
		params: map[string]any{"tenant_id": tenantID, "person_id": personID},
	}
}

// upsertPersonQuery upserts the Person by (tenant_id, person_id) — the
// caller has already resolved auto-merge adoption. channels is the Go-side
// union of existing + incoming channels. quarantine is monotonic: once
// quarantined, a later non-quarantine event never clears the flag (SPEC §5
// gate 4 propagation).
func upsertPersonQuery(p Person, channels []string) q {
	return q{
		text: `MERGE (p:Person {tenant_id: $tenant_id, person_id: $person_id})
ON CREATE SET p.created_at = $now
SET p.updated_at = $now,
    p.phone_hash = CASE WHEN $phone_hash <> '' THEN $phone_hash ELSE p.phone_hash END,
    p.name = CASE WHEN $name <> '' THEN $name ELSE p.name END,
    p.consent_summary = CASE WHEN $consent_summary <> '' THEN $consent_summary ELSE p.consent_summary END,
    p.quarantine = coalesce(p.quarantine, false) OR $quarantine,
    p.channels = $channels
RETURN p.person_id AS person_id`,
		params: map[string]any{
			"tenant_id":       p.TenantID,
			"person_id":       p.PersonID,
			"phone_hash":      p.PhoneHash,
			"name":            p.Name,
			"consent_summary": p.ConsentSummary,
			"quarantine":      p.Quarantine,
			"channels":        channels,
			"now":             FormatTime(p.UpdatedAt),
		},
	}
}

// upsertContactQuery upserts the Contact and wires HAS_CONTACT + PART_OF.
// tenant_id is set on BOTH edges (belt and braces, §5 gate 1).
func upsertContactQuery(c Contact, personID, tenantSlug string) q {
	text := `MERGE (c:Contact {tenant_id: $tenant_id, lead_id: $lead_id})
SET c.channel_of_first_touch = $channel, c.source = $source,
    c.captured_at = $captured_at, c.updated_at = $now`
	params := map[string]any{
		"tenant_id":   c.TenantID,
		"tenant_slug": tenantSlug,
		"lead_id":     c.LeadID,
		"person_id":   personID,
		"channel":     c.ChannelOfFirstTouch,
		"source":      c.Source,
		"captured_at": FormatTime(c.CapturedAt),
		"now":         FormatTime(time.Now()),
	}
	// SPEC-W30: optional staff/agent attribution for fraud detectors
	// (D2/D3/D4). Set only when the source event carried it — no empty
	// properties on the node.
	if strings.TrimSpace(c.CapturedBy) != "" {
		text += `, c.captured_by = $captured_by`
		params["captured_by"] = strings.TrimSpace(c.CapturedBy)
	}
	text += `
WITH c
MATCH (p:Person {tenant_id: $tenant_id, person_id: $person_id})
MERGE (p)-[h:HAS_CONTACT]->(c)
SET h.tenant_id = $tenant_id
WITH c
MERGE (t:Tenant {tenant_id: $tenant_id})
ON CREATE SET t.slug = $tenant_slug
MERGE (c)-[pt:PART_OF]->(t)
SET pt.tenant_id = $tenant_id`
	return q{text: text, params: params}
}

// captureLocationQuery wires (Contact)-[:CAPTURED_AT]->(Location) (only
// issued when the contact carries geo).
func captureLocationQuery(c Contact) q {
	return q{
		text: `MATCH (c:Contact {tenant_id: $tenant_id, lead_id: $lead_id})
MERGE (l:Location {tenant_id: $tenant_id, lga: $lga, ward: $ward})
SET l.lat = $lat, l.lon = $lon, l.updated_at = $now
MERGE (c)-[r:CAPTURED_AT]->(l)
SET r.tenant_id = $tenant_id`,
		params: map[string]any{
			"tenant_id": c.TenantID,
			"lead_id":   c.LeadID,
			"lga":       c.LGA,
			"ward":      c.Ward,
			"lat":       c.Lat,
			"lon":       c.Lon,
			"now":       FormatTime(time.Now()),
		},
	}
}

// linkConsentQuery upserts the Consent node and the CONSENTED edge;
// revocation stamps revoked_at on the node (the edge keeps its grant `at`).
func linkConsentQuery(c Consent, personID string) q {
	revoked := ""
	if c.RevokedAt != nil {
		revoked = FormatTime(*c.RevokedAt)
	}
	return q{
		text: `MERGE (s:Consent {tenant_id: $tenant_id, consent_id: $consent_id})
SET s.purpose = $purpose, s.granted_at = $granted_at, s.proof_ref = $proof_ref,
    s.revoked_at = CASE WHEN $revoked_at <> '' THEN $revoked_at ELSE s.revoked_at END,
    s.updated_at = $now
WITH s
MATCH (p:Person {tenant_id: $tenant_id, person_id: $person_id})
MERGE (p)-[r:CONSENTED]->(s)
SET r.tenant_id = $tenant_id, r.purpose = $purpose, r.at = $granted_at`,
		params: map[string]any{
			"tenant_id":  c.TenantID,
			"consent_id": c.ConsentID,
			"person_id":  personID,
			"purpose":    c.Purpose,
			"granted_at": FormatTime(c.GrantedAt),
			"revoked_at": revoked,
			"proof_ref":  c.ProofRef,
			"now":        FormatTime(time.Now()),
		},
	}
}

// upsertBookingQuery upserts Booking (+ Offering) and wires BOOKED + FOR.
func upsertBookingQuery(b Booking, personID string) q {
	text := `MERGE (b:Booking {tenant_id: $tenant_id, booking_id: $booking_id})
ON CREATE SET b.created_at = $created_at
SET b.status = $status, b.offering_id = $offering_id, b.updated_at = $now`
	params := map[string]any{
		"tenant_id":     b.TenantID,
		"booking_id":    b.BookingID,
		"person_id":     personID,
		"status":        b.Status,
		"offering_id":   b.OfferingID,
		"offering_name": b.OfferingName,
		"created_at":    FormatTime(b.CreatedAt),
		"now":           FormatTime(time.Now()),
	}
	if b.Showed != nil {
		text += `, b.showed = $showed`
		params["showed"] = *b.Showed
	}
	// SPEC-W30: optional staff attribution + cancellation timestamp for
	// ghost-booking detection (D6). Set only when present.
	if strings.TrimSpace(b.CreatedBy) != "" {
		text += `, b.created_by = $created_by`
		params["created_by"] = strings.TrimSpace(b.CreatedBy)
	}
	if b.CancelledAt != nil {
		text += `, b.cancelled_at = $cancelled_at`
		params["cancelled_at"] = FormatTime(*b.CancelledAt)
	}
	text += `
WITH b
MATCH (p:Person {tenant_id: $tenant_id, person_id: $person_id})
MERGE (p)-[r:BOOKED]->(b)
SET r.tenant_id = $tenant_id, r.at = $created_at
WITH b
MERGE (o:Offering {tenant_id: $tenant_id, offering_id: $offering_id})
SET o.name = CASE WHEN $offering_name <> '' THEN $offering_name ELSE o.name END,
    o.updated_at = $now
MERGE (b)-[f:FOR]->(o)
SET f.tenant_id = $tenant_id`
	return q{text: text, params: params}
}

// linkReferralQuery wires the W14-A referral tree edge.
func linkReferralQuery(tenantID, fromPersonID, toPersonID, program string, at time.Time) q {
	return q{
		text: `MATCH (a:Person {tenant_id: $tenant_id, person_id: $from_id}),
      (b:Person {tenant_id: $tenant_id, person_id: $to_id})
MERGE (a)-[r:REFERRED]->(b)
SET r.tenant_id = $tenant_id, r.program = $program, r.at = $at`,
		params: map[string]any{
			"tenant_id": tenantID,
			"from_id":   fromPersonID,
			"to_id":     toPersonID,
			"program":   program,
			"at":        FormatTime(at),
		},
	}
}

// personCandidatesQuery returns the embedding-carrying persons of one tenant
// (the similarity candidate pool). Same-tenant scoping is mandatory (SPEC
// §4: "AND same tenant").
func personCandidatesQuery(tenantID, excludePersonID string, limit int) q {
	return q{
		text: `MATCH (p:Person {tenant_id: $tenant_id})
WHERE p.person_id <> $exclude AND p.name_embedding IS NOT NULL
RETURN p.person_id AS person_id, p.name AS name, p.name_embedding AS embedding
LIMIT $limit`,
		params: map[string]any{"tenant_id": tenantID, "exclude": excludePersonID, "limit": limit},
	}
}

func setPersonEmbeddingQuery(tenantID, personID string, embedding []float32, at time.Time) q {
	return q{
		text: `MATCH (p:Person {tenant_id: $tenant_id, person_id: $person_id})
SET p.name_embedding = $embedding, p.updated_at = $now`,
		params: map[string]any{
			"tenant_id": tenantID, "person_id": personID,
			"embedding": embedding, "now": FormatTime(at),
		},
	}
}

// addMergeCandidateQuery records the merge PROPOSAL edge (never a merge).
func addMergeCandidateQuery(tenantID, aID, bID string, score float64, at time.Time) q {
	return q{
		text: `MATCH (a:Person {tenant_id: $tenant_id, person_id: $a_id}),
      (b:Person {tenant_id: $tenant_id, person_id: $b_id})
MERGE (a)-[m:MERGE_CANDIDATE]->(b)
SET m.tenant_id = $tenant_id, m.score = $score, m.created_at = $now`,
		params: map[string]any{
			"tenant_id": tenantID, "a_id": aID, "b_id": bID,
			"score": score, "now": FormatTime(at),
		},
	}
}

// applyEnrichmentQuery applies the lakehouse property map onto an EXISTING
// Person (MATCH, never MERGE — enrichment must not create/resurrect nodes,
// docs/graph.md §4). count(p) reports whether the node existed.
func applyEnrichmentQuery(tenantID, personID string, props map[string]any, snapshotDay string, at time.Time) q {
	return q{
		text: `MATCH (p:Person {tenant_id: $tenant_id, person_id: $person_id})
SET p += $props, p.enrichment_snapshot_day = $snapshot_day, p.updated_at = $now
RETURN count(p) AS n`,
		params: map[string]any{
			"tenant_id":    tenantID,
			"person_id":    personID,
			"props":        props,
			"snapshot_day": snapshotDay,
			"now":          FormatTime(at),
		},
	}
}

// upsertCaseQuery upserts the civic Case node (SPEC-W32 §3 WS-D; PII-free)
// and wires (Person)-[:REPORTED]->(Case) when personID is non-empty. Every
// node and edge carries tenant_id (SPEC-W28 §5 gate 1). Status/category are
// refreshed on re-delivery (MERGE idempotency).
func upsertCaseQuery(c Case, personID string) q {
	text := `MERGE (cs:Case {tenant_id: $tenant_id, case_id: $case_id})
ON CREATE SET cs.created_at = $created_at
SET cs.category = CASE WHEN $category <> '' THEN $category ELSE cs.category END,
    cs.status = CASE WHEN $status <> '' THEN $status ELSE cs.status END,
    cs.ward = CASE WHEN $ward <> '' THEN $ward ELSE cs.ward END,
    cs.updated_at = $now`
	params := map[string]any{
		"tenant_id":  c.TenantID,
		"case_id":    c.Ref,
		"person_id":  personID,
		"category":   c.Category,
		"status":     c.Status,
		"ward":       c.Ward,
		"created_at": FormatTime(c.CreatedAt),
		"now":        FormatTime(time.Now()),
	}
	if personID != "" {
		text += `
WITH cs
MATCH (p:Person {tenant_id: $tenant_id, person_id: $person_id})
MERGE (p)-[r:REPORTED]->(cs)
SET r.tenant_id = $tenant_id`
	}
	return q{text: text, params: params}
}

// caseLocationQuery wires (Case)-[:AT]->(Location) (only issued when the
// case carries geo). The Location MERGE key mirrors captureLocationQuery
// ({tenant_id, lga, ward}); lat/lon are SET as properties.
func caseLocationQuery(c Case) q {
	return q{
		text: `MATCH (cs:Case {tenant_id: $tenant_id, case_id: $case_id})
MERGE (l:Location {tenant_id: $tenant_id, lga: $lga, ward: $ward})
SET l.lat = $lat, l.lon = $lon, l.updated_at = $now
MERGE (cs)-[r:AT]->(l)
SET r.tenant_id = $tenant_id`,
		params: map[string]any{
			"tenant_id": c.TenantID,
			"case_id":   c.Ref,
			"lga":       c.LGA,
			"ward":      c.Ward,
			"lat":       c.Lat,
			"lon":       c.Lon,
			"now":       FormatTime(time.Now()),
		},
	}
}

// setCaseStatusQuery mirrors the civic status (StatusChanged). acked_at /
// resolved_at are stamped only when the event carried them (empty string
// keeps the existing property — CASE-guarded).
func setCaseStatusQuery(tenantID, ref, status string, ackedAt, resolvedAt *time.Time, at time.Time) q {
	acked, resolved := "", ""
	if ackedAt != nil {
		acked = FormatTime(*ackedAt)
	}
	if resolvedAt != nil {
		resolved = FormatTime(*resolvedAt)
	}
	return q{
		text: `MERGE (cs:Case {tenant_id: $tenant_id, case_id: $case_id})
SET cs.status = $status,
    cs.acked_at = CASE WHEN $acked_at <> '' THEN $acked_at ELSE cs.acked_at END,
    cs.resolved_at = CASE WHEN $resolved_at <> '' THEN $resolved_at ELSE cs.resolved_at END,
    cs.updated_at = $now`,
		params: map[string]any{
			"tenant_id":   tenantID,
			"case_id":     ref,
			"status":      status,
			"acked_at":    acked,
			"resolved_at": resolved,
			"now":         FormatTime(at),
		},
	}
}

// linkCaseMergedQuery wires (Case)-[:MERGED_INTO]->(canonical Case). The
// canonical node is MERGEd (stub) so an out-of-order Merged event that
// precedes the canonical ReportReceived is still safe; cs.merged_into keeps
// the pointer on the merged case itself.
func linkCaseMergedQuery(tenantID, ref, canonicalRef string, at time.Time) q {
	return q{
		text: `MERGE (cs:Case {tenant_id: $tenant_id, case_id: $case_id})
MERGE (cn:Case {tenant_id: $tenant_id, case_id: $canonical_id})
MERGE (cs)-[m:MERGED_INTO]->(cn)
SET m.tenant_id = $tenant_id, m.at = $now,
    cs.merged_into = $canonical_id, cs.updated_at = $now`,
		params: map[string]any{
			"tenant_id":    tenantID,
			"case_id":      ref,
			"canonical_id": canonicalRef,
			"now":          FormatTime(at),
		},
	}
}

// personExistsQuery is the pre-delete check (erasure must be idempotent and
// report found/not-found for the audit event).
func personExistsQuery(tenantID, personID string) q {
	return q{
		text: `MATCH (p:Person {tenant_id: $tenant_id, person_id: $person_id})
RETURN count(p) AS n`,
		params: map[string]any{"tenant_id": tenantID, "person_id": personID},
	}
}

// erasePersonQuery DETACH DELETEs the Person subgraph: Person + its Consent
// + its Contact nodes (all edges of deleted nodes go with them). Bookings
// and Offerings remain as transactional records.
func erasePersonQuery(tenantID, personID string) q {
	return q{
		text: `MATCH (p:Person {tenant_id: $tenant_id, person_id: $person_id})
OPTIONAL MATCH (p)-[:HAS_CONTACT]->(c:Contact {tenant_id: $tenant_id})
OPTIONAL MATCH (p)-[:CONSENTED]->(s:Consent {tenant_id: $tenant_id})
DETACH DELETE p, c, s`,
		params: map[string]any{"tenant_id": tenantID, "person_id": personID},
	}
}

// ---------------------------------------------------------------------------
// Cypher literal encoding for the GRAPH.QUERY `CYPHER k=v ...` parameter
// prefix (values are embedded as Cypher literals — strings single-quoted and
// escaped, so event data can never break out of the parameter position).
// ---------------------------------------------------------------------------

// cypherParamsPrefix renders `CYPHER k=v k2=v2 ` for parameterized
// GRAPH.QUERY execution. Deterministic key order (sorted) for testability.
func cypherParamsPrefix(params map[string]any) string {
	if len(params) == 0 {
		return ""
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("CYPHER ")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s ", k, cypherLiteral(params[k]))
	}
	return b.String()
}

// cypherLiteral encodes a Go value as a Cypher literal.
func cypherLiteral(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return "'" + strings.ReplaceAll(strings.ReplaceAll(t, `\`, `\\`), `'`, `\'`) + "'"
	case bool:
		if t {
			return "true"
		}
		return "false"
	case int:
		return fmt.Sprintf("%d", t)
	case int64:
		return fmt.Sprintf("%d", t)
	case float32:
		return strconv.FormatFloat(float64(t), 'g', -1, 32)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case []string:
		parts := make([]string, len(t))
		for i, s := range t {
			parts[i] = cypherLiteral(s)
		}
		return "[" + strings.Join(parts, ",") + "]"
	case []float32:
		parts := make([]string, len(t))
		for i, f := range t {
			parts[i] = strconv.FormatFloat(float64(f), 'g', -1, 32)
		}
		return "[" + strings.Join(parts, ",") + "]"
	case []float64:
		parts := make([]string, len(t))
		for i, f := range t {
			parts[i] = strconv.FormatFloat(f, 'g', -1, 64)
		}
		return "[" + strings.Join(parts, ",") + "]"
	case map[string]any:
		// Cypher map literal: keys must be bare identifiers (used for the
		// `SET p += $props` enrichment apply) — keys that are not valid
		// identifiers are skipped defensively (they could never be read
		// back as properties anyway).
		keys := make([]string, 0, len(t))
		for k := range t {
			if isCypherIdent(k) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + ": " + cypherLiteral(t[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return cypherLiteral(fmt.Sprint(v))
	}
}

// isCypherIdent reports whether s is usable as a bare Cypher identifier.
func isCypherIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		ok := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9')
		if !ok {
			return false
		}
	}
	return true
}
