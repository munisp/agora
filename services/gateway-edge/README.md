# gateway-edge

OpenDesk edge gateway (SPEC §3/§5/§12): per-tenant WebSocket fan-out of
booking events and live transcripts.

- Port: **7005**. APISIX routes `/ws/*` here (SPEC §12).
- Stack: Rust 2021, axum 0.7 (`ws` extractor, built on tokio-tungstenite),
  rdkafka, jsonwebtoken. Optional Fluvio consumer behind `--features fluvio-live`.

## Endpoints

| Method | Path | Description |
|---|---|---|
| GET | `/healthz` | dependency-aware liveness (F15-07): 200 `{"status":"ok"}` only while every consumer task (Kafka booking events, Kafka enriched turns, Fluvio transcript tail) is alive with a heartbeat ≤30s old; otherwise 503 `{"status":"degraded"}` with per-consumer detail |
| GET | `/metrics` | Prometheus text exposition |
| GET | `/ws?tenant={slug}&token={jwt}` | live booking events (Kafka `opendesk.booking.events`) |
| GET | `/ws/transcripts?tenant={slug}&token={jwt}` | live transcript tail (Fluvio `opendesk.transcripts-raw`) |
| GET | `/ws/intel?tenant={slug}&token={jwt}` | live enriched turns — sentiment/intent/entities (Kafka `opendesk.conversation.enriched`, SPEC-W3 §4) |

## AuthN/Z

JWT (RS256) validated against the Keycloak `opendesk` realm JWKS
(`KEYCLOAK_JWKS_URL`, cached with `JWKS_CACHE_TTL_SECS`, refreshed early on
unknown `kid`). Tenant authorization uses the `tenant_slugs` claim (SPEC §8
group attribute mapper): the `tenant` query param must be present in the
claim or the upgrade is rejected with 403. `EDGE_AUTH_DISABLED=true` disables
validation entirely — dev only.

## Fan-out & backpressure

Each tenant channel is a bounded `tokio::sync::broadcast` ring buffer
(`WS_CHANNEL_CAPACITY`, default 256). **Drop-slow policy**: consumers lagging
past capacity lose their oldest messages, receive a
`{"type":"lagged","dropped":n}` notice on the socket, and the drop is counted
in `gateway_events_dropped_slow_consumer_total`.

Event routing: CloudEvents `tenantid` extension (falling back to
`data.tenantId` / `data.tenant_id`) determines the target channel.
Transcript records route on `tenantId`.

## Sources

- **Kafka (primary)**: `opendesk.booking.events`, consumer group
  `gateway-edge`, offset reset `latest`.
- **Fluvio (live tail)**: `opendesk.transcripts-raw`, one partition consumer
  per partition, streaming from `Offset::end()`. Compiled with
  `--features fluvio-live`; the default build ships a stub that is
  **fail-closed** — the service refuses to start unless the operator
  explicitly opts in with `GATEWAY_EDGE_ALLOW_SIM=true` (SPEC §5: Fluvio mirror
  + Kafka fallback). The integration surface is isolated in
  `src/fluvio_consumer.rs` in case the pinned `fluvio` crate version drifts.

## Env vars

| Var | Default | Description |
|---|---|---|
| `PORT` | `7005` | HTTP listen port |
| `RUST_LOG` | `info` | tracing filter (JSON logs) |
| `KAFKA_BROKERS` | `kafka:9092` | Kafka bootstrap servers |
| `KAFKA_GROUP_ID` | `gateway-edge` | consumer group |
| `BOOKING_EVENTS_TOPIC` | `opendesk.booking.events` | booking events topic |
| `ENRICHED_TOPIC` | `opendesk.conversation.enriched` | enriched turns topic (→ `/ws/intel`) |
| `KEYCLOAK_JWKS_URL` | `http://keycloak:8080/realms/opendesk/protocol/openid-connect/certs` | JWKS endpoint |
| `KEYCLOAK_ISSUER` | `http://keycloak:8080/realms/opendesk` | expected `iss` |
| `KEYCLOAK_AUDIENCE` | _(unset)_ | expected `aud` (validated when set) |
| `EDGE_AUTH_DISABLED` | `false` | dev-only: skip JWT validation |
| `JWKS_CACHE_TTL_SECS` | `300` | JWKS cache TTL |
| `WS_CHANNEL_CAPACITY` | `256` | per-tenant broadcast buffer (drop-slow) |
| `FLUVIO_ENDPOINT` | `fluvio:9003` | Fluvio SC endpoint |
| `FLUVIO_TRANSCRIPTS_TOPIC` | `opendesk.transcripts-raw` | transcripts topic |
| `FLUVIO_PARTITIONS` | `6` | partitions to tail (SPEC §4: 6 partitions) |
| `GATEWAY_EDGE_ALLOW_SIM` | `false` | explicit opt-in to run without the Fluvio transcript tail in builds lacking `--features fluvio-live`; when unset/false the service **refuses to start** rather than silently simulate consumption |

## Run

```bash
EDGE_AUTH_DISABLED=true cargo run   # dev without Keycloak
cargo test                          # bus drop-slow + auth-claim unit tests
cargo build --features fluvio-live  # include the real Fluvio consumer
docker build -t opendesk/gateway-edge .
```

Graceful shutdown on SIGINT/SIGTERM; consumer tasks stop via a shutdown
watch channel before exit.
