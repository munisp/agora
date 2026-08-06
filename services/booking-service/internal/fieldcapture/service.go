package fieldcapture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/civic"
	"github.com/opendesk/booking-service/internal/leads"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// Service applies batched offline-queue captures exactly once per
// client_id (contract §4).
type Service struct {
	Store *Store
	// Leads creates the lead for kind=lead_capture (channel "field",
	// honoring the leads service's own 24h first-touch dedupe). Nil →
	// lead_capture items fail with a deterministic error.
	Leads *leads.Service
	// Civic creates the civic case for kind=civic_report (SPEC-W32 WS-A,
	// channel "pwa" — an agent filing on behalf of a citizen). Nil →
	// civic_report items fail with a deterministic error.
	Civic CivicSubmitter
	Log   *zap.Logger
}

// CivicSubmitter abstracts the civic module's report intake so the capture
// pipe stays decoupled (civic.Service satisfies it; tests use a fake).
type CivicSubmitter interface {
	Submit(ctx context.Context, tenantID uuid.UUID, tenantSlug, channel string, in civic.ReportInput) (store.CivicCase, error)
}

func (s *Service) log() *zap.Logger {
	if s.Log != nil {
		return s.Log
	}
	return zap.NewNop()
}

// Capture applies a batch of queue items, returning one result per item in
// request order. Item failures never fail the batch: the client drops
// applied/deduped items from its outbox and keeps/inspects error items
// (docs/field-capture.md §Client contract).
func (s *Service) Capture(ctx context.Context, tenant bookingops.TenantInfo, items []CaptureItem) []ItemResult {
	results := make([]ItemResult, 0, len(items))
	for i := range items {
		results = append(results, s.apply(ctx, tenant, items[i]))
	}
	return results
}

// apply processes one item behind its field_capture:{client_id}
// idempotency anchor.
func (s *Service) apply(ctx context.Context, tenant bookingops.TenantInfo, it CaptureItem) (res ItemResult) {
	res = ItemResult{ClientID: it.ClientID, Kind: it.Kind, Status: StatusError}
	if err := it.Validate(); err != nil {
		// Not anchored: the failure is deterministic, a replay fails the
		// same way (client_id may even be unusable as an anchor).
		res.Error = err.Error()
		return res
	}
	fresh, anchorStatus, anchorResult, err := s.Store.Anchor(ctx, tenant.ID, it)
	if err != nil {
		s.log().Error("field capture anchor failed", zap.String("client_id", it.ClientID), zap.Error(err))
		res.Error = "internal error"
		return res
	}
	if !fresh {
		// Replay of field_capture:{client_id}: return the ORIGINAL outcome
		// without re-applying side effects.
		res.Status = StatusDeduped
		if anchorStatus == "processing" {
			// A previous attempt died between anchor and resolve. Side
			// effects are unknown, so do NOT re-apply (lead creation is
			// 24h-deduped anyway, but a check-in would duplicate).
			res.Error = "previous attempt incomplete; resubmit with a new client_id to force re-application"
			return res
		}
		var stored ItemResult
		if len(anchorResult) > 0 {
			if err := json.Unmarshal(anchorResult, &stored); err == nil {
				res.LeadID = stored.LeadID
				res.CheckinID = stored.CheckinID
				res.CaseID = stored.CaseID
				res.CaseRef = stored.CaseRef
				res.Error = stored.Error
			}
		}
		return res
	}

	// Fresh anchor: apply the side effect.
	var applyErr error
	switch it.Kind {
	case KindLeadCapture:
		applyErr = s.applyLeadCapture(ctx, tenant, it, &res)
	case KindCheckin:
		applyErr = s.applyCheckin(ctx, tenant, it, &res)
	case KindCivicReport:
		applyErr = s.applyCivicReport(ctx, tenant, it, &res)
	}
	switch {
	case applyErr == nil:
		res.Status = StatusApplied
	case errors.Is(applyErr, leads.ErrInvalidInput), errors.Is(applyErr, civic.ErrInvalidInput), errors.Is(applyErr, ErrInvalidInput):
		// Deterministic failure: record it — replays dedupe to the same
		// outcome instead of re-running validation side effects.
		res.Status = StatusError
		res.Error = applyErr.Error()
	default:
		// Transient failure (DB etc.): release the anchor so the client's
		// retry re-applies cleanly instead of deduping onto a half state.
		res.Status = StatusError
		res.Error = "internal error"
		s.log().Error("field capture apply failed; anchor released for retry",
			zap.String("client_id", it.ClientID), zap.String("kind", it.Kind), zap.Error(applyErr))
		if relErr := s.Store.Release(ctx, tenant.ID, it.ClientID); relErr != nil {
			s.log().Error("field capture anchor release failed",
				zap.String("client_id", it.ClientID), zap.Error(relErr))
		}
		return res
	}
	if err := s.Store.Resolve(ctx, tenant.ID, it.ClientID, resolveStatus(res.Status), res); err != nil {
		s.log().Error("field capture resolve failed; anchor left in processing",
			zap.String("client_id", it.ClientID), zap.Error(err))
	}
	return res
}

// resolveStatus maps the API-facing status to the anchor column value.
func resolveStatus(apiStatus string) string {
	if apiStatus == StatusApplied {
		return "applied"
	}
	return "error"
}

// applyLeadCapture creates the lead for kind=lead_capture via the W13
// leads service (channel "field"; its 24h first-touch dedupe stacks under
// the client_id anchor).
func (s *Service) applyLeadCapture(ctx context.Context, tenant bookingops.TenantInfo, it CaptureItem, res *ItemResult) error {
	if s.Leads == nil {
		return errLeadsUnavailable
	}
	var p LeadCapturePayload
	if err := json.Unmarshal(it.Payload, &p); err != nil {
		return errInvalidPayload("lead_capture payload: " + err.Error())
	}
	if strings.TrimSpace(p.PhoneE164) == "" {
		return errInvalidPayload("lead_capture payload: phone_e164 is required")
	}
	lead, _, err := s.Leads.Create(ctx, leads.CreateInput{
		TenantID:   tenant.ID,
		PhoneE164:  p.PhoneE164,
		Channel:    leads.ChannelField,
		UTM:        p.UTM,
		CampaignID: p.CampaignID,
		LgaID:      p.LgaID,
		ConsentID:  p.ConsentID,
	}, tenant.Slug)
	if err != nil {
		return err
	}
	res.LeadID = &lead.ID
	return nil
}

// applyCheckin appends the geo check-in row for kind=checkin (the W8
// contact_locations store exposes no history — see package doc).
func (s *Service) applyCheckin(ctx context.Context, tenant bookingops.TenantInfo, it CaptureItem, res *ItemResult) error {
	var p CheckinPayload
	if err := json.Unmarshal(it.Payload, &p); err != nil {
		return errInvalidPayload("checkin payload: " + err.Error())
	}
	c := Checkin{
		ID:         uuid.New(),
		TenantID:   tenant.ID,
		ContactID:  p.ContactID,
		Note:       strings.TrimSpace(p.Note),
		Payload:    it.Payload,
		CapturedAt: it.CapturedAt,
	}
	if it.GPS != nil {
		lat, lng, acc := it.GPS.Lat, it.GPS.Lng, it.GPS.Accuracy
		c.Lat, c.Lng, c.AccuracyM = &lat, &lng, &acc
	}
	if err := s.Store.InsertCheckin(ctx, &c); err != nil {
		return err
	}
	res.CheckinID = &c.ID
	return nil
}

// applyCivicReport creates the civic case for kind=civic_report (SPEC-W32
// WS-A): payload = the public report body, channel=pwa (agent capturing on
// behalf of a citizen; agent attribution rides the device/user context of
// the capture request, the anchor row preserves the raw payload).
func (s *Service) applyCivicReport(ctx context.Context, tenant bookingops.TenantInfo, it CaptureItem, res *ItemResult) error {
	if s.Civic == nil {
		return errCivicUnavailable
	}
	var p CivicReportPayload
	if err := json.Unmarshal(it.Payload, &p); err != nil {
		return errInvalidPayload("civic_report payload: " + err.Error())
	}
	in := civic.ReportInput{
		CategorySlug:      p.CategorySlug,
		Description:       p.Description,
		Ward:              p.Ward,
		LGA:               p.LGA,
		LocationText:      p.LocationText,
		ReporterPhoneE164: p.ReporterPhoneE164,
		ReporterName:      p.ReporterName,
		Anonymous:         p.Anonymous,
		PhotoURL:          p.PhotoURL,
	}
	// The item-level GPS fix supplies the case location when the payload
	// carries none (field agents capture the fix on-site).
	if it.GPS != nil {
		lat, lng := it.GPS.Lat, it.GPS.Lng
		in.Lat, in.Lon = &lat, &lng
	}
	c, err := s.Civic.Submit(ctx, tenant.ID, tenant.Slug, civic.ChannelPWA, in)
	if err != nil {
		return err
	}
	res.CaseID = &c.ID
	res.CaseRef = c.Ref
	return nil
}

// errLeadsUnavailable is deterministic within a deployment (wiring gap) —
// recorded like a validation error so replays dedupe instead of hammering.
var errLeadsUnavailable = fmt.Errorf("%w: leads service unavailable", ErrInvalidInput)

// errCivicUnavailable mirrors errLeadsUnavailable for kind=civic_report.
var errCivicUnavailable = fmt.Errorf("%w: civic service unavailable", ErrInvalidInput)

// errInvalidPayload wraps ErrInvalidInput for payload-schema failures.
func errInvalidPayload(msg string) error { return fmt.Errorf("%w: %s", ErrInvalidInput, msg) }
