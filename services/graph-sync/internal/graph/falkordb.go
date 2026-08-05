// FalkorDB client: executes the pure Cypher builders (queries.go) over the
// Redis protocol with go-redis (the documented alternative to
// falkordb-go — FalkorDB speaks RESP; GRAPH.QUERY carries the parameterized
// Cypher). All reads/writes go through the Client interface so tests run
// against an in-memory fake.
package graph

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// commander is the minimal redis surface used (Do + Ping); *redis.Client
// satisfies it.
type commander interface {
	Do(ctx context.Context, args ...interface{}) *redis.Cmd
	Ping(ctx context.Context) *redis.StatusCmd
}

// FalkorDB is a Client backed by FalkorDB over the Redis protocol.
type FalkorDB struct {
	rdb   commander
	graph string
}

// NewFalkorDB dials FalkorDB at addr (host:port) and selects the graph.
func NewFalkorDB(addr, graphName string) *FalkorDB {
	return &FalkorDB{
		rdb:   redis.NewClient(&redis.Options{Addr: addr}),
		graph: graphName,
	}
}

// NewFalkorDBWith builds a client over an arbitrary commander (tests can
// inject a scripted fake).
func NewFalkorDBWith(rdb commander, graphName string) *FalkorDB {
	return &FalkorDB{rdb: rdb, graph: graphName}
}

// Ping checks store liveness.
func (f *FalkorDB) Ping(ctx context.Context) error { return f.rdb.Ping(ctx).Err() }

// Close releases the connection (no-op for injected Cmdables that are not
// *redis.Client).
func (f *FalkorDB) Close() error {
	if c, ok := f.rdb.(*redis.Client); ok {
		return c.Close()
	}
	return nil
}

// query executes one parameterized Cypher statement and returns the records
// as column-name → value maps. GRAPH.QUERY's non-compact reply is
// [header, records, statistics]: header cells are [type, name], records are
// rows of scalar values (we only project scalars/lists — never raw nodes).
func (f *FalkorDB) query(ctx context.Context, statement q) ([]map[string]any, error) {
	full := cypherParamsPrefix(statement.params) + statement.text
	res, err := f.rdb.Do(ctx, "GRAPH.QUERY", f.graph, full).Result()
	if err != nil {
		return nil, fmt.Errorf("graph query: %w", err)
	}
	return parseQueryResult(res)
}

// exec runs a statement whose records are ignored (pure writes).
func (f *FalkorDB) exec(ctx context.Context, statement q) error {
	_, err := f.query(ctx, statement)
	return err
}

// parseQueryResult decodes the non-compact GRAPH.QUERY reply.
func parseQueryResult(res any) ([]map[string]any, error) {
	top, ok := res.([]any)
	if !ok || len(top) < 2 {
		return nil, fmt.Errorf("unexpected GRAPH.QUERY reply shape %T", res)
	}
	header, ok := top[0].([]any)
	if !ok {
		return nil, fmt.Errorf("unexpected GRAPH.QUERY header shape %T", top[0])
	}
	cols := make([]string, 0, len(header))
	for _, h := range header {
		cell, ok := h.([]any)
		if !ok || len(cell) != 2 {
			return nil, fmt.Errorf("unexpected GRAPH.QUERY header cell %v", h)
		}
		name, _ := cell[1].(string)
		cols = append(cols, name)
	}
	rows, ok := top[1].([]any)
	if !ok {
		// write-only queries return [header, [], stats] or nil records
		return nil, nil
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		rec, ok := r.([]any)
		if !ok {
			continue
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			if i < len(rec) {
				row[c] = rec[i]
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Client implementation
// ---------------------------------------------------------------------------

// MarkProcessed implements Client (see interface doc).
func (f *FalkorDB) MarkProcessed(ctx context.Context, eventID, tenantID string, at time.Time) (bool, error) {
	if eventID == "" {
		return false, fmt.Errorf("event_id is required for the processed marker")
	}
	statement := markProcessedQuery(eventID, tenantID, at)
	rows, err := f.query(ctx, statement)
	if err != nil {
		return false, err
	}
	want := statement.params["now"].(string)
	for _, row := range rows {
		if s, _ := row["processed_at"].(string); s == want {
			return false, nil // marker newly created by THIS call
		}
	}
	return true, nil // marker pre-existed with an older timestamp
}

// UpsertTenant implements Client.
func (f *FalkorDB) UpsertTenant(ctx context.Context, tenantID, slug string, at time.Time) error {
	if tenantID == "" {
		return ErrTenantRequired
	}
	return f.exec(ctx, upsertTenantQuery(tenantID, slug, at))
}

// UpsertPerson implements Client: exact phone_hash match in the same tenant
// adopts the existing node (auto-merge, SPEC §4); the upsert then folds the
// properties onto it.
func (f *FalkorDB) UpsertPerson(ctx context.Context, p Person) (string, bool, error) {
	if p.TenantID == "" {
		return "", false, ErrTenantRequired
	}
	if p.PersonID == "" {
		return "", false, fmt.Errorf("person_id is required")
	}
	target := p.PersonID
	merged := false
	if p.PhoneHash != "" {
		rows, err := f.query(ctx, matchPersonByPhoneHashQuery(p.TenantID, p.PhoneHash))
		if err != nil {
			return "", false, err
		}
		for _, row := range rows {
			if id, _ := row["person_id"].(string); id != "" && id != p.PersonID {
				target = id
				merged = true
				break
			}
		}
	}
	// Channel union on the Go side (existing channels + incoming).
	channels := append([]string{}, p.Channels...)
	if existing, err := f.query(ctx, personChannelsQuery(p.TenantID, target)); err == nil {
		for _, row := range existing {
			for _, ch := range toStringSlice(row["channels"]) {
				if !contains(channels, ch) {
					channels = append(channels, ch)
				}
			}
		}
	}
	p.PersonID = target
	if err := f.exec(ctx, upsertPersonQuery(p, channels)); err != nil {
		return "", false, err
	}
	return target, merged, nil
}

// UpsertContact implements Client.
func (f *FalkorDB) UpsertContact(ctx context.Context, c Contact, personID string) error {
	if c.TenantID == "" {
		return ErrTenantRequired
	}
	if c.LeadID == "" {
		return fmt.Errorf("lead_id is required")
	}
	if personID == "" {
		return fmt.Errorf("person_id is required for HAS_CONTACT")
	}
	if err := f.exec(ctx, upsertContactQuery(c, personID, "")); err != nil {
		return err
	}
	if c.HasGeo || c.LGA != "" || c.Ward != "" {
		return f.exec(ctx, captureLocationQuery(c))
	}
	return nil
}

// LinkConsent implements Client.
func (f *FalkorDB) LinkConsent(ctx context.Context, c Consent, personID string) error {
	if c.TenantID == "" {
		return ErrTenantRequired
	}
	if c.ConsentID == "" || personID == "" {
		return fmt.Errorf("consent_id and person_id are required")
	}
	return f.exec(ctx, linkConsentQuery(c, personID))
}

// UpsertBooking implements Client.
func (f *FalkorDB) UpsertBooking(ctx context.Context, b Booking, personID string) error {
	if b.TenantID == "" {
		return ErrTenantRequired
	}
	if b.BookingID == "" || personID == "" {
		return fmt.Errorf("booking_id and person_id are required")
	}
	return f.exec(ctx, upsertBookingQuery(b, personID))
}

// LinkReferral implements Client.
func (f *FalkorDB) LinkReferral(ctx context.Context, tenantID, fromPersonID, toPersonID, program string, at time.Time) error {
	if tenantID == "" {
		return ErrTenantRequired
	}
	if fromPersonID == "" || toPersonID == "" || fromPersonID == toPersonID {
		return fmt.Errorf("referral requires two distinct person ids")
	}
	return f.exec(ctx, linkReferralQuery(tenantID, fromPersonID, toPersonID, program, at))
}

// PersonCandidates implements Client.
func (f *FalkorDB) PersonCandidates(ctx context.Context, tenantID, excludePersonID string, limit int) ([]PersonRef, error) {
	if tenantID == "" {
		return nil, ErrTenantRequired
	}
	if limit <= 0 {
		limit = 500
	}
	rows, err := f.query(ctx, personCandidatesQuery(tenantID, excludePersonID, limit))
	if err != nil {
		return nil, err
	}
	out := make([]PersonRef, 0, len(rows))
	for _, row := range rows {
		out = append(out, PersonRef{
			PersonID:  str(row["person_id"]),
			Name:      str(row["name"]),
			Embedding: toFloat32Slice(row["embedding"]),
		})
	}
	return out, nil
}

// SetPersonEmbedding implements Client.
func (f *FalkorDB) SetPersonEmbedding(ctx context.Context, tenantID, personID string, embedding []float32, at time.Time) error {
	if tenantID == "" {
		return ErrTenantRequired
	}
	if personID == "" || len(embedding) == 0 {
		return fmt.Errorf("person_id and embedding are required")
	}
	return f.exec(ctx, setPersonEmbeddingQuery(tenantID, personID, embedding, at))
}

// AddMergeCandidate implements Client.
func (f *FalkorDB) AddMergeCandidate(ctx context.Context, tenantID, personAID, personBID string, score float64, at time.Time) error {
	if tenantID == "" {
		return ErrTenantRequired
	}
	if personAID == "" || personBID == "" || personAID == personBID {
		return fmt.Errorf("merge candidate requires two distinct person ids")
	}
	return f.exec(ctx, addMergeCandidateQuery(tenantID, personAID, personBID, score, at))
}

// ApplyEnrichment implements Client (MATCH-then-SET; unknown/erased
// persons report applied=false).
func (f *FalkorDB) ApplyEnrichment(ctx context.Context, tenantID, personID string, props map[string]any, snapshotDay string, at time.Time) (bool, error) {
	if tenantID == "" {
		return false, ErrTenantRequired
	}
	if personID == "" {
		return false, fmt.Errorf("person_id is required")
	}
	if len(props) == 0 {
		return false, fmt.Errorf("enrichment carries no properties")
	}
	rows, err := f.query(ctx, applyEnrichmentQuery(tenantID, personID, props, snapshotDay, at))
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if toInt64(row["n"]) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// FindPersonByPhoneHash implements Client.
func (f *FalkorDB) FindPersonByPhoneHash(ctx context.Context, tenantID, phoneHash string) (string, error) {
	if tenantID == "" {
		return "", ErrTenantRequired
	}
	if phoneHash == "" {
		return "", nil
	}
	rows, err := f.query(ctx, matchPersonByPhoneHashQuery(tenantID, phoneHash))
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		if id := str(row["person_id"]); id != "" {
			return id, nil
		}
	}
	return "", nil
}

// ErasePerson implements Client (idempotent: pre-check reports found).
func (f *FalkorDB) ErasePerson(ctx context.Context, tenantID, personID string) (bool, error) {
	if tenantID == "" {
		return false, ErrTenantRequired
	}
	if personID == "" {
		return false, fmt.Errorf("person_id is required")
	}
	rows, err := f.query(ctx, personExistsQuery(tenantID, personID))
	if err != nil {
		return false, err
	}
	found := false
	for _, row := range rows {
		if toInt64(row["n"]) > 0 {
			found = true
		}
	}
	if !found {
		return false, nil
	}
	if err := f.exec(ctx, erasePersonQuery(tenantID, personID)); err != nil {
		return false, err
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// reply coercion helpers
// ---------------------------------------------------------------------------

func str(v any) string {
	s, _ := v.(string)
	return s
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	case string:
		var out int64
		_, _ = fmt.Sscanf(n, "%d", &out)
		return out
	}
	return 0
}

func toStringSlice(v any) []string {
	l, ok := v.([]any)
	if !ok {
		if s, ok := v.(string); ok && s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(l))
	for _, e := range l {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func toFloat32Slice(v any) []float32 {
	l, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]float32, 0, len(l))
	for _, e := range l {
		switch n := e.(type) {
		case float64:
			out = append(out, float32(n))
		case int64:
			out = append(out, float32(n))
		case string:
			var f float64
			if _, err := fmt.Sscanf(n, "%g", &f); err == nil {
				out = append(out, float32(f))
			}
		}
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.EqualFold(s, needle) {
			return true
		}
	}
	return false
}
