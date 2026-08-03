# Push Notifications (SPEC-W16 §1/§4) — Mobile & PWA Integration Guide

Wave 16. Push delivery lives in **notification-worker** (providers + paced
kinds + fan-out activity); device-token storage lives in **booking-service**
(`device_tokens` table + REST, Agent B). This document is the integration
contract for the admin-web PWA (Agent C), the field PWA (Agent C), and the
Expo mobile app (Agent D).

| Area | Service | Key artifacts |
| --- | --- | --- |
| FCM provider (HTTP v1 / legacy / mock) | notification-worker (:7003) | `internal/provider/fcm.go` |
| APNs provider (**STUB**) | notification-worker | `internal/provider/apns.go` (documented TODO, no fake claims) |
| Fan-out activity | notification-worker | `internal/activities/push.go` (`SendPushNotification`) |
| Paced kinds + classification | notification-worker | `internal/workflows/paced.go`, `internal/pacer/guards.go` |
| Device tokens (store + REST) | booking-service (:7002) | `internal/devices/` — `POST/DELETE /v1/devices`, `GET /internal/devices` |
| Offline queue (field PWA §4) | booking-service | `internal/fieldcapture/` — `POST /v1/field/capture` |

---

## 1. Contract (SPEC-W16 cross-agent §1)

### Device token registration (mobile/PWA → booking-service)

After the user grants the notification permission, the client registers its
push token with booking-service:

```
POST /v1/devices                      (X-Tenant-Slug, user JWT)
{ "token": "<fcm-or-apns-token>", "platform": "android|ios|web", "app": "admin|field" }

DELETE /v1/devices/{token}            (logout / token revoked)
```

`device_tokens {tenant_id, contact_id null, token, platform, app,
created_at, last_seen_at}` — RLS-scoped, owned by booking-service.

### Push delivery (notification-worker → providers)

A scheduling workflow (or any service on the Temporal task queue) sends push
through the **paced** path exactly like the other outbound kinds:

```jsonc
// NotifyPaced request (PacedSendRequest)
{
  "kind": "push_notification",          // TRANSACTIONAL — or "push_marketing" (MARKETING)
  "push": {
    "tenant_slug": "acme",
    "contact_id": "c-123",              // fetch tokens from booking-service...
    "tokens": [{"token": "...", "platform": "android"}],  // ...OR explicit list (skips the fetch)
    "phone": "+2348012345678",          // OPTIONAL — enables the DND check for push_marketing
    "title": "Booking confirmed",
    "body": "See you tomorrow at 10:00",
    "data": {"booking_id": "b-1"},      // string values only
    "app": "field"                      // OPTIONAL — restrict fetched tokens to one app
  }
}
```

When `contact_id` is set and no explicit `tokens` are given, the
`SendPushNotification` activity fetches the contact's devices via Dapr
service invocation:

```
GET /v1.0/invoke/booking/method/internal/devices?contact_id=c-123
    X-Tenant-Slug: acme
→ 200 [ {"tenant_id","contact_id","token","platform","app"}, ... ]
```

Fan-out: `android`/`web` tokens → **fcm**, `ios` tokens → **apns** (stub,
see §3). The activity returns **per-token results**
(`workflows.PushNotificationResult`): each entry carries
`{token, platform, provider, success, status_code, unregistered, error}`.
`unregistered: true` means the provider reported the token as gone (FCM
`UNREGISTERED`/`NotRegistered`) — the caller should prune it via
`DELETE /v1/devices/{token}`. Per-token failures are results, not activity
errors, so a Temporal retry never double-delivers to succeeded tokens.

### Compliance classification (internal/pacer/guards.go)

| Kind | Class | DND 2442 suppression | Quiet hours |
| --- | --- | --- | --- |
| `push_notification` | TRANSACTIONAL | never | never |
| `push_marketing` | MARKETING | when the payload carries `phone` (the registries are phone-keyed; token-only sends pass with the documented warn) | deferred on channel key `push` (`QUIET_HOURS_OVERRIDES` may set a `"push"` window) |

The `incident_alert` Priority fast-lane is untouched.

---

## 2. Provider configuration (notification-worker env)

| Env | Default | Meaning |
| --- | --- | --- |
| `FCM_MOCK` | `1` | Deterministic mock, no network (mirrors `KYC_MOCK`/`PAYOUT_MOCK`) |
| `FCM_CREDENTIALS_JSON` | — | Google service-account JSON → **HTTP v1** + OAuth2 (takes precedence) |
| `FCM_SERVER_KEY` | — | Legacy FCM server key → legacy `POST /fcm/send` (deprecated upstream; kept per contract) |
| `FCM_PROJECT_ID` | — | GCP project id for HTTP v1 (the credentials `project_id` wins) |
| `FCM_BASE_URL` | `https://fcm.googleapis.com` | Endpoint override (tests) |
| `APNS_KEY_ID` / `APNS_TEAM_ID` / `APNS_KEY_P8` / `APNS_TOPIC` | — | APNs stub config (see §3) |

### Mock mode (default developer experience)

`FCM_MOCK=1` (or unset): no network, deterministic results — the message
name is `projects/<project>/messages/mock-<sha256(token)[:16]>`. Documented
test hooks (mirroring the payout `MockProvider` hooks):

- token `mock-fail` → provider 500 failure
- token `mock-unregistered` → 404 `UNREGISTERED` (per-token result gets
  `unregistered: true`)
- anything else → success

Run the worker locally with zero Google setup; integration tests point
`FCM_BASE_URL` at an `httptest` fake instead.

### Live mode

1. Firebase console → project → service accounts → generate key JSON.
2. `FCM_MOCK=0`, `FCM_CREDENTIALS_JSON=<contents of the key file>`.
3. The worker mints OAuth2 access tokens itself (JWT-bearer grant, RS256,
   stdlib only — no Google client dependency) and caches them to expiry.

**ASSUMPTIONS** (also annotated in `internal/provider/fcm.go`):

- The HTTP v1 envelope follows the public docs shape
  `{"message":{token, notification:{title,body}, data}}`; platform-specific
  blocks (`android.priority`, `apns.*`) are omitted — FCM defaults apply.
- The legacy server-key API shape is the pre-deprecation docs shape; Google
  announced its shutdown, so prefer `FCM_CREDENTIALS_JSON`.
- The OAuth2 grant is RFC 7523 JWT-bearer with scope
  `https://www.googleapis.com/auth/firebase.messaging`.

---

## 3. APNs status: STUB (honest)

`internal/provider/apns.go` implements the provider interface (name,
`Configured()` from the four `APNS_*` envs) but **no delivery**: every send
fails with an explicit `apns provider not implemented (SPEC-W16 stub …)`
per-token result. There is deliberately **no mock and no fake success**. The
file carries a documented TODO (ES256 provider-token JWT from
`APNS_KEY_P8`, HTTP/2 `POST /3/device/{token}`, `apns-topic` header, 410
`Unregistered` pruning). Consequence: iOS tokens currently surface as failed
per-token results — route iOS users to web push or ship the TODO before
enabling iOS production traffic.

---

## 4. Client integration flows

### Admin-web / field PWA (web push via FCM)

1. Serve the PWA with its service worker (see `docs/pwa.md`).
2. Request `Notification` permission; obtain the FCM web-push token
   (Firebase JS SDK `getToken`, VAPID key from tenant config).
3. `POST /v1/devices {token, platform:"web", app:"admin"|"field"}` to
   booking-service (BFF forwards with `X-Tenant-Slug`).
4. On sign-out or `pushsubscriptionchange`, `DELETE /v1/devices/{token}`.

### Expo mobile (Agent D)

`src/push/register.ts`: `expo-notifications` →
`getDevicePushTokenAsync()` (FCM token on Android, APNs token on iOS) →
`POST /v1/devices` with `platform:"android"|"ios"`, `app:"admin"` (field
mode = same app, role-gated). Re-register on token refresh and after login.

### Field PWA offline queue (SPEC-W16 §4 — adjacent but separate)

The offline queue (`lead_capture` / `checkin` items in IndexedDB) flushes to
booking-service `POST /v1/field/capture` with a client-supplied idempotency
key (`field_capture:{uuid}`) — it does **not** go through push. Push is the
server→device direction only (e.g. a `push_notification` informing a field
agent of a new assignment); the offline queue is device→server.

---

## 5. Failure & pruning policy

- Provider 5xx/429/transport errors are retried (2 retries, 100/200 ms
  backoff) by the shared provider client; 4xx fails immediately.
- `unregistered: true` results should trigger
  `DELETE /v1/devices/{token}` (booking-service) so token lists stay clean.
- Activity errors are limited to contract-level failures (missing
  title/body, no token source, device-fetch failure) — safe for Temporal
  retry as nothing was delivered.
