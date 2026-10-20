# Agent entry (read first)

**Before any upstream merge, conflict resolve, or Grok/webimg gateway edit, read:**

1. [`docs/FORK_HOOKS.md`](docs/FORK_HOOKS.md) — patch set P1/P3/P4/P5 + mandatory hooks
2. Run `./deploy/upstream-sync/check-fork-hooks.sh` after sync or when touching hooks

If `check-fork-hooks.sh` fails, fix hooks before shipping a tag.

For upstream upgrades and fork releases, use the project skill
[`sub2api-upstream-release`](.agents/skills/sub2api-upstream-release/SKILL.md).
Preserve `.agents/skills/` when replaying the fork onto a new upstream base.

---

# AI Development Rules

This fork must stay easy to rebase onto upstream. Keep every change small,
isolated, reversible, and off by default unless the user explicitly asks for
different behavior.

## Hard Rules

1. Do not rewrite upstream behavior unless explicitly requested.
2. Prefer config switches, account `extra` fields, channel `features_config`,
   or small adapters over editing core flow.
3. New fork-only features must default to off.
4. Do not add dependencies unless the user explicitly approves them.
5. Do not touch more than 3 production files for one feature unless the user
   approves the larger scope first.
6. Do not add more than about 200 lines of production code for one feature
   unless the user approves the larger design first.
7. Do not mix unrelated changes in one commit.
8. Do not commit local environment files, cache directories, data dumps,
   generated lockfile churn, `node_modules`, or temporary test artifacts.
9. Do not refactor while implementing a feature.
10. If a change affects both frontend and backend, explain why before editing.

## Required Shape For Fork Features

Use this shape whenever possible:

1. one clear config, account `extra`, or channel feature flag
2. one small decision point in the existing flow
3. one isolated helper/service/file for fork behavior
4. one direct test for the new behavior

Example:

```go
if account.IsForkFeatureEnabled() {
	return s.forwardForkFeature(...)
}
return s.forwardDefault(...)
```

## Stop Conditions

Stop and ask before continuing if any of these become necessary:

- more than 3 production files
- more than about 200 production lines
- a database migration
- a new dependency
- generated files with large churn
- broad shared gateway routing changes
- a second implementation of existing upstream logic

## Testing Rules

For every change:

- run the smallest relevant test first
- run typecheck/build for the touched area when applicable
- report exactly what passed and what was not run

### API JSON contract fixtures (mandatory)

CI runs `TestAPIContracts` (`//go:build unit`) against golden JSON in
`backend/internal/server/api_contract_test.go`.

Whenever you **add / rename / remove a JSON field** on any HTTP response DTO
that appears in those fixtures (especially `/api/v1/keys`, admin list APIs):

1. Update **every** matching fixture in `api_contract_test.go` in the **same
   commit** as the DTO change (do not leave it for a follow-up).
2. Run before finishing:

```bash
cd backend
GOCACHE=/tmp/sub2api-go-build go test -tags=unit ./internal/server/ \
  -count=1 -run 'TestAPIContracts'
```

3. A failure that looks like:

```text
expected: map ... (len=25)
actual:   map ... (len=26)
+ "current_image_concurrency": 0
```

means the fixture is stale — fix the fixture, do not "fix" by removing the
real API field.

This is a recurring CI footgun after secondary features (e.g. adding
`current_image_concurrency`). Treat contract fixtures as part of the public
API surface.

## Commit Rules

Use fork commit prefixes for local-only changes:

```text
fork(scope): short description
```

Use normal `fix:` or `feat:` only when the patch is intended to be upstreamable.

Commit bodies should include:

```text
Why:
Scope:
Default behavior:
Merge risk:
Tests:
```

## Docker Version Rules

Fork Docker tags must follow:

```text
v99.<upstream_version>-plus.<patch>
v99.<upstream_version>-plus.<patch>-<short_sha>
stable
```

Examples:

```text
v99.0.1.161-plus.2
v99.0.1.161-plus.2-a128814
stable
```

Do not use vague production tags such as `dev`, `test`, `my-fix`, or
feature-name tags.

## Local Dev Startup (this machine)

Use `./backend/scripts/go.sh` for local Go commands; it uses this checkout's `.cache/go-build`. Do not create separate Go caches under `/tmp`.

Prefer **air** for backend hot reload. Do not leave a stale `./tmp/main` on :8080
while air is expected to own the port.

### Backend (API :8080)

```bash
cd /Volumes/Dev/project/sub2api
./scripts/dev-backend.sh
# equivalent:
# cd backend && air -c .air.toml
```

- Config: `backend/config.yaml` (`server.port: 8080`, postgres, redis)
- Binary: `backend/tmp/main` (air rebuilds on `.go` changes)
- Web image stage log: `WEBIMG_DEBUG=1` → `/tmp/webimg-stage.log`
- Air config: `backend/.air.toml` (`clean_on_exit = false`)

### Frontend (Vite :5173)

```bash
./scripts/dev-frontend.sh
# UI: http://127.0.0.1:5173
```

### Stop

```bash
./scripts/dev-stop.sh
```

### One-shot without air

```bash
cd backend
GOCACHE=/tmp/sub2api-go-build go build -o ./tmp/main ./cmd/server
WEBIMG_DEBUG=1 ./tmp/main
```

### Health check

```bash
curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/v1/models \
  -H 'Authorization: Bearer <api-key>'
```

When debugging web images, always restart/use air so the binary matches source.
Do not assume an old PID on :8080 has the latest code.

## Fork Feature: Web and WM Image Shadows

The Web image control plane uses dedicated credential-linked shadow accounts. The
validated `backend/internal/service/openaiimages/webdriver/` protocol package is
retained with focused error, retry and resource hardening. Preserve its protocol
flow and keep changes covered by driver tests. Published migration 177 remains
immutable for upgrade checksums.

- Read `docs/WEB_IMAGE_SHADOWS.md` for the approved behavior contract.
- `web_images` and `work_images` are fixed image shadow purposes; creation enables
  the chosen type. One parent can have Spark, Web and WM children together.
- Use ordinary account concurrency, priority, groups and schedulable state.
- Keep credentials and resource state tied to the parent identity. Web uses parent
  image quota; WM shares parent Codex/model quota without a second quota snapshot.
- Do not restore the legacy Web enable switches, inflight counters, or slot bypasses.
- Ordinary image APIs for other account types must continue to work.
- Batch shadow creation reuses the single-account lifecycle and preserves existing
  children, including their paused state. Batch status reads never probe upstream.
- Never restart generation after an accepted or uncertain submission; retry only
  the existing conversation's reads/downloads. Keep quota exhaustion separate from
  temporary Web throttling and shared parent credential/proxy failures.
- Use the backend model catalog and recommended defaults. Model or attachment
  settings cannot change the shadow type. WM requires a downloadable attachment.
- Mixed Web/WM groups require explicit gpt-image-2-web / gpt-image-2-work aliases;
  generic requests return 400 instead of silently switching quota sources.
- Migration 247 adds WM lifecycle constraints; published SQL stays immutable.
- Record actual output dimensions and execution model. Unknown upstream token
  usage or Codex cost must stay unknown; do not promise exact sizes or 4K.

## Release speed (this fork)

Tag Release defaults to **simple** mode (~3–5 min):

- linux/amd64 GHCR image only (`.goreleaser.simple.yaml`)
- no QEMU / arm64 / multi-OS archives

Full multi-arch release (~10–15 min): set repository variable `SIMPLE_RELEASE=false`,
or uncheck "simple_release" on workflow_dispatch.
