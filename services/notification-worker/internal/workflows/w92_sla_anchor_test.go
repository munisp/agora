package workflows

// SPEC-W92 C6 tests: the SupportEscalation SLA anchor is pack-declared.
// Redirection: reverting packWorkflowInput to `CreatedAt: in.StartsAt`
// unconditionally fails the ticket_created cases (the anchor would not
// move to BookingCreatedAt).

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPackWorkflowInput_SLAAnchor_W92(t *testing.T) {
	slotStart := time.Date(2026, 12, 1, 10, 0, 0, 0, time.UTC) // future slot
	ticketAt := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC) // row created earlier

	base := SagaInput{
		BookingID: "b-1", TenantID: "t-1", TenantSlug: "acme",
		ContactName: "Ada", ContactEmail: "ada@example.com",
		StartsAt: slotStart, BookingCreatedAt: ticketAt,
	}

	// ticket_created anchors at the booking row created_at, not the slot.
	in := base
	in.SLAAnchor = "ticket_created"
	out := packWorkflowInput("SupportEscalationWorkflow", in, "")
	esc, ok := out.(SupportEscalationInput)
	require.True(t, ok, "input type = %T", out)
	require.Equal(t, ticketAt, esc.CreatedAt)
	require.Equal(t, "ticket_created", esc.SLAAnchor)

	// booking_start keeps the legacy slot anchoring (explicit).
	in = base
	in.SLAAnchor = "booking_start"
	out = packWorkflowInput("SupportEscalationWorkflow", in, "")
	esc = out.(SupportEscalationInput)
	require.Equal(t, slotStart, esc.CreatedAt)
	require.Equal(t, "booking_start", esc.SLAAnchor)

	// Empty (pre-W92 caller) is byte-compatible with the legacy behavior.
	in = base
	out = packWorkflowInput("SupportEscalationWorkflow", in, "")
	esc = out.(SupportEscalationInput)
	require.Equal(t, slotStart, esc.CreatedAt)
	require.Equal(t, "", esc.SLAAnchor)

	// ticket_created with a ZERO BookingCreatedAt (in-flight pre-W92 saga)
	// falls back to StartsAt — never the zero time.
	in = base
	in.SLAAnchor = "ticket_created"
	in.BookingCreatedAt = time.Time{}
	out = packWorkflowInput("SupportEscalationWorkflow", in, "")
	esc = out.(SupportEscalationInput)
	require.Equal(t, slotStart, esc.CreatedAt)
}
