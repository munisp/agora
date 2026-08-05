# SPEC-W23 — Brand-surface rename: OpenDesk → Agora

**Goal:** rename the customer-visible brand layer to "Agora" while deliberately keeping all internal technical identifiers as `opendesk` (full code-level rename is explicitly deferred).

## IN SCOPE (change these)

1. **Markdown prose** — `README.md`, `docs/**/*.md`, `apps/*/README.md` (96 files contain "opendesk"/"OpenDesk").
   - Prose product-name mentions: `OpenDesk` → `Agora`; sentence-level lowercase `opendesk` referring to the product → `Agora`.
   - README.md gets a continuity line near the top: `> Agora (formerly OpenDesk) — internal module paths, Kafka topics, and env vars retain the opendesk identifier; see SPEC-W23.md.`
2. **UI-visible strings** — `apps/admin-web/app/**` (tsx), `apps/mobile/**` (tsx/theme.ts), `apps/field-pwa/index.html`, `apps/marketing/*.html`.
   - User-visible copy and `<title>` tags: `OpenDesk` → `Agora` (e.g. `<title>OpenDesk Field</title>` → `<title>Agora Field</title>`; marketing site titles; sign-in page copy).
3. **docker-compose project name** — top-level `name: opendesk` → `name: agora` in docker-compose*.yml. Do NOT rename individual service names, container_name entries, networks, or volumes (container names may be referenced in runbooks — a mismatch is acceptable, renaming them is not in scope).

## OUT OF SCOPE (never touch — hard gate)

- Code identifiers: Kafka topics (`opendesk.*`), Go module paths/package names, env vars (`OPENDESK_*`), config keys, file paths, URLs/domains, Docker service/container/network/volume names.
- npm package names and import specifiers (`@opendesk/admin-web`, `opendesk-field`) — deferred to the future full rename wave (changing them requires import rewrites).
- Anything inside backtick code spans or fenced code blocks in markdown — those quote identifiers and must stay byte-identical.
- Go/Rust/Python source files: zero changes this wave.

## METHOD RULES

- Work on `/mnt/agents/output/opendesk` (FUSE — md5 double-read every file you write).
- Prefer scripted sed/perl with explicit include/exclude rules over manual edits; keep a written list of every file changed (relpath + before/after md5).
- The tricky part is markdown code spans: a naive `s/OpenDesk/Agora/g` over .md files will corrupt identifiers inside backticks. Strategy: for each .md file, split into code-span/code-block segments vs prose segments (fenced ``` blocks AND inline `...` spans), rewrite only prose segments, reassemble, then diff-verify that no backtick region changed.

## ACCEPTANCE GATE (binary)

1. `grep -rn "OpenDesk" --include="*.md" .` — every remaining hit must be inside a backtick code span/block or explicitly whitelisted (the README continuity line, SPEC-W23 itself, CHANGELOG-style history if present).
2. `grep -rn "OpenDesk" apps/admin-web/app apps/mobile apps/field-pwa apps/marketing` (excluding node_modules, package.json, lockfiles) — zero hits outside code-identifier contexts (e.g. CSS class names like `.opendesk-x` may remain ONLY if renaming them would require touching class references; prefer renaming class + references together if trivial, else whitelist with a note).
3. `git`-level sanity: zero changes to any `*.go`, `*.rs`, `*.py`, `*.sql`, `package.json`, `*.yaml`/`*.yml` other than the compose `name:` line and markdown docs.
4. Compose file(s) still parse: `docker compose config -q` (or python yaml.safe_load) passes.
5. Changed-file manifest delivered: `<md5> <size> <relpath>` for every modified file, md5s double-read stable.
