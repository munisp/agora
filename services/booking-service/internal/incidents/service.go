package incidents

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/events"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// DeliveryStart is one signed dispatch delivery handed to the Wave-5
// WebhookDeliveryWorkflow (hosted by notification-worker). The JSON contract
// mirrors workflows.WebhookDeliveryInput with PayloadType "incident"
// (service boundary: duplicated, not shared).
type DeliveryStart struct {
	DeliveryID  string `json:"delivery_id"`
	URL         string `json:"url"`
	Secret      string `json:"secret"`
	EventType   string `json:"event_type"`
	PayloadType string `json:"payload_type"` // always "incident"
	IncidentID  string `json:"incident_id"`
	Body        []byte `json:"body"`
}

// PayloadTypeIncident marks incident deliveries in the Wave-5 workflow
// (mirrors notification-worker's workflows.PayloadTypeIncident).
const PayloadTypeIncident = "incident"

// AlertStart is one critical/high-severity outreach handed to the
// IncidentAlertWorkflow (hosted here; it delegates to the notification-worker
// paced fast-lane via the NotifyPaced activity, kind incident_alert).
type AlertStart struct {
	IncidentID string `json:"incident_id"`
	TenantID   string `json:"tenant_id"`
	TenantSlug string `json:"tenant_slug"`
	Channel    string `json:"channel"` // whatsapp | telegram | sms
	Phone      string `json:"phone"`
	Text       string `json:"text"`
}

// Starter abstracts Temporal workflow starts (temporalclient.Client
// satisfies it; tests use a fake).
type Starter interface {
	StartIncidentDelivery(ctx context.Context, in DeliveryStart) (string, error)
	StartIncidentAlert(ctx context.Context, in AlertStart) (string, error)
}

// UsageMetricIncidentAlert is the usage-events metric recorded for every
// incident outreach send (metered even though the pacer fast-lane bypasses
// the token bucket, SPEC-W11 Part B §5).
const UsageMetricIncidentAlert = "incident_alert_message"

// Service bundles the incident ingest/dispatch/outreach orchestration.
type Service struct {
	Store   *store.Store
	Starter Starter // nil: dispatch/outreach degrade to logged no-ops
	// AutoDispatch delivers every new incident to the tenant's active
	// endpoints on creation (INCIDENT_AUTO_DISPATCH, default true).
	AutoDispatch bool
	// UsageTopic meters outreach sends (opendesk.usage.events; empty
	// disables metering).
	UsageTopic string
	Log        *zap.Logger
}

func (s *Service) log() *zap.Logger {
	if s.Log != nil {
		return s.Log
	}
	return zap.NewNop()
}

// Ingest validates + persists an IDP (idempotent on incident_id) and, for
// NEW incidents, triggers auto-dispatch and critical/high outreach.
// created=false means the incident was already stored — the call is a
// no-op (consumer replay / duplicate webhook post).
func (s *Service) Ingest(ctx context.Context, idp IDP, tenantSlug string) (row store.Incident, created bool, err error) {
	idp.Complete()
	if err := idp.Validate(); err != nil {
		return row, false, err
	}
	payload, err := json.Marshal(idp)
	if err != nil {
		return row, false, fmt.Errorf("marshal idp: %w", err)
	}
	row = store.Incident{
		ID:              idp.IncidentID,
		TenantID:        idp.TenantID,
		ReferenceNumber: idp.ReferenceNumber,
		IncidentType:    idp.IncidentType,
		Severity:        idp.Severity,
		Payload:         payload,
	}
	created, err = s.Store.InsertIncident(ctx, &row)
	if err != nil {
		return row, false, err
	}
	if !created {
		return row, false, nil
	}
	// Side effects must not fail the ingest: dispatch/outreach errors are
	// logged; the row is durable and POST /v1/incidents/{id}/dispatch can
	// always re-drive delivery manually.
	if s.AutoDispatch {
		if _, derr := s.Dispatch(ctx, idp.TenantID, idp.IncidentID); derr != nil {
			s.log().Error("incident auto-dispatch failed",
				zap.String("incident_id", idp.IncidentID.String()), zap.Error(derr))
		}
	}
	if idp.NeedsOutreach() {
		if oerr := s.Outreach(ctx, idp, tenantSlug); oerr != nil {
			s.log().Error("incident outreach failed",
				zap.String("incident_id", idp.IncidentID.String()), zap.Error(oerr))
		}
	}
	return row, true, nil
}

// Dispatch delivers the incident's IDP to every ACTIVE dispatch endpoint of
// the tenant: one pending ledger row + one Wave-5 WebhookDeliveryWorkflow
// (payload type "incident") per endpoint, then marks the incident
// dispatched. Idempotent: the delivery id is deterministic
// (incident×endpoint), so a repeated dispatch upserts no duplicate rows and
// duplicate workflow starts are rejected as already-running.
func (s *Service) Dispatch(ctx context.Context, tenantID, incidentID uuid.UUID) ([]store.IncidentDelivery, error) {
	inc, err := s.Store.GetIncident(ctx, tenantID, incidentID)
	if err != nil {
		return nil, err
	}
	endpoints, err := s.Store.ListDispatchEndpoints(ctx, tenantID, true)
	if err != nil {
		return nil, err
	}
	if len(endpoints) == 0 {
		return nil, nil
	}
	out := make([]store.IncidentDelivery, 0, len(endpoints))
	for _, ep := range endpoints {
		d := store.IncidentDelivery{
			ID:          DeliveryID(incidentID, ep.URL),
			TenantID:    tenantID,
			IncidentID:  incidentID,
			EndpointURL: ep.URL,
		}
		if err := s.Store.InsertIncidentDelivery(ctx, &d); err != nil {
			return out, err
		}
		if s.Starter != nil {
			_, err := s.Starter.StartIncidentDelivery(ctx, DeliveryStart{
				DeliveryID:  d.ID.String(),
				URL:         ep.URL,
				Secret:      ep.Secret,
				EventType:   EventTypeIDPCreated,
				PayloadType: PayloadTypeIncident,
				IncidentID:  incidentID.String(),
				Body:        inc.Payload,
			})
			if err != nil {
				return out, fmt.Errorf("start delivery workflow: %w", err)
			}
		}
		out = append(out, d)
	}
	if err := s.Store.MarkIncidentDispatched(ctx, tenantID, incidentID); err != nil {
		return out, err
	}
	return out, nil
}

// DeliveryID derives the deterministic ledger-row id for incident×endpoint
// (uuid v5-style: SHA-256 of the pair, RFC-4122 version bits set).
func DeliveryID(incidentID uuid.UUID, url string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(incidentID.String()+"|"+url))
}

// Outreach sends the critical/high-severity alert through the
// IncidentAlertWorkflow → NotifyPaced(kind incident_alert, priority) path.
// Phone resolution: the IDP callback number wins; with only a contact id we
// look the contact up for its phone. Metering: one usage outbox row per
// outreach, so the priority fast-lane stays billed despite bypassing the
// CPS bucket.
func (s *Service) Outreach(ctx context.Context, idp IDP, tenantSlug string) error {
	phone := ""
	if idp.CallbackNumber != nil {
		phone = *idp.CallbackNumber
	}
	if phone == "" && idp.ContactID != nil {
		contact, err := s.Store.GetContact(ctx, idp.TenantID, *idp.ContactID)
		if err != nil {
			return fmt.Errorf("resolve outreach contact: %w", err)
		}
		phone = contact.Phone
	}
	if phone == "" {
		return nil // no reachable number after all; nothing to send
	}
	if s.Starter == nil {
		s.log().Warn("incident outreach skipped: no workflow starter",
			zap.String("incident_id", idp.IncidentID.String()))
		return nil
	}
	_, err := s.Starter.StartIncidentAlert(ctx, AlertStart{
		IncidentID: idp.IncidentID.String(),
		TenantID:   idp.TenantID.String(),
		TenantSlug: tenantSlug,
		Channel:    idp.OutreachChannel(),
		Phone:      phone,
		Text:       idp.OutreachText(),
	})
	if err != nil {
		return err
	}
	s.meter(ctx, idp, tenantSlug)
	return nil
}

// meter writes one incident_alert_message usage record to the outbox
// (best-effort: metering must never block outreach).
func (s *Service) meter(ctx context.Context, idp IDP, tenantSlug string) {
	if s.UsageTopic == "" {
		return
	}
	payload, err := json.Marshal(events.New("booking-service", "com.opendesk.usage.UsageRecord", tenantSlug, idp.TenantID.String(), map[string]any{
		"tenant_id": idp.TenantID.String(),
		"metric":    UsageMetricIncidentAlert,
		"value":     1,
		"ts":        time.Now().UTC(),
		"meta": map[string]any{
			"incident_id":      idp.IncidentID.String(),
			"reference_number": idp.ReferenceNumber,
		},
	}))
	if err != nil {
		s.log().Warn("incident usage record marshal failed; skipping metering", zap.Error(err))
		return
	}
	if err := s.Store.EnqueueOutbox(ctx, idp.IncidentID, s.UsageTopic, payload); err != nil {
		s.log().Warn("incident usage record enqueue failed; skipping metering", zap.Error(err))
	}
}
