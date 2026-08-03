package geo

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/store"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

// geoTestEnv builds a workflow test environment with stub DB activities
// registered under their production names (mirroring notification-worker's
// waitlist workflow tests).
type geoTestEnv struct {
	env         *testsuite.TestWorkflowEnvironment
	recipients  []store.CampaignRecipient // sorted by contact id
	batchCalls  []AudienceBatchRequest
	recorded    [][]store.CampaignRecipient
	completed   bool
	failed      bool
	unsentIDs   map[uuid.UUID]bool // nil → everything unsent
	audienceErr error              // non-nil → AudienceBatch fails
}

func newGeoTestEnv(t *testing.T, n int) *geoTestEnv {
	t.Helper()
	g := &geoTestEnv{}
	for i := 0; i < n; i++ {
		g.recipients = append(g.recipients, store.CampaignRecipient{
			ContactID: uuid.New(),
			Name:      fmt.Sprintf("Contact-%03d", i),
			Phone:     fmt.Sprintf("+1555%07d", i),
		})
	}
	sort.Slice(g.recipients, func(i, j int) bool {
		return g.recipients[i].ContactID.String() < g.recipients[j].ContactID.String()
	})

	g.env = (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()

	// Keyset-paginated audience stub.
	g.env.RegisterActivityWithOptions(func(ctx context.Context, req AudienceBatchRequest) ([]store.CampaignRecipient, error) {
		g.batchCalls = append(g.batchCalls, req)
		if g.audienceErr != nil {
			return nil, g.audienceErr
		}
		var out []store.CampaignRecipient
		for _, r := range g.recipients {
			if req.After == "" || r.ContactID.String() > req.After {
				out = append(out, r)
			}
		}
		if req.Limit > 0 && len(out) > req.Limit {
			out = out[:req.Limit]
		}
		return out, nil
	}, activity.RegisterOptions{Name: ActivityGeoAudienceBatch})

	// Idempotent-skip stub: drops ledger-sent contacts.
	g.env.RegisterActivityWithOptions(func(ctx context.Context, req FilterUnsentRequest) ([]store.CampaignRecipient, error) {
		var out []store.CampaignRecipient
		for _, r := range req.Recipients {
			if g.unsentIDs == nil || g.unsentIDs[r.ContactID] {
				out = append(out, r)
			}
		}
		return out, nil
	}, activity.RegisterOptions{Name: ActivityGeoFilterUnsent})

	g.env.RegisterActivityWithOptions(func(ctx context.Context, req RecordSendsRequest) (int, error) {
		g.recorded = append(g.recorded, req.Recipients)
		return len(req.Recipients), nil
	}, activity.RegisterOptions{Name: ActivityGeoRecordSends})

	// NotifyPaced stub (executed by notification-worker in production,
	// where it returns a PacedSendResult since SPEC-W12); tests override
	// behavior with OnActivity mocks.
	g.env.RegisterActivityWithOptions(func(ctx context.Context, req PacedSendRequest) (PacedSendResult, error) {
		return PacedSendResult{Status: PacedSendStatusSent}, nil
	}, activity.RegisterOptions{Name: ActivityNotifyPaced})

	g.env.RegisterActivityWithOptions(func(ctx context.Context, req CampaignStatusRequest) error {
		g.completed = true
		return nil
	}, activity.RegisterOptions{Name: ActivityGeoCompleteCampaign})
	g.env.RegisterActivityWithOptions(func(ctx context.Context, req CampaignStatusRequest) error {
		g.failed = true
		return nil
	}, activity.RegisterOptions{Name: ActivityGeoFailCampaign})
	return g
}

func (g *geoTestEnv) input(batchSize int) GeoCampaignInput {
	return GeoCampaignInput{
		CampaignID: uuid.NewString(),
		TenantID:   uuid.NewString(),
		TenantSlug: "acme",
		Channel:    "whatsapp",
		Message:    "Hi {name}, flash sale this weekend!",
		BatchSize:  batchSize,
	}
}

// Batching: 120 recipients with batch 50 are paged 50/50/20; every send
// goes through NotifyPaced with kind geo_campaign; each sent batch is
// recorded; the campaign completes.
func TestGeoCampaignWorkflowBatchesAudience(t *testing.T) {
	g := newGeoTestEnv(t, 120)

	var paced []PacedSendRequest
	g.env.OnActivity(ActivityNotifyPaced, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			paced = append(paced, args.Get(1).(PacedSendRequest))
		}).Return(PacedSendResult{Status: PacedSendStatusSent}, nil)

	g.env.ExecuteWorkflow(GeoCampaignWorkflow, g.input(50))
	require.True(t, g.env.IsWorkflowCompleted())
	require.NoError(t, g.env.GetWorkflowError())

	require.Len(t, g.batchCalls, 4, "3 pages + 1 empty terminator")
	require.Equal(t, 50, g.batchCalls[0].Limit)
	require.Equal(t, "", g.batchCalls[0].After, "first page starts from the beginning")
	require.Equal(t, g.recipients[49].ContactID.String(), g.batchCalls[1].After, "keyset pagination")
	require.Len(t, paced, 120)
	for _, req := range paced {
		require.Equal(t, PacedSendGeoCampaign, req.Kind)
		require.NotNil(t, req.Geo)
		require.Equal(t, "whatsapp", req.Geo.Channel)
	}
	require.Len(t, g.recorded, 3, "one RecordSends per non-empty sent batch")
	require.True(t, g.completed)
	require.False(t, g.failed)
}

// Personalization: the {name} token is substituted per recipient before
// the paced send.
func TestGeoCampaignWorkflowPersonalizesNameToken(t *testing.T) {
	g := newGeoTestEnv(t, 2)

	texts := map[string]string{}
	g.env.OnActivity(ActivityNotifyPaced, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			req := args.Get(1).(PacedSendRequest)
			texts[req.Geo.Phone] = req.Geo.Text
		}).Return(PacedSendResult{Status: PacedSendStatusSent}, nil)

	g.env.ExecuteWorkflow(GeoCampaignWorkflow, g.input(50))
	require.NoError(t, g.env.GetWorkflowError())
	require.Len(t, texts, 2)
	for _, r := range g.recipients {
		require.Equal(t, "Hi "+r.Name+", flash sale this weekend!", texts[r.Phone],
			"{name} substituted per recipient")
	}
}

// Idempotent replay: contacts already in the send ledger are filtered out
// before any NotifyPaced call.
func TestGeoCampaignWorkflowSkipsAlreadySent(t *testing.T) {
	g := newGeoTestEnv(t, 5)
	// Simulate a previous run that already sent to recipients 1 and 3.
	g.unsentIDs = map[uuid.UUID]bool{}
	for i, r := range g.recipients {
		g.unsentIDs[r.ContactID] = i != 1 && i != 3
	}

	var pacedPhones []string
	g.env.OnActivity(ActivityNotifyPaced, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			pacedPhones = append(pacedPhones, args.Get(1).(PacedSendRequest).Geo.Phone)
		}).Return(PacedSendResult{Status: PacedSendStatusSent}, nil)

	g.env.ExecuteWorkflow(GeoCampaignWorkflow, g.input(50))
	require.NoError(t, g.env.GetWorkflowError())
	require.Equal(t, []string{g.recipients[0].Phone, g.recipients[2].Phone, g.recipients[4].Phone}, pacedPhones,
		"contacts already sent for this campaign id are skipped")
	require.Len(t, g.recorded, 1)
	require.Len(t, g.recorded[0], 3)
}

// A failed send skips that recipient without aborting the campaign
// (waitlist backfill pattern); only successful sends are recorded.
func TestGeoCampaignWorkflowContinuesAfterSendFailure(t *testing.T) {
	g := newGeoTestEnv(t, 3)

	g.env.OnActivity(ActivityNotifyPaced, mock.Anything, mock.MatchedBy(func(req PacedSendRequest) bool {
		return req.Geo != nil && req.Geo.Phone == g.recipients[1].Phone
	})).Return(PacedSendResult{}, errors.New("provider down")).Once()
	g.env.OnActivity(ActivityNotifyPaced, mock.Anything, mock.MatchedBy(func(req PacedSendRequest) bool {
		return req.Geo != nil && req.Geo.Phone != g.recipients[1].Phone
	})).Return(PacedSendResult{Status: PacedSendStatusSent}, nil)

	g.env.ExecuteWorkflow(GeoCampaignWorkflow, g.input(50))
	require.True(t, g.env.IsWorkflowCompleted())
	require.NoError(t, g.env.GetWorkflowError())
	require.Len(t, g.recorded, 1)
	require.Len(t, g.recorded[0], 2, "failed recipient is not recorded as sent")
	require.True(t, g.completed)
}

// An audience-fetch failure fails the workflow and flips the campaign to
// failed (terminal status transition on the disconnected context).
func TestGeoCampaignWorkflowFailureMarksCampaignFailed(t *testing.T) {
	g := newGeoTestEnv(t, 0)
	g.audienceErr = errors.New("db down")

	g.env.ExecuteWorkflow(GeoCampaignWorkflow, g.input(50))
	require.True(t, g.env.IsWorkflowCompleted())
	require.Error(t, g.env.GetWorkflowError())
	require.True(t, g.failed, "campaign must be marked failed")
	require.False(t, g.completed)
}

// Personalize is a pure substitution.
func TestPersonalize(t *testing.T) {
	require.Equal(t, "Hi Ada!", Personalize("Hi {name}!", "Ada"))
	require.Equal(t, "no token", Personalize("no token", "Ada"))
	require.Equal(t, "{name}{name}", Personalize("{name}{name}", "{name}"))
}

// MaskPhone keeps prefix + last two digits, hides the middle (PII).
func TestMaskPhone(t *testing.T) {
	require.Equal(t, "+23*******01", MaskPhone("+23480000001"))
	require.Equal(t, "***", MaskPhone("1234"))
	require.Equal(t, "***", MaskPhone(""))
}
