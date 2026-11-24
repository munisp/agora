# Chatwoot integration (SPEC-W118)

Self-hosted Chatwoot CE is the **human-agent console** for OpenDesk. The
chatwoot-bridge service syncs every channel conversation into it and
brings agent replies back — so the CRM (conversation-service timeline +
OpenSearch advanced search) sees all channels in one place.

> Cloud caveat: Chatwoot Cloud's free plan lost API/webhook access on 16
> July 2026; the MIT self-hosted Community Edition retains both. OpenDesk
> therefore runs the self-hosted stack (infra/docker-compose.crm.yml).

## Stack

| Container | Image | Role |
|---|---|---|
| chatwoot-rails | chatwoot/chatwoot:v4.18.0 | agent UI + API (host port 3120) |
| chatwoot-sidekiq | chatwoot/chatwoot:v4.18.0 | jobs (webhook delivery) |
| chatwoot-prepare | chatwoot/chatwoot:v4.18.0 | one-shot `db:chatwoot_prepare` |
| chatwoot-redis | redis:7-alpine | sidekiq queue + cache |
| chatwoot-bridge | services/chatwoot-bridge | the sync service (internal :7021) |

Chatwoot uses the shared postgres with its own `chatwoot` database
(third-party app, same trust model as Twenty). Only the bridge gets a
least-privilege role (`app_chatwoot_bridge_login`).

## Per-tenant onboarding (operator runbook)

1. Open the Chatwoot UI (`http://localhost:3120`), create an **account**
   for the tenant, then an **API-channel inbox** (Settings → Inboxes →
   Add inbox → API). Note the account id and inbox id from the URLs.
2. Create an API access token (Profile Settings → Access Token) for an
   administrator user of that account.
3. Register the mapping (from inside the compose network — the bridge is
   deliberately not host-published, GF4):

   ```sh
   docker compose exec chatwoot-bridge curl -sS      -X POST http://localhost:7021/internal/tenants/register \
     -H "X-Internal-Token: $CHATWOOT_BRIDGE_INTERNAL_TOKEN" \
     -H "Content-Type: application/json" \
     -d '{"tenant_id": "<opendesk-tenant-uuid>", "account_id": 1,
          "inbox_id": 2, "api_access_token": "<token>"}'
   ```

   The bridge validates the credential live, mints the per-tenant webhook
   token and auto-registers the account webhook (subscriptions:
   `message_type`) pointing back at itself. The response echoes the
   `webhook_url` — if `webhook_registered` is false (Chatwoot
   unreachable), register that URL manually under Settings → Integrations
   → Webhooks.
4. Optional hardening: pass `"hmac_secret": "<random>"` at registration
   and configure the same secret on the Chatwoot webhook; the bridge then
   requires a valid `X-Chatwoot-Signature` on every delivery.

From the next turn onward, the tenant's channel messages appear in the
Chatwoot inbox and agent replies flow back as `agent` turns.

## Privacy posture (deliberate)

Turn text forwarded to Chatwoot is the **PII-redacted** transcript event
text (W34 GF3 — redaction happens before publication). Human agents see
exactly what the CRM's OpenSearch index contains; raw PII stays in the
conversation DB with its own access path. Do not "fix" this by consuming
a raw source — that would bypass the platform's NDPA storage-limitation
posture.

## Loop prevention (two independent breakers)

1. **Outbound echo**: bridge-posted messages carry
   `source_id = od-<event id>`; the webhook drops any delivery whose
   source_id is in the `chatwoot_sent_messages` registry.
2. **Inbound echo**: turns appended from a Chatwoot reply are published
   with `data.origin = "chatwoot"`; the outbound consumer skips them.

## Accepted residuals (SPEC-W118 §8)

- At-least-once consumer: a crash between the Chatwoot POST and the
  registry insert can double-post ONE message on redelivery.
- Attachment-only Chatwoot messages are ignored (text-only v1).
- Contact per conversation (no cross-conversation contact merge yet).
- One API inbox per tenant (no per-channel inboxes yet).
