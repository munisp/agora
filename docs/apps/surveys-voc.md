# Surveys & VoC (NPS / CSAT / CES) — SPEC-W20 Agent B

Enterprise app `surveys-voc`: NPS/CSAT/CES/custom surveys with token-gated
public response collection, paced invite delivery over SMS / marketing push
(DND + quiet-hours automatic), a results dashboard (NPS, distributions,
breakdowns) and a naive VoC themes view. Backend: booking-service
`internal/surveys/` (self-contained package, anti-collision contract). UI:
admin-web `/app/{orgSlug}/apps/surveys-voc`.

| Area | Artifact |
| --- | --- |
| Model + validation + scoring + results/themes | `services/booking-service/internal/surveys/surveys.go` |
| Store (RLS, invites, PUBLIC token respond path) | `services/booking-service/internal/surveys/store.go` |
| Handlers + RegisterRoutes + tenant context | `services/booking-service/internal/surveys/handlers.go` |
| CloudEvents / metering / PacedSend envelopes | `services/booking-service/internal/surveys/events.go` |
| Tests (embedded-postgres, RLS, respond path) | `internal/surveys/*_test.go` |
| Admin UI | `apps/admin-web/app/app/[orgSlug]/apps/surveys-voc/`, `apps/admin-web/components/apps/surveys-voc/` |

## Entitlement

- **app_id: `surveys-voc`** — the INTEGRATOR gates the tenant-scoped route
  group via `internal/appgate` (W18) and wires it into the identity app
  catalog.
- **Exception:** `POST /v1/surveys/respond` is registered OUTSIDE the gated
  group on purpose (public customer path) — do NOT wrap it with the group
  middleware, JWT or appgate.

## Data model

All three tables: `ENABLE` + `FORCE ROW LEVEL SECURITY`, `tenant_isolation`
policy on `app.tenant_id`, bootstrap DDL idempotent + `pg_policies`-guarded
(mirroring `internal/devices/store.go`).

`surveys`:

| Column | Type | Notes |
| --- | --- | --- |
| `id` | uuid PK `gen_random_uuid()` | |
| `tenant_id` | uuid NOT NULL | RLS key |
| `name` | text | required, ≤200 B |
| `status` | text CHECK | `draft\|active\|paused\|archived` |
| `kind` | text CHECK | `nps\|csat\|ces\|custom` |
| `questions` | jsonb | `[{id, type rating\|text\|single\|multi, label, options [], required}]` |
| `trigger_kind` | text CHECK | `manual\|ticket_resolved\|booking_completed` |
| `channel` | text CHECK | `sms\|push_marketing` |
| `created_at` / `updated_at` | timestamptz | |

`survey_invites`:

| Column | Type | Notes |
| --- | --- | --- |
| `id` | uuid PK | |
| `tenant_id` / `survey_id` / `contact_id` | uuid NOT NULL | |
| `token` | text UNIQUE | **128-bit random hex** (32 chars) — the public respond capability |
| `status` | text CHECK | `queued\|sent\|answered\|expired` |
| `sent_at` / `answered_at` / `created_at` | timestamptz | |

`survey_responses`:

| Column | Type | Notes |
| --- | --- | --- |
| `id` | uuid PK | |
| `tenant_id` / `survey_id` | uuid NOT NULL | |
| `invite_id` / `contact_id` | uuid NULL | copied from the invite |
| `answers` | jsonb | `{question_id: value}` — number (rating), string (text/single), string[] (multi) |
| `score` | int NULL | first rating answer for nps/csat/ces; always null for custom |
| `submitted_at` | timestamptz | |

### The PUBLIC respond path (security contract)

`survey_invites` carries a **second, SELECT-only RLS policy**
`invite_token_access`:

```sql
CREATE POLICY invite_token_access ON survey_invites FOR SELECT
    USING (token = nullif(current_setting('app.invite_token', true), ''));
```

`POST /v1/surveys/respond` resolves the tenant **from the invite token,
never from `X-Tenant-Slug` or a JWT**: the transaction sets
`app.invite_token`, reads exactly the one invite the policy exposes
(unknown token → zero rows → **404**), then sets the standard
`app.tenant_id` from the invite row and proceeds fully tenant-scoped
(survey read, response insert, invite flip — one transaction). The token
policy is SELECT-only, tokens are unguessable (128-bit `crypto/rand` hex),
and the invite flip is status-guarded so a replayed or racing second
submit gets **409 `already_answered`** (idempotent per invite). Expired
invites → **410**.

**Rate limiting is an OPS concern**: enforce per-IP throttling of
`POST /v1/surveys/respond` at the APISIX edge (the endpoint is public by
design; a burst of garbage tokens costs one indexed SELECT each, a burst
of valid submits is bounded by the idempotency guard).

### Survey status machine

```
draft → active → paused ↔ active → archived
draft → archived          (archived is terminal: any PATCH → 409)
```

## Endpoints

Base: booking-service `/v1/surveys`. Tenant via `X-Tenant-Slug` (except
`/respond`). AuthZ (integrator wires, contract §3): reads →
`view_analytics`, writes → `manage_bookings` — recommended shape:
method-aware composition of httpapi's existing `require()` (GET/HEAD →
`view_analytics`, else `manage_bookings`).

```bash
# Create a survey (201; always starts draft; defaults kind=nps, trigger=manual, channel=sms)
curl -X POST $API/v1/surveys/surveys -H "X-Tenant-Slug: acme" -H "Authorization: Bearer $JWT" -d '{
  "name": "NPS — Q3", "kind": "nps", "channel": "sms",
  "questions": [
    {"id": "nps", "type": "rating", "label": "How likely are you to recommend us?", "required": true},
    {"id": "why", "type": "text", "label": "Why that score?"},
    {"id": "channel", "type": "single", "label": "Preferred channel", "options": ["sms", "app"]}
  ]
}'

# List (200) — filters: status, kind
curl "$API/v1/surveys/surveys?status=active" -H "X-Tenant-Slug: acme" …   # → {"surveys": [...]}

# Get one (200) — survey + invite/response stats rollup
curl $API/v1/surveys/surveys/$SID -H "X-Tenant-Slug: acme" …   # → {"survey": {...}, "stats": {invites_queued, invites_sent, invites_answered, invites_expired, responses}}

# Patch (200) — name/kind/channel/trigger_kind/questions, status via the machine (409 on illegal)
curl -X PATCH $API/v1/surveys/surveys/$SID … -d '{"status": "active"}'

# Send invites (200) — survey must be active (409 otherwise)
curl -X POST $API/v1/surveys/surveys/$SID/send … -d '{"contact_ids": ["<uuid>", "<uuid>"]}'
# → {"invites": [{id, contact_id, token, link, status}], "invites_created": 2,
#    "sent": 2, "queued": 0, "skipped": [{contact_id, reason: not_found|no_phone}],
#    "sends_deferred": false}

# PUBLIC respond (201) — NO tenant header, NO JWT
curl -X POST $API/v1/surveys/respond -d '{
  "token": "9f2b…128-bit-hex…", "answers": {"nps": 9, "why": "fast delivery", "channel": "sms"}
}'
# → {"response": {id, survey_id, score, submitted_at}, "survey": {id, name, kind}}
# errors: 404 unknown token · 409 already_answered (replay) · 410 expired · 400 invalid answers

# Results (200)
curl $API/v1/surveys/surveys/$SID/results -H "X-Tenant-Slug: acme" …
# → {"results": {survey_id, kind, response_count, score_distribution {"9": 4, …},
#     scored_count, nps (kind=nps only), promoters, passives, detractors,
#     mean_score, questions: [{id, type, label, answer_count, options: [{option, count}]}]},
#    "truncated": false}

# VoC themes (200) — survey_id optional (omit → all tenant surveys)
curl "$API/v1/surveys/voc/themes?survey_id=$SID" -H "X-Tenant-Slug: acme" …
# → {"themes": [{term, count} ×≤20], "responses_scanned": 128, "naive": true, "note": "…not NLP"}
```

### Question + answer validation

- Question types `rating | text | single | multi`; `single`/`multi` require
  ≥2 options; ids are unique per survey (blank ids auto-assign `q1…qn`).
- Rating answers are integers in **0–10** (one scale for every kind: NPS is
  0–10; CSAT/CES conventions 1–5 are subsets — the mean is scale-agnostic).
- `single` answers must be one of the options; `multi` an array of valid
  options; required questions must be answered; unknown answer keys are
  ignored (surveys may be edited after invites went out).
- Score = the FIRST rating question's answer for `nps|csat|ces`; `custom`
  never scores.

## Events (CloudEvents, transactional outbox)

Topic **`opendesk.surveys.events.v1`** (contract §5 — sent/answered):

| Type | Fires | Data |
| --- | --- | --- |
| `com.opendesk.surveys.InviteSent` | per invite whose paced send command was enqueued | `{tenant_id, survey_id, invite_id, contact_id, channel, ts}` |
| `com.opendesk.surveys.ResponseReceived` | per accepted response (never on the 409 replay path) | `{tenant_id, survey_id, response_id, invite_id?, contact_id?, kind, score?, submitted_at}` |

Invite delivery uses the fire-and-forget **PacedSend CloudEvent contract**
(notification-worker `notifyoutbox` consumer → `PacedSendWorkflow` →
`NotifyPaced`): one `com.opendesk.notifications.PacedSend` event per invite
on **`opendesk.notifications.outbox`**, whose data IS a
`workflows.PacedSendRequest`:

- `channel: sms` → `{kind: "geo_campaign", geo_campaign: {tenant_slug, campaign_id: <survey id>, channel: "sms", phone, name, text}}` (the worker's only SMS marketing route);
- `channel: push_marketing` → `{kind: "push_marketing", push: {tenant_slug, contact_id, phone?, title, body, data}}`.

Both kinds are **MARKETING-class** in the worker pacer table, so DND
suppression (activity-side) and quiet-hours deferral (workflow-side,
20:00–08:00 Africa/Lagos default) apply **automatically**. Invite
`status: sent` means the command was enqueued to the outbox; end-to-end
delivery state stays in the notification-worker.

## Metering

Metric **`survey_response_received`** (contract §4) on
`opendesk.usage.events`, value always 1, emitted once per accepted response
(never on replays), meta `{survey_id, response_id, kind, invite_id?, score?}`.

## Config envs (for the integrator — zero-config safe defaults)

| Env | Default | Notes |
| --- | --- | --- |
| `SURVEYS_DATABASE_URL` | (fall back to `DATABASE_URL`) | dedicated pool for `DialStore` (maxConns 4) |
| `SURVEYS_EVENTS_TOPIC` | `opendesk.surveys.events.v1` | empty disables lifecycle events |
| `SURVEYS_NOTIFICATIONS_TOPIC` | `opendesk.notifications.outbox` | empty disables invite sends — invites stay `queued`, send answers `sends_deferred: true` |
| `USAGE_EVENTS_TOPIC` | `opendesk.usage.events` | existing; empty disables the meter |
| `SURVEYS_PUBLIC_BASE_URL` | `https://app.opendesk.ng/s` | invite link base; links render as `<base>?t=<token>` |

Also extend `infra/kafka/create-topics.sh` with `opendesk.surveys.events.v1`
(integration step, SPEC-W20 §Integration).

## Limitations & follow-ups

- **Trigger automation is OUT of scope**: `ticket_resolved` /
  `booking_completed` are stored on the survey but nothing auto-sends —
  only manual `POST /send` ships. Follow-up: consume helpdesk/booking
  lifecycle events and call the same send path.
- **No hosted respondent page**: the invite link target
  (`SURVEYS_PUBLIC_BASE_URL`) must be a surface that POSTs
  `/v1/surveys/respond`; a hosted form is a follow-up.
- **VoC themes are NAIVE keyword frequency** (lowercase, stopword-stripped,
  top 20) — explicitly not NLP; no stemming, no language detection.
- **Invite expiry has no cron**: `expired` is a reserved status an operator
  can set (SQL/backoffice); auto-expiry is a follow-up.
- **Rate limiting** of the public respond endpoint is an APISIX edge
  concern (see above), not app code.
- Results aggregation scans at most 10 000 responses (`truncated: true`
  beyond; `response_count` stays exact); themes scan the latest 5 000.
- Re-sending to the same contact creates a NEW invite (fresh token); old
  unanswered invites keep working.
