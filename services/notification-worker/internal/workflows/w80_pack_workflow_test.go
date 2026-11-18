package workflows

// SPEC-W80 C1/C3 tests:
//  - C1: SagaInput.WorkflowName (pack.temporalWorkflow, "workflow_name")
//    drives the pack child workflow when set + known; unset behaves exactly
//    as the legacy industry switch; unknown names fall back to SalonDeposit.
//  - C3: the saga resolves the pack reminders block via GetPackReminders and
//    passes offsets/channels into the ReminderWorkflow input.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

func TestPackWorkflowForIndustry_W80(t *testing.T) {
	cases := []struct {
		name     string
		in       SagaInput
		expected string
	}{
		// C1 core: a pack-declared workflow name wins over the industry
		// switch (healthcare maps to SalonDeposit in the legacy switch but
		// the healthcare pack declares ClinicIntakeWorkflow).
		{"pack-declared clinic intake for healthcare industry",
			SagaInput{Industry: "healthcare", WorkflowName: "ClinicIntakeWorkflow"}, "ClinicIntakeWorkflow"},
		{"pack-declared consultancy for unknown industry",
			SagaInput{Industry: "b2b-saas", WorkflowName: "ConsultancyFollowupWorkflow"}, "ConsultancyFollowupWorkflow"},
		{"pack-declared salon deposit overrides clinic industry switch",
			SagaInput{Industry: "clinic", WorkflowName: "SalonDepositWorkflow"}, "SalonDepositWorkflow"},
		{"pack-declared support escalation",
			SagaInput{Industry: "neighborhood-watch", WorkflowName: "SupportEscalationWorkflow"}, "SupportEscalationWorkflow"},
		// Backward compat: unset WorkflowName = legacy mapping, unchanged.
		{"unset keeps legacy clinic mapping",
			SagaInput{Industry: "clinic"}, "ClinicIntakeWorkflow"},
		{"unset keeps legacy consultancy mapping",
			SagaInput{Industry: "consultancy"}, "ConsultancyFollowupWorkflow"},
		{"unset keeps legacy support-desk mapping",
			SagaInput{Industry: "support-desk"}, "SupportEscalationWorkflow"},
		{"unset unknown industry falls back to salon deposit",
			SagaInput{Industry: "healthcare"}, "SalonDepositWorkflow"},
		{"unset empty industry falls back to salon deposit",
			SagaInput{}, "SalonDepositWorkflow"},
		// Unknown workflow names are NOT honored: legacy switch applies.
		{"unknown workflow name falls back to legacy industry switch",
			SagaInput{Industry: "clinic", WorkflowName: "BogusWorkflow"}, "ClinicIntakeWorkflow"},
		{"unknown workflow name and unknown industry falls back to salon deposit",
			SagaInput{Industry: "healthcare", WorkflowName: "BogusWorkflow"}, "SalonDepositWorkflow"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, PackWorkflowForIndustry(tc.in))
		})
	}
}

// C1 redirection proof at the workflow level: a healthcare-industry saga
// input whose pack declares ClinicIntakeWorkflow must start the clinic
// intake child workflow (base maps healthcare -> SalonDepositWorkflow).
func TestBookingSagaWorkflow_PackDeclaredWorkflowNameWins(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(BookingSagaWorkflow)
	registerSagaStubs(env)

	in := sagaTestInput()
	in.Industry = "healthcare"
	in.WorkflowName = "ClinicIntakeWorkflow"

	env.OnActivity(ActivityReserveSlot, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(ActivityHoldDeposit, mock.Anything, mock.Anything).Return("hold-1", nil)
	env.OnActivity(ActivityConfirmBooking, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(ActivityNotifyPaced, mock.Anything, mock.Anything).Return(nil)

	var clinicStarted, salonStarted bool
	env.OnWorkflow("ClinicIntakeWorkflow", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			clinicStarted = true
			require.IsType(t, ClinicIntakeInput{}, args.Get(1))
		}).Return(nil).Once()
	env.OnWorkflow("SalonDepositWorkflow", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { salonStarted = true }).Return(nil).Maybe()

	env.ExecuteWorkflow(BookingSagaWorkflow, in)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.True(t, clinicStarted, "pack-declared ClinicIntakeWorkflow must start for the healthcare saga")
	require.False(t, salonStarted, "legacy SalonDepositWorkflow must NOT start when the pack declares a workflow")
	env.AssertExpectations(t)
}

// C1 backward compat: an in-flight (pre-W80) saga input without
// WorkflowName takes the legacy industry switch, byte-identical behavior.
func TestBookingSagaWorkflow_UnsetWorkflowNameKeepsLegacyMapping(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(BookingSagaWorkflow)
	registerSagaStubs(env)

	in := sagaTestInput()
	in.Industry = "healthcare" // unknown to the legacy switch -> SalonDeposit

	env.OnActivity(ActivityReserveSlot, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(ActivityHoldDeposit, mock.Anything, mock.Anything).Return("hold-1", nil)
	env.OnActivity(ActivityConfirmBooking, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(ActivityNotifyPaced, mock.Anything, mock.Anything).Return(nil)

	var salonStarted bool
	env.OnWorkflow("SalonDepositWorkflow", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { salonStarted = true }).Return(nil).Once()

	env.ExecuteWorkflow(BookingSagaWorkflow, in)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.True(t, salonStarted, "unset WorkflowName must keep the legacy industry-switch mapping")
	env.AssertExpectations(t)
}

// C3: the saga resolves pack reminders via GetPackReminders and passes
// offsets/channels into the ReminderWorkflow input.
func TestBookingSagaWorkflow_PackRemindersPassedToReminderWorkflow(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(BookingSagaWorkflow)
	registerSagaStubs(env)
	env.RegisterActivityWithOptions(func(ctx context.Context, industry string) (PackReminders, error) {
		return PackReminders{}, nil
	}, activity.RegisterOptions{Name: ActivityGetPackReminders})

	in := sagaTestInput()
	in.Industry = "clinic"

	env.OnActivity(ActivityReserveSlot, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(ActivityHoldDeposit, mock.Anything, mock.Anything).Return("hold-1", nil)
	env.OnActivity(ActivityConfirmBooking, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(ActivityNotifyPaced, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(ActivityGetPackReminders, mock.Anything, "clinic").
		Return(PackReminders{Offsets: []string{"72h", "24h", "2h"}, Channels: []string{"email"}}, nil).Once()

	var reminderIn ReminderInput
	env.OnWorkflow("ReminderWorkflow", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { reminderIn = args.Get(1).(ReminderInput) }).Return(nil).Once()

	env.ExecuteWorkflow(BookingSagaWorkflow, in)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, []string{"72h", "24h", "2h"}, reminderIn.ReminderOffsets)
	require.Equal(t, []string{"email"}, reminderIn.ReminderChannels)
	env.AssertExpectations(t)
}

// C3: when the pack has no reminders block (or the lookup fails), the
// ReminderWorkflow input carries no offsets and the legacy T-24h/T-1h
// defaults apply.
func TestBookingSagaWorkflow_NoPackRemindersKeepsDefaults(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(BookingSagaWorkflow)
	registerSagaStubs(env)
	env.RegisterActivityWithOptions(func(ctx context.Context, industry string) (PackReminders, error) {
		return PackReminders{}, nil
	}, activity.RegisterOptions{Name: ActivityGetPackReminders})

	in := sagaTestInput()

	env.OnActivity(ActivityReserveSlot, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(ActivityHoldDeposit, mock.Anything, mock.Anything).Return("hold-1", nil)
	env.OnActivity(ActivityConfirmBooking, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(ActivityNotifyPaced, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(ActivityGetPackReminders, mock.Anything, mock.Anything).
		Return(PackReminders{}, nil).Once()

	var reminderIn ReminderInput
	env.OnWorkflow("ReminderWorkflow", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { reminderIn = args.Get(1).(ReminderInput) }).Return(nil).Once()

	env.ExecuteWorkflow(BookingSagaWorkflow, in)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Empty(t, reminderIn.ReminderOffsets)
	require.Empty(t, reminderIn.ReminderChannels)
	env.AssertExpectations(t)
}

// C3: parseReminderOffsets keeps valid positive durations and drops
// malformed/non-positive entries (malformed packs degrade to defaults).
func TestParseReminderOffsets_W80(t *testing.T) {
	require.Equal(t,
		[]time.Duration{72 * time.Hour, 24 * time.Hour, 2 * time.Hour},
		parseReminderOffsets([]string{"72h", "24h", "2h"}))
	require.Empty(t, parseReminderOffsets(nil))
	require.Empty(t, parseReminderOffsets([]string{"bogus", "-1h", "0s"}))
	require.Equal(t, []time.Duration{time.Hour}, parseReminderOffsets([]string{"junk", "1h"}))
}

// C3: ReminderWorkflow uses pack offsets when provided (one paced send per
// offset, kinds derived from the durations).
func TestReminderWorkflow_PackOffsets(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()

	in := ReminderInput{
		BookingID:       "b-1",
		TenantID:        "t-1",
		TenantSlug:      "acme",
		ContactPhone:    "+15550001111",
		ContactName:     "Caller",
		StartsAt:        time.Now().Add(3 * time.Hour),
		ReminderOffsets: []string{"2h30m", "1h"},
	}

	env.RegisterActivityWithOptions(func(ctx context.Context, in ReminderInput) (string, error) {
		return "confirmed", nil
	}, activity.RegisterOptions{Name: ActivityGetBookingStatus})
	env.RegisterActivityWithOptions(func(ctx context.Context, req PacedSendRequest) error {
		return nil
	}, activity.RegisterOptions{Name: ActivityNotifyPaced})

	var kinds []string
	env.OnActivity(ActivityNotifyPaced, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			req := args.Get(1).(PacedSendRequest)
			kinds = append(kinds, req.Reminder.Kind)
		}).Return(nil).Twice()

	env.ExecuteWorkflow(ReminderWorkflow, in)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.ElementsMatch(t, []string{"2h30m0s", "1h0m0s"}, kinds)
	env.AssertExpectations(t)
}
