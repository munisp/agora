// GDPR data-subject workflows (SPEC-W3 §2 innovation 13):
//
//   - GdprExportWorkflow gathers everything the platform holds about a data
//     subject (identified by phone and/or e-mail) across booking,
//     conversation, payments (ledger) and the CRM, packs it into one JSON
//     bundle and uploads it to the MinIO `exports` bucket via a plain S3 PUT.
//     The workflow result is the object path (dev) — a presigned URL can be
//     issued from the same object in prod.
//   - GdprEraseWorkflow publishes a PrivacyEraseRequested tombstone
//     CloudEvent to opendesk.privacy.events (via Dapr pubsub); booking,
//     conversation and crm-sync consume it and anonymize/delete their copy
//     of the subject's data.
//
// Both workflows are started by booking-service's POST /v1/privacy/{export,
// erase} endpoints (manage_bookings, SPEC-W3 §2).
package workflows

import (
	"encoding/json"
	"time"

	"go.temporal.io/sdk/workflow"
)

// GdprInput is the input contract of GdprExportWorkflow/GdprEraseWorkflow.
// It mirrors booking-service's temporalclient.GdprRequest (JSON-compatible;
// duplicated per service-boundary rules).
type GdprInput struct {
	TenantID   string `json:"tenant_id"`
	TenantSlug string `json:"tenant_slug"`
	Phone      string `json:"phone,omitempty"`
	Email      string `json:"email,omitempty"`
}

// Contact returns the primary contact reference (phone preferred).
func (in GdprInput) Contact() string {
	if in.Phone != "" {
		return in.Phone
	}
	return in.Email
}

// GDPR activity names (registered by cmd/worker).
const (
	ActivityGdprCollectBookings      = "GdprCollectBookings"
	ActivityGdprCollectConversations = "GdprCollectConversations"
	ActivityGdprCollectLedger        = "GdprCollectLedger"
	ActivityGdprCollectCrmPerson     = "GdprCollectCrmPerson"
	ActivityGdprUploadExport         = "GdprUploadExport"
	ActivityGdprPublishErase         = "GdprPublishEraseTombstone"
)

// JSON-LD context versioning (SPEC-W54 scope B):
//
//   - GdprExportContextIRI identifies the versioned inline @context of the
//     export bundle. Bump the /vN suffix on ANY change to the term mapping
//     below (same discipline as ontology/agora.ttl's version IRI).
//   - AgoraOntologyNamespace is the agora ontology term namespace — the same
//     `agora:` namespace as ontology/agora.ttl (SPEC-W54 scope A; placeholder
//     namespace). Terms referenced here MUST exist in agora.ttl.
const (
	GdprExportContextIRI   = "https://agora.example/context/v1"
	AgoraOntologyNamespace = "https://agora.example/ontology#"
)

// GdprContextTerm is one JSON-LD term definition in the bundle @context.
type GdprContextTerm struct {
	ID        string `json:"@id"`
	Container string `json:"@container,omitempty"`
}

// GdprBundleContext is the inline JSON-LD @context object of the export
// bundle: every top-level bundle key is mapped onto an ontology/agora.ttl or
// schema.org term so the export is self-describing linked data. Field order
// here is the marshal order (struct → deterministic bytes).
type GdprBundleContext struct {
	// ContextVersion carries GdprExportContextIRI as a term definition so
	// the context version is machine-readable from the bundle itself.
	ContextVersion string          `json:"context_version"`
	Agora          string          `json:"agora"`
	Schema         string          `json:"schema"`
	Subject        GdprContextTerm `json:"subject"`
	Bookings       GdprContextTerm `json:"bookings"`
	Conversations  GdprContextTerm `json:"conversations"`
	LedgerBalance  GdprContextTerm `json:"ledger_balance"`
	CrmPerson      GdprContextTerm `json:"crm_person"`
}

// GdprJSONLDContext returns the current (v1) inline @context for the export
// bundle. Pure constant — safe for Temporal workflow determinism.
func GdprJSONLDContext() GdprBundleContext {
	return GdprBundleContext{
		ContextVersion: GdprExportContextIRI,
		Agora:          AgoraOntologyNamespace,
		Schema:         "https://schema.org/",
		Subject:        GdprContextTerm{ID: "schema:Person"},
		Bookings:       GdprContextTerm{ID: "schema:Reservation", Container: "@set"},
		Conversations:  GdprContextTerm{ID: "agora:Conversation", Container: "@set"},
		LedgerBalance:  GdprContextTerm{ID: "agora:LedgerBalance"},
		CrmPerson:      GdprContextTerm{ID: "schema:Person"},
	}
}

// GdprExportBundle is the JSON document uploaded to MinIO.
type GdprExportBundle struct {
	// Context is the JSON-LD @context key (SPEC-W54 scope B — additive:
	// every other field is unchanged in name/type/semantics).
	Context       GdprBundleContext `json:"@context"`
	Subject       GdprInput         `json:"subject"`
	Bookings      json.RawMessage   `json:"bookings"`
	Conversations json.RawMessage   `json:"conversations"`
	// Knowledge is intentionally absent: knowledge documents are tenant
	// content, not data-subject PII (SPEC-W3 §2: "knowledge n/a").
	LedgerBalance json.RawMessage `json:"ledger_balance"`
	CrmPerson     json.RawMessage `json:"crm_person"`
}

// GdprExportWorkflow runs the cross-service collection and returns the
// MinIO object path of the uploaded bundle.
func GdprExportWorkflow(ctx workflow.Context, in GdprInput) (string, error) {
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 45 * time.Second,
	}
	ctx = workflow.WithActivityOptions(ctx, ao)

	var bundle GdprExportBundle
	bundle.Context = GdprJSONLDContext() // constant value — deterministic
	bundle.Subject = in
	if err := workflow.ExecuteActivity(ctx, ActivityGdprCollectBookings, in).Get(ctx, &bundle.Bookings); err != nil {
		return "", err
	}
	if err := workflow.ExecuteActivity(ctx, ActivityGdprCollectConversations, in).Get(ctx, &bundle.Conversations); err != nil {
		return "", err
	}
	if err := workflow.ExecuteActivity(ctx, ActivityGdprCollectLedger, in).Get(ctx, &bundle.LedgerBalance); err != nil {
		return "", err
	}
	if err := workflow.ExecuteActivity(ctx, ActivityGdprCollectCrmPerson, in).Get(ctx, &bundle.CrmPerson); err != nil {
		return "", err
	}

	var path string
	if err := workflow.ExecuteActivity(ctx, ActivityGdprUploadExport, bundle).Get(ctx, &path); err != nil {
		return "", err
	}
	return path, nil
}

// GdprEraseWorkflow publishes the erase tombstone; downstream consumers do
// the actual anonymization/deletion asynchronously.
func GdprEraseWorkflow(ctx workflow.Context, in GdprInput) error {
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
	}
	ctx = workflow.WithActivityOptions(ctx, ao)
	return workflow.ExecuteActivity(ctx, ActivityGdprPublishErase, in).Get(ctx, nil)
}
