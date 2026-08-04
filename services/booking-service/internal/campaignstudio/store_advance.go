package campaignstudio

// store_advance.go — due-enrollment advance + queued-send methods of Store (split from store.go for transport-size limits; no behavior change).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Step advancement
// ---------------------------------------------------------------------------

// QueuedSend is one send-step effect collected by AdvanceDue and handed
// to the StudioSendWorkflow (the enrollment has ALREADY advanced; the
// workflow only performs the paced send + outcome recording).
type QueuedSend struct {
	EnrollmentID uuid.UUID `json:"enrollment_id"`
	ContactID    uuid.UUID `json:"contact_id"`
	StepIdx      int       `json:"step_idx"`
	Kind         string    `json:"kind"` // sms | push_marketing | whatsapp
	Phone        string    `json:"phone,omitempty"`
	Name         string    `json:"name"`
	Text         string    `json:"text"` // rendered template ({name} substituted)
	// TemplateName/Language/Params carry the whatsapp step's Meta template
	// send fields (SPEC-W21 Agent A; empty for the sms/push kinds).
	TemplateName string   `json:"template_name,omitempty"`
	Language     string   `json:"language,omitempty"`
	Params       []string `json:"params,omitempty"`
}

// AdvanceResult summarizes one POST /journeys/{id}/step invocation.
type AdvanceResult struct {
	Scanned    int          `json:"scanned"`
	Advanced   int          `json:"advanced"`     // moved one step, still active
	Completed  int          `json:"completed"`    // moved past the last step
	Exited     int          `json:"exited"`       // branch false / contact missing
	Skipped    int          `json:"skipped"`      // ussd sends / missing channel address
	WaitNotDue int          `json:"wait_not_due"` // wait steps whose time has not come
	Sends      []QueuedSend `json:"sends"`        // paced sends to dispatch
	// SendsDeferred marks due send enrollments left in place because the
	// dispatcher (Temporal starter) is unavailable — the next step call
	// with dispatch picks them up.
	SendsDeferred bool `json:"sends_deferred"`
	// CompletedEnrollments carries the completed rows for the handler's
	// journey_completed CloudEvents (post-commit, best-effort).
	CompletedEnrollments []Enrollment `json:"-"`
}

// AdvanceDue advances up to limit active enrollments of journey j by ONE
// step each, transactionally:
//
//	wait   → due when last_step_at + wait_hours <= now, else left in place
//	send   → paced-send payload queued (unless dispatch=false, then left
//	         in place and SendsDeferred set); ussd / missing phone are
//	         advanced + counted as skipped (no outbound path)
//	branch → condition evaluated on contact attrs: true advances, false
//	         exits with reason branch_condition_false
//
// Advancing past the last step completes the enrollment. Every effect
// writes a studio_step_events row in the SAME transaction (audit +
// per-step stats). FOR UPDATE SKIP LOCKED keeps concurrent step callers
// (operator + CRON) from double-processing the same enrollment.
func (s *Store) AdvanceDue(ctx context.Context, tenantID uuid.UUID, j Journey, now time.Time, limit int, dispatch bool) (AdvanceResult, error) {
	res := AdvanceResult{Sends: []QueuedSend{}, CompletedEnrollments: []Enrollment{}}
	if limit <= 0 {
		limit = 200
	}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+enrollmentCols+` FROM studio_enrollments
			  WHERE tenant_id=$1 AND journey_id=$2 AND state='active'
			  ORDER BY enrolled_at LIMIT $3
			  FOR UPDATE SKIP LOCKED`,
			tenantID, j.ID, limit)
		if err != nil {
			return err
		}
		var due []Enrollment
		for rows.Next() {
			e, err := scanEnrollment(rows)
			if err != nil {
				rows.Close()
				return err
			}
			due = append(due, e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		res.Scanned = len(due)

		for _, e := range due {
			if e.StepIdx >= len(j.Steps) {
				// Defensive: an enrollment already past the end (e.g. steps
				// were shortened while paused) completes immediately.
				if err := s.completeEnrollment(ctx, tx, tenantID, e); err != nil {
					return err
				}
				res.Completed++
				e.State = EnrollCompleted
				res.CompletedEnrollments = append(res.CompletedEnrollments, e)
				continue
			}
			step := j.Steps[e.StepIdx]
			switch step.Type {
			case StepWait:
				if e.LastStepAt.Add(time.Duration(step.WaitHours) * time.Hour).After(now) {
					res.WaitNotDue++
					continue
				}
				if err := s.advanceOne(ctx, tx, tenantID, j.ID, e, EventWaitPassed, nil, len(j.Steps)); err != nil {
					return err
				}
			case StepBranch:
				attrs, err := s.loadContactAttrs(ctx, tx, tenantID, e.ContactID)
				if err != nil {
					if exitErr := s.exitEnrollment(ctx, tx, tenantID, j.ID, e, "contact_missing"); exitErr != nil {
						return exitErr
					}
					res.Exited++
					continue
				}
				if EvaluateCondition(step.Condition, attrs) {
					if err := s.advanceOne(ctx, tx, tenantID, j.ID, e, EventBranchTrue, nil, len(j.Steps)); err != nil {
						return err
					}
				} else {
					if err := s.exitEnrollment(ctx, tx, tenantID, j.ID, e, "branch_condition_false"); err != nil {
						return err
					}
					res.Exited++
					continue
				}
			case StepSend:
				if step.Kind == KindUSSD {
					// No outbound USSD binding exists (documented
					// limitation): advance + count as skipped.
					if err := s.advanceOne(ctx, tx, tenantID, j.ID, e, EventSendSkipped,
						map[string]any{"reason": "ussd_no_outbound_binding", "kind": step.Kind}, len(j.Steps)); err != nil {
						return err
					}
					res.Skipped++
					break
				}
				if !dispatch {
					res.SendsDeferred = true
					continue
				}
				attrs, err := s.loadContactAttrs(ctx, tx, tenantID, e.ContactID)
				if err != nil {
					if exitErr := s.exitEnrollment(ctx, tx, tenantID, j.ID, e, "contact_missing"); exitErr != nil {
						return exitErr
					}
					res.Exited++
					continue
				}
				if (step.Kind == KindSMS || step.Kind == KindWhatsApp) && attrs[FieldPhone] == "" {
					// SMS/WhatsApp need a phone; without one the enrollment
					// advances with a skip (documented — no dead-lettering).
					if err := s.advanceOne(ctx, tx, tenantID, j.ID, e, EventSendSkipped,
						map[string]any{"reason": "missing_phone", "kind": step.Kind}, len(j.Steps)); err != nil {
						return err
					}
					res.Skipped++
					break
				}
				qs := QueuedSend{
					EnrollmentID: e.ID,
					ContactID:    e.ContactID,
					StepIdx:      e.StepIdx,
					Kind:         step.Kind,
					Phone:        attrs[FieldPhone],
					Name:         attrs[FieldName],
					Text:         strings.ReplaceAll(step.Template, "{name}", attrs[FieldName]),
					TemplateName: step.TemplateName,
					Language:     step.Language,
					Params:       step.Params,
				}
				payload, _ := json.Marshal(map[string]any{"kind": qs.Kind, "phone": qs.Phone})
				if err := s.advanceOne(ctx, tx, tenantID, j.ID, e, EventSendQueued,
					map[string]any{"kind": qs.Kind, "payload": json.RawMessage(payload)}, len(j.Steps)); err != nil {
					return err
				}
				res.Sends = append(res.Sends, qs)
			default:
				return fmt.Errorf("%w: unknown step type %q in stored journey", ErrInvalidInput, step.Type)
			}
			// advanceOne moved the enrollment; classify the outcome.
			if e.StepIdx+1 >= len(j.Steps) {
				res.Completed++
				e.State = EnrollCompleted
				res.CompletedEnrollments = append(res.CompletedEnrollments, e)
			} else {
				res.Advanced++
			}
		}
		return nil
	})
	return res, err
}

// advanceOne moves enrollment e to step_idx+1 (or completes it when the
// journey has stepCount steps) and writes the step event row, atomically.
func (s *Store) advanceOne(ctx context.Context, tx pgx.Tx, tenantID, journeyID uuid.UUID, e Enrollment, eventKind string, payload map[string]any, stepCount int) error {
	newIdx := e.StepIdx + 1
	completed := newIdx >= stepCount
	if completed {
		if _, err := tx.Exec(ctx,
			`UPDATE studio_enrollments SET step_idx=$3, state='completed', last_step_at=now()
			  WHERE tenant_id=$1 AND id=$2`,
			tenantID, e.ID, newIdx); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec(ctx,
			`UPDATE studio_enrollments SET step_idx=$3, last_step_at=now()
			  WHERE tenant_id=$1 AND id=$2`,
			tenantID, e.ID, newIdx); err != nil {
			return err
		}
	}
	if err := s.insertStepEvent(ctx, tx, tenantID, journeyID, e.ID, e.StepIdx, eventKind, payload); err != nil {
		return err
	}
	if completed {
		return s.insertStepEvent(ctx, tx, tenantID, journeyID, e.ID, e.StepIdx, EventCompleted, nil)
	}
	return nil
}

// exitEnrollment flips an enrollment to exited with a reason + event row.
func (s *Store) exitEnrollment(ctx context.Context, tx pgx.Tx, tenantID, journeyID uuid.UUID, e Enrollment, reason string) error {
	if _, err := tx.Exec(ctx,
		`UPDATE studio_enrollments SET state='exited', exited_reason=$3, last_step_at=now()
		  WHERE tenant_id=$1 AND id=$2`,
		tenantID, e.ID, reason); err != nil {
		return err
	}
	kind := EventExited
	if reason == "branch_condition_false" {
		kind = EventBranchFalse
	}
	return s.insertStepEvent(ctx, tx, tenantID, journeyID, e.ID, e.StepIdx, kind,
		map[string]any{"reason": reason})
}

// completeEnrollment completes an enrollment already past the last step.
func (s *Store) completeEnrollment(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e Enrollment) error {
	if _, err := tx.Exec(ctx,
		`UPDATE studio_enrollments SET state='completed', last_step_at=now()
		  WHERE tenant_id=$1 AND id=$2`,
		tenantID, e.ID); err != nil {
		return err
	}
	return s.insertStepEvent(ctx, tx, tenantID, e.JourneyID, e.ID, e.StepIdx, EventCompleted, nil)
}

// insertStepEvent appends one audit/stat row.
func (s *Store) insertStepEvent(ctx context.Context, tx pgx.Tx, tenantID, journeyID, enrollmentID uuid.UUID, stepIdx int, kind string, payload map[string]any) error {
	p := []byte(`{}`)
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		p = b
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO studio_step_events (tenant_id, journey_id, enrollment_id, step_idx, kind, payload)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		tenantID, journeyID, enrollmentID, stepIdx, kind, p)
	return err
}

// loadContactAttrs builds the attribute view of one contact (+ its latest
// lead) for condition evaluation and send rendering. Returns ErrNotFound
// when the contact does not exist in the tenant.
func (s *Store) loadContactAttrs(ctx context.Context, tx pgx.Tx, tenantID, contactID uuid.UUID) (ContactAttrs, error) {
	attrs := ContactAttrs{}
	var name, phone, email, source, externalID string
	err := tx.QueryRow(ctx,
		`SELECT name, COALESCE(phone,''), COALESCE(email,''),
		        COALESCE(source,''), COALESCE(external_id,'')
		   FROM contacts WHERE tenant_id=$1 AND id=$2`,
		tenantID, contactID).Scan(&name, &phone, &email, &source, &externalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	attrs[FieldName] = name
	attrs[FieldPhone] = phone
	attrs[FieldEmail] = email
	attrs[FieldSource] = source
	attrs[FieldExternalID] = externalID
	if phone != "" {
		var status, channel, campaign string
		var leadCreated *time.Time
		err := tx.QueryRow(ctx,
			`SELECT status, channel_of_first_touch, COALESCE(campaign_id::text,''), created_at
			   FROM leads WHERE tenant_id=$1 AND phone_e164=$2
			   ORDER BY created_at DESC LIMIT 1`,
			tenantID, phone).Scan(&status, &channel, &campaign, &leadCreated)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			attrs[FieldLeadStatus] = status
			attrs[FieldLeadChannel] = channel
			attrs[FieldLeadCampaignID] = campaign
			if leadCreated != nil {
				attrs[FieldLeadCreatedAt] = leadCreated.UTC().Format(time.RFC3339)
			}
		}
	}
	return attrs, nil
}

// RecordSendOutcome writes the send_sent / send_suppressed / send_failed
// step event for a previously queued send (called by the StudioSendWorkflow
// activity after each paced send resolves).
func (s *Store) RecordSendOutcome(ctx context.Context, tenantID, journeyID, enrollmentID uuid.UUID, stepIdx int, kind, reason string) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		payload := map[string]any{}
		if reason != "" {
			payload["reason"] = reason
		}
		return s.insertStepEvent(ctx, tx, tenantID, journeyID, enrollmentID, stepIdx, kind, payload)
	})
}
