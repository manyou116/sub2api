# Agent entry (read first)

**Before any upstream merge, conflict resolve, or Grok/webimg gateway edit, read:**

1. [`docs/FORK_HOOKS.md`](docs/FORK_HOOKS.md) — patch set P1–P4 + mandatory hooks  
2. Run `./deploy/upstream-sync/check-fork-hooks.sh` after sync or when touching hooks  

If `check-fork-hooks.sh` fails, fix hooks before shipping a tag.

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
<upstream_version>-fork.<patch>
<upstream_version>-fork.<patch>-<short_sha>
stable
```

Examples:

```text
0.1.138-fork.1
0.1.138-fork.1-a128814
stable
```

Do not use vague production tags such as `dev`, `test`, `my-fix`, or
feature-name tags.

## Local Dev Startup (this machine)

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

## Fork Feature: OpenAI Web Images

Goal: ChatGPT **Web image quota** reverse path for `gpt-image-2`, default off.

### Keep isolated (new files preferred)

- `backend/internal/service/openaiimages/webdriver/` — reverse driver (SSE then poll)
- `backend/internal/service/openai_web_images_service.go` — control plane (quota/inflight/redis)
- `backend/internal/service/openai_images_legacy_web.go` — gateway entry for web path
- `backend/internal/handler/admin/openai_web_images_handler.go` — admin API
- `frontend/src/constants/openaiWebImages.ts` + capacity-cell integrations

### Minimal upstream touch points

- `config.Gateway.OpenAIWebImages` (runtime defaults only; no global enable switch)
- `wire` Provide + `routes/admin` register
- Scheduler hooks: `isAccountSchedulableForOpenAIRequest` / text-429 bypass for images only
- `ForwardImages` → web path when account `extra.openai_web_images.enabled` (sole enable switch)

### Do not commit (local only)

- `backend/.air.toml`, `DEV_LOCAL.md`, `/tmp/*` stage logs
- one-off debug cmds under `backend/cmd/webimg*`

### Runtime contract

- SSE open: no conversation GET poll
- After SSE end: adaptive poll + 429 backoff (not image-quota cooldown)
- Only Free-plan image-quota text → web cooldown / remaining=0



## Release speed (this fork)

Tag Release defaults to **simple** mode (~3–5 min):

- linux/amd64 GHCR image only (`.goreleaser.simple.yaml`)
- no QEMU / arm64 / multi-OS archives

Full multi-arch release (~10–15 min): set repository variable `SIMPLE_RELEASE=false`,
or uncheck "simple_release" on workflow_dispatch.
