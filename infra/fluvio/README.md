# infra/fluvio — Fluvio streaming (SPEC §5)

## What runs here (honest dev setup)
There is no official batteries-included single-container Fluvio image for plain
Docker (InfinyOn targets Kubernetes / the `fluvio` CLI on a host). The honest
dev approximation in `docker-compose.core.yml` is:

- **`fluvio`** — `infinyon/fluvio:latest` running `start-cluster.sh`, which
  launches the SC (`fluvio-run sc`, bound to `0.0.0.0:9003`) plus one local SPU
  (`fluvio-run spu`, id 5001). This is a **single-node dev cluster**, not a
  production topology.
- **`fluvio-topics`** — a one-shot sidecar (same image) that runs
  `setup-topics.sh`: points the CLI profile at `fluvio:9003` and creates topic
  **`opendesk.transcripts-raw`** (6 partitions, rf 1, matching Kafka dev sizing).

The exact `fluvio-run` flags can drift between image tags; if the cluster
container fails after a `:latest` bump, check `docker logs fluvio` and adjust
`start-cluster.sh` (or pin a tag). Everything here is dev-only.

## Topic contract
- `opendesk.transcripts-raw` — fed by **conversation-service** for
  high-throughput edge/telephony transcript ingestion.
- The WASM smart module **`pii-redact`** (owned by another agent under
  `infra/fluvio/pii-redact/`) redacts phones/emails before the sink to
  OpenSearch + the lakehouse. Deploy it after build with:
  ```bash
  fluvio smartmodule create pii-redact --wasm-file infra/fluvio/pii-redact/target/wasm32-wasip1/release/pii_redact.wasm
  ```
- **gateway-edge** consumes `opendesk.booking.events` via Fluvio mirror with
  Kafka fallback for WebSocket fan-out (SPEC §5).

## Manual topic management
```bash
docker exec fluvio-topics sh /opt/fluvio/setup-topics.sh   # re-run (idempotent)
docker exec fluvio fluvio topic list                       # requires profile; see setup-topics.sh
```

## Deployment (smartmodule + kafka-sink connector)

`deploy.sh` performs the full deploy of the redaction path:

```bash
./infra/fluvio/deploy.sh
```

1. **Build** the `pii-redact` smartmodule — via `smdk build` when the CLI is
   installed, otherwise a `cargo build --target wasm32-wasip1` fallback
   (host or `rust:1` container; same artifact at
   `pii-redact/target/wasm32-wasip1/release/pii_redact.wasm`).
2. **Load** it into the dev cluster (`fluvio smartmodule create`, falling
   back to `update`), using the local `fluvio` CLI or a one-shot
   `infinyon/fluvio` container on the `opendesk` network.
3. **Verify** the Kafka sink topic `opendesk.conversation.transcripts`
   exists (runs the kafka-topics init if not).
4. **Deploy the connector** described by `kafka-sink-connector.yaml`:
   kafka-sink, topic `opendesk.transcripts-raw` → Kafka
   `opendesk.conversation.transcripts`, with
   `opendesk/pii-redact@0.1.0` in `transforms` — redaction happens inline
   before records reach Kafka/OpenSearch/the lakehouse.

**Honest connector status:** step 4 requires the CDK CLI
(`cdk deploy start -c infra/fluvio/kafka-sink-connector.yaml`) or a
k8s-hosted Fluvio cluster; without `cdk`, the script loads the smartmodule
and prints the exact command instead of failing — the connector runtime is
not containerised in this dev stack. Until the connector runs, the
Kafka-mirror path (conversation-service producing directly, plus the
gateway-edge mirror) remains the live transcript flow.

## Enabling the Fluvio transcript sink (W47-H / A-MW MW-M1)

The full Fluvio plane is deployed (cluster, topics, pii-redact smartmodule,
gateway-edge live-tail) but **no shipped image can produce to the Fluvio
topic by default** — the conversation-service Dockerfile does not install the
optional `fluvio` pip extra (`pyproject.toml`: `fluvio = ["fluvio==0.16.1"]`),
so `TRANSCRIPT_SINK=fluvio` logs a warning and falls back to `KafkaSink`
(`sinks.py build_sink` — fail-soft, same topic name on Kafka; never silent
data loss). The Dockerfile is service code and was out of W47-H's ownership
scope; **flagged to the lead** for the one-line change below.

Exact enable steps:

1. **Service code (lead/W47-E):** in `services/conversation-service/Dockerfile`
   change the install to pull the extra: `pip install .[fluvio]` (or
   `pip install fluvio==0.16.1` alongside `pip install .`).
2. **Compose (already wired, W47-H):** the conversation service now exposes
   `TRANSCRIPT_SINK` (default `kafka`) and `FLUVIO_TOPIC`
   (default `opendesk.transcripts-raw`) env passthroughs in
   `docker-compose.yml`. Set `TRANSCRIPT_SINK=fluvio` in `.env` after step 1.
   The Fluvio python client connects via its profile — inside the compose
   network the SC is `fluvio:9003` (see `start-cluster.sh`); the client's
   default profile targets localhost, so the service image must also carry a
   Fluvio profile pointing at `fluvio:9003` (or the client must gain an
   endpoint env — a second small service-code change).
3. **Connector (manual, unchanged):** `./infra/fluvio/deploy.sh` — needs the
   `cdk` CLI; without it the script prints the exact `cdk deploy start`
   command and the Kafka-mirror path stays live.

Kafka remains the default sink; nothing above flips the default stack.
