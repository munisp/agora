# Roles & Capabilities

Agora access control is layered: **Keycloak** issues realm roles in the JWT,
**APISIX** authenticates requests at the gateway and injects identity headers,
**Permify** authorizes tenant-scoped mutations in the services, and the
**admin-web dashboard** gates which sections a user can see and reach.

This document is the single role × capability matrix (SPEC-W7 Part C).

## Role × capability matrix

| Capability | owner | admin | staff | viewer | analyst | billing |
|---|:-:|:-:|:-:|:-:|:-:|:-:|
| Dashboard overview, bookings, schedule | ✔ | ✔ | ✔ | ✔ | — | — |
| Manage catalog (offerings/team/availability) | ✔ | ✔ | — | — | — | — |
| Manage bookings (book/reschedule/cancel) | ✔ | ✔ | ✔ | — | — | — |
| Publish/edit public site + white label | ✔ | ✔ | — | — | — | — |
| **Analytics dashboards (KPIs, text-to-SQL)** | ✔ | ✔ | — | — | ✔ | — |
| **Billing section (invoices, payouts, QR)** | ✔ | — | — | — | — | ✔ |
| Generate/void invoices, rate cards | ✔ | — | — | — | — | ✔¹ |

¹ The billing-engine (SPEC-W7 Part B) requires realm role `owner` or `admin`
for `POST /v1/invoices/generate` and `POST /v1/invoices/{id}/void` at the
service level; the dashboard additionally hides the whole section from anyone
without `owner`/`billing`.

## Layer-by-layer mapping

### Keycloak realm roles (`infra/keycloak/realm-opendesk.json`)

| Realm role | Description |
|---|---|
| `owner` | Tenant owner — full control including billing and deletion |
| `admin` | Tenant admin — manage catalog, bookings, site |
| `staff` | Tenant staff — manage bookings |
| `viewer` | Read-only dashboard access |
| `analyst` | KPI/analytics dashboards, no mutations |
| `billing` | invoices, rate cards, payment links |

Roles are carried in the access token's `realm_access.roles` claim. The
admin-web session helper (`apps/admin-web/lib/auth.ts`) decodes that claim
into `session.realmRoles`; capability checks go through
`apps/admin-web/lib/roles.ts` (`canViewAnalytics`, `canViewBilling`) so the
server-side guards and the client-side nav hiding can never drift apart.

### Permify permissions (`infra/permify/schema.perm`)

| Permify permission | Definition |
|---|---|
| `manage_catalog` | `owner or admin` |
| `manage_bookings` | `owner or admin or member` |
| `view_dashboard` | `owner or admin or member or viewer` |
| `publish_site` | `owner or admin` |
| `view_analytics` | `owner or admin or analyst` |
| `view_billing` | `owner or billing` |
| `manage_billing` | `owner or billing` |

(Permify `member` corresponds to Keycloak `staff`.)

### APISIX routes (`infra/apisix/apisix.yaml`)

| Route | Auth | Role enforcement |
|---|---|---|
| `/api/bookings/*` | Keycloak JWT (openid-connect, bearer_only) | Permify check in booking-service per mutation |
| `/api/bookings/public/*`, `/api/bookings/portal/*` | none (tenant-safe by construction) | — |
| `/api/payments/*` | Keycloak JWT | service-level |
| `/api/conversations/*` | Keycloak JWT | tenant-scoped reads |
| `/api/knowledge/*` | Keycloak JWT | service-level |
| `/api/billing/*` | Keycloak JWT; gateway injects `x-user-roles` | billing-engine: `X-Tenant-ID` must match; generate/void need `owner`/`admin` (403 otherwise) |
| `/webhooks/paystack` | none — HMAC-SHA512 signature (`x-paystack-signature`) | — |

### Dashboard sections (`apps/admin-web`)

| Section | Visible to | Guard |
|---|---|---|
| `/app/{org}/analytics` (KPI cards, trends, text-to-SQL) | owner, admin, analyst | server-side redirect in `page.tsx` + nav hiding in `org-nav.tsx` |
| `/app/{org}/billing` (invoices, QR, payouts) | owner, billing | server-side redirect in `page.tsx` + nav hiding in `org-nav.tsx` |
| all other sections | any authenticated tenant member (`view_dashboard`) | tenant membership via `tenant_slugs` claim in `layout.tsx` |

`staff` and `viewer` see **neither** Analytics nor Billing — both the nav
entry and the route itself are closed.

## Assigning roles (onboarding)

1. Open the Keycloak admin console → realm `opendesk` → **Users** → pick the
   user → **Role mapping** → **Assign role**.
2. Assign exactly one *primary* role (`owner`, `admin`, `staff`, `viewer`)
   and optionally add `analyst` and/or `billing` as overlays:
   - Bookkeeper who should not touch bookings: `viewer` + `billing`.
   - Data analyst: `viewer` + `analyst`.
   - Owner keeps everything: `owner` alone already implies all permissions.
3. The user signs out and back in (roles are read from the access token at
   sign-in and on every token refresh — the admin-web re-decodes
   `realm_access.roles` after each refresh).
4. Mirror the assignment in Permify for tenant-scoped enforcement: create the
   relationship tuple `organization:{tenant_id}#{relation}@user:{sub}` with
   relation `owner`/`admin`/`member`/`viewer`/`analyst`/`billing` (same
   tooling as the existing role tuples).

## White-label setup

Per-tenant branding lives in the public-site theme (`theme` jsonb on
`PUT /api/bookings/v1/site`), edited in **Public Site → White label**:

| Field | Theme key | Effect |
|---|---|---|
| Logo URL | `logoUrl` | header logo on `/p/{siteSlug}` |
| Primary colour | `primaryColor` | buttons/accents on the public page + embed |
| Brand display name | `brandName` | replaces the tenant business name in the public header/footer and the page `<title>` |
| Custom domain | `customDomain` | informational note only |

Custom domains: the `customDomain` field is a note for operators — the actual
mapping is done at the edge. Point the DNS record (CNAME) at the APISIX
gateway and add a host-based route that rewrites `book.example.com/*` to the
admin-web `/p/{siteSlug}` page. The booking page is tenant-safe regardless:
it only renders published sites fetched by slug.
