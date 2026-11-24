package search

// SPEC-W119 §3.1: the projection syncer — a DB-polling projector that keeps
// the bookings/contacts/leads OpenSearch indices in (eventually consistent)
// sync with Postgres.
//
// Why polling instead of event emission: contacts and leads write paths
// emit no lifecycle events today; a poller is self-healing (watermark/hash
// resume after any outage), uniformly catches GDPR erasure tombstones
// (UPDATEs caught by the watermark/hash) and hard deletes (contact hash
// anti-join purge), and needs zero write-path churn.
//
// Tenant safety (dual layer — see internal/store/search_sync.go header):
// every fetch carries an explicit tenant_id predicate AND runs inside
// store.withTenant (app.tenant_id GUC) so the RLS policy is a second
// enforcement layer, never the only one. Tenants are enumerated from the
// sites registry (the one intentionally non-RLS tenant-keyed table).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/opendesk/booking-service/internal/store"
)

// Index names — concrete indices created by infra/opensearch/setup-indices.sh.
const (
	BookingsIndex = "bookings"
	ContactsIndex = "contacts"
	LeadsIndex    = "leads"
)

// DefaultBatchSize is the per-sweep fetch page (SPEC-W119 §3.1: LIMIT 500).
const DefaultBatchSize = 500

// entity names for search_sync_state rows.
const (
	entityBookings = "bookings"
	entityContacts = "contacts" // hash-diff; no watermark row is written
	entityLeads    = "leads"
)

// Response is the HTTP-facing result: raw _source docs + total.
type Response struct {
	Hits  []json.RawMessage `json:"hits"`
	Total int64             `json:"total"`
}

// Handlers implements httpapi.Searcher against a live cluster.
type Handlers struct {
	OS *Client
}

func (h *Handlers) SearchBookings(ctx context.Context, tenantID string, p Params) (Response, error) {
	hits, total, err := h.OS.Search(ctx, BookingsIndex, BuildBookingQuery(tenantID, p))
	return Response{Hits: hits, Total: total}, err
}

func (h *Handlers) SearchContacts(ctx context.Context, tenantID string, p Params) (Response, error) {
	hits, total, err := h.OS.Search(ctx, ContactsIndex, BuildContactQuery(tenantID, p))
	return Response{Hits: hits, Total: total}, err
}

func (h *Handlers) SearchLeads(ctx context.Context, tenantID string, p Params) (Response, error) {
	hits, total, err := h.OS.Search(ctx, LeadsIndex, BuildLeadQuery(tenantID, p))
	return Response{Hits: hits, Total: total}, err
}

// SyncStore is the store surface the syncer needs (satisfied by
// *store.Store; an interface so sync tests can fake the persistence side
// without a database).
type SyncStore interface {
	EnsureSearchSyncSchema(ctx context.Context) error
	ListSyncTenantIDs(ctx context.Context) ([]uuid.UUID, error)
	GetSyncCursor(ctx context.Context, tenantID uuid.UUID, entity string) (time.Time, uuid.UUID, error)
	SetSyncCursor(ctx context.Context, tenantID uuid.UUID, entity string, watermark time.Time, lastID uuid.UUID) error
	FetchBookingsForSync(ctx context.Context, tenantID uuid.UUID, after time.Time, afterID uuid.UUID, limit int) ([]store.SyncBooking, error)
	FetchLeadsForSync(ctx context.Context, tenantID uuid.UUID, after time.Time, afterID uuid.UUID, limit int) ([]store.SyncLead, error)
	ScanContactsForSync(ctx context.Context, tenantID uuid.UUID, afterID uuid.UUID, limit int) ([]store.SyncContact, error)
	ContactHashMap(ctx context.Context, tenantID uuid.UUID) (map[uuid.UUID]string, error)
	UpsertContactHash(ctx context.Context, tenantID, contactID uuid.UUID, hash string) error
	DeleteContactHash(ctx context.Context, contactID uuid.UUID) error
	DeletedContactIDs(ctx context.Context, tenantID uuid.UUID) ([]uuid.UUID, error)
}

// Syncer polls Postgres and projects rows into OpenSearch. It NEVER blocks
// HTTP serving: constructed in main only when OPENSEARCH_ADDR != "", errors
// are logged and the sweep retries next tick.
type Syncer struct {
	Store     SyncStore
	OS        *Client
	Log       *zap.Logger
	Interval  time.Duration
	BatchSize int

	// osIndex/osDelete are same-package test seams (nil → the real OS
	// client): sync behavior tests run without a cluster. NOT a production
	// mock — production wiring always leaves both nil.
	osIndex  func(ctx context.Context, index, id string, doc any) error
	osDelete func(ctx context.Context, index, id string) error
}

func (s *Syncer) log() *zap.Logger {
	if s.Log != nil {
		return s.Log
	}
	return zap.NewNop()
}

func (s *Syncer) batchSize() int {
	if s.BatchSize > 0 {
		return s.BatchSize
	}
	return DefaultBatchSize
}

func (s *Syncer) indexDoc(ctx context.Context, index, id string, doc any) error {
	if s.osIndex != nil {
		return s.osIndex(ctx, index, id, doc)
	}
	return s.OS.IndexDoc(ctx, index, id, doc)
}

func (s *Syncer) deleteDoc(ctx context.Context, index, id string) error {
	if s.osDelete != nil {
		return s.osDelete(ctx, index, id)
	}
	return s.OS.DeleteDoc(ctx, index, id)
}

// Run bootstraps the bookkeeping schema (retry loop — Postgres may still be
// starting), performs an immediate sweep, then ticks on Interval. Returns
// when ctx is cancelled.
func (s *Syncer) Run(ctx context.Context) {
	for {
		if err := s.Store.EnsureSearchSyncSchema(ctx); err != nil {
			s.log().Warn("search sync: ensure schema failed; retrying", zap.Error(err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}
		break
	}
	s.SweepOnce(ctx)
	interval := s.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.SweepOnce(ctx)
		}
	}
}

// SweepOnce runs one full pass over every tenant. A poison tenant (bad row,
// cluster hiccup) is logged and SKIPPED — it never stalls the others.
func (s *Syncer) SweepOnce(ctx context.Context) {
	tenants, err := s.Store.ListSyncTenantIDs(ctx)
	if err != nil {
		s.log().Warn("search sync: tenant enumeration failed", zap.Error(err))
		return
	}
	for _, tid := range tenants {
		if err := ctx.Err(); err != nil {
			return
		}
		if err := s.syncBookings(ctx, tid); err != nil {
			s.log().Warn("search sync: bookings sweep failed; tenant skipped",
				zap.String("tenant_id", tid.String()), zap.Error(err))
			continue
		}
		if err := s.syncLeads(ctx, tid); err != nil {
			s.log().Warn("search sync: leads sweep failed; tenant skipped",
				zap.String("tenant_id", tid.String()), zap.Error(err))
			continue
		}
		if err := s.syncContacts(ctx, tid); err != nil {
			s.log().Warn("search sync: contacts sweep failed; tenant skipped",
				zap.String("tenant_id", tid.String()), zap.Error(err))
			continue
		}
	}
}

// BookingDoc renders one bookings-index document (times RFC3339 UTC).
func BookingDoc(b store.SyncBooking) map[string]any {
	doc := map[string]any{
		"tenant_id":     b.TenantID.String(),
		"booking_id":    b.ID.String(),
		"public_ref":    b.PublicRef,
		"status":        b.Status,
		"source":        b.Source,
		"contact_name":  b.ContactName,
		"contact_phone": b.ContactPhone,
		"offering_id":   b.OfferingID.String(),
		"starts_at":     b.StartsAt.UTC().Format(time.RFC3339),
		"ends_at":       b.EndsAt.UTC().Format(time.RFC3339),
		"created_at":    b.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":    b.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if b.TeamMemberID != nil {
		doc["team_member_id"] = b.TeamMemberID.String()
	}
	return doc
}

// LeadDoc renders one leads-index document; nil CampaignID/PromoCode are
// omitted so term filters never match an empty string.
func LeadDoc(l store.SyncLead) map[string]any {
	doc := map[string]any{
		"tenant_id":              l.TenantID.String(),
		"lead_id":                l.ID.String(),
		"phone_e164":             l.PhoneE164,
		"channel_of_first_touch": l.ChannelOfFirstTouch,
		"status":                 l.Status,
		"created_at":             l.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":             l.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if l.CampaignID != nil {
		doc["campaign_id"] = l.CampaignID.String()
	}
	if l.PromoCode != nil {
		doc["promo_code"] = *l.PromoCode
	}
	return doc
}

// ContactDoc renders one contacts-index document. synced_at stamps the
// projection moment (not part of the content hash).
func ContactDoc(c store.SyncContact, syncedAt time.Time) map[string]any {
	return map[string]any{
		"tenant_id":   c.TenantID.String(),
		"contact_id":  c.ID.String(),
		"name":        c.Name,
		"phone":       c.Phone,
		"email":       c.Email,
		"notes":       c.Notes,
		"source":      c.Source,
		"external_id": c.ExternalID,
		"synced_at":   syncedAt.UTC().Format(time.RFC3339),
	}
}

// contactContentHash is sha256 over the indexed field tuple joined by \x1f
// (a separator that cannot appear in the tuple unambiguously). Any content
// change — including an erasure tombstone blanking PII — flips the hash.
func contactContentHash(c store.SyncContact) string {
	h := sha256.New()
	for _, f := range []string{c.Name, c.Phone, c.Email, c.Notes, c.Source, c.ExternalID} {
		h.Write([]byte(f))
		h.Write([]byte{0x1f})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// syncBookings advances the (updated_at, id) watermark cursor, indexing
// every changed row (new bookings AND tombstone UPDATEs).
func (s *Syncer) syncBookings(ctx context.Context, tenantID uuid.UUID) error {
	wm, lastID, err := s.Store.GetSyncCursor(ctx, tenantID, entityBookings)
	if err != nil {
		return err
	}
	for {
		rows, err := s.Store.FetchBookingsForSync(ctx, tenantID, wm, lastID, s.batchSize())
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, b := range rows {
			if err := s.indexDoc(ctx, BookingsIndex, b.ID.String(), BookingDoc(b)); err != nil {
				return err
			}
			wm, lastID = b.UpdatedAt, b.ID
		}
		// Cursor advances per batch so a crash mid-sweep resumes, not restarts.
		if err := s.Store.SetSyncCursor(ctx, tenantID, entityBookings, wm, lastID); err != nil {
			return err
		}
		if len(rows) < s.batchSize() {
			return nil
		}
	}
}

// syncLeads mirrors syncBookings over the leads table (no join).
func (s *Syncer) syncLeads(ctx context.Context, tenantID uuid.UUID) error {
	wm, lastID, err := s.Store.GetSyncCursor(ctx, tenantID, entityLeads)
	if err != nil {
		return err
	}
	for {
		rows, err := s.Store.FetchLeadsForSync(ctx, tenantID, wm, lastID, s.batchSize())
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, l := range rows {
			if err := s.indexDoc(ctx, LeadsIndex, l.ID.String(), LeadDoc(l)); err != nil {
				return err
			}
			wm, lastID = l.UpdatedAt, l.ID
		}
		if err := s.Store.SetSyncCursor(ctx, tenantID, entityLeads, wm, lastID); err != nil {
			return err
		}
		if len(rows) < s.batchSize() {
			return nil
		}
	}
}

// syncContacts hash-diffs the whole tenant contact set (the table has no
// timestamp columns, so a watermark cannot work): unchanged rows are
// skipped, new/changed rows are upserted + hashed, and hash rows whose
// contact vanished (hard delete) purge the doc + the hash row.
func (s *Syncer) syncContacts(ctx context.Context, tenantID uuid.UUID) error {
	hashes, err := s.Store.ContactHashMap(ctx, tenantID)
	if err != nil {
		return err
	}
	syncedAt := time.Now().UTC()
	var afterID uuid.UUID
	for {
		rows, err := s.Store.ScanContactsForSync(ctx, tenantID, afterID, s.batchSize())
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		for _, c := range rows {
			sum := contactContentHash(c)
			if hashes[c.ID] != sum {
				if err := s.indexDoc(ctx, ContactsIndex, c.ID.String(), ContactDoc(c, syncedAt)); err != nil {
					return err
				}
				if err := s.Store.UpsertContactHash(ctx, tenantID, c.ID, sum); err != nil {
					return err
				}
			}
			afterID = c.ID
		}
		if len(rows) < s.batchSize() {
			break
		}
	}
	deleted, err := s.Store.DeletedContactIDs(ctx, tenantID)
	if err != nil {
		return err
	}
	for _, id := range deleted {
		if err := s.deleteDoc(ctx, ContactsIndex, id.String()); err != nil {
			return err
		}
		if err := s.Store.DeleteContactHash(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// entityList documents the entity set (used by tests in this package).
var entityList = []string{entityBookings, entityContacts, entityLeads}
