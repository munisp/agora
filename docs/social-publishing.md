# Social publishing — honest provider configuration guide (U3)

This is the operator-facing truth about the social-publisher provider rails
(Meta, TikTok, X) in booking-service (`internal/socialpub/provider/`). For
the app model/endpoints see `docs/apps/social-publisher.md`.

## TL;DR

- **There is no env var that turns on live publishing.** The real-API path
  is an honest stub: with mocks off, every publish/launch/stats call fails
  closed with `provider unreachable: <provider> real API not configured:
  credential wiring is a follow-up (set <PROVIDER>_MOCK=1 or see
  docs/apps/social-publisher.md)` (`provider.notConfigured`, an
  `Error{StatusCode: 0}`). Real Meta/TikTok/X client wiring is a documented
  follow-up — no code claims otherwise.
- **Mock mode is the only working mode today**, and it is strictly opt-in
  (W39 SIM-005): a provider is mocked only when the master switch OR its
  per-provider switch is explicitly truthy.
- The settings read API exposes a `config_status` field (SPEC-W45) so the
  admin UI renders "not configured — connect provider" honestly instead of
  implying a live connection; connected `social_accounts` rows are records
  only (there is no OAuth flow).

## Environment variables (exact names)

Resolved in `provider.MockEnabledFromEnv` / booking-service
`internal/config/config.go`:

| Env | Code default | Dev compose (`.env.example`) | Meaning |
|---|---|---|---|
| `SOCIAL_MOCK` | `0` (OFF) | `0` | Master mock switch. Truthy (`1`/`true`/`yes`/`on`) → ALL providers mocked. |
| `META_MOCK` | `0` | `1` | Per-provider mock for Meta (Facebook/Instagram). |
| `TIKTOK_MOCK` | `0` | `1` | Per-provider mock for TikTok. |
| `X_MOCK` | `0` | `1` | Per-provider mock for X (Twitter). |
| `SOCIAL_EVENTS_TOPIC` | `opendesk.social.events.v1` | same | Lifecycle CloudEvents topic (empty disables events; see `docs/event-contracts.md` §3). |

A provider is in mock mode when `SOCIAL_MOCK` **or** its own
`<PROVIDER>_MOCK` is truthy (`provider.MockEnabled(master, perProvider)`).
With all four unset/false the provider is the real-API stub above. The dev
compose default (`SOCIAL_MOCK=0` + per-provider `=1`) means: mocked in dev,
and flipping one provider to its real rail later means setting only that
provider's switch to `0` — but note the real rail is still the stub until
credential wiring lands (follow-up below).

## What works in mock mode

Deterministic, no network (safe for dev/CI):

- `PublishPost` → `mock-post-<provider>-<sha256[:16]>` of the stable request
  key; account_ref `mock-fail` → provider error (post lands `failed`).
- `LaunchAd` → `mock-ad-<provider>-<sha256[:16]>`; an ad name containing
  `mock-reject` → `Rejected=true` with a policy-style reason (drives the
  `AdRejected` event + `rejected` status).
- `AdStats` → plausible, deterministic impressions/reach/clicks/spend_kobo
  derived from the ad id hash (same id → same stats).
- Metering counts only real (non-mock) publishes (W39 SIM-006 —
  `IsMock()`); the UI stats endpoint discloses `{"mock": true}` and renders
  a "mock data" badge.

## Going live (the actual follow-up)

1. Meta: app review for `pages_manage_posts` / `ads_management`, long-lived
   Page token; political ads additionally need Meta's multi-week
   authorization (runbook in `docs/apps/social-publisher.md` §"Meta
   political-ads authorization"). TikTok: Marketing API advertiser token.
   X: Ads API OAuth.
2. Implement the real `provider.Publisher` clients behind the existing
   interface (`Deps.Publishers` — no handler changes), storing tokens in the
   secrets manager keyed by `account_ref`.
3. Set `SOCIAL_MOCK=0` and the provider's `*_MOCK=0`.

Until step 2 lands, `config_status` stays `notConfigured` and publish calls
return the stub error — that is intentional, not a bug.
