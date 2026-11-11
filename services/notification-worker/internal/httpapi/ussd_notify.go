package httpapi

// SPEC-W47-C (UIMOB-06): USSD menu-action confirmation SMS intake.
//
// messaging-gateway terminates the Africa's Talking USSD session and, when
// the tenant pack menu selection carries an action (book / status /
// handoff / info), dispatches the action and delivers the promised
// confirmation SMS through THIS service — the existing paced send path
// (PacedSendWorkflow → NotifyPaced: CPS pacing + sender rotation; kind
// ussd_action is transactional, so the user-requested confirmation is
// never DND-suppressed or quiet-hours deferred).
//
// Security: machine-only route (K2) — X-Internal-Token ==
// NOTIFICATION_INTERNAL_TOKEN, constant-time, fail-closed when unset
// (requireInternalToken), exactly like /v1/signals. The gateway is a
// sidecar-less in-cluster caller (SPEC-W45 ORPH O2), so the command rides
// HTTP service invocation, not the Kafka outbox.
//
// Idempotency: the workflow ID derives from session_id + action, so a
// retried USSD callback (Africa's Talking at-least-once) hits
// WorkflowExecutionAlreadyStarted and never sends twice.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/opendesk/notification-worker/internal/workflows"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.uber.org/zap"
)

// ussdActionNotifyRequest is the body of POST /v1/notify/ussd-action.
type ussdActionNotifyRequest struct {
	TenantSlug string `json:"tenant_slug"`
	Phone      string `json:"phone"` // E.164 recipient (the USSD caller)
	Text       string `json:"text"`  // rendered confirmation/info text
	Action     string `json:"action"`
	SessionID  string `json:"session_id"` // provider USSD session id (idempotency anchor)
}

// sendUSSDActionConfirmation serves POST /v1/notify/ussd-action: starts one
// PacedSendWorkflow (kind ussd_action) for the confirmation SMS.
func (s *Server) sendUSSDActionConfirmation(w http.ResponseWriter, r *http.Request) {
	if s.Temporal == nil {
		s.Log.Error("ussd-action notify hit but no Temporal client is wired (fail-closed 503)")
		http.Error(w, `{"error":"notification pipeline not configured"}`, http.StatusServiceUnavailable)
		return
	}
	var req ussdActionNotifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}
	req.TenantSlug = strings.TrimSpace(req.TenantSlug)
	req.Phone = strings.TrimSpace(req.Phone)
	req.Text = strings.TrimSpace(req.Text)
	req.Action = strings.TrimSpace(req.Action)
	req.SessionID = strings.TrimSpace(req.SessionID)
	if req.TenantSlug == "" || req.Phone == "" || req.Text == "" {
		http.Error(w, `{"error":"tenant_slug, phone and text are required"}`, http.StatusBadRequest)
		return
	}
	workflowID := "ussd-action-" + req.SessionID + "-" + req.Action
	if req.SessionID == "" || req.Action == "" {
		// Callers are contract-bound to carry both; a missing anchor falls
		// back to a random id (at-least-once beats dropping; documented
		// redelivery risk — same posture as the notifyoutbox consumer).
		workflowID = "ussd-action-" + uuid.NewString()
	}
	_, err := s.Temporal.ExecuteWorkflow(r.Context(), client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: s.TaskQueue,
	}, workflows.WorkflowTypePacedSend, workflows.PacedSendRequest{
		Kind: workflows.PacedSendUSSDAction,
		USSDAction: &workflows.PacedUSSDActionSend{
			TenantSlug: req.TenantSlug,
			Phone:      req.Phone,
			Text:       req.Text,
			Action:     req.Action,
		},
	})
	if err != nil {
		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &alreadyStarted) || strings.Contains(err.Error(), "already started") {
			// Idempotent replay of the same session+action: no second SMS.
			writeJSON(w, http.StatusOK, map[string]any{"workflow_id": workflowID, "duplicate": true})
			return
		}
		s.Log.Error("start ussd-action confirmation workflow", zap.Error(err))
		http.Error(w, `{"error":"failed to start send workflow"}`, http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"workflow_id": workflowID})
}
