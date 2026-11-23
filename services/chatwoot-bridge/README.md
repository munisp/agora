# chatwoot-bridge

Bidirectional sync between OpenDesk conversations and a self-hosted
Chatwoot CE instance (SPEC-W118): every channel message (web chat,
WhatsApp, voice, USSD, API) becomes visible to human agents in Chatwoot,
and human-agent replies in Chatwoot land back in conversation-service —
so the CRM timeline and OpenSearch advanced search (SPEC-W117) see every
channel in one place.

Architecture Option A: messaging-gateway keeps owning the provider
channels; Chatwoot is the human-agent console; this bridge is the only
component that talks to both. Python 3.12, FastAPI, asyncpg (FORCE RLS),
aiokafka (direct-broker consumer, W10 TranscriptIndexer idiom).

## Flows

**Outbound (OpenDesk -> Chatwoot)** — `app/consumer.py` consumes
`ConversationTurn` CloudEvents from `opendesk.conversation.transcripts`:

- skips `data.origin == "chatwoot"` (own echo — loop breaker), roles
  outside {user, agent}, malformed events, unmapped/disabled tenants;
- first turn of a conversation: fetch the conversation via
  conversation-service's internal route, create the Chatwoot contact
  (`identifier = od-<conversation_id>`) + conversation, store the link;
- post the message with `source_id = od-<cloudevent id>` (user ->
  incoming, agent -> outgoing) and record it in the sent registry.
- Turn text arrives ALREADY PII-redacted (W34 GF3): human agents see
  exactly what the CRM's OpenSearch index sees. Deliberate NDPA posture.

**Inbound (Chatwoot -> OpenDesk)** — `POST /v1/chatwoot/webhook/{tenant_id}/{token}`
(`app/webhook.py`): per-tenant token (constant-time, 404 on unknown),
optional X-Chatwoot-Signature HMAC when `hmac_secret` is configured.
Only human-agent replies are processed (event `message_type` +
`message_type` == outgoing + not private + text + source_id NOT in
the sent registry + conversation reverse-mapped to a link); the turn is
appended with Idempotency-Key
`chatwoot-<account_id>-<cw_conversation_id>-<message_id>` and published
with `origin="chatwoot"` so the outbound consumer skips it.

## Admin (internal only, X-Internal-Token)

- `POST /internal/tenants/register` — validates the Chatwoot credential
  LIVE (inbox must be visible), mints a stable per-tenant webhook token,
  auto-registers the account webhook (best-effort; the URL is in the
  response for manual setup).
- `GET /internal/tenants/{tenant_id}` — mapping with masked token.
- `DELETE /internal/tenants/{tenant_id}` — mapping + links + registry.

No APISIX route exists for this service (standing rule); it is reachable
only on the compose network. Run registration via
`docker compose exec chatwoot-bridge curl ...` — see
docs/integrations/chatwoot.md.

## Environment

| Var | Default | Notes |
|---|---|---|
| `PG_DSN` | `postgres://app_chatwoot_bridge_login:…@postgres:5432/chatwoot_bridge` | least-privilege role (05-app-roles.sql) |
| `CHATWOOT_BASE_URL` | `http://chatwoot-rails:3000` | rails API origin |
| `BRIDGE_PUBLIC_BASE_URL` | `http://chatwoot-bridge:7021` | where Chatwoot calls back |
| `CONVERSATION_SERVICE_URL` | `http://conversation:7007` | internal routes |
| `CHATWOOT_CONVERSATION_INTERNAL_TOKEN` | — | must EQUAL conversation's `CONVERSATION_INTERNAL_TOKEN` |
| `CHATWOOT_BRIDGE_INTERNAL_TOKEN` | — | server side of the bridge's `/internal/*` |
| `KAFKA_BROKERS` | `kafka:9092` | comma-separated |
| `TRANSCRIPTS_TOPIC` | `opendesk.conversation.transcripts` | |
| `BRIDGE_GROUP` | `chatwoot-bridge` | consumer group |
| `SYNC_ENABLED` | `true` | kill switch for the outbound consumer only |
| `PORT` | `7021` | |

## Tests

`pytest tests/` — 44 tests: DDL parity (offline), RLS on embedded
Postgres (pgserver), Chatwoot client shapes (MockTransport), consumer
outcome matrix, webhook + admin ASGI routes. No network, no Kafka.
