# SPEC-W21 — Election-Channel Readiness (WhatsApp campaign kind + Social Publisher)

Two builders + one integration step. Delivery protocol identical to SPEC-W12
§Delivery ($HOME workspaces; additive rsync; md5-verify FROM /mnt; real tails).
Context: the platform already ships campaign-studio journeys (sms/push_marketing
kinds), geo-campaigns with ward/LGA targeting, DND/quiet-hours enforcement, and a
messaging-gateway with whatsapp/telegram/sms providers. W21 closes the two gaps
for election outreach: (1) WhatsApp business-initiated TEMPLATE sends as a
first-class campaign channel, (2) social media publishing/ads.

## Anti-collision architecture (binds everyone — same as W19/W20)
- Agent A owns: services/notification-worker/** (paced kind + activity + tests),
  services/messaging-gateway/** (IF a gateway-side change is the right idiom —
  inspect first), services/booking-service/internal/campaignstudio/studio.go +
  evaluate.go + workflow.go ONLY for the new step kind (additive; do NOT refactor),
  plus their tests. NOTHING else in booking-service.
- Agent B owns: services/booking-service/internal/socialpub/**,
  apps/admin-web/app/app/[orgSlug]/apps/social-publisher/**,
  apps/admin-web/components/apps/social-publisher/**, docs/apps/social-publisher.md.
  NOTHING else.
- Neither touches: httpapi/server.go, cmd/server/main.go, config.go, go.mod/go.sum,
  catalog.yaml, .env.example, org-nav, components/apps/types.ts. The INTEGRATOR
  wires Deps/routes/envs/appgate/catalog/topics/env-example after both land.
- Agent A documents envs + the exact send contract in its report; Agent B
  documents envs in package doc + docs page.

## Shared contracts (as W19/W20)
RLS FORCE tenant_isolation on every new table; uuid PKs; money kobo int64;
TenantFromContext idiom; CloudEvents outbox idiom (graceful no-op when topic
empty); embedded-postgres store tests + RLS isolation + handler tests;
gofmt/build/vet green per package; UI per W18/W19 idioms (unwrap<T>() read-only,
BFF /api/bookings/v1/..., honest states, tsc --noEmit green from $HOME copy).

## Agent A — whatsapp_campaign channel kind
Goal: campaign-studio journey steps can send WhatsApp TEMPLATE messages
(business-initiated), with the same DND/quiet-hours/pacing guarantees as SMS
marketing.
1. notification-worker: add kind `whatsapp_campaign` to workflows/paced.go —
   payload PacedWhatsAppCampaignSend {tenant_slug, contact_id, phone,
   template_name, language (default en), params []string (positional template
   params, max 10), campaign_id null}. MARKETING classification (same guard
   path as geo_campaign/push_marketing: DND registry + tenant opt-out +
   quiet-hours deferral). Mirror the existing kind plumbing end-to-end:
   consumer case, PacedSendWorkflow branch, activity.
2. Activity transport: inspect how geo_campaign SMS actually leaves the worker
   (activities/geo.go + channels.go) and how messaging-gateway's whatsapp.go
   provider is structured. Choose the ESTABLISHED idiom (direct Meta Cloud API
   call from the worker with WHATSAPP_CLOUD_API_TOKEN + WHATSAPP_PHONE_NUMBER_ID
   envs, or POST to a messaging-gateway internal endpoint if one exists for
   outbound). Whichever you choose: WHATSAPP_MOCK=1 must be the zero-config
   default (mock logs the send + returns a fake wamid, exactly the FCM_MOCK
   posture from W16). Document the real-credential setup in the docs page.
3. campaignstudio: steps jsonb gains kind `whatsapp` (validation: kind whatsapp
   REQUIRES template_name; template text field repurposed as params hint —
   inspect the step struct and extend minimally: add template_name, language,
   params fields to the step type, keep unknown-kind rejection for everything
   else). Step execution enqueues the whatsapp_campaign PacedSend CloudEvent
   mirroring how sms/push_marketing steps enqueue today.
4. Tests: kind classification/guard tests (marketing → DND applied), payload
   marshal contract test, mock-send activity test, studio step-validation tests
   (whatsapp without template_name → 400), step-execution enqueue test.
5. Docs: update docs/apps/campaign-studio.md (whatsapp step section) + add the
   env table to your report for the integrator (WHATSAPP_MOCK,
   WHATSAPP_CLOUD_API_TOKEN, WHATSAPP_PHONE_NUMBER_ID, WHATSAPP_BUSINESS_ACCOUNT_ID
   if used). Note honestly: Meta template approval + quality-rating ramp are
   external prerequisites (add to docs).

## Agent B — social-publisher app
Backend internal/socialpub (self-contained, RegisterRoutes/NewStore/DialStore
like W19/W20 packages):
Tables (all FORCE RLS tenant_isolation):
- social_accounts {id, tenant_id, provider meta|tiktok|x, account_ref,
  display_name, status connected|expired|revoked, political_ads_authorized bool
  DEFAULT false, created_at, updated_at}
- social_creatives {id, tenant_id, name, kind text|image|video, body text,
  media_url text null, disclaimer_text text null, created_at, updated_at}
- social_posts {id, tenant_id, account_id, creative_id, status draft|queued|
  publishing|published|failed, provider_post_id null, error null, published_at
  null, created_at}
- social_ads {id, tenant_id, account_id, creative_id, name, objective
  awareness|traffic|engagement, budget_kobo int64, daily_budget_kobo int64,
  targeting jsonb {lgas []text, age_min int, age_max int, interests []text},
  political bool DEFAULT false, disclaimer_text text null, status draft|review|
  active|paused|rejected, provider_ad_id null, error null, created_at,
  updated_at}
Endpoints /v1/social: GET/POST/PATCH /accounts; GET/POST/PATCH /creatives;
GET/POST /posts + POST /posts/{id}/publish (adapter; mock default) + GET
/posts/{id}; GET/POST/PATCH /ads + POST /ads/{id}/launch + GET /ads/{id}/stats.
Gates (test them): launch with political=true → 422 unless account
 political_ads_authorized=true AND effective disclaimer_text non-empty (ad's
 own or creative's); publish/launch on revoked|expired account → 409; budget
 >0; daily ≤ total; targeting age_min ≤ age_max, 18..100.
Provider seam: internal/socialpub/provider/{provider.go,meta.go,tiktok.go,x.go}
 — interface Publisher {PublishPost, LaunchAd, AdStats}; SOCIAL_MOCK=1 default
 per provider (META_MOCK/TIKTOK_MOCK/X_MOCK or one SOCIAL_MOCK — your choice,
 document) returning deterministic sandbox ids (mock-post-*, mock-ad-*) and
 plausible stats; real API wiring documented as credential-follow-up in docs
 (honest stub posture, like W16 APNs). No real OAuth: account connect = record
 + status; docs runbook covers Meta political-ads authorization (external,
 weeks) and token setup.
Metering: social_ad_launched (opendesk.usage.events). Events: topic
 opendesk.social.events.v1 — com.opendesk.social.PostPublished, AdLaunched,
 AdRejected (payloads exclude creative body).
UI /apps/social-publisher: accounts list (provider + authorization badges),
creatives editor (kind picker, body, disclaimer field), posts queue (publish
button + status pills), ads wizard (objective, budget ₦ display from kobo,
targeting form, political toggle that FORCES the disclaimer field + shows the
authorization requirement inline), stats tiles. Docs docs/apps/social-publisher.md
(model, endpoints + curl, gates, provider setup, political-ads runbook,
limitations incl. no-OAuth + mock defaults).
app_id for integrator: social-publisher.

## Integration step (after both land — lead dispatches)
1. booking-service: wire socialpub (Deps, RegisterRoutes, appgate social-publisher,
   requireReadWrite, config envs with safe defaults incl. mock=1, main.go dial).
   campaignstudio needs NO wiring change (Agent A's change is package-internal)
   but FULL module tests must stay green.
2. identity-service: catalog.yaml gains the 17th app row social-publisher
   (mirror the W19/W20 rows' shape: nav_route /apps/social-publisher, tier,
   description; verify loader test still passes).
3. infra/kafka/create-topics.sh: opendesk.social.events.v1.
4. .env.example: ONE additive W21 block (social mocks + whatsapp envs from
   Agent A's report) — integrator is the SOLE editor this wave.
5. w21_routes_test.go mirroring w19/w20 (social group gated; mount proofs).
Then independent verification gate → push.
