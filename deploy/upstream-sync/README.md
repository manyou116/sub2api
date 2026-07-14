# Upstream sync → merge into **this fork**

When [Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api) has a **new pure tag**
`vX.Y.Z`, GitHub Action on **your fork** (`manyou116/sub2api`) will:

| Result | Action **here** (your repo) |
|--------|-----------------------------|
| Merge clean | Merge into `SYNC_BASE_BRANCH` (default `main`) **and push** |
| Merge clean | Create tag `v99.X.Y.Z-plus.N` → triggers image Release |
| Conflicts | Open PR only (you resolve, then merge) |

It does **not** change Wei-Shaw/sub2api. All writes are to **your** `origin`.

## How it discovers upstream tags

GitHub cannot natively subscribe one private/public repo to another repo’s
`release` events without a webhook. This workflow:

1. **Hourly cron** on your fork: `git ls-remote` Wei-Shaw tags  
2. **Manual** Actions → Upstream Sync → Run  
3. Optional **repository_dispatch** `upstream-release` with `{ "tag": "v0.1.154" }`

## Required setup (your fork)

### 1. Base branch must contain your fork commits

`vars.SYNC_BASE_BRANCH` (default `main`) must point to the branch you keep
as “product main”.

If remote `main` is still an old history, either:

```bash
# one-time: make main = current fork tip (3 commits on upstream 0.1.153)
git push --force-with-lease origin main
```

or set:

```text
SYNC_BASE_BRANCH=main
```

until you realign `main`.

### 2. Actions permissions

Settings → Actions → General → Workflow permissions → **Read and write**.

### 3. Variables (optional)

| Variable | Default | Meaning |
|----------|---------|---------|
| `UPSTREAM_REPO` | `Wei-Shaw/sub2api` | Upstream |
| `FORK_SLUG` | `plus` | Tag middle part |
| `SYNC_BASE_BRANCH` | `main` | **Where merges land on your fork** |

### 4. Token (if branch protection blocks GITHUB_TOKEN)

Secret `SYNC_TOKEN` = PAT with `repo` scope, used for push + PR.

## Tag scheme

```text
v99.0.1.154-plus.1   # first fork release based on upstream v0.1.154
v99.0.1.154-plus.2   # second fork-only fix still on 0.1.154
```

Why `99.` prefix?

In-app update check compares only the first three numeric segments of
`VERSION` against Wei-Shaw/sub2api. A tag like `0.1.153-plus.1` parses as
`[0,1,0]` and falsely offers an upgrade to official `0.1.153` (one-click
would replace the fork binary). `99.0.1.153-plus.1` parses as `[99,0,1]`,
so the admin UI never prompts upgrade to official releases.

Docker image tags and embedded `VERSION` both follow this form.

## Local manual (same idea)

```bash
./deploy/upstream-sync/sync.sh
# or
UPSTREAM_TAG=v0.1.154 ./deploy/upstream-sync/sync.sh
```

Then push the base branch + tag yourself if not using Actions.
