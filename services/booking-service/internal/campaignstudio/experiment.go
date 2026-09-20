package campaignstudio

// A/B experiment wiring against the model-registry (SPEC-W45 U6, work
// order H-4): journey send steps may carry experiment_id; at send time the
// StudioSendWorkflow resolves the recipient's arm via the registry and the
// variant is recorded on the send outcome; the conversion webhook reports
// the outcome back to the registry.
//
//   - Assignment POSTs {MODEL_REGISTRY_URL}/v1/registry/experiments/{id}/assignment
//     with X-Internal-Token (MODEL_REGISTRY_INTERNAL_TOKEN — the registry's
//     experiments routes are internal-token gated, SPEC-W45 K23). The
//     registry MAY 404 this POST (its canonical assignment read is the GET
//     form — the POST is the forward contract): assignment is FAIL-OPEN —
//     any 404 / unset URL / registry outage / malformed answer resolves to
//     the control arm "champion" with a WARN log, because a broken
//     experiment rail must never block a marketing send.
//   - Outcome reporting POSTs /v1/registry/experiments/{id}/outcomes and is
//     FAIL-CLOSED: a failed report is an error to the caller (the webhook
//     answers 502) so conversion signal is never silently dropped.
//
// Outcome shape: the registry's OutcomeRequest validates
// assigned_arm ∈ {champion,challenger} and carries a MODEL outcome
// (predicted_label/predicted_score/true_label). A marketing conversion is
// not a model prediction, so the report is NEUTRAL — predicted_label=1,
// predicted_score=0.5 (an uninformative score that does not bias the
// arm precision/recall curves) — and the real conversion signal travels in
// true_label (1 = converted, 0 = not converted).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Experiment arms (model-registry contract).
const (
	ControlArm    = "champion"
	ChallengerArm = "challenger"
)

// registryHTTPTimeout bounds one registry call.
const registryHTTPTimeout = 5 * time.Second

// RegistryClient is the model-registry experiments client.
type RegistryClient struct {
	// BaseURL is MODEL_REGISTRY_URL (no trailing slash). Empty → every
	// assignment fails open to the control arm; outcome reports error.
	BaseURL string
	// InternalToken is MODEL_REGISTRY_INTERNAL_TOKEN (X-Internal-Token,
	// SPEC-W45 K23).
	InternalToken string
	// HTTPClient is injectable for tests; nil → a 5s-timeout default.
	HTTPClient *http.Client
	Log        *zap.Logger
}

func (c *RegistryClient) log() *zap.Logger {
	if c != nil && c.Log != nil {
		return c.Log
	}
	return zap.NewNop()
}

func (c *RegistryClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: registryHTTPTimeout}
}

// RegistryFromEnv builds the client from MODEL_REGISTRY_URL /
// MODEL_REGISTRY_INTERNAL_TOKEN. Nil when MODEL_REGISTRY_URL is unset —
// assignment then fails open to the control arm activity-side and the
// conversion webhook skips outcome reporting (both logged, never fatal).
func RegistryFromEnv(log *zap.Logger) *RegistryClient {
	if log == nil {
		log = zap.NewNop()
	}
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("MODEL_REGISTRY_URL")), "/")
	if base == "" {
		log.Warn("MODEL_REGISTRY_URL unset — campaign-studio experiment assignment fails open to the control arm; conversion outcome reporting is skipped")
		return nil
	}
	if os.Getenv("MODEL_REGISTRY_INTERNAL_TOKEN") == "" {
		log.Warn("MODEL_REGISTRY_INTERNAL_TOKEN unset — model-registry experiment calls will 401 once K23 enforcement is on (assignment fails open; outcome reports fail)")
	}
	return &RegistryClient{
		BaseURL:       base,
		InternalToken: os.Getenv("MODEL_REGISTRY_INTERNAL_TOKEN"),
		Log:           log,
	}
}

// assignmentRequest is the POST .../experiments/{id}/assignment body.
type assignmentRequest struct {
	TenantID uuid.UUID `json:"tenant_id"`
	PersonID string    `json:"person_id"`
}

// assignmentResponse is the tolerated answer shape (arm is what matters).
type assignmentResponse struct {
	Arm string `json:"arm"`
}

// AssignVariant resolves one recipient's experiment arm. FAIL-OPEN: unset
// URL, transport errors, non-2xx (including the 404 the registry may
// answer for the POST form) and malformed/unknown arms all resolve to
// ControlArm with a WARN — the send itself must never be gated on the
// experiment rail.
func (c *RegistryClient) AssignVariant(ctx context.Context, experimentID, tenantID uuid.UUID, personID string) string {
	failOpen := func(reason string, err error) string {
		c.log().Warn("experiment assignment failed; failing open to control arm",
			zap.String("experiment_id", experimentID.String()),
			zap.String("reason", reason), zap.Error(err))
		return ControlArm
	}
	if c == nil || c.BaseURL == "" {
		return failOpen("registry not configured", nil)
	}
	body, err := json.Marshal(assignmentRequest{TenantID: tenantID, PersonID: personID})
	if err != nil {
		return failOpen("marshal", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/v1/registry/experiments/"+experimentID.String()+"/assignment", bytes.NewReader(body))
	if err != nil {
		return failOpen("request build", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.InternalToken != "" {
		req.Header.Set("X-Internal-Token", c.InternalToken)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return failOpen("transport", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return failOpen(fmt.Sprintf("registry status %d", resp.StatusCode), nil)
	}
	var out assignmentResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return failOpen("decode", err)
	}
	switch out.Arm {
	case ControlArm, ChallengerArm:
		return out.Arm
	default:
		return failOpen("unknown arm "+fmt.Sprintf("%q", out.Arm), nil)
	}
}

// outcomeRequest mirrors the registry's OutcomeRequest (service boundary:
// duplicated, not shared).
type outcomeRequest struct {
	TenantID       uuid.UUID `json:"tenant_id"`
	PersonID       string    `json:"person_id"`
	AssignedArm    string    `json:"assigned_arm"` // champion | challenger
	PredictedLabel int       `json:"predicted_label"`
	PredictedScore float64   `json:"predicted_score"`
	TrueLabel      int       `json:"true_label"`
}

// ReportOutcome posts one conversion outcome to the registry. FAIL-CLOSED:
// any failure (unset URL, transport, non-2xx, invalid arm) is an error —
// conversion signal must never be dropped silently. The prediction fields
// are neutral (1, 0.5); the conversion signal travels in true_label.
func (c *RegistryClient) ReportOutcome(ctx context.Context, experimentID, tenantID uuid.UUID, personID, assignedArm string, converted bool) error {
	if c == nil || c.BaseURL == "" {
		return fmt.Errorf("report outcome: registry not configured")
	}
	if assignedArm != ControlArm && assignedArm != ChallengerArm {
		return fmt.Errorf("report outcome: assigned_arm %q (want champion|challenger)", assignedArm)
	}
	trueLabel := 0
	if converted {
		trueLabel = 1
	}
	body, err := json.Marshal(outcomeRequest{
		TenantID:       tenantID,
		PersonID:       personID,
		AssignedArm:    assignedArm,
		PredictedLabel: 1,   // neutral: not a model prediction …
		PredictedScore: 0.5, // … uninformative score, does not bias the curves
		TrueLabel:      trueLabel,
	})
	if err != nil {
		return fmt.Errorf("report outcome: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/v1/registry/experiments/"+experimentID.String()+"/outcomes", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("report outcome: request build: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.InternalToken != "" {
		req.Header.Set("X-Internal-Token", c.InternalToken)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("report outcome: transport: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("report outcome: registry status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}
