// Package graph defines the tenant knowledge-graph write model (SPEC-W28 §3)
// and the GraphClient seam. All graph access goes through Cypher over the
// Redis protocol (FalkorDB); the Client interface is implemented by the
// FalkorDB client (falkordb.go) and by an in-memory fake in tests.
//
// Schema v1 invariants enforced here:
//   - tenant_id is mandatory on every node AND every edge (belt and braces,
//     SPEC-W28 §5 gate 1).
//   - phones are stored ONLY as phone_hash — SHA-256(salt|tenant|phone),
//     the same salted-hash posture as the leads dedupe scheme
//     (booking-service/internal/leads DedupeKey). Raw phone PII never
//     touches the graph.
//   - name is the only plaintext PII in the graph and is deleted on erasure.
package graph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"time"
)

// Client is the graph write/read seam (SPEC-W28 §4: "abstract behind a
// GraphClient interface so tests use a fake"). Every method is tenant-scoped:
// an empty TenantID is a programming error and returns ErrTenantRequired.
type Client interface {
	// Ping checks graph-store liveness (used by /healthz).
	Ping(ctx context.Context) error
	// Close releases the underlying connection.
	Close() error

	// MarkProcessed is the idempotency marker (SPEC-W28 §4: SET processed
	// marker; skip duplicates — mirrors the W24 consumer patterns). It
	// returns already=true when the event_id was processed before; on
	// already=true the caller MUST skip the event entirely.
	MarkProcessed(ctx context.Context, eventID, tenantID string, at time.Time) (already bool, err error)

	// UpsertTenant anchors the Tenant node (single node per tenant).
	UpsertTenant(ctx context.Context, tenantID, slug string, at time.Time) error

	// UpsertPerson idempotently upserts a Person. Entity resolution (SPEC
	// §4): an exact phone_hash match against a DIFFERENT person_id in the
	// same tenant auto-merges — the properties are folded onto the existing
	// node and its person_id is returned with merged=true. The returned id
	// is the node all subsequent edges MUST attach to.
	UpsertPerson(ctx context.Context, p Person) (personID string, merged bool, err error)

	// UpsertContact upserts a Contact node and wires
	// (Person)-[:HAS_CONTACT]->(Contact)-[:PART_OF]->(Tenant), plus
	// (Contact)-[:CAPTURED_AT]->(Location) when the contact carries geo.
	UpsertContact(ctx context.Context, c Contact, personID string) error

	// LinkConsent upserts a Consent node and the
	// (Person)-[:CONSENTED {purpose, at}]->(Consent) edge; a non-nil
	// RevokedAt stamps revoked_at (revocation).
	LinkConsent(ctx context.Context, c Consent, personID string) error

	// UpsertBooking upserts a Booking (+ Offering) and wires
	// (Person)-[:BOOKED]->(Booking)-[:FOR]->(Offering).
	UpsertBooking(ctx context.Context, b Booking, personID string) error

	// LinkReferral wires (Person)-[:REFERRED {at, program}]->(Person)
	// (referral tree, W14-A seam). Both persons must exist in the tenant.
	LinkReferral(ctx context.Context, tenantID, fromPersonID, toPersonID, program string, at time.Time) error

	// PersonCandidates returns persons of the tenant that carry a stored
	// name_embedding (entity-resolution candidate pool), excluding
	// excludePersonID. Used only for embedding-similarity merge proposals.
	PersonCandidates(ctx context.Context, tenantID, excludePersonID string, limit int) ([]PersonRef, error)

	// SetPersonEmbedding stores the nomic-embed-text embedding used for
	// entity resolution on the Person node.
	SetPersonEmbedding(ctx context.Context, tenantID, personID string, embedding []float32, at time.Time) error

	// AddMergeCandidate records a MERGE_CANDIDATE edge between two persons
	// of the same tenant (SPEC §4: embedding similarity ≥ 0.92 proposes a
	// merge — it NEVER auto-merges).
	AddMergeCandidate(ctx context.Context, tenantID, personAID, personBID string, score float64, at time.Time) error

	// ApplyEnrichment SETs the nightly lakehouse enrichment properties
	// (graph_enrichment.py: bookings_total/ltv_cents/no_show_rate/
	// cac_channel_ngn_30d/propensity_*/... — the map is applied opaquely)
	// onto the matching Person node, tenant-scoped. It NEVER creates the
	// node: unknown or erased persons are dropped (applied=false) — the
	// graph is event-sourced, enrichment must not resurrect nodes
	// (docs/graph.md §4).
	ApplyEnrichment(ctx context.Context, tenantID, personID string, props map[string]any, snapshotDay string, at time.Time) (applied bool, err error)

	// FindPersonByPhoneHash resolves the person_id of an exact phone_hash
	// match within the tenant ("" when none) — used by phone-only erasure
	// tombstones.
	FindPersonByPhoneHash(ctx context.Context, tenantID, phoneHash string) (string, error)

	// ErasePerson DETACH DELETEs the Person subgraph for tenant+person:
	// the Person node, its Consent nodes and its Contact nodes (with all
	// their edges). Bookings/Offerings are kept as transactional records
	// (the BOOKED edge is removed with the Person). Returns found=false
	// when no such person existed (erasure is idempotent).
	ErasePerson(ctx context.Context, tenantID, personID string) (found bool, err error)
}

// ErrTenantRequired is returned when a graph write carries no tenant_id
// (SPEC-W28 §5 gate 1: tenant_id mandatory on every node).
var ErrTenantRequired = fmt.Errorf("tenant_id is required on every graph node")

// Person mirrors the §3 Person node.
type Person struct {
	PersonID       string
	TenantID       string
	PhoneHash      string   // sha256 hex — NEVER the raw phone
	Name           string   // only plaintext PII in the graph (deleted on erasure)
	Channels       []string // sms|voice|whatsapp|email|web|field
	ConsentSummary string   // compact summary, e.g. "marketing,reminders"
	Quarantine     bool     // imported, consent-unverified (§5 gate 4)
	UpdatedAt      time.Time
}

// PersonRef is the entity-resolution candidate projection.
type PersonRef struct {
	PersonID  string
	Name      string
	Embedding []float32
}

// Contact mirrors the §3 Contact node (lead capture: field PWA / web / import).
type Contact struct {
	LeadID              string
	TenantID            string
	ChannelOfFirstTouch string
	Source              string
	CapturedAt          time.Time
	// CapturedBy is optional staff/agent attribution (SPEC-W30 fraud
	// detectors D2/D3/D4). Populated only when the source event carries
	// agent_id / staff_id / captured_by; empty when upstream omits it.
	CapturedBy string
	// Geo (CAPTURED_AT Location): optional. HasGeo=false skips the Location.
	LGA, Ward string
	Lat, Lon  float64
	HasGeo    bool
}

// Consent mirrors the §3 Consent node.
type Consent struct {
	ConsentID string
	TenantID  string
	Purpose   string
	GrantedAt time.Time
	RevokedAt *time.Time
	ProofRef  string
}

// Booking mirrors the §3 Booking node.
type Booking struct {
	BookingID    string
	TenantID     string
	Status       string
	OfferingID   string
	OfferingName string
	CreatedAt    time.Time
	Showed       *bool
	// CreatedBy is optional staff attribution (SPEC-W30 detector D6);
	// CancelledAt is stamped on booking.cancelled events (D6 flash
	// create->cancel detection).
	CreatedBy   string
	CancelledAt *time.Time
}

// PhoneHash computes the salted SHA-256 phone hash (SPEC-W28 §3: "same
// SHA-256+salt scheme as leads dedupe" — cf. booking-service
// internal/leads.DedupeKey). Canonical form: lowercase hex of
// sha256(salt|tenant_id|digits(phone)). The tenant is part of the input so
// hashes are not joinable across tenants even with the same salt.
func PhoneHash(salt, tenantID, phone string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s",
		salt, tenantID, normalizePhone(phone))))
	return hex.EncodeToString(sum[:])
}

// normalizePhone reduces a phone to digits only (leading +, spaces, dashes
// stripped) — matching the leads dedupe normalization posture
// (lower(trim(...)) is a no-op on digits).
func normalizePhone(phone string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(phone)) {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Cosine returns the cosine similarity of two embedding vectors (0 when
// either is empty/degenerate; SPEC §4 threshold: ≥ 0.92).
func Cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		fa, fb := float64(a[i]), float64(b[i])
		dot += fa * fb
		na += fa * fa
		nb += fb * fb
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// UTC normalizes a timestamp for storage: every graph timestamp is UTC
// RFC3339Nano (dual-TZ safety — offsets in incoming events never leak into
// the graph).
func UTC(t time.Time) time.Time { return t.UTC() }

// FormatTime renders a stored timestamp (UTC RFC3339Nano).
func FormatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
