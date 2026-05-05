# Repo-tracked git hooks

These hooks run automatically before commits to catch the kind of lint
errors that broke main on v0.1.129 / v0.1.130 / v0.1.131 (gofmt drift +
errcheck on type assertions in test files).

## One-time setup (per clone)

```bash
bash .githooks/install.sh
```

This sets `git config core.hooksPath .githooks` for this repo only.

## What `pre-commit` checks

Only on **staged** files (fast):

| Stage | Tool | Required? |
| --- | --- | --- |
| Go files (`*.go`) | `gofmt -l` | ✅ blocks if missing |
| Go files (`*.go`) under `backend/` | `golangci-lint run` (uses `backend/.golangci.yml`) | optional — install via `brew install golangci-lint`, otherwise skipped with a warning |
| Frontend (`*.vue,ts,tsx,js,jsx`) | `eslint` via `frontend/node_modules` | optional — skipped if `frontend/node_modules` missing |

## Bypass for emergencies

```bash
git commit --no-verify -m "..."
```

But CI will still catch it — your branch will go red.

## Why we use raw git hooks (not husky / pre-commit framework)

- No new npm/python dependency at the repo root.
- Cross-platform (macOS/Linux); Windows users with Git Bash work too.
- Tracked in version control so every contributor gets the same checks.
