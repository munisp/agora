// GDPR worker registration (SPEC-W45 K19 / ORPH O1): GdprExportWorkflow,
// GdprEraseWorkflow and the six Gdpr* activities whose names are the
// contract of workflows/gdpr.go:45-50. Extracted behind the tiny registry
// seam so a unit test can assert the registration set without a Temporal
// server.
package main

import (
	"github.com/opendesk/notification-worker/internal/activities"
	"github.com/opendesk/notification-worker/internal/workflows"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/workflow"
)

// gdprRegistry is the registration surface used by registerGdpr
// (worker.Worker satisfies it; tests capture with a fake).
type gdprRegistry interface {
	RegisterWorkflowWithOptions(w any, options workflow.RegisterOptions)
	RegisterActivityWithOptions(a any, options activity.RegisterOptions)
}

// registerGdpr hosts the GDPR data-subject workflows + activities on the
// worker. Activity names are fixed by workflows/gdpr.go — the workflows
// execute them BY NAME, so a rename here silently breaks every export.
func registerGdpr(r gdprRegistry, acts *activities.Activities) {
	r.RegisterWorkflowWithOptions(workflows.GdprExportWorkflow, workflow.RegisterOptions{Name: "GdprExportWorkflow"})
	r.RegisterWorkflowWithOptions(workflows.GdprEraseWorkflow, workflow.RegisterOptions{Name: "GdprEraseWorkflow"})

	r.RegisterActivityWithOptions(acts.GdprCollectBookings, activity.RegisterOptions{Name: workflows.ActivityGdprCollectBookings})
	r.RegisterActivityWithOptions(acts.GdprCollectConversations, activity.RegisterOptions{Name: workflows.ActivityGdprCollectConversations})
	r.RegisterActivityWithOptions(acts.GdprCollectLedger, activity.RegisterOptions{Name: workflows.ActivityGdprCollectLedger})
	r.RegisterActivityWithOptions(acts.GdprCollectCrmPerson, activity.RegisterOptions{Name: workflows.ActivityGdprCollectCrmPerson})
	r.RegisterActivityWithOptions(acts.GdprUploadExport, activity.RegisterOptions{Name: workflows.ActivityGdprUploadExport})
	r.RegisterActivityWithOptions(acts.GdprPublishEraseTombstone, activity.RegisterOptions{Name: workflows.ActivityGdprPublishErase})
}
