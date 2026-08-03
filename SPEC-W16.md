# SPEC-W16 — PWA + Native Mobile (CAC App, Wave 5 of 5)

Wave 16. Four builders, strict ownership. Same delivery protocol (workspace under $HOME —
/tmp gets wiped; additive copy to /mnt; md5-verify FROM /mnt; real tails).
Honesty requirement: native mobile is CODE-COMPLETE, tsc-verified, with a documented
"not built on device/CI" caveat — no fabricated build claims.

## Cross-agent contracts (bind everyone)

1. **Push notifications**: notification-worker gains providers `fcm` (FCM HTTP v1,
   FCM_SERVER_KEY or service-account json env FCM_CREDENTIALS_JSON — mark HTTP shape
   ASSUMPTION where docs are ambiguous, FCM_MOCK=1 default) and `apns` (stub: provider
   interface + config + documented TODO, APNs key env names; NO fake implementation claims).
   New paced kind `push_notification` classified TRANSACTIONAL (not DND-suppressed, no quiet
   hours — but a `push_marketing` kind IS marketing-classified). Device tokens stored in
   booking-service: `device_tokens {tenant_id, contact_id null, token, platform:"android|ios|web",
   app:"admin|field", created_at, last_seen_at}` RLS + REST POST/DELETE /v1/devices (called by
   mobile/PWA after permission grant) — BOOKING owns this (Agent B), notification-worker
   fetches tokens via Dapr invoke GET /internal/devices?contact_id= (Agent A codes to this
   contract; Agent B implements it).
2. **PWA manifest**: name "OpenDesk", short_name per app, theme aligned to design tokens,
   icons 192/512 (maskable) — generated as simple branded SVG→PNG placeholders via a script
   (scripts/gen-pwa-icons.sh using python PIL; committed output pngs).
3. **Service worker strategy**: app-shell cache-first; /api/* network-first with 3s timeout
   + offline fallback JSON; NEVER cache /voice, /webhooks, or auth callbacks; version constant
   OPENDESK_SW_V for cache busting.
4. **Field PWA offline queue**: IndexedDB queue {id, kind:"lead_capture|checkin", payload,
   captured_at, gps:{lat,lng,accuracy} null}; flush on 'online' + Background Sync where
   available; server dedupe via client-supplied idempotency key (field_capture:{uuid}).
5. **Mobile (Expo RN)**: apps/mobile — Expo SDK 51+, TypeScript, expo-router; shared API
   client module mirrors the BFF contracts (x-tenant-slug headers); screens: login (Keycloak
   via expo-auth-session PKCE — ASSUMPTION annotated), today/dashboard, leads list + capture,
   referrals + leaderboard, incidents inbox (W11), push registration via expo-notifications.
   NO native module beyond Expo SDK (no custom dev clients). Field mode = same app, role-gated.

## Agent A — notification-worker push providers + kind
Owns: services/notification-worker/internal/provider/ (NEW fcm.go, apns.go — follow existing
provider idioms), internal/activities/push.go (NEW SendPushNotification activity: fetch tokens
via Dapr invoke booking GET /internal/devices?contact_id= OR explicit token list in payload,
fan-out via provider, per-token results), internal/workflows/paced.go (ADDITIVE kinds
push_notification/push_marketing + payload struct), internal/pacer/guards.go (ADDITIVE:
push_marketing → marketing class), config (ADDITIVE FCM_*/APNS_*), cmd/worker/main.go
(ADDITIVE registration), tests (httptest FCM fake, mock default, classification tests),
docs/push-notifications.md (NEW: contract §1/§4 + mobile/PWA integration guide).
go build/vet/test green.

## Agent B — booking-service device tokens + field capture API
Owns: services/booking-service/internal/devices/ (NEW: model, RLS store, handlers —
POST /v1/devices, DELETE /v1/devices/{token}, GET /internal/devices?contact_id= (service-to-
service, X-Tenant-Slug pattern), GET /v1/devices (view_analytics, platform/app filters)),
internal/fieldcapture/ (NEW: POST /v1/field/capture — accepts batched offline queue items
{client_id, kind, payload, captured_at, gps}, idempotent on client_id per contract §4,
creates leads via the leads service (kind lead_capture) or geo check-in events (kind checkin
→ append to contact locations history if the W8 location store has one — inspect; else store
in a field_checkins table), manage_bookings OR a new lightweight field role perm — inspect
middleware and pick the least-privileged existing fit, documented), wiring additive
(server.go/main.go/config.go), tests (embedded-postgres), docs/field-capture.md (NEW).
go build/vet/test green.

## Agent C — admin-web PWA + field PWA
Owns:
- apps/admin-web/public/manifest.webmanifest + sw.js + icons/ (per §2/§3; icons via
  scripts/gen-pwa-icons.sh — write the script AND run it, commit pngs)
- apps/admin-web: PWA registration (small client component, register sw.js on load,
  skip in dev), offline fallback page, install-prompt UX (beforeinstallprompt, dismissible,
  localStorage snooze 14d)
- apps/field-pwa/ (NEW standalone minimal Next.js OR plain Vite-free static PWA — INSPECT the
  repo's tooling first; if a second Next app is heavy, build a dependency-free static PWA:
  index.html + app.js + sw.js + manifest, dark-on-warm design matching the platform):
  login via existing widget/tenant slug flow (simplest working auth: tenant slug + staff PIN
  against a REAL endpoint if one exists — inspect; else document the gap and gate the UI to
  a demo mode honestly), lead capture form (name/phone/notes + GPS auto-attach with consent
  copy), offline queue per §4 (IndexedDB, outbox list, sync status), geo check-in button,
  works on 2G: <150KB total initial payload, no framework if static.
- docs/pwa.md (NEW).
Verify: tsc --noEmit for admin-web; node --check for static JS; validate manifest JSON;
bash -n the icon script + actually run it.

## Agent D — apps/mobile (Expo React Native) + docs
Owns: apps/mobile/ (NEW ENTIRE: package.json (Expo SDK 51 pins — exact versions, no
 caret on expo), app.json (name, slug, plugins: expo-notifications, expo-secure-store),
 tsconfig.json, babel.config.js, app/ (expo-router screens per §5: login, (tabs)/today,
 (tabs)/leads, (tabs)/growth, (tabs)/incidents, lead-capture modal), src/api/client.ts
 (fetch wrapper: base URL from app config, x-tenant-slug, bearer token from SecureStore,
 typed endpoints mirroring BFF: leads/referrals/payouts/incidents/devices),
 src/push/register.ts (expo-notifications token → POST /v1/devices per §1),
 src/auth/keycloak.ts (expo-auth-session PKCE — ASSUMPTION annotated discovery URL),
 components/ (Screen, Card, ListItem, StatTile — warm low-saturation theme tokens
 mirroring admin-web palette), README.md), docs/mobile.md (NEW: setup, EAS build commands,
 push setup FCM/APNs, current limitations).
Constraints: TypeScript strict; verify with npx tsc --noEmit using a LOCAL tsconfig (install
typescript + @types/react only into a scratch dir or use npx -y typescript — do NOT require
expo installed to typecheck: declare minimal module shims src/types/expo-shims.d.ts for
expo-* imports so tsc passes WITHOUT node_modules of expo — document this shim approach;
if you CAN npm install expo deps quickly, prefer real types). Honest caveat in README +
docs: code-complete, not device-built.
NO other directories touched.

## Delivery protocol: identical to SPEC-W12 §Delivery (but $HOME workspaces).
