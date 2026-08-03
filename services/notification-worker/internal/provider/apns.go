package provider

// Apple Push Notification service provider — STUB ONLY (SPEC-W16 contract
// §1: "apns (stub: provider interface + config + documented TODO, APNs key
// env names; NO fake implementation claims)").
//
// This file intentionally contains NO delivery implementation and NO mock:
// SendPush always fails with an explicit "not implemented" / "not
// configured" error so that per-token results surface honestly as failures
// (the activity fan-out records them; the send is never silently dropped
// nor faked as delivered).
//
// Configuration env names (wired by cmd/worker via internal/config):
//
//	APNS_KEY_ID   — APNs auth key id (.p8 key identifier)
//	APNS_TEAM_ID  — Apple Developer team id
//	APNS_KEY_P8   — PEM content of the .p8 signing key (ES256)
//	APNS_TOPIC    — app bundle id (apns-topic header)
//
// TODO(real APNs implementation, follow-up wave):
//  1. Parse APNS_KEY_P8 (PEM PKCS#8, *ecdsa.PrivateKey, P-256) and mint the
//     APNs provider token: ES256 JWT {iss: APNS_TEAM_ID, iat} with header
//     {alg: ES256, kid: APNS_KEY_ID}, cached ≤ 55 min (APNs rejects iat
//     older than 1h).
//  2. HTTP/2 POST https://api.push.apple.com/3/device/{token} (sandbox:
//     https://api.sandbox.push.apple.com) with headers authorization:
//     bearer <jwt>, apns-topic: APNS_TOPIC, apns-push-type: alert,
//     apns-priority: 10; body {"aps":{"alert":{"title","body"}}} + custom
//     data merged top-level.
//  3. Map APNs status/reason: 200 → success; 410/Unregistered or
//     400/BadDeviceToken → Unregistered (prune); 429/5xx → retryable
//     through the shared Client.
//  4. Route iOS tokens here from activities.SendPushNotification (the
//     platform→provider mapping already selects "apns" for ios).

import (
	"context"
)

// APNS is the APNs provider stub. It satisfies PushProvider so the wiring
// (config, main.go registration, activity platform routing) is real and
// testable; the delivery itself is deliberately unimplemented.
type APNS struct {
	KeyID  string // APNS_KEY_ID
	TeamID string // APNS_TEAM_ID
	KeyP8  string // APNS_KEY_P8 (PEM content of the .p8 key)
	Topic  string // APNS_TOPIC (bundle id)
}

// Name implements PushProvider.
func (a *APNS) Name() string { return "apns" }

// Configured implements PushProvider: all four APNs envs must be set.
func (a *APNS) Configured() bool {
	return a.KeyID != "" && a.TeamID != "" && a.KeyP8 != "" && a.Topic != ""
}

// SendPush implements PushProvider — STUB: never delivers. The error is
// explicit about why, so callers and per-token results stay honest.
func (a *APNS) SendPush(_ context.Context, msg PushMessage) (int, []byte, error) {
	if !a.Configured() {
		return 0, nil, &Error{StatusCode: 0, Body: "apns not configured: set APNS_KEY_ID, APNS_TEAM_ID, APNS_KEY_P8 and APNS_TOPIC (note: APNs delivery is not yet implemented — STUB, see TODO in apns.go)"}
	}
	return 0, nil, &Error{StatusCode: 0, Body: "apns provider not implemented (SPEC-W16 stub — documented TODO in apns.go; no delivery was attempted)"}
}
