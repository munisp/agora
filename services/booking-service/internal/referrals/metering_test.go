package referrals

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// The usage payload mirrors the geo_campaign_message / incident_alert_message
// CloudEvents shape (com.opendesk.usage.UsageRecord on opendesk.usage.events).
type usageEvent struct {
	Type      string         `json:"type"`
	Source    string         `json:"source"`
	Subject   string         `json:"subject"`
	TenantID  string         `json:"tenantid"`
	DataValue map[string]any `json:"data"`
}

func decodeUsage(t *testing.T, payload []byte) usageEvent {
	t.Helper()
	var evt usageEvent
	require.NoError(t, json.Unmarshal(payload, &evt))
	return evt
}

func TestMarshalReferralVerifiedUsageRecord(t *testing.T) {
	tenantID, referralID, campaignID := uuid.New(), uuid.New(), uuid.New()
	payload, err := MarshalReferralVerifiedUsageRecord("acme", tenantID, referralID, "agent", "agent-42", &campaignID)
	require.NoError(t, err)

	evt := decodeUsage(t, payload)
	require.Equal(t, "com.opendesk.usage.UsageRecord", evt.Type)
	require.Equal(t, "booking-service", evt.Source)
	require.Equal(t, "acme", evt.Subject)
	require.Equal(t, tenantID.String(), evt.TenantID)
	require.Equal(t, tenantID.String(), evt.DataValue["tenant_id"])
	require.Equal(t, UsageMetricReferralVerified, evt.DataValue["metric"])
	require.Equal(t, "referral_verified", evt.DataValue["metric"])
	require.InDelta(t, 1, evt.DataValue["value"], 0)
	meta, ok := evt.DataValue["meta"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, referralID.String(), meta["referral_id"])
	require.Equal(t, "agent", meta["referrer_type"])
	require.Equal(t, "agent-42", meta["referrer_id"])
	require.Equal(t, campaignID.String(), meta["campaign_id"])
}

func TestMarshalReferralVerifiedUsageRecordOrganicOmitsCampaign(t *testing.T) {
	payload, err := MarshalReferralVerifiedUsageRecord("acme", uuid.New(), uuid.New(), "contact", "+23480000001", nil)
	require.NoError(t, err)
	meta := decodeUsage(t, payload).DataValue["meta"].(map[string]any)
	_, hasCampaign := meta["campaign_id"]
	require.False(t, hasCampaign, "organic referrals carry no campaign_id")
}
