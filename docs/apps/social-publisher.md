# Social Publisher (`social-publisher`)

SPEC-W21 Agent B — social media publishing and paid ads for the election
channel: connected accounts → reusable creatives → a posts queue → gated
paid ads (the **political-ads gates** are the point of the app).

- Backend: `services/booking-service/internal/socialpub/` (self-contained
  package; `RegisterRoutes` + `NewStore`/`DialStore` per the W21
  anti-collision contract — the integrator wires Deps/routes/config and
  the appgate entitlement `app_id = social-publisher` over the whole
  `/v1/social` route group).
- Provider seam: `services/booking-service/internal/socialpub/provider/`
  (`meta.go` / `tiktok.go` / `x.go` behind one `Publisher` interface).
- UI: `apps/admin-web/app/app/[orgSlug]/apps/social-publisher/` +
  `apps/admin-web/components/apps/social-publisher/`.

## Model

All four tables are FORCE-RLS `tenant_isolation` (idempotent embedded
`ensureSchema`, pg_policies-guarded — the devices/store.go idiom). Budgets
are **kobo int64** (₦1 = 100 kobo).

- **social_accounts** — `{id, tenant_id, provider meta|tiktok|x,
  account_ref, display_name, status connected|expired|revoked,
  political_ads_authorized bool DEFAULT false, created_at, updated_at}`.
  "Connect" is a **record only** — there is no OAuth flow (see
  Limitations). `political_ads_authorized` mirrors the provider-side
  authorization state; it is set by an operator AFTER the external process
  completes (runbook below).
- **social_creatives** — `{id, tenant_id, name, kind text|image|video,
  body, media_url null, disclaimer_text null, created_at, updated_at}`.
  `media_url` is required for image/video. The creative's disclaimer is
  the **fallback** the launch gate checks when the ad carries none.
- **social_posts** — `{id, tenant_id, account_id, creative_id, status
  draft|queued|publishing|published|failed, provider_post_id null, error
  null, published_at null, created_at}`. Publish is explicit
  (draft|queued|failed → published; failed is retryable).
- **social_ads** — `{id, tenant_id, account_id, creative_id, name,
  objective awareness|traffic|engagement, budget_kobo int64,
  daily_budget_kobo int64, targeting jsonb {lgas []text, age_min int,
  age_max int, interests []text}, political bool DEFAULT false,
  disclaimer_text null, status draft|review|active|paused|rejected,
  provider_ad_id null, error null, created_at, updated_at}`.

  Ad status machine (operator-driven; `→review` happens ONLY via the
  launch endpoint):

  ```
  draft → review → active ⇄ paused
     │        └─────┴──→ rejected (terminal)
     └───────→ rejected
  ```

## Endpoints (`/v1/social`)

Integrator wiring: tenant middleware → JWT → appgate `social-publisher` →
perms (recommended: GET → `view_analytics`, writes → `manage_bookings`).

```
GET    /v1/social/accounts?provider=&status=
POST   /v1/social/accounts                  # connect (record only)
PATCH  /v1/social/accounts/{id}             # status / display_name / account_ref / political flag
GET    /v1/social/creatives?kind=
POST   /v1/social/creatives
PATCH  /v1/social/creatives/{id}
GET    /v1/social/posts?status=&account_id=
POST   /v1/social/posts                     # {account_id, creative_id, status draft|queued (default queued)}
GET    /v1/social/posts/{id}
POST   /v1/social/posts/{id}/publish        # provider publish (mock default)
GET    /v1/social/ads?status=&account_id=
POST   /v1/social/ads                       # budget/age gates at input (400)
PATCH  /v1/social/ads/{id}                  # field edits (draft|review|rejected) + status machine
POST   /v1/social/ads/{id}/launch           # the gated launch
GET    /v1/social/ads/{id}/stats            # provider stats (mock default)
```

### curl walkthrough

```bash
B=http://localhost:8080; H='-H "content-type: application/json" -H "x-tenant-slug: acme"'

# 1. Connect an account (record only — no OAuth).
curl -s $H -X POST $B/v1/social/accounts -d '{
  "provider":"meta","account_ref":"page-123","display_name":"Campaign Page",
  "political_ads_authorized":true}'

# 2. Create a creative (with the fallback disclaimer).
curl -s $H -X POST $B/v1/social/creatives -d '{
  "name":"Town hall","kind":"text","body":"Town hall Saturday 10am.",
  "disclaimer_text":"Paid for by the Progress Committee"}'

# 3. Queue + publish a post.
POST_ID=$(curl -s $H -X POST $B/v1/social/posts -d \
  '{"account_id":"<acct>","creative_id":"<creative>"}' | jq -r .post.id)
curl -s $H -X POST $B/v1/social/posts/$POST_ID/publish
# → {"post":{"status":"published","provider_post_id":"mock-post-meta-…", …}}

# 4. Create + launch a political ad (gated).
AD_ID=$(curl -s $H -X POST $B/v1/social/ads -d '{
  "account_id":"<acct>","creative_id":"<creative>","name":"GOTV Ward 4",
  "objective":"awareness","budget_kobo":500000,"daily_budget_kobo":100000,
  "targeting":{"lgas":["Ikeja"],"age_min":18,"age_max":65,"interests":["politics"]},
  "political":true}' | jq -r .ad.id)
curl -s $H -X POST $B/v1/social/ads/$AD_ID/launch
# → {"ad":{"status":"review","provider_ad_id":"mock-ad-meta-…"}, "rejected":false}

# 5. Stats (deterministic while the mock is the default).
curl -s $H $B/v1/social/ads/$AD_ID/stats
```

## Hard gates (each covered by a handler test)

| Gate | Status | Detail |
|---|---|---|
| `political=true` launch without `account.political_ads_authorized` | **422** | Authorization is an external provider process (runbook below). |
| `political=true` launch without an effective disclaimer | **422** | Effective = ad's own `disclaimer_text`, else the creative's. |
| publish/launch on an `expired`\|`revoked` account | **409** | Reconnect (PATCH status → connected) first. |
| launch a non-draft ad / illegal status edge | **409** | State machine is enforced in the store (`FOR UPDATE`). |
| `budget_kobo ≤ 0`, `daily_budget_kobo ≤ 0` or `daily > total` | **400** | Checked at create AND update. |
| `targeting.age_min/age_max` outside `18..100` or `min > max` | **400** | 18+ floor matches the providers' political-ads policies. |

## Provider seam + mock defaults

`internal/socialpub/provider` — one `Publisher` interface
(`PublishPost`, `LaunchAd`, `AdStats`), three providers. **The mock is the
zero-config default** (same posture as W16 `FCM_MOCK=1`): no network,
deterministic sandbox ids (`mock-post-<provider>-<sha[:16]>`,
`mock-ad-<provider>-<sha[:16]>`) and plausible, deterministic stats
(impressions/reach/clicks/spend_kobo derived from the ad id hash).

Documented mock test hooks:

- `account_ref = "mock-fail"` → provider error on publish/launch
  (post lands in `failed`, launch answers 502).
- ad name containing `"mock-reject"` → policy rejection at launch
  (ad lands in `rejected`, `AdRejected` event, 200 with
  `{"rejected":true,"reason":…}`).

Mock switches (integrator wires; defaults in parentheses):

| Env | Default | Meaning |
|---|---|---|
| `SOCIAL_MOCK` | `1` | Master mock switch. |
| `META_MOCK` / `TIKTOK_MOCK` / `X_MOCK` | `1` | Per-provider mock. A provider leaves mock mode only when BOTH `SOCIAL_MOCK=0` AND its own switch are `0`. |
| `SOCIAL_EVENTS_TOPIC` | `opendesk.social.events.v1` | Lifecycle CloudEvents topic (empty disables). |
| `USAGE_EVENTS_TOPIC` | `opendesk.usage.events` | `social_ad_launched` metering (empty disables). |
| `DATABASE_URL` | — | `DialStore` fallback pool. |

With `*_MOCK=0` the provider seam currently answers an **honest
"not configured" stub** (same posture as the W16 APNs stub — the seam is
real, the credential wiring is a follow-up; no fake success claims). The
UI stats endpoint discloses `{"mock": true}` and renders a "mock data"
badge while the mock serves.

## Meta political-ads authorization runbook (EXTERNAL — plan for WEEKS)

Running political/issue ads on Meta is gated by Meta, not by this app.
The app's 422 gate mirrors Meta's server-side enforcement so operators
fail fast and honestly. The external prerequisites:

1. **Personal identity confirmation** for every ad-account admin
   (government ID + sometimes video selfie) at
   `facebook.com/id` — days to weeks.
2. **Page + ad account setup**: the Page must be linked to the ad
   account; the ad account must have a valid payment method.
3. **"Paid for by" disclaimer creation** in Meta's Ad Library flow:
   Meta verifies the funding entity (organization name, address, tax/business
   registration, website, email domain match). Verification is manual and
   takes **days to weeks**; budget the calendar accordingly.
4. **Ads about social issues, elections or politics** must then reference
   the approved disclaimer; Meta's review rejects ads without it (and may
   restrict the account on repeat violations).
5. Only AFTER steps 1–4 show green in Meta's UI does an operator tick
   `political_ads_authorized` on the Agora account record and copy the
   approved "Paid for by …" string into the ad/creative disclaimer.

TikTok and X have their own political-ad policies (TikTok broadly
prohibits political ads in many markets; X re-allowed them with its own
authorization program) — check current policy before launch; the app's
gate is provider-agnostic.

## Token setup (real API wiring — follow-up)

There is **no OAuth flow** in this wave: connecting an account records the
reference + status only. When the real provider clients land:

1. Meta: create a Meta app, complete App Review for
   `pages_manage_posts` / `ads_management`, generate a long-lived
   Page/User access token — store it in the secrets manager keyed by
   `account_ref`.
2. TikTok: Marketing API app → advertiser access token.
3. X: developer account → Ads API OAuth tokens.
4. Set `SOCIAL_MOCK=0` + the per-provider `*_MOCK=0` and wire the real
   `provider.Publisher` implementations behind the same interface (the
   handlers take them via `Deps.Publishers` — no handler change needed).

## Events + metering

- **Topic `opendesk.social.events.v1`** (integrator adds it to
  `infra/kafka/create-topics.sh`):
  - `com.opendesk.social.PostPublished` — `{tenant_id, post_id,
    account_id, creative_id, provider, provider_post_id, published_at}`.
  - `com.opendesk.social.AdLaunched` — `{tenant_id, ad_id, account_id,
    creative_id, provider, objective, budget_kobo, daily_budget_kobo,
    political, disclaimer_present, provider_ad_id}`.
  - `com.opendesk.social.AdRejected` — `{tenant_id, ad_id, account_id,
    creative_id, provider, objective, political, reason}`.
  - **Payloads NEVER include the creative body**, media URL or disclaimer
    text (privacy contract).
- **Metering** on `opendesk.usage.events`: one `social_ad_launched`
  `UsageRecord` (value 1) per successful launch — never on
  rejected/failed launches.

## UI

`/apps/social-publisher` (nav_route registered by the integrator in the
W18 catalog; reads `view_analytics`, writes `manage_bookings`):

- **Accounts** — provider + status badges, political-authorization badge,
  connect/edit dialogs (connect discloses the no-OAuth posture inline).
- **Creatives** — kind picker, body, media URL (required for
  image/video), disclaimer field.
- **Posts** — the queue with status pills + publish/retry buttons
  (disabled with an explanatory tooltip when the account is
  expired/revoked — the backend still answers 409).
- **Ads** — the wizard: objective, ₦ budgets (kobo fields with naira
  hints), targeting form (LGAs, 18–100 age band, interests) and the
  **political toggle that forces the disclaimer field and shows the
  authorization requirement inline**; launch/pause/resume buttons with the
  gate blocker as a tooltip; launch rejections surface as warning toasts.
- **Stats** — impressions/reach/clicks(CTR)/spend tiles with the honest
  "mock data" badge while the mock serves.

## Limitations (honest list)

- **No OAuth** — connect is a record; tokens are provisioned out-of-band
  (runbook above).
- **Mock providers are the default** — ids and stats are deterministic
  sandboxes, disclosed in API responses and the UI.
- **Real API clients are stubs** — `*_MOCK=0` answers "not configured"
  until the credential follow-up lands (same posture as W16 APNs).
- **Publish/launch are synchronous** — no scheduling/queue worker; a
  scheduled-publishing worker is a follow-up.
- **No creative-asset upload** — `media_url` references externally hosted
  media; it is stored verbatim, never fetched server-side.
- Meta template/policy review, political authorization and quality-rating
  ramps are **external prerequisites** (see runbook).
