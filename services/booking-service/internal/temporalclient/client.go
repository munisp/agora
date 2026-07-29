// Package temporalclient wraps the Temporal Go SDK client used to start the
// BookingSagaWorkflow (hosted by notification-worker, SPEC §6).
package temporalclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/geo"
	"github.com/opendesk/booking-service/internal/incidents"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

// WorkflowType is the registered name of the booking saga workflow.
const WorkflowType = "BookingSagaWorkflow"

// GDPR workflow types (hosted by notification-worker, SPEC-W3 §2).
const (
	GdprExportWorkflowType = "GdprExportWorkflow"
	GdprEraseWorkflowType  = "GdprEraseWorkflow"
)

// GdprRequest is the input contract of GdprExportWorkflow/GdprEraseWorkflow
// (mirrors notification-worker's workflows.GdprInput — JSON-compatible,
// duplicated per service-boundary rules).
type GdprRequest struct {
	TenantID   string `json:"tenant_id"`
	TenantSlug string `json:"tenant_slug"`
	Phone      string `json:"phone,omitempty"`
	Email      string `json:"email,omitempty"`
}

// Client implements bookingops.SagaStarter against a Temporal server.
type Client struct {
	tc        client.Client
	taskQueue string
}

// Dial connects to Temporal (host:port, namespace, task queue per SPEC §6).
func Dial(hostPort, namespace, taskQueue string) (*Client, error) {
	tc, err := client.Dial(client.Options{
		HostPort:  hostPort,
		Namespace: namespace,
	})
	if err != nil {
		return nil, fmt.Errorf("dial temporal: %w", err)
	}
	return &Client{tc: tc, taskQueue: taskQueue}, nil
}

// Close releases the underlying connection.
func (c *Client) Close() { c.tc.Close() }

// Underlying exposes the raw SDK client so main can run a worker on the
// same connection (GeoCampaignWorkflow host, SPEC-W8 A2).
func (c *Client) Underlying() client.Client { return c.tc }

// StartGeoCampaign starts GeoCampaignWorkflow (hosted by booking-service's
// own worker on the same task queue) with workflow ID
// "geo-campaign-{campaignID}" so duplicate launches are idempotent.
// Implements geo.CampaignStarter.
func (c *Client) StartGeoCampaign(ctx context.Context, in geo.GeoCampaignInput) (string, error) {
	id := "geo-campaign-" + in.CampaignID
	opts := client.StartWorkflowOptions{
		ID:                    id,
		TaskQueue:             c.taskQueue,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
	}
	_, err := c.tc.ExecuteWorkflow(ctx, opts, geo.WorkflowType, in)
	if err != nil {
		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &alreadyStarted) {
			return id, nil
		}
		return "", fmt.Errorf("execute %s: %w", geo.WorkflowType, err)
	}
	return id, nil
}

// StartBookingSaga starts BookingSagaWorkflow with workflow ID
// "booking-saga-{bookingID}" so duplicate starts are idempotent.
func (c *Client) StartBookingSaga(ctx context.Context, in bookingops.SagaInput) (string, error) {
	opts := client.StartWorkflowOptions{
		ID:        "booking-saga-" + in.BookingID,
		TaskQueue: c.taskQueue,
		// A duplicate workflow ID for the same booking means the saga is
		// already running — treat as success below.
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
	}
	run, err := c.tc.ExecuteWorkflow(ctx, opts, WorkflowType, in)
	if err != nil {
		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &alreadyStarted) {
			return "already-started", nil
		}
		return "", fmt.Errorf("execute %s: %w", WorkflowType, err)
	}
	return run.GetRunID(), nil
}

// gdprWorkflowID derives a deterministic workflow ID so repeated export/erase
// requests for the same contact are idempotent (reject-duplicate policy).
func gdprWorkflowID(kind string, in GdprRequest) string {
	sum := sha256.Sum256([]byte(in.TenantID + "|" + in.Phone + "|" + in.Email))
	return fmt.Sprintf("gdpr-%s-%s-%s", kind, in.TenantID, hex.EncodeToString(sum[:])[:12])
}

// StartGdprExport starts GdprExportWorkflow; returns the workflow ID.
func (c *Client) StartGdprExport(ctx context.Context, in GdprRequest) (string, error) {
	return c.startGdpr(ctx, GdprExportWorkflowType, "export", in)
}

// StartGdprErase starts GdprEraseWorkflow; returns the workflow ID.
func (c *Client) StartGdprErase(ctx context.Context, in GdprRequest) (string, error) {
	return c.startGdpr(ctx, GdprEraseWorkflowType, "erase", in)
}

// WebhookDeliveryWorkflowType is the Wave-5 outbound webhook delivery
// workflow (hosted by notification-worker), reused for incident dispatch
// with payload type "incident" (SPEC-W11 Part B §4).
const WebhookDeliveryWorkflowType = "WebhookDeliveryWorkflow"

// StartIncidentDelivery starts one WebhookDeliveryWorkflow for an incident
// dispatch delivery with workflow ID "incident-delivery-{deliveryID}" so
// duplicate starts (re-dispatch of the same incident×endpoint) are
// idempotent. Implements incidents.Starter.
func (c *Client) StartIncidentDelivery(ctx context.Context, in incidents.DeliveryStart) (string, error) {
	id := "incident-delivery-" + in.DeliveryID
	opts := client.StartWorkflowOptions{
		ID:                    id,
		TaskQueue:             c.taskQueue,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
	}
	_, err := c.tc.ExecuteWorkflow(ctx, opts, WebhookDeliveryWorkflowType, in)
	if err != nil {
		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &alreadyStarted) {
			return id, nil
		}
		return "", fmt.Errorf("execute %s: %w", WebhookDeliveryWorkflowType, err)
	}
	return id, nil
}

// StartIncidentAlert starts IncidentAlertWorkflow (hosted by
// booking-service's own worker) with workflow ID
// "incident-alert-{incidentID}" so a re-ingested incident does not
// double-alert. Implements incidents.Starter.
func (c *Client) StartIncidentAlert(ctx context.Context, in incidents.AlertStart) (string, error) {
	id := "incident-alert-" + in.IncidentID
	opts := client.StartWorkflowOptions{
		ID:                    id,
		TaskQueue:             c.taskQueue,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
	}
	_, err := c.tc.ExecuteWorkflow(ctx, opts, incidents.WorkflowTypeAlert, in)
	if err != nil {
		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &alreadyStarted) {
			return id, nil
		}
		return "", fmt.Errorf("execute %s: %w", incidents.WorkflowTypeAlert, err)
	}
	return id, nil
}

func (c *Client) startGdpr(ctx context.Context, wfType, kind string, in GdprRequest) (string, error) {
	id := gdprWorkflowID(kind, in)
	opts := client.StartWorkflowOptions{
		ID:                    id,
		TaskQueue:             c.taskQueue,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
	}
	_, err := c.tc.ExecuteWorkflow(ctx, opts, wfType, in)
	if err != nil {
		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &alreadyStarted) {
			return id, nil
		}
		return "", fmt.Errorf("execute %s: %w", wfType, err)
	}
	return id, nil
}
