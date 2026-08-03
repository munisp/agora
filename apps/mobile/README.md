# OpenDesk Field (apps/mobile)

Expo React Native field app for OpenDesk tenants — today dashboard, lead
capture, referrals + leaderboard, incidents inbox, push registration.
SPEC-W16 Agent D (contract §5).

> **Honesty caveat (read first):** this app is **code-complete and
> type-checked** (`tsc --noEmit` passes against module shims — see
> "Typechecking without Expo" below), but it has **not been built or run on
> a device, emulator, or CI** as part of this delivery. No device/EAS build
> claims are made. First `npm install && npx expo start` / EAS build may
> surface runtime-only issues (navigation edge cases, native config plugin
> output, push on physical devices) that static checks cannot catch.

## Stack

- Expo SDK 51 (exact pin `"expo": "51.0.28"`, no caret), React Native 0.74.5, React 18.2.0
- expo-router v3 (file-based routing under `app/`)
- expo-auth-session (Keycloak OIDC, authorization code + PKCE)
- expo-secure-store (token + tenant-slug storage — never AsyncStorage)
- expo-notifications (push token registration)
- TypeScript strict

No native modules beyond the Expo SDK — the app runs in Expo Go and in a
standard EAS build (no custom dev client).

## Setup

```bash
cd apps/mobile
npm install
npx expo start          # then press a (Android) / i (iOS) or scan the QR with Expo Go
```

Configuration lives in `app.json → expo.extra` (read via expo-constants in
`src/config.ts`):

| Key                    | Default                                   | Meaning |
|------------------------|-------------------------------------------|---------|
| `apiBase`              | `http://localhost:9080/api/bookings`      | APISIX gateway base for the booking-service BFF |
| `keycloak.issuer`      | `http://localhost:8080/realms/opendesk`   | Keycloak realm issuer URL |
| `keycloak.clientId`    | `opendesk-field`                          | Public OIDC client for this app |
| `keycloak.scopes`      | `openid profile email offline_access`     | Requested scopes |

For device testing on a LAN, point `apiBase` / `keycloak.issuer` at
routable hosts (localhost inside a phone is the phone). Use EAS build
profiles (`eas.json` → per-profile `extra`) for staging/prod values.

## Keycloak client setup (one-time, ASSUMPTION)

The app derives the OIDC discovery document from
`${keycloak.issuer}/.well-known/openid-configuration` (**ASSUMPTION**:
standard discovery path — annotated in `src/auth/keycloak.ts`). Provision a
**public** client in the realm:

- Client ID: `opendesk-field`
- Client authentication: **off** (public client)
- Standard flow: on; **PKCE code challenge method: S256**
- Valid redirect URIs: `opendesk-field://auth/callback` (plus the Expo Go
  dev URI shown by `npx expo start` during development)
- Web origins: `+`

## EAS build

```bash
npm install -g eas-cli          # once
eas login
eas build:configure             # generates eas.json (not committed here)
eas build -p android --profile preview   # APK for internal testing
eas build -p ios --profile preview       # requires Apple Developer account
eas submit -p android / ios              # store submission
```

EAS Build runs in Expo's cloud — no local Android SDK / Xcode needed.
`app.json` already pins the `expo-notifications` and `expo-secure-store`
config plugins; `eas.json` is intentionally not committed (it carries
per-org project ids) — `eas build:configure` scaffolds it.

## Push notifications

Flow (SPEC-W16 contract §1):

1. `src/push/register.ts` requests OS permission and calls
   `Notifications.getExpoPushTokenAsync()`.
2. The token is registered with booking-service:
   `POST /v1/devices {token, platform: "android"|"ios", app: "field"}`
   (with `Authorization: Bearer …` + `X-Tenant-Slug: …`).
3. On sign-out the app calls `DELETE /v1/devices/{token}`.
4. Server-side fan-out (FCM provider now, APNs stub) lives in
   notification-worker — see `docs/push-notifications.md`.

Production notes:

- **Android/FCM**: add your `google-services.json` to the project and set
  `android.googleServicesFile` in `app.json`, then upload the FCM server
  credentials to Expo (`eas credentials`) so Expo's push service can reach
  FCM. The server side reads `FCM_SERVER_KEY` / `FCM_CREDENTIALS_JSON`
  (notification-worker, `FCM_MOCK=1` by default in dev).
- **iOS/APNs**: enable the Push Notifications capability (the
  `expo-notifications` config plugin does this) and upload an APNs key via
  `eas credentials`. The APNs provider in notification-worker is a
  documented stub (contract §1) — delivery to iOS devices rides Expo's
  push service until the native provider lands.
- Push does not work in emulators without Play services / on the iOS
  simulator — `registerForPushNotifications()` fails soft (returns null).

## Typechecking without Expo (shim strategy)

The repo gate is `npx tsc --noEmit` **without** installing the Expo
dependency tree. `src/types/expo-shims.d.ts` declares minimal, honest
module shims for every `expo-*` / `react-native` import this app uses, and
`tsconfig.json` is self-contained (strict, does **not** extend
`expo/tsconfig.base`, which requires the `expo` package on disk):

```bash
# from a scratch dir (keeps node_modules out of the repo):
npm install typescript@5.3.3 @types/react@18.2.79
# then, with that typescript on PATH and @types/react resolvable:
npx tsc --noEmit -p apps/mobile/tsconfig.json
```

With a full `npm install` the real package types resolve first and the
shims are inert — this was verified: `tsc --noEmit` passes both against
the shims alone (no Expo installed) **and** against the real Expo SDK 51
type tree with the shims present. You may still delete
`src/types/expo-shims.d.ts` once you develop against real node_modules,
and/or switch tsconfig to `"extends": "expo/tsconfig.base"` if you prefer
Expo's defaults. The shims only cover the APIs this app calls — extend
them if you add imports.

## Project layout

```
app/                    expo-router routes
  index.tsx             session bounce (splash → /login or /(tabs)/today)
  login.tsx             tenant slug + Keycloak PKCE sign-in
  (tabs)/today.tsx      my bookings dashboard (GET /v1/bookings?mine=true)
  (tabs)/leads.tsx      lead inbox (GET /v1/leads) + status transitions
  (tabs)/growth.tsx     referrals + leaderboard + payouts (SPEC-W14)
  (tabs)/incidents.tsx  incidents inbox + dispatch (SPEC-W11)
  lead-capture.tsx      modal — POST /v1/leads channel="field"
src/
  api/client.ts         fetch wrapper: base URL, X-Tenant-Slug, Bearer
  api/types.ts          typed mirrors of the Go BFF contracts
  api/growth.ts         leaderboard/formatting (mirrors admin-web)
  auth/keycloak.ts      OIDC PKCE flow (ASSUMPTION-annotated discovery)
  auth/session.ts       SecureStore persistence
  auth/useSession.tsx   React session context + refresh-on-launch
  push/register.ts      expo-notifications → POST /v1/devices
  types/expo-shims.d.ts module shims for the no-Expo typecheck gate
components/             Screen, Card, ListItem, StatTile, ui atoms
```

## BFF contracts consumed (all via the APISIX gateway, base
`/api/bookings`, header `X-Tenant-Slug`)

| Endpoint | Used by |
|---|---|
| `GET /v1/bookings?mine=true&status=` | Today |
| `GET /v1/leads?status=&channel=` | Leads |
| `POST /v1/leads` | Lead capture modal |
| `POST /v1/leads/{id}/status` | Leads (advance) |
| `POST /v1/field/capture` | Offline batch (contract §4, ASSUMPTION envelope) |
| `GET /v1/referrals?status=` · `POST /v1/referrals` | Growth |
| `GET /v1/payouts?status=&limit=` | Growth (leaderboard paid totals) |
| `GET /v1/incidents?status=` · `POST /v1/incidents/{id}/dispatch` | Incidents |
| `POST /v1/devices` · `DELETE /v1/devices/{token}` | Push registration (contract §1) |

## Current limitations

- **Not built on device/CI** — see the caveat at the top.
- Role gating is server-side only (perms like `manage_bookings` /
  `view_analytics`); the UI surfaces 403s honestly instead of hiding
  actions. "Field mode" = same app, same role model (contract §5).
- No on-device offline queue (the standalone field PWA owns IndexedDB
  offline capture per contract §4); `submitFieldCapture()` in the API
  client implements the batch shape for a future connectivity-aware path.
- Push tokens are Expo push tokens routed via Expo's service (FCM-backed);
  a native FCM token path and the APNs provider are documented follow-ups
  (see docs/push-notifications.md).
- Leaderboard is computed client-side from `/v1/referrals` +
  `/v1/payouts`, mirroring admin-web; very large tenants may want a
  server-side aggregate endpoint later.
