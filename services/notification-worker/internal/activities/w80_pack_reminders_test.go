package activities

// SPEC-W80 C3 + B7 tests:
//  - C3: GetPackReminders resolves the pack reminders block from a
//    custom-offset pack fixture; unknown industry / nil registry yield
//    empty defaults.
//  - B7 (G9): the booking-service callers in this service send
//    X-Internal-Token (internal/devices, v1/leads/resolve).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/opendesk/notification-worker/internal/daprc"
	"github.com/opendesk/notification-worker/internal/packs"
	"github.com/opendesk/notification-worker/internal/workflows"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

const customOffsetPackYAML = `id: fixturepack
displayName: Fixture Pack
terminology:
  offering: session
  team_member: coach
  booking: session
  contact: client
agentPersona: "Fixture persona."
bookingPolicy:
  depositPercent: 0
  noShowFeeCents: 0
  cancellationWindowHours: 12
temporalWorkflow: ConsultancyFollowupWorkflow
offerings:
- name: Discovery
  duration_min: 45
  buffer_min: 0
  price_cents: 0
  capacity: 1
reminders:
  offsets: ["96h", "6h"]
  channels: ["email"]
knowledgeSeed:
- title: Fixture
  body: "Fixture body."
dashboardLabels:
  bookingSingular: Session
  bookingPlural: Sessions
  customerTerm: Client
`

func fixtureRegistry(t *testing.T) *packs.Registry {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fixturepack.yaml"), []byte(customOffsetPackYAML), 0o644))
	reg, err := packs.Load(dir)
	require.NoError(t, err)
	return reg
}

func TestGetPackReminders_CustomOffsetFixture(t *testing.T) {
	a := &Activities{
		Industry: IndustryDeps{Packs: fixtureRegistry(t)},
		Log:      zap.NewNop(),
	}
	rem, err := a.GetPackReminders(context.Background(), "fixturepack")
	require.NoError(t, err)
	require.Equal(t, []string{"96h", "6h"}, rem.Offsets)
	require.Equal(t, []string{"email"}, rem.Channels)
}

func TestGetPackReminders_UnknownIndustryAndNilRegistry(t *testing.T) {
	a := &Activities{
		Industry: IndustryDeps{Packs: fixtureRegistry(t)},
		Log:      zap.NewNop(),
	}
	rem, err := a.GetPackReminders(context.Background(), "no-such-industry")
	require.NoError(t, err)
	require.Empty(t, rem.Offsets)
	require.Empty(t, rem.Channels)

	aNil := &Activities{Log: zap.NewNop()}
	rem, err = aNil.GetPackReminders(context.Background(), "fixturepack")
	require.NoError(t, err)
	require.Empty(t, rem.Offsets)
	require.Empty(t, rem.Channels)
}

// B7: resolvePushTargets (GET internal/devices) must forward
// X-Internal-Token = BookingInternalToken alongside X-Tenant-Slug.
func TestResolvePushTargetsSendsInternalToken(t *testing.T) {
	var gotToken, gotTenant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Internal-Token")
		gotTenant = r.Header.Get("X-Tenant-Slug")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]DeviceToken{
			{TenantID: "t-1", ContactID: "c-1", Token: "tok-1", Platform: "android"},
		})
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	a := pushTestActivities(t)
	a.Dapr = daprc.New(u.Hostname(), port)
	a.BookingInternalToken = "w80-secret"

	targets, err := a.resolvePushTargets(context.Background(), pushInputForTokenTest())
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, "w80-secret", gotToken, "X-Internal-Token must be forwarded (B7)")
	require.Equal(t, "acme", gotTenant)

	// Empty token -> no header (peer fails closed).
	gotToken = "unset"
	a.BookingInternalToken = ""
	_, err = a.resolvePushTargets(context.Background(), pushInputForTokenTest())
	require.NoError(t, err)
	require.Equal(t, "", gotToken, "empty token must not send the header")
}

func pushInputForTokenTest() workflows.PacedPushNotificationSend {
	return workflows.PacedPushNotificationSend{TenantSlug: "acme", ContactID: "c-1", Title: "Hello"}
}

// B7: DaprLeadPhoneResolver (POST v1/leads/resolve) must forward
// X-Internal-Token.
func TestLeadPhoneResolverSendsInternalToken(t *testing.T) {
	var gotToken, gotTenant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Internal-Token")
		gotTenant = r.Header.Get("X-Tenant-Slug")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"phones": map[string]string{"l-1": "+15550001111"}})
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	r := &DaprLeadPhoneResolver{
		Dapr:          daprc.New(u.Hostname(), port),
		BookingAppID:  "booking",
		InternalToken: "w80-secret",
	}
	phones, err := r.ResolveLeadPhones(context.Background(), "acme", []string{"l-1"})
	require.NoError(t, err)
	require.Equal(t, "+15550001111", phones["l-1"])
	require.Equal(t, "w80-secret", gotToken, "X-Internal-Token must be forwarded (B7)")
	require.Equal(t, "acme", gotTenant)

	// Empty token -> no header.
	gotToken = "unset"
	r.InternalToken = ""
	_, err = r.ResolveLeadPhones(context.Background(), "acme", []string{"l-1"})
	require.NoError(t, err)
	require.Equal(t, "", gotToken, "empty token must not send the header")
}
