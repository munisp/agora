# pii-redact — Fluvio WASM smart module

Redacts PII from transcript records on `opendesk.transcripts-raw` (SPEC §5)
before they are sunk to OpenSearch / the lakehouse.

## What it does

`filter_map` smart module over JSON transcript records:

```json
{"conversationId": "...", "tenantId": "...", "role": "user|agent", "text": "...", "ts": "...", "audioUrl?": "..."}
```

- Redacts phone numbers (E.164-ish: optional `+`, 8–15 digits with common
  separators — e.g. `+1 (415) 555-0132`, `+44 20 7946 0958`) → `[PHONE REDACTED]`
- Redacts email addresses → `[EMAIL REDACTED]`
- Sets `"redacted": true` and passes the record through with its key
- Drops (maps to `None`) records that don't parse as a transcript, so sinks
  never see unredacted free text

Pure redaction logic (`redact_text`, `redact_record`) is unit-tested with
sample records (`cargo test`).

## Build & load (smdk)

Requires the Fluvio CLI and the smartmodule development kit:

```bash
curl -fsS https://hub.infinyon.cloud/install/install.sh | bash   # fluvio CLI
fluvio install smdk                                               # smdk
rustup target add wasm32-unknown-unknown
```

```bash
cd infra/fluvio/pii-redact
smdk build                # -> target/wasm32-unknown-unknown/release/pii_redact.wasm
smdk test --text '{"conversationId":"c1","tenantId":"acme","role":"user","text":"call +14155550132","ts":"2024-01-01T00:00:00Z"}'
smdk load                 # registers the smart module with the cluster in SmartModule.toml
```

Verify:

```bash
fluvio smartmodule list          # expect opendesk/pii-redact@0.1.0
```

## Topic setup

`topic-setup.sh` creates the raw topic (6 partitions, SPEC §4 partition count):

```bash
./topic-setup.sh                 # uses `fluvio` CLI against the current profile
FLUVIO_PROFILE=opendesk ./topic-setup.sh
```

## Usage

Consume with the smart module applied (edge live-tail, sink connectors):

```bash
fluvio consume opendesk.transcripts-raw --smartmodule opendesk/pii-redact@0.1.0
```

For the OpenSearch / lakehouse sink connectors, reference the smart module in
the connector config:

```yaml
transforms:
  - uses: opendesk/pii-redact@0.1.0
```

## Notes

- Crate: `fluvio-smartmodule` (the renamed successor of `smartmodule-sdk`),
  built to `wasm32-unknown-unknown` as a `cdylib`. Note: 0.6.x has no
  `derive` feature — the `#[smartmodule]` macro is re-exported from the
  bundled `fluvio-smartmodule-derive` dependency by default.
- The wasm link intentionally leaves host imports (`copy_records`, …)
  undefined; `.cargo/config.toml` passes `--allow-undefined` to rust-lld
  for the `wasm32-unknown-unknown` target so `cargo build` / `smdk build`
  work out of the box (the imports are resolved by the Fluvio SPU runtime).
- The raw topic intentionally keeps unredacted records inside the platform
  trust boundary; redaction is enforced at consumption/sink time. gateway-edge
  live-tails the raw topic for `/ws/transcripts`; external sinks must apply
  this smart module.
