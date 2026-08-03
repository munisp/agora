# Mobile app (apps/mobile) — setup, build, push

The OpenDesk field app is an Expo React Native app (SDK 51, expo-router)
covering the field surfaces of the CAC program: today dashboard, lead
capture, referrals + leaderboard, incidents inbox, and push registration
(SPEC-W16 contract §5). Source and developer docs live in
`apps/mobile/README.md`; this page is the operator-facing summary.

> **Status:** code-complete and `tsc --noEmit`-verified (against committed
> module shims — no Expo install required; see the README's shim section).
> **It has not been built on a device, emulator, or CI.** Treat the first
> `expo start` / EAS build as the real smoke test.

## Setup

```bash
cd apps/mobile
npm install
npx expo start        # Expo Go on a phone, or a/i for emulators
```

Runtime configuration is in `app.json → expo.extra` (surfaced through
expo-constants in `src/config.ts`):

- `apiBase` — the APISIX gateway base for the booking-service BFF,
  e.g. `https://gw.example.com/api/bookings` (default
  `http://localhost:9080/api/bookings`, matching the local compose stack).
- `keycloak.issuer` / `keycloak.clientId` / `keycloak.scopes` — the OIDC
  realm and public client. The app discovers endpoints via
  `${issuer}/.well-known/openid-configuration` (ASSUMPTION: standard
  discovery path) and signs in with authorization code + PKCE (S256), so
  no client secret ships on device. Create the public client
  `opendesk-field` with redirect URI `opendesk-field://auth/callback` and
  PKCE S256 enforced (step-by-step in the README).

Every API call carries `Authorization: Bearer <keycloak token>` and
`X-Tenant-Slug: <slug>`; booking-service's tenantMiddleware validates the
pair server-side. Nothing tenant-scoped is trusted client-side.

## EAS build

```bash
npm install -g eas-cli && eas login
eas build:configure                      # scaffolds eas.json (not committed — per-org ids)
eas build -p android --profile preview   # internal-test APK/AAB
eas build -p ios --profile preview       # needs an Apple Developer account
eas submit -p android / ios
```

The app uses no native modules beyond Expo SDK 51, so the managed workflow
(Expo Go + EAS Build) is sufficient — no custom dev client. The
`expo-notifications` and `expo-secure-store` config plugins are declared
in `app.json`.

## Push notifications (FCM/APNs)

Contract: SPEC-W16 §1 and docs/push-notifications.md (notification-worker
providers + `push_notification` kind; device-token storage in
booking-service).

App side (`src/push/register.ts`):

1. After sign-in, request OS permission → `getExpoPushTokenAsync()`.
2. `POST /v1/devices {token, platform: "android"|"ios", app: "field"}`
   (tenant-scoped, bearer auth). Re-registration is skipped when the token
   is unchanged (stored in SecureStore).
3. Sign-out → `DELETE /v1/devices/{token}`.

Operator checklist:

- **Android (FCM):** place `google-services.json` in `apps/mobile/`, set
  `android.googleServicesFile` in `app.json`, upload FCM credentials to
  Expo (`eas credentials`). Server side: notification-worker FCM provider
  reads `FCM_SERVER_KEY` or `FCM_CREDENTIALS_JSON` (`FCM_MOCK=1` default
  in dev).
- **iOS (APNs):** the `expo-notifications` plugin enables the capability;
  upload an APNs key via `eas credentials`. The notification-worker APNs
  provider is currently a documented stub (contract §1) — iOS delivery
  rides Expo's push service until the native provider is implemented.
- Push requires a physical device with Play services (Android) or a
  physical iPhone; registration fails soft elsewhere.

`push_notification` is TRANSACTIONAL-classified (not DND-suppressed, no
quiet hours); marketing pushes must use the `push_marketing` kind
(docs/dnd-quiet-hours.md).

## API surface consumed

All calls go through the gateway under `/api/bookings` with
`X-Tenant-Slug`:

- `GET /v1/bookings?mine=true` — today dashboard (server resolves the
  caller's team member from the JWT email claim)
- `GET/POST /v1/leads`, `POST /v1/leads/{id}/status` — leads inbox +
  field capture (channel `field`)
- `POST /v1/field/capture` — batched offline queue items
  `{client_id, kind, payload, captured_at, gps}`, idempotent on
  `client_id` (contract §4; the batch envelope `{items:[…]}` is an
  annotated ASSUMPTION pending docs/field-capture.md)
- `GET/POST /v1/referrals`, `GET /v1/payouts` — growth tab; leaderboard
  ranked by verified+converted with paid totals, computed exactly as the
  admin-web Growth dashboard does
- `GET /v1/incidents`, `POST /v1/incidents/{id}/dispatch` — incidents
  inbox (SPEC-W11)
- `POST /v1/devices`, `DELETE /v1/devices/{token}` — push registration
  (contract §1)

## Current limitations

- Not device/CI-built (see status note); runtime verification pending.
- Authorization is enforced server-side; the app shows honest 403 errors
  rather than role-hiding UI. Field mode = same app, role-gated
  (contract §5).
- No on-device offline queue in the native app (the field PWA owns
  IndexedDB offline capture, contract §4); the typed `POST
  /v1/field/capture` client is ready for a future offline path.
- iOS push depends on Expo's service until the APNs provider lands;
  FCM HTTP-shape ASSUMPTIONS are documented in docs/push-notifications.md.
