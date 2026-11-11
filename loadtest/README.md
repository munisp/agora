# loadtest — k6 scripts (SPEC-W48-D, recon F8)

Greenfield k6 load/smoke assets for the booking public legs and the service
health sweep. Thresholds are the normative gates from
`docs/performance-budgets.md` (B1–B4 customer-API budget: p50 ≤ 300 ms,
p99 ≤ 1.5 s; SLO 1 decomposition). These scripts **assert shape and latency
budgets only** — nothing here mocks payments or any other production path.

## Scripts

| Script | Targets | Gate |
|---|---|---|
| `k6/booking-public.js` | GET public availability, POST waitlist claim, POST pay leg (W48-A A3), GET public reviews (W48-A A4) — via the APISIX gateway | per-leg `p(50)<300`, `p(99)<1500`; `http_req_failed<5%` |
| `k6/smoke.js` | `/healthz` of every core service on the compose network + optional overlays | `p(99)<1500`; core-tier `http_req_failed<1%` |

## Run

### booking-public (through the gateway, from the host)

The gateway is the only host-published surface (APISIX :9080, GF4), so this
script runs from the host against the edge:

```sh
# local k6
k6 run \
  -e BASE_URL=http://localhost:9080 \
  -e SITE_SLUG=<a-published-site-slug> \
  -e BOOKING_ID=<a-real-booking-id> \
  -e VUS=5 -e DURATION=1m \
  loadtest/k6/booking-public.js

# or via docker (no local k6 install)
docker run --rm -i --network host \
  -v "$PWD/loadtest:/loadtest" grafana/k6:0.52.0 run \
  -e BASE_URL=http://localhost:9080 \
  -e SITE_SLUG=<a-published-site-slug> \
  /loadtest/k6/booking-public.js
```

Notes:

- With the default nil-UUID `BOOKING_ID` the pay leg exercises the 404/409
  contract paths only. Point it at a real payable booking to see the 200
  `{link, tx_ref}` shape (requires `FLUTTERWAVE_SECRET_KEY` configured in
  payments-service, otherwise the endpoint fails closed with 503 — which the
  script also accepts as a valid contract answer).
- The waitlist-claim leg posts a nil token and asserts the rejection contract
  (400/404/410) plus gateway 429 behavior — valid tokens are single-use
  secrets and are never fabricated here.

### smoke (in the compose network)

Backends are not host-published, so run k6 inside the `agora_opendesk`
network (root compose `name: agora`, network `opendesk`):

```sh
docker run --rm -i --network agora_opendesk \
  -v "$PWD/loadtest:/loadtest" grafana/k6:0.52.0 run \
  /loadtest/k6/smoke.js
```

Every service URL is env-overridable (`IDENTITY_URL`, `BOOKING_URL`, …;
service name uppercased, dashes → underscores, suffix `_URL`). The
`graph-service` and `credit-bureau` probes are optional-tier: they are
reported but never fail the run (those services only exist under their
compose overlays).

## Validation

```sh
node --check loadtest/k6/booking-public.js
node --check loadtest/k6/smoke.js
```
