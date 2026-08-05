# apps/admin-web — Agora dashboard + public booking page

Next.js 15 (App Router) + TypeScript (strict) + Tailwind CSS v4 with a small
hand-written shadcn-style component set (`components/ui`). No Clerk, no
Convex, no ElevenLabs — auth is Keycloak OIDC via Auth.js v5, data flows
through the APISIX gateway.

## Routes

| Route | Purpose |
|---|---|
| `/` | Marketing landing page |
| `/sign-in` | Auth.js sign-in (Keycloak, Authorization Code + PKCE) |
| `/app/[orgSlug]` | Tenant dashboard (protected by `middleware.ts`) — overview, bookings (live via WebSocket), offerings, team, availability, knowledge, voice-agent, public-site, billing, settings |
| `/p/[siteSlug]` | Public booking page (no auth): offerings, slot picker, phone-confirmed booking form, chat widget, LiveKit voice button |
| `/api/auth/*` | Auth.js handlers |
| `/api/[[...path]]` | BFF proxy → APISIX, attaches the session access token |
| `/voice/*` | Next.js rewrite → `${API_BASE_URL}/voice/*` (same-origin voice runtime) |
| `/healthz` | Health check |

## Environment variables

See `.env.example`. Key ones:

| Var | Default | Notes |
|---|---|---|
| `AUTH_SECRET` | — | Auth.js cookie secret (required) |
| `AUTH_URL` | `http://localhost:3001` | Public URL of this app |
| `AUTH_TRUST_HOST` | `true` | Needed behind APISIX/Docker |
| `KEYCLOAK_ISSUER` | `http://keycloak:8080/realms/opendesk` | Server-reachable issuer (discovery, token exchange) |
| `KEYCLOAK_ISSUER_PUBLIC` | — | Optional browser-reachable issuer for the authorize redirect |
| `KEYCLOAK_CLIENT_ID` | `admin-web` | Public client, PKCE (no secret needed) |
| `API_BASE_URL` | `http://localhost:9080` | Server-side APISIX base for the BFF + /voice rewrite (`http://apisix:9080` in compose) |
| `NEXT_PUBLIC_API_BASE` | `http://localhost:9080` | Public gateway base (browser-visible) |
| `NEXT_PUBLIC_WS_BASE` | `ws://localhost:9080/ws` | gateway-edge WS fan-out; trailing `/ws` optional |

## Run

```bash
cp .env.example .env.local   # adjust for your setup
npm install
npm run dev                  # http://localhost:3001
```

Typecheck: `npm run typecheck`. Production: `npm run build && npm start`,
or the multi-stage `Dockerfile` (standalone output, port 3001).

## API surface used (via BFF → APISIX, SPEC §12)

Tenant scoping: admin calls send `X-Tenant-Slug: {slug}` (booking-service
resolves tenants from this header); the `?tenant={slug}` query param is also
kept for services that still read it.

- `GET/PATCH /api/identity/v1/tenants/{slug}`
- `GET /api/bookings/v1/bookings?from=&to=` ·
  `POST /api/bookings/v1/bookings/{id}/reschedule|cancel`
- `GET/POST/PATCH/DELETE /api/bookings/v1/offerings[/{id}]`
- `GET/POST/PATCH/DELETE /api/bookings/v1/team-members[/{id}]`
- `GET/PUT /api/bookings/v1/team-members/{id}/availability` (PUT replaces the member's weekly rule set)
- `GET/PUT /api/bookings/v1/site` (tenant-scoped)
- `GET/POST/DELETE /api/knowledge/v1/documents[/{id}]` · `GET /api/knowledge/v1/search?q=`
- `GET /api/payments/v1/accounts/{tenant}/balance` ·
  `POST /api/payments/v1/payouts` body `{ tenant_id, amount_cents?, currency? }`
- Public: `GET /api/bookings/public/sites/{slug}` · `GET .../offerings` ·
  `GET .../availability?offering_id=&date=` · `POST .../bookings`
- `POST /voice/chat` · `POST /voice/session` (LiveKit token) — same-origin via the /voice/* rewrite
- WS `{NEXT_PUBLIC_WS_BASE}/ws?tenant={slug}&token={accessToken}` — booking events

### Documented assumptions

List endpoints may return either a bare JSON array or `{ "items": [...] }`
(both are handled); availability `PUT` has replace semantics with body
`{ rules: [{ weekday, start_min, end_min }] }`; the payout response is a
`Payout` object.

## Notes

- Session tokens carry the Keycloak `tenant_slugs` claim; the org layout
  refuses to render a workspace for slugs not in the claim (defence in depth
  on top of Permify checks in the services).
- Expired access tokens are refreshed in the Auth.js JWT callback.
- `components/voice-session-button.tsx` is the clearly-marked LiveKit
  integration seam: it does a real token fetch + room connect with
  `livekit-client`; the surrounding call UX is intentionally minimal.
