package consumer

// Reverse sync worker: Twenty -> OpenDesk (SPEC-CRM §B). Consumes the
// CloudEvents that POST /webhooks/twenty publishes to opendesk.crm.events
// (consumer group `crm-sync-reverse`, DLQ after 3 attempts via the shared
// Consumer):
//
//   - person.created / person.updated -> fetch the full Person from Twenty,
//     resolve the tenant slug, and upsert the booking-service contact via
//     Dapr service invocation (POST /internal/contacts/upsert,
//     external_source='twenty', external_id=personId).
//   - task.updated with status DONE -> when sync_map kind=booking_task maps
//     the Twenty task to an OpenDesk booking, append a CRM note to that
//     booking (POST /internal/bookings/{id}/crm-note).
//
// LOOP PREVENTION (echo suppression): the forward syncer stamps
// sync_map.last_synced_at on every person write. When an inbound person
// webhook arrives within EchoWindow (default 10s) of that stamp, it is our
// own write echoing back and is skipped + acked. The booking-service write
// side emits no events at all (contacts have no outbox; crm-notes bypass it),
// so no CloudEvent ever travels back onto opendesk.booking.events from here.
//
// REMAINING RACE (documented, accepted): a person edited in Twenty within the
// same 10s window as a forward-sync write is suppressed together with the
// echo and converges only on the next Twenty-side edit (last-write-wins is
// Twenty for contacts). Narrowing the window trades echo leaks for missed
// edits; 10s covers the webhook delivery latency we observe in-compose.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/crm-sync-service/internal/events"
	"github.com/opendesk/crm-sync-service/internal/metrics"
	"github.com/opendesk/crm-sync-service/internal/syncmap"
	"github.com/opendesk/crm-sync-service/internal/twentyc"
	"go.uber.org/zap"
)

// CloudEvent types produced by the webhook intake for Twenty events
// ("com.opendesk.crm.twenty." + Twenty's event name).
const (
	TypeTwentyPersonCreated = "com.opendesk.crm.twenty.person.created"
	TypeTwentyPersonUpdated = "com.opendesk.crm.twenty.person.updated"
	TypeTwentyTaskUpdated   = "com.opendesk.crm.twenty.task.updated"
	// TypeTicketEscalated is emitted by the notification-worker's
	// EscalateTicket activity (support-desk pack, first-response SLA
	// breach) onto opendesk.crm.events: data carries {tenant_slug,
	// tenant_id, ticket_id (= OpenDesk booking id), priority:"high",
	// reason, sla}. The reverse worker flags the booking's Twenty task
	// HIGH priority (SPEC-W80 A9).
	TypeTicketEscalated = "com.opendesk.crm.TicketEscalated"
)

// DefaultEchoWindow is how long after a forward-sync write an inbound person
// webhook is treated as our own echo.
const DefaultEchoWindow = 10 * time.Second

// errUnresolvable marks tenants that cannot be resolved from sync_map /
// Twenty (e.g. a person of a foreign workspace). Such events are skip+ack —
// retrying cannot heal them and they are not poison payloads.
var errUnresolvable = errors.New("tenant unresolvable")

// ReverseMapStore is the sync_map read surface of the reverse worker.
type ReverseMapStore interface {
	Get(ctx context.Context, kind, opendeskID string, tenantID *uuid.UUID) (syncmap.Mapping, error)
	GetByTwentyID(ctx context.Context, kind, twentyID string) (syncmap.Mapping, error)
}

// BookingInvoker performs Dapr service invocation against booking-service
// (interface for testability; *daprc.Client satisfies it).
type BookingInvoker interface {
	InvokeService(ctx context.Context, appID, method string, headers map[string]string, payload, out any) error
}

// ReverseSyncer applies Twenty webhook CloudEvents to OpenDesk.
type ReverseSyncer struct {
	Twenty *twentyc.Client
	// Router (2026-11-23): the instance a webhook came from (by its workspaceId) or a tenant's own instance.
	Router       *twentyc.Router
	Map          ReverseMapStore
	Invoker      BookingInvoker
	BookingAppID string        // Dapr app-id of booking-service (default "booking")
	EchoWindow   time.Duration // echo suppression window (default DefaultEchoWindow)
	// BookingInternalToken (BOOKING_INTERNAL_TOKEN, SPEC-W80 A11/B7):
	// booking-service gates its internal routes (/internal/contacts/upsert,
	// /internal/bookings/{id}/crm-note, ...) behind internAuth — every
	// invoke must carry X-Internal-Token matching that value. Empty => the
	// header is omitted and the peer fails closed (warned at boot).
	BookingInternalToken string
	Metrics              *metrics.Registry
	Log                  *zap.Logger
}

func (s *ReverseSyncer) echoWindow() time.Duration {
	if s.EchoWindow > 0 {
		return s.EchoWindow
	}
	return DefaultEchoWindow
}

func (s *ReverseSyncer) bookingAppID() string {
	if s.BookingAppID != "" {
		return s.BookingAppID
	}
	return "booking"
}

// bookingHeaders builds the invoke headers for booking-service internal
// routes: the tenant scope plus the B7 internAuth token (X-Internal-Token
// == BOOKING_INTERNAL_TOKEN, SPEC-W80 A11). The token header is omitted
// only when unconfigured — the peer then fails closed (503) and the boot
// warning in main explains why.
func (s *ReverseSyncer) bookingHeaders(slug string) map[string]string {
	h := map[string]string{"X-Tenant-Slug": slug}
	if s.BookingInternalToken != "" {
		h["X-Internal-Token"] = s.BookingInternalToken
	}
	return h
}

// twEvent is the Twenty client of the instance a webhook event came from (its workspaceId).
func (s *ReverseSyncer) twEvent(evt events.CloudEvent) *twentyc.Client {
	if s.Router != nil {
		ws, _ := evt.Data["workspaceId"].(string)
		return s.Router.ForWorkspace(ws)
	}
	return s.Twenty
}

// twTenant is the Twenty client holding a tenant's records.
func (s *ReverseSyncer) twTenant(tenantID *uuid.UUID) *twentyc.Client {
	if s.Router != nil {
		return s.Router.ForTenant(tenantID)
	}
	return s.Twenty
}

// HandleCRMEvent processes one opendesk.crm.events CloudEvent.
func (s *ReverseSyncer) HandleCRMEvent(ctx context.Context, evt events.CloudEvent) error {
	switch evt.Type {
	case TypeTwentyPersonCreated, TypeTwentyPersonUpdated:
		return s.handlePerson(ctx, evt)
	case TypeTwentyTaskUpdated:
		return s.handleTaskUpdated(ctx, evt)
	case TypeTicketEscalated:
		return s.handleTicketEscalated(ctx, evt)
	default:
		s.Log.Debug("ignoring crm event", zap.String("type", evt.Type))
		return nil
	}
}

// webhookRecord extracts the changed record (+ its id) from the Twenty
// webhook payload carried in evt.Data. Twenty v1 webhook payloads are
// {targetUrl, event, objectMetadata, workspaceId, webhookId, eventDate,
// record, updatedFields}; the record id is authoritative.
func webhookRecord(evt events.CloudEvent) (map[string]any, string) {
	rec, _ := evt.Data["record"].(map[string]any)
	id, _ := rec["id"].(string)
	return rec, id
}

// handlePerson syncs a Twenty person into a booking-service contact.
func (s *ReverseSyncer) handlePerson(ctx context.Context, evt events.CloudEvent) error {
	_, personID := webhookRecord(evt)
	if personID == "" {
		return permanent(fmt.Errorf("%s missing record.id", evt.Type))
	}

	// Echo suppression + mapping lookup in one read.
	var contactMapping *syncmap.Mapping
	m, err := s.Map.GetByTwentyID(ctx, KindContact, personID)
	switch {
	case err == nil:
		if m.LastSyncedAt != nil && time.Since(*m.LastSyncedAt) < s.echoWindow() {
			s.Metrics.Inc("reverse_echo_suppressed")
			s.Log.Info("suppressing echo of our own person sync",
				zap.String("person_id", personID),
				zap.Duration("age", time.Since(*m.LastSyncedAt)))
			return nil
		}
		mm := m
		contactMapping = &mm
	case errors.Is(err, syncmap.ErrNotFound):
		// Person unknown to the forward sync — resolve via company domain.
	default:
		return fmt.Errorf("sync_map contact lookup: %w", err)
	}

	person, err := s.twEvent(evt).GetPerson(ctx, personID)
	if err != nil {
		return fmt.Errorf("get twenty person: %w", err)
	}
	if person.PrimaryPhone() == "" && person.PrimaryEmail() == "" {
		// Contacts are keyed on phone/email; without either the booking-side
		// upsert cannot match or create deterministically — skip + ack.
		s.Log.Info("person has neither phone nor email; skipping contact upsert",
			zap.String("person_id", personID))
		return nil
	}
	slug, err := s.resolveTenantSlug(ctx, s.twEvent(evt), contactMapping, person.CompanyID)
	if errors.Is(err, errUnresolvable) {
		s.Metrics.Inc("reverse_tenant_unresolvable")
		s.Log.Info("tenant unresolvable for person; skipping (ack)",
			zap.String("person_id", personID), zap.String("company_id", person.CompanyID))
		return nil
	}
	if err != nil {
		return err
	}

	payload := map[string]any{
		"name":            person.DisplayName(),
		"phone":           person.PrimaryPhone(),
		"email":           person.PrimaryEmail(),
		"notes":           "",
		"external_source": "twenty",
		"external_id":     personID,
	}
	if err := s.Invoker.InvokeService(ctx, s.bookingAppID(), "internal/contacts/upsert",
		s.bookingHeaders(slug), payload, nil); err != nil {
		return fmt.Errorf("invoke booking contact upsert: %w", err)
	}
	s.Metrics.Inc("reverse_person_upserted")
	s.Log.Info("person reverse-synced to contact",
		zap.String("person_id", personID), zap.String("tenant_slug", slug))
	return nil
}

// handleTaskUpdated appends a CRM note to the OpenDesk booking when its
// Twenty task is marked DONE.
func (s *ReverseSyncer) handleTaskUpdated(ctx context.Context, evt events.CloudEvent) error {
	rec, taskID := webhookRecord(evt)
	if taskID == "" {
		return permanent(fmt.Errorf("%s missing record.id", evt.Type))
	}
	status, _ := rec["status"].(string)
	if status == "" {
		// Payload without the new status: fetch the current task state.
		t, err := s.twEvent(evt).GetTask(ctx, taskID)
		if err != nil {
			return fmt.Errorf("get twenty task: %w", err)
		}
		status = t.Status
	}
	if status != "DONE" {
		s.Log.Debug("task not DONE; skipping", zap.String("task_id", taskID), zap.String("status", status))
		return nil
	}
	m, err := s.Map.GetByTwentyID(ctx, KindBookingTask, taskID)
	if errors.Is(err, syncmap.ErrNotFound) {
		// Not an OpenDesk-synced task (created inside Twenty) — nothing to do.
		s.Log.Debug("no booking_task mapping for DONE task; skipping", zap.String("task_id", taskID))
		return nil
	}
	if err != nil {
		return fmt.Errorf("sync_map booking_task lookup: %w", err)
	}
	var slug string
	if m.TenantID != nil && *m.TenantID != uuid.Nil {
		slug, err = s.slugForTenant(ctx, *m.TenantID)
		if errors.Is(err, errUnresolvable) || errors.Is(err, syncmap.ErrNotFound) {
			s.Metrics.Inc("reverse_tenant_unresolvable")
			s.Log.Info("tenant unresolvable for DONE task; skipping (ack)",
				zap.String("task_id", taskID), zap.String("booking_id", m.OpenDeskID))
			return nil
		}
		if err != nil {
			return err
		}
	}
	if slug == "" {
		s.Metrics.Inc("reverse_tenant_unresolvable")
		return nil
	}
	payload := map[string]any{
		"text":   fmt.Sprintf("Twenty task %s marked DONE.", taskID),
		"source": "twenty",
	}
	if err := s.Invoker.InvokeService(ctx, s.bookingAppID(),
		"internal/bookings/"+m.OpenDeskID+"/crm-note",
		s.bookingHeaders(slug), payload, nil); err != nil {
		return fmt.Errorf("invoke booking crm-note: %w", err)
	}
	s.Metrics.Inc("reverse_task_completed")
	s.Log.Info("DONE task reverse-synced to booking crm-note",
		zap.String("task_id", taskID), zap.String("booking_id", m.OpenDeskID))
	return nil
}

// handleTicketEscalated (SPEC-W80 A9): the notification-worker's
// EscalateTicket activity (support-desk pack, first-response SLA breach)
// publishes com.opendesk.crm.TicketEscalated with data {tenant_slug,
// tenant_id, ticket_id (= OpenDesk booking id), priority, reason, sla}.
// The booking's Twenty task is resolved via sync_map (kind=booking, same
// mapping the forward syncer wrote) and PATCHed to priority HIGH. No
// mapping (ticket never synced / booking predates the sync) is a graceful
// no-op + debug log — never a retry, never a DLQ.
func (s *ReverseSyncer) handleTicketEscalated(ctx context.Context, evt events.CloudEvent) error {
	d, err := events.DataAs[TicketEscalatedData](evt)
	if err != nil {
		return permanent(err)
	}
	if d.TicketID == "" {
		return permanent(fmt.Errorf("%s missing ticket_id", evt.Type))
	}
	tenantUUID := parseUUID(d.TenantID)
	m, err := s.Map.Get(ctx, KindBooking, d.TicketID, tenantUUID)
	if errors.Is(err, syncmap.ErrNotFound) {
		s.Log.Debug("no booking task mapping for escalated ticket; skipping",
			zap.String("ticket_id", d.TicketID), zap.String("tenant_id", d.TenantID))
		return nil
	}
	if err != nil {
		return fmt.Errorf("sync_map booking lookup: %w", err)
	}
	if err := s.twTenant(tenantUUID).PatchTask(ctx, m.TwentyID, map[string]any{"priority": "HIGH"}); err != nil {
		return fmt.Errorf("patch escalated task priority: %w", err)
	}
	s.Metrics.Inc("reverse_ticket_escalated")
	s.Log.Info("escalated ticket: Twenty task priority set HIGH",
		zap.String("ticket_id", d.TicketID), zap.String("task_id", m.TwentyID),
		zap.String("reason", d.Reason))
	return nil
}

// TicketEscalatedData is the data payload of com.opendesk.crm.TicketEscalated
// (notification-worker EscalateTicket activity, support-desk pack).
type TicketEscalatedData struct {
	TenantSlug string `json:"tenant_slug"`
	TenantID   string `json:"tenant_id"`
	TicketID   string `json:"ticket_id"` // OpenDesk booking id
	Priority   string `json:"priority"`
	Reason     string `json:"reason"`
	SLA        string `json:"sla"`
}

// resolveTenantSlug maps a person to its OpenDesk tenant slug:
//  1. via the contact mapping (person -> contact -> tenant uuid -> tenant
//     mapping -> company domain "<slug>.opendesk.local");
//  2. otherwise via the person's company: the company must be an OpenDesk
//     tenant company (sync_map kind=tenant) and its domainName yields the
//     slug.
//
// Returns errUnresolvable (skip+ack) when neither path yields a slug.
func (s *ReverseSyncer) resolveTenantSlug(ctx context.Context, tw *twentyc.Client, cm *syncmap.Mapping, companyID string) (string, error) {
	if cm != nil && cm.TenantID != nil && *cm.TenantID != uuid.Nil {
		slug, err := s.slugForTenant(ctx, *cm.TenantID)
		if err == nil {
			return slug, nil
		}
		if !errors.Is(err, errUnresolvable) && !errors.Is(err, syncmap.ErrNotFound) {
			return "", err
		}
		// Fall through to the company-domain path.
	}
	if companyID != "" {
		// The company must be a mapped OpenDesk tenant company.
		if _, err := s.Map.GetByTwentyID(ctx, KindTenant, companyID); err != nil {
			if errors.Is(err, syncmap.ErrNotFound) {
				return "", fmt.Errorf("%w: company %s is not a mapped tenant", errUnresolvable, companyID)
			}
			return "", fmt.Errorf("sync_map tenant lookup: %w", err)
		}
		comp, err := tw.GetCompany(ctx, companyID)
		if err != nil {
			return "", fmt.Errorf("get twenty company: %w", err)
		}
		if slug, ok := twentyc.SlugFromTenantDomain(comp.DomainName.PrimaryLinkURL); ok {
			return slug, nil
		}
	}
	return "", fmt.Errorf("%w: person (company %q)", errUnresolvable, companyID)
}

// slugForTenant resolves tenant uuid -> kind=tenant mapping -> Twenty company
// -> domain slug.
func (s *ReverseSyncer) slugForTenant(ctx context.Context, tenantID uuid.UUID) (string, error) {
	tm, err := s.Map.Get(ctx, KindTenant, tenantID.String(), &tenantID)
	if err != nil {
		return "", err // ErrNotFound possible
	}
	comp, err := s.twTenant(&tenantID).GetCompany(ctx, tm.TwentyID)
	if err != nil {
		return "", fmt.Errorf("get twenty company: %w", err)
	}
	if slug, ok := twentyc.SlugFromTenantDomain(comp.DomainName.PrimaryLinkURL); ok {
		return slug, nil
	}
	return "", fmt.Errorf("%w: company domain %q", errUnresolvable, comp.DomainName.PrimaryLinkURL)
}
