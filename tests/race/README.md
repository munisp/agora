# tests/race — full booking-service `-race` run (W41-4)

Execution-only evidence task: the whole booking-service test suite under
the Go race detector. Rationale for the flags:

* `-p 1` — the package tests pin fixed Postgres ports (5562/5563/5564 are
  triple-booked across packages; httpapi uses 5432); parallel package runs
  would cross-collide.
* The store tests boot REAL embedded PostgreSQL via
  fergusstrange/embedded-postgres (downloads PG binaries at test time —
  network required) and skip under `-short`.
* gcc is present, so `-race` (CGO-backed) works.

## Exact command (copy-paste)

```bash
# one-time toolchain
mkdir -p /tmp/go && tar -C /tmp -xzf /mnt/agents/go1.23.4.linux-amd64.tar.gz
# never build inside the /mnt mirror — copy the tree first
rm -rf /tmp/race-ws && mkdir -p /tmp/race-ws
cp -r /mnt/agents/output/opendesk/services/booking-service /tmp/race-ws/
cd /tmp/race-ws/booking-service
GOFLAGS=-mod=readonly GOCACHE=/tmp/race-ws/gocache GOMODCACHE=/tmp/race-ws/gomodcache \
  GOPROXY=https://goproxy.cn,direct \   # proxy.golang.org unreachable from this sandbox (verified 2026-08-17)
  /tmp/go/bin/go test -race -p 1 ./... 2>&1 | tee /tmp/race-ws/race.log
echo "exit=$?"
```

Expected duration on 2 CPU / 4 GB: 10-25 min (module downloads + PG binary
download + instrumented suite). The suite spawns real Postgres child
processes; keep the runner at >=4 GB.

## Skip taxonomy (fill honestly in RESULTS-booking.md)

Classify every skipped test as one of:

* `SHORT` — skipped under `-short` only (not applicable: we run full);
* `EXTERNAL_BLOCKED` — needs Kafka/Temporal/Dapr/OpenSearch/LiveKit/docker;
* `ENV` — missing binaries/network (enumerate exactly which);
* `PREEXISTING` — failing/skipped before W41 (must be proven, e.g. by git
  history or a non-race baseline run).

Any genuine race report is a NEW finding: do not suppress — file it and fix
in-wave.
