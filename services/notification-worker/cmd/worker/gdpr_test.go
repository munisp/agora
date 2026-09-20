// SPEC-W45 K19 regression guard: the GDPR workflows + all six Gdpr*
// activities must be registered on the worker (ORPH O1 — without this the
// booking /v1/privacy endpoints 202 into a Temporal void).
package main

import (
	"testing"

	"github.com/opendesk/notification-worker/internal/activities"
	"github.com/opendesk/notification-worker/internal/workflows"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/workflow"
)

type fakeGdprRegistry struct {
	workflows  []string
	activities []string
}

func (f *fakeGdprRegistry) RegisterWorkflowWithOptions(_ any, options workflow.RegisterOptions) {
	f.workflows = append(f.workflows, options.Name)
}

func (f *fakeGdprRegistry) RegisterActivityWithOptions(_ any, options activity.RegisterOptions) {
	f.activities = append(f.activities, options.Name)
}

func TestRegisterGdpr_ContainsWorkflowAndActivityNames(t *testing.T) {
	reg := &fakeGdprRegistry{}
	registerGdpr(reg, &activities.Activities{})

	wantWorkflows := []string{"GdprExportWorkflow", "GdprEraseWorkflow"}
	for _, name := range wantWorkflows {
		if !contains(reg.workflows, name) {
			t.Errorf("workflow %q not registered (got %v)", name, reg.workflows)
		}
	}

	// The six activity names are the contract of workflows/gdpr.go:45-50 —
	// GdprExportWorkflow/GdprEraseWorkflow execute them BY NAME.
	wantActivities := []string{
		workflows.ActivityGdprCollectBookings,
		workflows.ActivityGdprCollectConversations,
		workflows.ActivityGdprCollectLedger,
		workflows.ActivityGdprCollectCrmPerson,
		workflows.ActivityGdprUploadExport,
		workflows.ActivityGdprPublishErase,
	}
	for _, name := range wantActivities {
		if !contains(reg.activities, name) {
			t.Errorf("activity %q not registered (got %v)", name, reg.activities)
		}
	}
	if len(reg.activities) != len(wantActivities) {
		t.Errorf("expected exactly %d GDPR activities, got %d (%v)",
			len(wantActivities), len(reg.activities), reg.activities)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
