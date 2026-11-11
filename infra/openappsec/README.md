# infra/openappsec — open-appsec WAF (profile-gated, detect-learn by default)

**The default stack runs with NO WAF.** The `openappsec` agent and the
`apisix-appsec` gateway attachment are both behind the compose profile
`appsec` (see the enable-path comment block above the services in
`infra/docker-compose.edge.yml`).

## Enable

```bash
docker compose stop apisix            # ports 9080/9180 collide otherwise
docker compose --profile appsec up -d openappsec apisix-appsec
```

`apisix-appsec` (ghcr.io/openappsec/apisix-attachment) serves the SAME
`infra/apisix/apisix.yaml` standalone config on the same ports, sharing IPC
with the `openappsec` agent (ghcr.io/openappsec/agent), which mounts this
directory at `/ext/appsec`.

## Policy posture

`local_policy.yaml` runs everything in **`detect-learn`**: requests are
inspected and logged, nothing is blocked. That is deliberate for dev — but no
doc or demo may claim blocking WAF protection from the default stack
(A-MW MW-M5). To enforce, flip `policies.default.mode` (and the specific
rules) to `prevent-learn` after the contextual ML engine has learned the
traffic baseline; the agent watches the file and hot-reloads, no restart
needed. `override-mode: as-top-level` on the practices makes them inherit the
policy-level mode, so one flag flips enforcement.
