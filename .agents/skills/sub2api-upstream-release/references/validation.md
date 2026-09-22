# Validation For An Upstream Replay

Run commands from the candidate worktree, not a stale main checkout. Use an
external Go cache such as `/tmp/sub2api-go-build`. Discover Go from `go.mod`,
pnpm from the current workflow/package metadata, and golangci-lint from CI.
The v0.2.7 replay used pnpm 9.15.9 and golangci-lint 2.13.2; these are historical
examples, not permanent pins. Frozen lockfiles prevent accidental pnpm churn.

## Focused Regressions

From `backend/`, run the relevant existing tests first:

```bash
GOCACHE=/tmp/sub2api-go-build go test -tags=unit ./internal/handler/admin ./internal/service -count=1 -run 'TestAccountHandlerTestOpenAIResponsesPrompt|TestAccountTestServiceOpenAIResponsesAgentIdentityRetryPreservesPrompt|TestBudgeting'
GOCACHE=/tmp/sub2api-go-build go test -tags=unit ./internal/server -count=1 -run TestAPIContracts
GOCACHE=/tmp/sub2api-go-build go test ./internal/pkg/proxybudget ./internal/pkg/httpclient ./migrations -count=1
GOCACHE=/tmp/sub2api-go-build go test -tags=integration ./internal/repository -count=1 -run TestPlatformMigrationsPreserveExistingKiroQuota
```

Inspect selected test names first so renamed tests cannot silently yield
`[no tests to run]`. Tests using PostgreSQL/testcontainers need working Docker;
do not substitute a live production database.

- Migration CHECK constraints: 224, 237 and the v0.2.7 OpenCode migration 238
  illustrate the risk. Extend the integration fixture for each new constraint
  rebuild, including existing Kiro data and idempotency. Preserve finite quota
  limits and usage while respecting upstream unlimited-quota cleanup semantics.
  Check old SQL with `git diff OLD_MAIN -- backend/migrations` and compare the
  last published release as well. Additions are expected; modifications of
  previously shipped SQL need investigation.
- Responses prompts: verify handler-to-mock-upstream JSON, OAuth/API-key modes,
  whitespace defaults, Unicode/newlines/quotes, Agent Identity retries and SSE.
- Proxy budgets: test the HTTP wrapper and real WebSocket pool integration,
  including optional reader-loop, frame, ping and force-close capabilities.
  `TestPythonLedgerContract` requires `PROXY_BUDGET_TEST_URL` pointing at an
  isolated fixture; report it as skipped when unavailable, not as verified
  against a live ledger.
- Frontend platforms: inspect `Record<Platform, ...>`, model/provider filters,
  group route choices, quota modal rows and payloads, and locale completeness.
  Combine new upstream platforms with Kiro without adding unsupported targets
  to composite routes. Update API contract fixtures alongside DTO changes.

## Full Release Checks

From `backend/`:

```bash
GOCACHE=/tmp/sub2api-go-build go test -tags=unit ./...
GOCACHE=/tmp/sub2api-go-build go test -tags=integration ./...
GOCACHE=/tmp/sub2api-go-build golangci-lint run --timeout=30m
```

Use the verified pnpm version to install with `--frozen-lockfile`, then run
`lint:check`, `typecheck`, `build`, the repository's `test-frontend-critical`
target, and tests covering conflicted fork UI. `make test-frontend` invokes
plain `pnpm`; ensure it resolves to that same version. Do not change dependency
versions merely to satisfy the local tool installation.

After frontend build, from `backend/`:

```bash
GOCACHE=/tmp/sub2api-go-build go build -tags=embed -o /tmp/sub2api-replay-check ./cmd/server
```

From the repository root, run the shell checks in
`.github/workflows/backend-ci.yml`, then the fork hook script and whitespace
check. Keep current workflow commands authoritative. Record failures honestly;
a green local unit suite does not imply lint, integration or release success.

## Remote Evidence

Use `gh run list --commit FINAL_SHA` and inspect each main/tag workflow, not an
older successful run. Allow in-progress jobs to finish, inspect failed job logs
without exposing secrets, and avoid repeatedly polling unchanged results.

Inspect `ghcr.io/manyou116/sub2api:99.X.Y.Z-plus.N` using
`docker buildx imagetools inspect`; inspect `.Image` for version/revision labels
and architecture. Git tags include `v`, while the current GoReleaser image
templates use the version without it. Derive aliases from those templates;
do not assume `stable` exists or change aliases during verification.

For a binary version smoke check, use the inspected image digest:

```bash
docker run --rm --platform linux/amd64 --network none --entrypoint /app/sub2api IMAGE_AT_DIGEST --version
```

Adjust the platform to the published manifest. This checks the built artifact
only; it does not verify migrations on production data or live API behavior.
