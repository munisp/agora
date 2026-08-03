package provider

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// The APNs provider is a STUB (SPEC-W16 §1): interface + config + TODO,
// no fake implementation. These tests pin the honest failure modes so a
// later "accidental success" cannot sneak in.

func TestAPNSStubUnconfigured(t *testing.T) {
	a := &APNS{}
	require.Equal(t, "apns", a.Name())
	require.False(t, a.Configured())

	_, _, err := a.SendPush(context.Background(), PushMessage{Token: "tok", Title: "T"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "APNS_KEY_ID")
	require.Contains(t, err.Error(), "not yet implemented")
}

func TestAPNSStubConfiguredStillNotImplemented(t *testing.T) {
	a := &APNS{KeyID: "K", TeamID: "T", KeyP8: "-----BEGIN PRIVATE KEY-----", Topic: "com.opendesk.app"}
	require.True(t, a.Configured())

	_, _, err := a.SendPush(context.Background(), PushMessage{Token: "tok", Title: "T"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not implemented",
		"the stub must never claim a delivery")
	// The failure is local (status 0), so the shared retry machinery would
	// not mistake it for a provider response.
	pe, ok := err.(*Error)
	require.True(t, ok)
	require.Equal(t, 0, pe.StatusCode)
}

func TestAPNSPartialConfigIsUnconfigured(t *testing.T) {
	a := &APNS{KeyID: "K", TeamID: "T"}
	require.False(t, a.Configured(), "all four APNS_* envs are required")
}
