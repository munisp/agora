package campaignstudio

// Step execution (SPEC-W19 Agent D): AdvanceDue advances due enrollments
// one step per POST /journeys/{id}/step call. The whole batch runs in ONE
// transaction with FOR UPDATE SKIP LOCKED (concurrent operator + CRON
// callers cannot double-process the same enrollment), and send-step
// payloads are queued inside the same transaction — a step call retried
// after a crash finds the enrollments already advanced, so no send is
// queued twice (idempotency contract). The handler starts one
// StudioSendWorkflow post-commit for the batch.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ContactAttrs is the attribute view condition evaluation reads (contact
// columns + latest-lead fields via the phone join).
type ContactAttrs map[string]string

// QueuedSend is one send payload collected by AdvanceDue, handed to the
// StudioSendWorkflow batch.
type QueuedSend struct {
	EnrollmentID uuid.UUID `json:"enrollment_id"`
	ContactID    uuid.UUID `json:"contact_id"`
	StepIdx      int       `json:"step_idx"`
	Kind         string    `json:"kind"`              // sms | push_marketing | whatsapp
	Phone        string    `json:"phone"`             // recipient (also DND guard input)
	Name         string    `json:"name"`              // {name} substitution source
	Text         string    `json:"text,omitempty"`    // rendered template ({name} substituted; sms/push)
	// TemplateName / Language / Params carry the whatsapp template payload
	// (SPEC-W21; template_name was validated at journey save).
	TemplateName string   `json:"template_name,omitempty"`
	Language     string   `json:"language,omitempty"`
	Params       []string `json:"params,omitempty"`
}

// AdvanceResult reports one AdvanceDue call (the API response shape).
type AdvanceResult struct {
	Scanned       int          `json:"scanned"`
	Advanced      int          `json:"advanced"`
	Completed     int          `json:"completed"`
	Exited        int          `json:"exited"`
	Skipped       int          `json:"skipped"`
	WaitNotDue    int          `json:"wait_not_due"`
	Sends         []QueuedSend `json:"-"`                 // handed to the workflow starter
	SendsQueued   int          `json:"sends_queued"`
	SendsDeferred bool         `json:"sends_deferred"` // dispatch disabled (no Temporal starter)
}

// AdvanceDue advances up to limit active enrollments of one journey by one
// step each (wait due / send queue / branch eval). dispatch=false marks
// sends deferred (handler has no Temporal starter) and leaves the
// enrollments advanced WITHOUT queueing payloads — the next dispatched
// call re-queues them? No: advancement already happened, so deferred
// sends are simply reported as deferred (the operator retries when
// Temporal is back — the enrollment advanced honestly and the send event
// records the deferral). Wait: that would LOSE sends. So dispatch=false
// does NOT advance send steps at all — they stay due until dispatch is
// available (see advanceOne).
func (s *Store) AdvanceDue(ctx context.Context, tenantID uuid.UUID, j Journey, now time.Time, limit int, dispatch bool) (AdvanceResult, error) {
	if limit <= 0 {
		limit = StepBatchSizeDefault
	}
	res := AdvanceResult{Sends: []QueuedSend{}}
	if j.Status != StatusActive {
		return res, fmt.Errorf("%w: step requires an active journey (status is %s)", ErrConflict, j.Status)
	}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, journey_id, contact_id, step_idx, state, enrolled_at, last_step_at, exited_reason
			   FROM studio_enrollments
			   WHERE journey_id=$1 AND state='active'
			   ORDER BY enrolled_at
			   LIMIT $2
			   FOR UPDATE SKIP LOCKED`,
			j.ID, limit)
		if err != nil {
			return err
		}
		var batch []Enrollment
		for rows.Next() {
			var e Enrollment
			if err := rows.Scan(&e.ID, &e.TenantID, &e.JourneyID, &e.ContactID,
				&e.StepIdx, &e.State, &e.EnrolledAt, &e.LastStepAt, &e.ExitedReason); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		res.Scanned = len(batch)

		for _, e := range batch {
			outcome, err := s.advanceOne(ctx, tx, tenantID, j, e, now, dispatch, &res)
			if err != nil {
				return err
			}
			switch outcome {
			case "advanced":
				res.Advanced++
			case "completed":
				res.Completed++
			case "exited":
				res.Exited++
			case "skipped":
				res.Skipped++
			case "wait_not_due":
				res.WaitNotDue++
			}
		}
		res.SendsQueued = len(res.Sends)
		res.SendsDeferred = !dispatch && res.SendsQueued == 0 && res.hadDueSends
		return nil
	})
	return res, err
}

// hadDueSends marks that a send step was due but left un-advanced because
// dispatch is disabled (drives SendsDeferred).
func (r *AdvanceResult) markDeferred() { r.hadDueSends = true }

// unexported bookkeeping
// (kept off the JSON contract).
type advanceExtras struct{ hadDueSends bool }

// advanceOne advances one enrollment exactly one step. It is a pure state
// transition helper; all side effects (row updates, event rows, queued
// sends) happen inside the caller's transaction.
func (s *Store) advanceOne(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, j Journey, e Enrollment, now time.Time, dispatch bool, res *AdvanceResult) (string, error) {
	if e.StepIdx >= len(j.Steps) {
		// Already past the end (should not happen for active rows, but be
		// robust): complete it.
		if err := s.completeEnrollment(ctx, tx, tenantID, e); err != nil {
			return "", err
		}
		return "completed", nil
	}
	step := j.Steps[e.StepIdx]

	switch step.Type {
	case StepWait:
		due := e.LastStepAt.Add(time.Duration(step.WaitHours) * time.Hour)
		if now.Before(due) {
			return "wait_not_due", nil
		}
		if err := s.advanceStep(ctx, tx, tenantID, j.ID, e, EventWaitPassed, nil); err != nil {
			return "", err
		}
		return s.finishAdvance(ctx, tx, tenantID, j, e)

	case StepSend:
		attrs, err := s.loadContactAttrs(ctx, tx, tenantID, e.ContactID)
		if errors.Is(err, ErrNotFound) {
			if err := s.exitEnrollment(ctx, tx, tenantID, j.ID, e, "contact_missing"); err != nil {
				return "", err
			}
			return "exited", nil
		}
		if err != nil {
			return "", err
		}
		if step.Kind == KindUSSD {
			// No outbound USSD binding exists (documented limitation):
			// advance + count as skipped.
			if err := s.advanceStep(ctx, tx, tenantID, j.ID, e, EventSendSkipped,
				map[string]any{"reason": "ussd_no_outbound_binding"}); err != nil {
				return "", err
			}
			if err := s.noteSkipped(res); err != nil {
				return "", err
			}
			return s.finishAdvance(ctx, tx, tenantID, j, e)
		}
		phone := attrs[FieldPhone]
		if phone == "" {
			// SMS and WhatsApp sends are phone-addressed; push fan-out
			// tolerates a missing phone (token-addressed, DND warn).
			if step.Kind != KindPushMarketing {
				if err := s.advanceStep(ctx, tx, tenantID, j.ID, e, EventSendSkipped,
					map[string]any{"reason": "missing_phone"}); err != nil {
					return "", err
				}
				if err := s.noteSkipped(res); err != nil {
					return "", err
				}
				return s.finishAdvance(ctx, tx, tenantID, j, e)
			}
		}
		if !dispatch {
			// No Temporal starter: leave the enrollment in place (due) so
			// the send is NOT lost; the response reports sends_deferred.
			res.markDeferred()
			return "wait_not_due", nil
		}
		text := renderTemplate(step.Template, attrs)
		res.Sends = append(res.Sends, QueuedSend{
			EnrollmentID: e.ID,
			ContactID:    e.ContactID,
			StepIdx:      e.StepIdx,
			Kind:         step.Kind,
			Phone:        phone,
			Name:         attrs[FieldName],
			Text:         text,
			TemplateName: step.TemplateName,
			Language:     step.Language,
			Params:       renderParams(step.Params, attrs),
		})
		if err := s.advanceStep(ctx, tx, tenantID, j.ID, e, EventSendQueued,
			map[string]any{"kind": step.Kind}); err != nil {
			return "", err
		}
		return s.finishAdvance(ctx, tx, tenantID, j, e)

	case StepBranch:
		attrs, err := s.loadContactAttrs(ctx, tx, tenantID, e.ContactID)
		if errors.Is(err, ErrNotFound) {
			if err := s.exitEnrollment(ctx, tx, tenantID, j.ID, e, "contact_missing"); err != nil {
				return "", err
			}
			return "exited", nil
		}
		if err != nil {
			return "", err
		}
		ok, err := EvaluateCondition(step.Condition, attrs)
		if err != nil {
			return "", err
		}
		if !ok {
			if err := s.exitEnrollment(ctx, tx, tenantID, j.ID, e, "branch_condition_false"); err != nil {
				return "", err
			}
			return "exited", nil
		}
		if err := s.advanceStep(ctx, tx, tenantID, j.ID, e, EventBranchTrue, nil); err != nil {
			return "", err
		}
		return s.finishAdvance(ctx, tx, tenantID, j, e)
	}
	return "", fmt.Errorf("%w: unknown step type %q", ErrInvalidInput, step.Type)
}

// finishAdvance completes the enrollment when the just-advanced step was
// the last one.
func (s *Store) finishAdvance(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, j Journey, e Enrollment) (string, error) {
	if e.StepIdx+1 >= len(j.Steps) {
		if err := s.completeEnrollment(ctx, tx, tenantID, e); err != nil {
			return "", err
		}
		return "completed", nil
	}
	return "advanced", nil
}

// noteSkipped counts one skipped send in the result (kept a method so the
// JSON shape stays the single source).
func (s *Store) noteSkipped(res *AdvanceResult) error {
	res.Skipped++
	return nil
}

// advanceStep moves the enrollment one step forward and records the event.
func (s *Store) advanceStep(ctx context.Context, tx pgx.Tx, tenantID, journeyID uuid.UUID, e Enrollment, eventKind string, payload map[string]any) error {
	if _, err := tx.Exec(ctx,
		`UPDATE studio_enrollments SET step_idx=step_idx+1, last_step_at=now()
		  WHERE tenant_id=$1 AND id=$2`,
		tenantID, e.ID); err != nil {
		return err
	}
	return s.insertStepEvent(ctx, tx, tenantID, journeyID, e.ID, e.StepIdx, eventKind, payload)
}
