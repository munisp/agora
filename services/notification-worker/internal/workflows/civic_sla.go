package workflows

// SPEC-W32 WS-B: civic reporting — SLA timers + citizen status notifications.
//
// Civic cases (SPEC-W32 §2) ride the W11 incidents lifecycle on the
// booking-service side; this worker owns the two time-driven halves:
//
//  1. CivicSLAWorkflow — one durable workflow per civic case, started by the
//     civicoutbox consumer on com.opendesk.civic.ReportReceived with the
//     deterministic workflow ID civic-sla-{tenant}-{ref} (redeliveries hit
//     WorkflowExecutionAlreadyStarted, so a case never gets duplicate
//     timers). It holds two durable timers: ack_due_at and resolve_due_at.
//     com.opendesk.civic.StatusChanged is delivered as the SignalCivicStatus
//     signal: triaged/assigned/in_progress SATISFY the ack timer,
//     resolved/closed satisfy BOTH (closed completes the run immediately).
//     com.opendesk.civic.Merged arrives as SignalCivicMerged: the timers are
//     cancelled (the canonical case's own SLA workflow owns the SLA from
//     then on) and the run completes with the canonical ref recorded.
//     A timer that fires unsatisfied runs the ReportCivicSLABreach activity
//     (kind ack|resolve), which posts booking-service's internal callback
//     POST /v1/civic/internal/cases/{ref}/sla-breach — that internal route
//     sets the sla_breach_* flag AND notifies the case's mda_queue dispatch
//     endpoint through the W11 incident delivery path (signed webhook +
//     incident_deliveries ledger, owned by booking-service where the
//     endpoint URL/secret live) — plus emits the escalation CloudEvent.
//
//  2. CivicStatusNotifyWorkflow — one per StatusChanged that carries a
//     reporter phone (wants_updates): a TRANSACTIONAL-class paced send
//     "Case {ref}: now {status}" through the EXISTING paced-send machinery
//     (GuardedPacedSend → NotifyPaced → SendCivicStatusUpdate; CPS pacing +
//     sender rotation unchanged). Transactional class means the DND guard
//     never suppresses it (SPEC-W32 §0.4: service requests, not marketing),
//     but quiet hours DO hold it: inside the 20:00-08:00 window the workflow
//     durably Sleeps until the window opens, then sends. Every attempt
//     lands in the civic delivery ledger (activity-side, per execution, so
//     Temporal retries produce one row per attempt).
//
// Merged-case rule (SPEC-W32 §4.3): notifications reference the CANONICAL
// ref. The consumer resolves ref→canonical from Merged events and passes
// CanonicalRef through; the message text and the ledger use it.

import (
	"fmt"
	"time"

	"github.com/opendesk/notification-worker/internal/pacer"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const (
	// WorkflowTypeCivicSLA is the registered name of CivicSLAWorkflow.
	WorkflowTypeCivicSLA = "CivicSLAWorkflow"
	// WorkflowTypeCivicStatusNotify is the registered name of
	// CivicStatusNotifyWorkflow.
	WorkflowTypeCivicStatusNotify = "CivicStatusNotifyWorkflow"

	// SignalCivicStatus delivers a com.opendesk.civic.StatusChanged payload
	// (CivicStatusSignal) to the running CivicSLAWorkflow.
	SignalCivicStatus = "civic-status"
	// SignalCivicMerged delivers a com.opendesk.civic.Merged payload
	// (CivicMergedSignal): the case was merged into a canonical case.
	SignalCivicMerged = "civic-merged"

	// ActivityReportCivicSLABreach is the name of the SLA-breach callback
	// activity (booking-service internal route + escalation event).
	ActivityReportCivicSLABreach = "ReportCivicSLABreach"
	// ActivitySendCivicStatusUpdate is the name of the citizen status
	// notification send activity (invoked via NotifyPaced kind civic_status).
	ActivitySendCivicStatusUpdate = "SendCivicStatusUpdate"

	// PacedSendCivicStatus routes to SendCivicStatusUpdate: TRANSACTIONAL
	// class (SPEC-W32 §0.4 — bypasses DND; quiet-hours hold is workflow-side
	// in CivicStatusNotifyWorkflow).
	PacedSendCivicStatus = "civic_status"

	// CivicBreachKindAck marks an acknowledgement-SLA breach.
	CivicBreachKindAck = "ack"
	// CivicBreachKindResolve marks a resolution-SLA breach.
	CivicBreachKindResolve = "resolve"
)

// CivicSLAWorkflowID derives the deterministic workflow ID of a case's SLA
// workflow (SPEC-W32 §3 WS-B: civic-sla-{tenant}-{ref}).
func CivicSLAWorkflowID(tenant, ref string) string {
	return fmt.Sprintf("civic-sla-%s-%s", tenant, ref)
}

// CivicSLAInput starts a CivicSLAWorkflow (payload of
// com.opendesk.civic.ReportReceived, unpacked consumer-side).
type CivicSLAInput struct {
	TenantID   string `json:"tenant_id"`
	TenantSlug string `json:"tenant_slug"`
	Ref        string `json:"ref"`
	// MDAQueue is the dispatch-endpoint key the breach callback notifies
	// (civic_cases.mda_queue; may be empty when routing is unresolved).
	MDAQueue     string    `json:"mda_queue,omitempty"`
	AckDueAt     time.Time `json:"ack_due_at"`
	ResolveDueAt time.Time `json:"resolve_due_at"`
}

// CivicStatusSignal is the SignalCivicStatus payload: the case's lifecycle
// status changed (com.opendesk.civic.StatusChanged).
type CivicStatusSignal struct {
	Status string `json:"status"`
	// AckDueAt / ResolveDueAt carry booking-service's RECOMPUTED SLA dues
	// (triage can change the category, hence the SLA — SPEC-W32 W3). They
	// are pointers so old events without due times decode as nil = "don't
	// re-arm"; a non-nil value re-arms the corresponding pending timer.
	AckDueAt     *time.Time `json:"ack_due_at,omitempty"`
	ResolveDueAt *time.Time `json:"resolve_due_at,omitempty"`
}

// CivicMergedSignal is the SignalCivicMerged payload: the case was merged
// into CanonicalRef; its SLA timers are cancelled and later notifications
// reference the canonical case.
type CivicMergedSignal struct {
	CanonicalRef string `json:"canonical_ref"`
}

// CivicSLAState is the terminal state of a CivicSLAWorkflow run (also the
// workflow result — asserted by tests).
type CivicSLAState struct {
	Ref          string `json:"ref"`
	CanonicalRef string `json:"canonical_ref"`
	Acked        bool   `json:"acked"`
	Resolved     bool   `json:"resolved"`
	Closed       bool   `json:"closed"`
	Merged       bool   `json:"merged"`
	AckBreached  bool   `json:"ack_breached"`
	// ResolveBreached is true when the resolve timer fired unsatisfied.
	ResolveBreached bool `json:"resolve_breached"`
}

// CivicSLABreachReport is the ReportCivicSLABreach activity input: one
// unsatisfied SLA timer.
type CivicSLABreachReport struct {
	TenantID   string `json:"tenant_id"`
	TenantSlug string `json:"tenant_slug"`
	Ref        string `json:"ref"`
	// Kind is CivicBreachKindAck | CivicBreachKindResolve.
	Kind string `json:"kind"`
	// MDAQueue is the dispatch-endpoint key booking-service notifies via
	// the W11 incident delivery path.
	MDAQueue string `json:"mda_queue,omitempty"`
}

// PacedCivicStatusSend carries the SendCivicStatusUpdate arguments
// (SPEC-W32 §3 WS-B). The JSON contract is duplicated by any civic
// producer (service boundary: duplicated, not shared).
type PacedCivicStatusSend struct {
	TenantID   string `json:"tenant_id,omitempty"`
	TenantSlug string `json:"tenant_slug"`
	// Ref is the case reference the message names — the CANONICAL ref when
	// the case was merged (SPEC-W32 §4.3).
	Ref string `json:"ref"`
	// Status is the new lifecycle status (triaged|assigned|in_progress|
	// resolved|closed).
	Status  string `json:"status"`
	Channel string `json:"channel"` // sms (default) | whatsapp | telegram
	Phone   string `json:"phone"`
	// Text is the rendered message: "Case {ref}: now {status}".
	Text string `json:"text"`
}

// CivicStatusNotifyInput starts a CivicStatusNotifyWorkflow.
type CivicStatusNotifyInput struct {
	TenantID   string `json:"tenant_id"`
	TenantSlug string `json:"tenant_slug"`
	Ref        string `json:"ref"`
	// CanonicalRef overrides Ref in the message text + ledger when the case
	// was merged (empty = Ref).
	CanonicalRef string `json:"canonical_ref,omitempty"`
	Status       string `json:"status"`
	Phone        string `json:"phone"`
	Channel      string `json:"channel"` // sms (default) | whatsapp | telegram
}

// EffectiveRef is the reference the notification names: the canonical ref
// after a merge, the case's own ref otherwise.
func (in CivicStatusNotifyInput) EffectiveRef() string {
	if in.CanonicalRef != "" {
		return in.CanonicalRef
	}
	return in.Ref
}

// civicAckSatisfied reports whether a new status satisfies the ack SLA.
func civicAckSatisfied(status string) bool {
	switch status {
	case "triaged", "assigned", "in_progress", "resolved", "closed":
		return true
	}
	return false
}

// civicResolveSatisfied reports whether a new status satisfies the resolve SLA.
func civicResolveSatisfied(status string) bool {
	return status == "resolved" || status == "closed"
}

// civicActivityOptions is the shared activity configuration of both civic
// workflows (mirrors PacedSendWorkflow's policy).
func civicActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		HeartbeatTimeout:    30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    3,
		},
	}
}

// CivicSLAWorkflow holds the ack/resolve SLA timers of one civic case.
// Continue-as-new safe: a case is short-lived and the workflow carries no
// growing history (two timers + bounded signals), so a long-running case
// simply finishes on resolved/closed/merged; a ReportReceived redelivery
// restarts the deterministic ID, which Temporal rejects while a run is
// open and re-runs (idempotent timers) after a completed one.
func CivicSLAWorkflow(ctx workflow.Context, in CivicSLAInput) (CivicSLAState, error) {
	ctx = workflow.WithActivityOptions(ctx, civicActivityOptions())
	state := CivicSLAState{Ref: in.Ref, CanonicalRef: in.Ref}
	log := workflow.GetLogger(ctx)

	statusCh := workflow.GetSignalChannel(ctx, SignalCivicStatus)
	mergedCh := workflow.GetSignalChannel(ctx, SignalCivicMerged)

	selector := workflow.NewSelector(ctx)

	// Timer bookkeeping. ackOpen/resolveOpen mark a timer whose outcome is
	// still of interest (armed and neither satisfied nor breach-reported).
	// armAck/armResolve (re-)arm a timer for a NEW due time: the superseded
	// timer is cancelled, and a generation counter + the future's Get error
	// keep a cancelled/superseded timer from ever firing the breach path.
	var cancelAck, cancelResolve workflow.CancelFunc
	ackGen, resolveGen := 0, 0
	ackOpen, resolveOpen := false, false
	ackFired, resolveFired := false, false

	armAck := func(due time.Time) {
		if cancelAck != nil {
			cancelAck()
		}
		ackGen++
		gen := ackGen
		tctx, cancel := workflow.WithCancel(ctx)
		cancelAck = cancel
		delay := due.Sub(workflow.Now(ctx))
		if delay < 0 {
			delay = 0 // already due: breach promptly, never sleep negative
		}
		ackOpen = true
		selector.AddFuture(workflow.NewTimer(tctx, delay), func(f workflow.Future) {
			if f.Get(ctx, nil) == nil && gen == ackGen {
				ackFired = true
			}
		})
	}
	armResolve := func(due time.Time) {
		if cancelResolve != nil {
			cancelResolve()
		}
		resolveGen++
		gen := resolveGen
		tctx, cancel := workflow.WithCancel(ctx)
		cancelResolve = cancel
		delay := due.Sub(workflow.Now(ctx))
		if delay < 0 {
			delay = 0
		}
		resolveOpen = true
		selector.AddFuture(workflow.NewTimer(tctx, delay), func(f workflow.Future) {
			if f.Get(ctx, nil) == nil && gen == resolveGen {
				resolveFired = true
			}
		})
	}

	if !in.AckDueAt.IsZero() {
		armAck(in.AckDueAt)
	}
	if !in.ResolveDueAt.IsZero() {
		armResolve(in.ResolveDueAt)
	}

	var statusSig CivicStatusSignal
	var mergedSig CivicMergedSignal
	gotStatus, gotMerged := false, false
	selector.AddReceive(statusCh, func(ch workflow.ReceiveChannel, _ bool) {
		ch.Receive(ctx, &statusSig)
		gotStatus = true
	})
	selector.AddReceive(mergedCh, func(ch workflow.ReceiveChannel, _ bool) {
		ch.Receive(ctx, &mergedSig)
		gotMerged = true
	})

	for {
		if state.Merged || state.Closed || (!ackOpen && !resolveOpen) {
			return state, nil
		}
		selector.Select(ctx)

		if gotMerged {
			gotMerged = false
			state.Merged = true
			if mergedSig.CanonicalRef != "" {
				state.CanonicalRef = mergedSig.CanonicalRef
			}
			// The canonical case's own SLA workflow owns the SLA now.
			if cancelAck != nil {
				cancelAck()
			}
			if cancelResolve != nil {
				cancelResolve()
			}
			log.Info("civic case merged; SLA timers cancelled",
				"ref", in.Ref, "canonical_ref", state.CanonicalRef)
			continue
		}
		if gotStatus {
			gotStatus = false
			log.Info("civic case status", "ref", in.Ref, "status", statusSig.Status)
			if civicAckSatisfied(statusSig.Status) && !state.Acked {
				state.Acked = true
				if ackOpen {
					cancelAck()
					ackOpen = false
				}
			}
			if civicResolveSatisfied(statusSig.Status) && !state.Resolved {
				state.Resolved = true
				if resolveOpen {
					cancelResolve()
					resolveOpen = false
				}
			}
			if statusSig.Status == "closed" {
				state.Closed = true
				if cancelAck != nil {
					cancelAck()
				}
				if cancelResolve != nil {
					cancelResolve()
				}
			}
			// SPEC-W32 W3: booking recomputes the SLA dues at triage
			// (category change → different SLA); a non-nil due re-arms the
			// corresponding pending timer. A past due fires the breach path
			// promptly (arm* clamps the delay to 0). Breach-reported timers
			// are never re-armed.
			if statusSig.AckDueAt != nil && !state.Acked && !state.AckBreached && !state.Closed && !state.Merged {
				in.AckDueAt = *statusSig.AckDueAt
				log.Info("re-arming ack SLA timer", "ref", in.Ref, "ack_due_at", in.AckDueAt.String())
				armAck(in.AckDueAt)
			}
			if statusSig.ResolveDueAt != nil && !state.Resolved && !state.ResolveBreached && !state.Closed && !state.Merged {
				in.ResolveDueAt = *statusSig.ResolveDueAt
				log.Info("re-arming resolve SLA timer", "ref", in.Ref, "resolve_due_at", in.ResolveDueAt.String())
				armResolve(in.ResolveDueAt)
			}
		}
		if ackFired {
			ackFired = false
			ackOpen = false
			if !state.Acked && !state.AckBreached {
				state.AckBreached = true
				rep := CivicSLABreachReport{
					TenantID: in.TenantID, TenantSlug: in.TenantSlug,
					Ref: state.CanonicalRef, Kind: CivicBreachKindAck, MDAQueue: in.MDAQueue,
				}
				if err := workflow.ExecuteActivity(ctx, ActivityReportCivicSLABreach, rep).Get(ctx, nil); err != nil {
					return state, fmt.Errorf("report ack SLA breach: %w", err)
				}
			}
		}
		if resolveFired {
			resolveFired = false
			resolveOpen = false
			if !state.Resolved && !state.ResolveBreached {
				state.ResolveBreached = true
				rep := CivicSLABreachReport{
					TenantID: in.TenantID, TenantSlug: in.TenantSlug,
					Ref: state.CanonicalRef, Kind: CivicBreachKindResolve, MDAQueue: in.MDAQueue,
				}
				if err := workflow.ExecuteActivity(ctx, ActivityReportCivicSLABreach, rep).Get(ctx, nil); err != nil {
					return state, fmt.Errorf("report resolve SLA breach: %w", err)
				}
			}
		}
	}
}

// CivicStatusNotifyWorkflow delivers one citizen status update:
// "Case {ref}: now {status}" via the paced-send machinery. TRANSACTIONAL
// class → the DND guard passes it untouched; quiet hours (20:00-08:00
// tenant tz, SPEC-W12 §8 contract default) still HOLD it — the workflow
// durably Sleeps until the window opens, then sends. Every send attempt
// lands in the civic delivery ledger (activity-side).
func CivicStatusNotifyWorkflow(ctx workflow.Context, in CivicStatusNotifyInput) (PacedSendResult, error) {
	var res PacedSendResult
	if in.Phone == "" {
		return res, fmt.Errorf("civic status notify: phone is required (case %s)", in.EffectiveRef())
	}
	ctx = workflow.WithActivityOptions(ctx, civicActivityOptions())
	// Contract defaults (20:00-08:00 Africa/Lagos, SPEC-W12 §8), exactly
	// like PacedSendWorkflow: the civic events topic carries no per-tenant
	// override.
	quiet := QuietHoursFromEnv("", "", nil)

	channel := in.Channel
	if channel == "" {
		channel = "sms"
	}
	// Transactional civic updates respect quiet hours (SPEC-W32 §3 WS-B) —
	// a workflow-side hold, since the transactional classification itself
	// never defers.
	open, inWindow, err := pacer.QuietHoursOpenAt(workflow.Now(ctx), channel, quiet)
	if err != nil {
		return res, err
	}
	if inWindow {
		delay := open.Sub(workflow.Now(ctx))
		if delay > 0 {
			workflow.GetLogger(ctx).Info("quiet hours: holding civic status update until window opens",
				"ref", in.EffectiveRef(), "status", in.Status,
				"window_open", open.String(), "delay", delay.String())
			if err := workflow.Sleep(ctx, delay); err != nil {
				return res, err
			}
		}
	}

	send := PacedSendRequest{
		Kind: PacedSendCivicStatus,
		Civic: &PacedCivicStatusSend{
			TenantID:   in.TenantID,
			TenantSlug: in.TenantSlug,
			Ref:        in.EffectiveRef(),
			Status:     in.Status,
			Channel:    channel,
			Phone:      in.Phone,
			Text:       fmt.Sprintf("Case %s: now %s", in.EffectiveRef(), in.Status),
		},
	}
	return GuardedPacedSend(ctx, send, quiet)
}
