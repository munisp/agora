package consumer

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/opendesk/graph-sync/internal/events"
	"github.com/opendesk/graph-sync/internal/graph"
)

// fakeGraph is the in-memory GraphClient fake (SPEC-W28 §4: consumer unit
// tests with an in-memory graph fake). It mirrors the Cypher semantics:
// auto-merge on exact phone_hash, monotonic quarantine, channel union.
type fakeGraph struct {
	mu sync.Mutex

	processed map[string]string // event_id -> tenant_id
	tenants   map[string]string // tenant_id -> slug
	persons   map[string]*graph.Person
	contacts  map[string]*graph.Contact
	consents  map[string]*graph.Consent
	bookings  map[string]*graph.Booking

	// edges: person_id -> set of ids (all tenant-scoped by construction:
	// nodes are keyed tenant|id and lookups always pass tenantID).
	hasContact map[string]map[string]bool
	consented  map[string]map[string]bool
	booked     map[string]map[string]bool
	referred   [][2]string

	embeddings      map[string][]float32 // tenant|person_id -> embedding
	mergeCandidates []mergeCandidate
	enrichment      map[string]enrichmentRecord

	pingErr error
}

type mergeCandidate struct {
	TenantID string
	A, B     string
	Score    float64
}

func newFakeGraph() *fakeGraph {
	return &fakeGraph{
		processed:  map[string]string{},
		tenants:    map[string]string{},
		persons:    map[string]*graph.Person{},
		contacts:   map[string]*graph.Contact{},
		consents:   map[string]*graph.Consent{},
		bookings:   map[string]*graph.Booking{},
		hasContact: map[string]map[string]bool{},
		consented:  map[string]map[string]bool{},
		booked:     map[string]map[string]bool{},
		embeddings: map[string][]float32{},
	}
}

func key(tenantID, id string) string { return tenantID + "|" + id }

func (f *fakeGraph) Ping(context.Context) error { return f.pingErr }
func (f *fakeGraph) Close() error               { return nil }

func (f *fakeGraph) MarkProcessed(_ context.Context, eventID, tenantID string, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if tenantID == "" {
		return false, graph.ErrTenantRequired
	}
	if _, ok := f.processed[eventID]; ok {
		return true, nil
	}
	f.processed[eventID] = tenantID
	return false, nil
}

func (f *fakeGraph) UpsertTenant(_ context.Context, tenantID, slug string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if tenantID == "" {
		return graph.ErrTenantRequired
	}
	if slug != "" {
		f.tenants[tenantID] = slug
	} else if _, ok := f.tenants[tenantID]; !ok {
		f.tenants[tenantID] = ""
	}
	return nil
}

func (f *fakeGraph) UpsertPerson(_ context.Context, p graph.Person) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p.TenantID == "" {
		return "", false, graph.ErrTenantRequired
	}
	target := p.PersonID
	merged := false
	if p.PhoneHash != "" {
		for _, existing := range f.persons {
			if existing.TenantID == p.TenantID && existing.PhoneHash == p.PhoneHash && existing.PersonID != p.PersonID {
				target = existing.PersonID
				merged = true
				break
			}
		}
	}
	k := key(p.TenantID, target)
	existing, ok := f.persons[k]
	if !ok {
		cp := p
		cp.PersonID = target
		cp.Channels = dedupeStrings(p.Channels)
		f.persons[k] = &cp
		return target, merged, nil
	}
	// fold properties (Cypher ON MATCH semantics)
	if p.PhoneHash != "" {
		existing.PhoneHash = p.PhoneHash
	}
	if p.Name != "" {
		existing.Name = p.Name
	}
	if p.ConsentSummary != "" {
		existing.ConsentSummary = p.ConsentSummary
	}
	existing.Quarantine = existing.Quarantine || p.Quarantine // monotonic
	existing.Channels = dedupeStrings(append(existing.Channels, p.Channels...))
	existing.UpdatedAt = p.UpdatedAt
	return target, merged, nil
}

func (f *fakeGraph) UpsertContact(_ context.Context, c graph.Contact, personID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.TenantID == "" {
		return graph.ErrTenantRequired
	}
	cp := c
	f.contacts[key(c.TenantID, c.LeadID)] = &cp
	link(f.hasContact, key(c.TenantID, personID), c.LeadID)
	if _, ok := f.tenants[c.TenantID]; !ok {
		f.tenants[c.TenantID] = ""
	}
	return nil
}

func (f *fakeGraph) LinkConsent(_ context.Context, c graph.Consent, personID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.TenantID == "" {
		return graph.ErrTenantRequired
	}
	k := key(c.TenantID, c.ConsentID)
	if existing, ok := f.consents[k]; ok {
		if c.RevokedAt != nil {
			existing.RevokedAt = c.RevokedAt
		}
	} else {
		cp := c
		f.consents[k] = &cp
	}
	link(f.consented, key(c.TenantID, personID), c.ConsentID)
	return nil
}

func (f *fakeGraph) UpsertBooking(_ context.Context, b graph.Booking, personID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b.TenantID == "" {
		return graph.ErrTenantRequired
	}
	k := key(b.TenantID, b.BookingID)
	if existing, ok := f.bookings[k]; ok {
		existing.Status = b.Status
		if b.Showed != nil {
			existing.Showed = b.Showed
		}
	} else {
		cp := b
		f.bookings[k] = &cp
	}
	link(f.booked, key(b.TenantID, personID), b.BookingID)
	return nil
}

func (f *fakeGraph) LinkReferral(_ context.Context, tenantID, from, to, _ string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if tenantID == "" {
		return graph.ErrTenantRequired
	}
	f.referred = append(f.referred, [2]string{key(tenantID, from), key(tenantID, to)})
	return nil
}

func (f *fakeGraph) PersonCandidates(_ context.Context, tenantID, exclude string, _ int) ([]graph.PersonRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []graph.PersonRef{}
	for k, emb := range f.embeddings {
		p, ok := f.persons[k]
		if !ok || p.TenantID != tenantID || p.PersonID == exclude {
			continue
		}
		out = append(out, graph.PersonRef{PersonID: p.PersonID, Name: p.Name, Embedding: emb})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PersonID < out[j].PersonID })
	return out, nil
}

func (f *fakeGraph) SetPersonEmbedding(_ context.Context, tenantID, personID string, embedding []float32, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.persons[key(tenantID, personID)]; !ok {
		return fmt.Errorf("person not found")
	}
	f.embeddings[key(tenantID, personID)] = embedding
	return nil
}

func (f *fakeGraph) AddMergeCandidate(_ context.Context, tenantID, a, b string, score float64, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if tenantID == "" {
		return graph.ErrTenantRequired
	}
	f.mergeCandidates = append(f.mergeCandidates, mergeCandidate{TenantID: tenantID, A: a, B: b, Score: score})
	return nil
}

// enrichment records applied property maps per tenant|person (the Person
// struct itself is the event-sourced shape; enrichment props are opaque).
type enrichmentRecord struct {
	Props       map[string]any
	SnapshotDay string
}

func (f *fakeGraph) ApplyEnrichment(_ context.Context, tenantID, personID string, props map[string]any, snapshotDay string, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if tenantID == "" {
		return false, graph.ErrTenantRequired
	}
	k := key(tenantID, personID)
	if _, ok := f.persons[k]; !ok {
		return false, nil // unknown/erased person — drop (never resurrect)
	}
	if f.enrichment == nil {
		f.enrichment = map[string]enrichmentRecord{}
	}
	f.enrichment[k] = enrichmentRecord{Props: props, SnapshotDay: snapshotDay}
	return true, nil
}

func (f *fakeGraph) FindPersonByPhoneHash(_ context.Context, tenantID, phoneHash string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.persons {
		if p.TenantID == tenantID && p.PhoneHash == phoneHash {
			return p.PersonID, nil
		}
	}
	return "", nil
}

func (f *fakeGraph) ErasePerson(_ context.Context, tenantID, personID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key(tenantID, personID)
	if _, ok := f.persons[k]; !ok {
		return false, nil
	}
	// DETACH DELETE the Person subgraph: Person + its Consents + Contacts.
	for consentID := range f.consented[k] {
		delete(f.consents, key(tenantID, consentID))
	}
	for leadID := range f.hasContact[k] {
		delete(f.contacts, key(tenantID, leadID))
	}
	delete(f.consented, k)
	delete(f.hasContact, k)
	delete(f.booked, k)
	delete(f.embeddings, k)
	delete(f.enrichment, k)
	delete(f.persons, k)
	kept := f.mergeCandidates[:0]
	for _, mc := range f.mergeCandidates {
		if !(mc.TenantID == tenantID && (mc.A == personID || mc.B == personID)) {
			kept = append(kept, mc)
		}
	}
	f.mergeCandidates = kept
	return true, nil
}

func link(m map[string]map[string]bool, from, to string) {
	if m[from] == nil {
		m[from] = map[string]bool{}
	}
	m[from][to] = true
}

func dedupeStrings(in []string) []string {
	out := in[:0]
	seen := map[string]bool{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// fakeAudit records published audit events.
type fakeAudit struct {
	mu     sync.Mutex
	events []publishedEvent
	err    error
}

type publishedEvent struct {
	Topic string
	Key   string
	Evt   events.CloudEvent
}

func (f *fakeAudit) Publish(_ context.Context, topic, key string, evt events.CloudEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.events = append(f.events, publishedEvent{Topic: topic, Key: key, Evt: evt})
	return nil
}

// fakeEmbedder is a scripted Embedder.
type fakeEmbedder struct {
	vectors map[string][]float32 // input text -> vector
	err     error
	calls   int
}

func (f *fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if v, ok := f.vectors[text]; ok {
		return v, nil
	}
	return []float32{1, 0, 0}, nil
}

func (f *fakeEmbedder) Degraded() bool { return f.err != nil }
