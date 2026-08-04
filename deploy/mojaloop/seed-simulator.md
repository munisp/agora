# Mojaloop simulator seed (SPEC-W17, Agent C)

**Config/manifest + docs only. There is NO live Mojaloop cluster in this
environment, and this wave makes no live-install claims.** The uploaded §8
seeding strategy calls for a seeded Mojaloop simulator; the offline-
deliverable form is the deterministic participant manifest in
[`simulator-participants.yaml`](./simulator-participants.yaml) plus this
runbook.

## What the manifest seeds

- 1 simulated **hub** (`hub-seed-1`) — central ledger + ALS inside the
  simulator.
- 5 simulated **DFSPs** with deterministic participant ids
  `dfsp-seed-1..5`, all currency **NGN**.
- `env=staging` namespace label and `is-synthetic=true` markers on every
  entity.

## Isolation warnings (§8.9 — read before applying anywhere)

1. **Staging namespace only.** Never apply the manifest to a production hub
   or any scheme connected to real DFSPs/settlement accounts.
2. **No external wiring.** Simulator transfers must not reach a real ALS,
   oracle, or settlement bank; keep the simulator's endpoints cluster-local.
3. **Synthetic money only.** Pair with the TigerBeetle synthetic accounts
   manifest (`account_type=90`, §8.8, Agent B's
   `tigerbeetle_accounts.json`) so seeded balances are unambiguously
   non-production on the ledger seam from W14.
4. Seeded participant ids are deterministic (`dfsp-seed-N`) — reseeding
   replaces, never duplicates.

## Applying (when a simulator cluster exists)

```bash
kubectl -n staging apply -f deploy/mojaloop/simulator-participants.yaml
# then point the Mojaloop simulator's participant seeding job at the
# rendered manifest (simulator-specific; out of scope for this wave).
```

Out of scope for W17 (flagged, not silently dropped): live Helm install of
the Mojaloop simulator, ALS/oracle seeding, and end-to-end transfer tests —
all require a running cluster.
