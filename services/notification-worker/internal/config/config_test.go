package config

import "testing"

// SIM-010: FCM_MOCK must default to false — the deterministic mock is an
// explicit dev/test opt-in (KYC_MOCK idiom). With the mock off and no FCM
// credentials configured, sends fail closed (provider/fcm.go).

func TestFCMMockDefaultsOff(t *testing.T) {
	t.Setenv("FCM_MOCK", "")
	if Load().FCMMock {
		t.Errorf("FCM_MOCK unset must default to false (fail-closed posture), got true")
	}
}

func TestFCMMockOptIn(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE"} {
		t.Setenv("FCM_MOCK", v)
		if !Load().FCMMock {
			t.Errorf("FCM_MOCK=%q must enable the mock (explicit opt-in)", v)
		}
	}
}

func TestFCMMockExplicitlyOff(t *testing.T) {
	for _, v := range []string{"0", "false", "garbage"} {
		t.Setenv("FCM_MOCK", v)
		if Load().FCMMock {
			t.Errorf("FCM_MOCK=%q must keep the mock disabled", v)
		}
	}
}
