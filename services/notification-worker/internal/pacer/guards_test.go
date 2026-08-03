package pacer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// SPEC-W12 contract §3: the explicit marketing/transactional
// classification table.
func TestClassifyKind(t *testing.T) {
	marketing := []string{"geo_campaign", "promo", "broadcast", "drip"}
	for _, kind := range marketing {
		require.Equal(t, ClassMarketing, ClassifyKind(kind), "%s must be marketing", kind)
	}
	transactional := []string{
		"confirmation", "reminder", "incident_alert", "otp", // contract §3 list
		"waitlist_claim", "deposit_reminder", "noshow_followup",
		"intake_reminder", "follow_up", "proposal_reminder", "staff_alert",
	}
	for _, kind := range transactional {
		require.Equal(t, ClassTransactional, ClassifyKind(kind), "%s must be transactional", kind)
	}
	// Unknown kinds default to transactional (guards apply only to
	// explicitly-marketing kinds).
	require.Equal(t, ClassTransactional, ClassifyKind(""))
	require.Equal(t, ClassTransactional, ClassifyKind("some_future_kind"))
}

func TestParseQuietWindow(t *testing.T) {
	w, err := ParseQuietWindow("20:00-08:00")
	require.NoError(t, err)
	require.Equal(t, QuietWindow{StartMin: 20 * 60, EndMin: 8 * 60}, w)

	w, err = ParseQuietWindow("12:30-14:00")
	require.NoError(t, err)
	require.Equal(t, QuietWindow{StartMin: 12*60 + 30, EndMin: 14 * 60}, w)

	for _, bad := range []string{"", "20:00", "20:00-08:00-01:00", "24:00-08:00", "20:60-08:00",
		"20:00-20:00", "aa:bb-cc:dd", "20-08"} {
		_, err := ParseQuietWindow(bad)
		require.Error(t, err, "window %q must be rejected", bad)
	}
}

var lagos = func() *time.Location {
	l, err := time.LoadLocation("Africa/Lagos")
	if err != nil {
		panic(err)
	}
	return l
}()

// Window membership + next-open math, incl. overnight windows.
func TestQuietWindowContainsAndOpenAfter(t *testing.T) {
	overnight := QuietWindow{StartMin: 20 * 60, EndMin: 8 * 60}
	at := func(day, h, m int) time.Time {
		return time.Date(2026, 1, day, h, m, 0, 0, lagos)
	}

	// Overnight 20:00-08:00.
	require.True(t, overnight.Contains(at(1, 20, 0)), "window start is inclusive")
	require.True(t, overnight.Contains(at(1, 23, 59)))
	require.True(t, overnight.Contains(at(2, 0, 0)))
	require.True(t, overnight.Contains(at(2, 7, 59)))
	require.False(t, overnight.Contains(at(2, 8, 0)), "window end is exclusive")
	require.False(t, overnight.Contains(at(2, 12, 0)))
	require.False(t, overnight.Contains(at(2, 19, 59)))

	// Open instants: before midnight → tomorrow 08:00; after midnight →
	// today 08:00.
	require.Equal(t, at(2, 8, 0), overnight.OpenAfter(at(1, 21, 30)))
	require.Equal(t, at(2, 8, 0), overnight.OpenAfter(at(2, 3, 15)))

	// Same-day window 12:00-14:00.
	midday := QuietWindow{StartMin: 12 * 60, EndMin: 14 * 60}
	require.True(t, midday.Contains(at(1, 13, 0)))
	require.False(t, midday.Contains(at(1, 14, 0)))
	require.Equal(t, at(1, 14, 0), midday.OpenAfter(at(1, 12, 45)))
}

// QuietHoursOpenAt: tenant-timezone evaluation + per-channel overrides.
func TestQuietHoursOpenAt(t *testing.T) {
	cfg := QuietHoursConfig{
		DefaultWindow: "20:00-08:00",
		Overrides:     map[string]string{"sms": "22:00-06:00"},
		Timezone:      "Africa/Lagos",
	}
	// 21:00 Lagos on Jan 1 (Lagos is UTC+1 → 20:00 UTC).
	night := time.Date(2026, 1, 1, 20, 0, 0, 0, time.UTC)
	open, inWindow, err := QuietHoursOpenAt(night, "whatsapp", cfg)
	require.NoError(t, err)
	require.True(t, inWindow)
	require.Equal(t, time.Date(2026, 1, 2, 8, 0, 0, 0, lagos), open)

	// The sms override (22:00-06:00) is NOT active at 21:00 Lagos.
	_, inWindow, err = QuietHoursOpenAt(night, "sms", cfg)
	require.NoError(t, err)
	require.False(t, inWindow, "sms override must shadow the default window")

	// 12:00 Lagos: outside every window.
	noon := time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC)
	_, inWindow, err = QuietHoursOpenAt(noon, "", cfg)
	require.NoError(t, err)
	require.False(t, inWindow)

	// Defaults: empty config = contract defaults (20:00-08:00 Africa/Lagos).
	open, inWindow, err = QuietHoursOpenAt(night, "", QuietHoursConfig{})
	require.NoError(t, err)
	require.True(t, inWindow)
	require.Equal(t, time.Date(2026, 1, 2, 8, 0, 0, 0, lagos), open)

	// Bad tz / bad window surface errors (main validates these at boot).
	_, _, err = QuietHoursOpenAt(night, "", QuietHoursConfig{Timezone: "Mars/Olympus"})
	require.Error(t, err)
	_, _, err = QuietHoursOpenAt(night, "", QuietHoursConfig{DefaultWindow: "bogus"})
	require.Error(t, err)
}

// fakeDND records lookups and returns scripted answers.
type fakeDND struct {
	suppressed bool
	reason     string
	err        error
	gotTenant  string
	gotPhone   string
	calls      int
}

func (f *fakeDND) IsSuppressed(_ context.Context, tenantSlug, phone string) (bool, string, error) {
	f.calls++
	f.gotTenant, f.gotPhone = tenantSlug, phone
	return f.suppressed, f.reason, f.err
}

// The DND guard: marketing suppressed + counted, transactional exempt.
func TestGuardsPreSend(t *testing.T) {
	ctx := context.Background()
	marketing := GuardInput{Kind: "geo_campaign", TenantSlug: "acme", Phone: "+2348012345678", Channel: "sms"}

	t.Run("marketing suppressed on global list", func(t *testing.T) {
		dnd := &fakeDND{suppressed: true, reason: ReasonGlobalDND}
		g := NewGuards(GuardConfig{DNDEnforcement: true, DND: dnd}, zap.NewNop())
		dec := g.PreSend(ctx, marketing)
		require.True(t, dec.Suppress)
		require.Equal(t, ReasonGlobalDND, dec.Reason)
		require.Equal(t, ClassMarketing, dec.Class)
		require.Equal(t, 1, dnd.calls)
		require.Equal(t, "acme", dnd.gotTenant)
		require.Equal(t, map[string]uint64{ReasonGlobalDND: 1}, g.SuppressedStats())
	})

	t.Run("marketing suppressed on tenant opt-out", func(t *testing.T) {
		dnd := &fakeDND{suppressed: true, reason: ReasonTenantOptOut}
		g := NewGuards(GuardConfig{DNDEnforcement: true, DND: dnd}, zap.NewNop())
		dec := g.PreSend(ctx, marketing)
		require.True(t, dec.Suppress)
		require.Equal(t, ReasonTenantOptOut, dec.Reason)
		require.Equal(t, map[string]uint64{ReasonTenantOptOut: 1}, g.SuppressedStats())
	})

	t.Run("marketing not on any list passes", func(t *testing.T) {
		dnd := &fakeDND{}
		g := NewGuards(GuardConfig{DNDEnforcement: true, DND: dnd}, zap.NewNop())
		dec := g.PreSend(ctx, marketing)
		require.False(t, dec.Suppress)
		require.Empty(t, g.SuppressedStats())
	})

	t.Run("transactional kinds never consult the registry", func(t *testing.T) {
		dnd := &fakeDND{suppressed: true, reason: ReasonGlobalDND}
		g := NewGuards(GuardConfig{DNDEnforcement: true, DND: dnd}, zap.NewNop())
		for _, kind := range []string{"incident_alert", "reminder", "confirmation", "otp"} {
			dec := g.PreSend(ctx, GuardInput{Kind: kind, Phone: "+2348012345678"})
			require.False(t, dec.Suppress, "%s must pass", kind)
		}
		require.Equal(t, 0, dnd.calls, "transactional kinds must not hit the DND store")
	})

	t.Run("enforcement disabled passes", func(t *testing.T) {
		dnd := &fakeDND{suppressed: true, reason: ReasonGlobalDND}
		g := NewGuards(GuardConfig{DNDEnforcement: false, DND: dnd}, zap.NewNop())
		require.False(t, g.PreSend(ctx, marketing).Suppress)
		require.Equal(t, 0, dnd.calls)
	})

	t.Run("nil store passes", func(t *testing.T) {
		g := NewGuards(GuardConfig{DNDEnforcement: true}, zap.NewNop())
		require.False(t, g.PreSend(ctx, marketing).Suppress)
	})

	t.Run("store error fails open", func(t *testing.T) {
		dnd := &fakeDND{err: errors.New("db down")}
		g := NewGuards(GuardConfig{DNDEnforcement: true, DND: dnd}, zap.NewNop())
		require.False(t, g.PreSend(ctx, marketing).Suppress)
	})

	t.Run("counters accumulate per reason", func(t *testing.T) {
		dnd := &fakeDND{suppressed: true, reason: ReasonGlobalDND}
		g := NewGuards(GuardConfig{DNDEnforcement: true, DND: dnd}, zap.NewNop())
		g.PreSend(ctx, marketing)
		dnd.reason = ReasonTenantOptOut
		g.PreSend(ctx, marketing)
		g.PreSend(ctx, marketing)
		require.Equal(t, map[string]uint64{ReasonGlobalDND: 1, ReasonTenantOptOut: 2}, g.SuppressedStats())
	})
}
