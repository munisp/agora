package workflows

// SPEC-W95 C5-B tests: the pack-configured intake cutoff rides the saga
// input into the ClinicIntakeWorkflow child input.
// Redirection: reverting packWorkflowInput to drop the IntakeCutoffHours
// pass-through fails the mapped case (the child input would be zero).

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPackWorkflowInput_IntakeCutoffHours_W95(t *testing.T) {
	base := SagaInput{
		BookingID: "b-1", TenantID: "t-1", TenantSlug: "acme",
		ContactName: "Ada", ContactEmail: "ada@example.com",
		StartsAt: time.Date(2026, 12, 1, 10, 0, 0, 0, time.UTC),
	}

	// Pack-configured cutoff (6h) flows into the child workflow input.
	in := base
	in.IntakeCutoffHours = 6
	out := packWorkflowInput("ClinicIntakeWorkflow", in, "")
	clinic, ok := out.(ClinicIntakeInput)
	require.True(t, ok, "input type = %T", out)
	require.Equal(t, 6, clinic.IntakeCutoffHours)

	// Zero (pre-W95 caller / pack without the field) maps to zero — the
	// workflow then applies the legacy T-2h default.
	in = base
	out = packWorkflowInput("ClinicIntakeWorkflow", in, "")
	clinic = out.(ClinicIntakeInput)
	require.Equal(t, 0, clinic.IntakeCutoffHours)
}
