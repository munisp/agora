# deploy/k3s — edge appliance (MVP)

Kustomize base for running OpenDesk core services on a single-node **k3s**
edge appliance (a salon/clinic back-office box) with Kafka MirrorMaker2
store-and-forward to the central cluster.

## Honest scope (what this is / is not)

**Is:**

- A working kustomize base: `namespace` + **3 representative Deployments**
  (`identity`, `booking`, `notification`) with Services, probes, and the
  same env wiring as the compose stack.
- Image names follow the compose build convention (`opendesk-<service>`,
  project name `opendesk` + service name). Build on the appliance
  (`docker build` + `k3s ctr images import` or `kind load`), or retag and
  push to a registry the appliance can reach.
- A **kind config** to rehearse the appliance locally, and a **MirrorMaker2
  skeleton** (Strimzi CR) for edge→central replication of
  `opendesk.transcripts-raw`.

**Is not (yet):**

- The remaining 7 app services are **not** included — they follow the exact
  same Deployment+Service pattern as the three here; add them as they are
  validated on the appliance (voice-agent-runtime additionally needs the
  `voice` profile model volumes/Ollama story decided).
- **Middleware is not in this base.** The manifests reference in-cluster
  DNS names (`postgres`, `kafka`, `redis`, `temporal`, `permify`,
  `keycloak`) — the MVP assumes either (a) a minimal middleware set deployed
  alongside (single-replica Postgres with the **local-path provisioner** —
  k3s ships it as the default StorageClass; no extra setup needed), or
  (b) tunnelled access back to the central cluster's middleware. Option (a)
  with local-path volumes is the intended appliance default; the middleware
  manifests are a follow-up.
- **No dapr sidecars (SPEC-W44 F16-9 — precise gap).** The compose stack
  injects `daprd` per service; the edge MVP runs services directly (same
  ports/env). Verified against the code: booking/notification/identity
  default `DAPR_HOST=daprd-<service>` (e.g. booking-service
  `internal/config/config.go` → `envStr("DAPR_HOST", "daprd-booking")`), and
  these manifests deliberately do NOT set `DAPR_HOST`, so any Dapr
  invoke/binding path (notification messaging bindings, saga deposit
  activities, consent checks) resolves to a non-existent host and fails.
  Paths that read a direct `*_URL` env (e.g. messaging-gateway
  `CONVERSATION_URL`) can be pointed at in-cluster Service DNS instead.
  **Fix path:** install the Dapr control plane on the appliance and
  uncomment the `dapr.io/enabled`/`dapr.io/app-id`/`dapr.io/app-port`
  annotation blocks added (commented) on the booking/notification pod
  templates this wave; identity's annotation recipe is documented in
  `identity-deployment.yaml`. Until then the saga/Dapr dataplane still
  expects the compose middleware, as stated below.
  Stated plainly: **today this base proves the services run on
  k3s; the full saga/Dapr dataplane still expects the compose middleware.**
- **gateway-edge is not in this base** (SPEC-W44 F15-01 parity note): when
  an edge Deployment is added, its image MUST be built with
  `--build-arg CARGO_FEATURES=fluvio-live` (the root compose now passes this
  by default; the Dockerfile ARG default flips to `fluvio-live` in the same
  wave). Without the feature, `main.rs` refuses to boot unless
  `GATEWAY_EDGE_ALLOW_SIM=true` — a default-built edge image crash-loops.
- **Internal tokens (SPEC-W44 K2):** `secret.yaml` carries DEV-ONLY
  placeholders for `identity/booking/notification/payments/billing`
  internal tokens; the three Deployments consume them via `secretKeyRef`
  (503 fail-closed when unset). Replace via SOPS/SealedSecrets/ESO for any
  real deployment, same rule as the DB credentials.
- MM2 needs the Strimzi operator installed and a real central endpoint
  (see `mirror-maker2.yaml` comments). It has not been run end-to-end.

## Usage

```bash
# Rehearse locally with kind:
kind create cluster --config deploy/k3s/kind-config.yaml --name opendesk-edge
kubectl apply -k deploy/k3s/
kubectl -n opendesk get pods

# On a real k3s appliance:
kubectl apply -k deploy/k3s/
```

Images must exist in the appliance's containerd before apply, e.g.:

```bash
docker build -t opendesk-booking:latest services/booking-service
docker save opendesk-booking:latest | sudo k3s ctr images import -
# (repeat per service; with kind: `kind load docker-image opendesk-booking:latest --name opendesk-edge`)
```

## Files

| File | Purpose |
| --- | --- |
| `namespace.yaml` | `opendesk` namespace |
| `identity-deployment.yaml` | identity-service Deployment+Service (:7001) |
| `booking-deployment.yaml` | booking-service Deployment+Service (:7002) |
| `notification-deployment.yaml` | notification-worker Deployment+Service (:7003) |
| `kustomization.yaml` | base wiring all of the above + MM2 |
| `kind-config.yaml` | local rehearsal cluster (gateway :9080, grafana :3002 NodePorts) |
| `mirror-maker2.yaml` | Strimzi KafkaMirrorMaker2 edge→central skeleton |
