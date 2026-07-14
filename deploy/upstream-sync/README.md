# Upstream sync → merge into **this fork**

When [Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api) has a **new pure tag**
`vX.Y.Z`, GitHub Action on **your fork** (`manyou116/sub2api`) will:

| Result | Action **here** (your repo) |
|--------|-----------------------------|
| Merge clean | Merge into `SYNC_BASE_BRANCH` (default `main`) **and push** |
| Merge clean | Create tag `vX.Y.Z-webimg.N` → triggers image Release |
| Conflicts | Open PR only (you resolve, then merge) |

It does **not** change Wei-Shaw/sub2api. All writes are to **your** `origin`.

## How it discovers upstream tags

GitHub cannot natively subscribe one private/public repo to another repo’s
`release` events without a webhook. This workflow:

1. **Hourly cron** on your fork: `git ls-remote` Wei-Shaw tags  
2. **Manual** Actions → Upstream Sync → Run  
3. Optional **repository_dispatch** `upstream-release` with `{ "tag": "v0.1.154" }`

## Required setup (your fork)

### 1. Base branch must contain your webimg commits

`vars.SYNC_BASE_BRANCH` (default `main`) must point to the branch you keep
as “product main”.

If remote `main` is still an old history, either:

```bash
# one-time: make main = current fork tip (3 commits on upstream 0.1.153)
git push --force-with-lease origin main
```

or set:

```text
SYNC_BASE_BRANCH=release/v0.1.153-webimg.1
```

until you realign `main`.

### 2. Actions permissions

Settings → Actions → General → Workflow permissions → **Read and write**.

### 3. Variables (optional)

| Variable | Default | Meaning |
|----------|---------|---------|
| `UPSTREAM_REPO` | `Wei-Shaw/sub2api` | Upstream |
| `FORK_SLUG` | `webimg` | Tag middle part |
| `SYNC_BASE_BRANCH` | `main` | **Where merges land on your fork** |

### 4. Token (if branch protection blocks GITHUB_TOKEN)

Secret `SYNC_TOKEN` = PAT with `repo` scope, used for push + PR.

## Tag scheme

```text
v0.1.154-webimg.1   # first fork release based on upstream v0.1.154
v0.1.154-webimg.2   # second fork-only fix still on 0.1.154
```

## Local manual (same idea)

```bash
./deploy/upstream-sync/sync.sh
# or
UPSTREAM_TAG=v0.1.154 ./deploy/upstream-sync/sync.sh
```

Then push the base branch + tag yourself if not using Actions.
