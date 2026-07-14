# Fork Hooks & Patch Set (Agent Required Reading)

> **Agents must read this file before changing merge-sensitive code or
> resolving upstream-sync conflicts.**  
> Goal: keep `manyou116/sub2api` easy to merge onto `Wei-Shaw/sub2api` without
> silently dropping fork features (see presencePenalty regression after v0.1.155).

Related docs:

- `AGENTS.md` — hard rules (small diffs, default-off, isolated files)
- `docs/FORK_MAINTENANCE.md` — tags / rebuild routine
- `docs/FORK_DELTA.md` — historical large-delta notes
- `docs/OPENAI_WEB_IMAGES.md` — webimg product behavior
- `deploy/upstream-sync/README.md` — auto-merge workflow

Verify hooks any time:

```bash
./deploy/upstream-sync/check-fork-hooks.sh
```

---

## 1. Patch set (semantic units)

Treat fork work as **replayable patches**, not a pile of drive-by commits.

| ID | Name | What it owns | Preferred home (low conflict) |
|----|------|--------------|-------------------------------|
| **P1** | webimg | ChatGPT Web `gpt-image-2`, quota, inflight, durable cooldown, UI | `backend/internal/service/openaiimages/**`, `openai_web_images_*.go`, migration `177_*` |
| **P2** | grok-compat | OpenAI-client → xAI body scrub (`presencePenalty` etc.), chat bridge routing helpers | `openai_gateway_grok_openai_compat.go` only + short call sites |
| **P3** | infra | graceful drain, GHCR release defaults, `v99.*-plus.N` tags, upstream-sync | `.github/**`, `deploy/upstream-sync/**`, small `main` drain |
| **P4** | observ/capacity | text vs image concurrency accounting when webimg enabled | `group_capacity_service.go`, capacity UI cells |

**Rules**

1. Implement logic in **fork-owned files** whenever possible.
2. Touch upstream hot files only with a **3–15 line hook** (if/call/return).
3. Never “take entire upstream file” during conflict resolve if it deletes a P1/P2 hook.
4. After each upstream merge: run `./deploy/upstream-sync/check-fork-hooks.sh`.
5. Develop with small commits if needed; **before/after a stable upstream base**, squash into P1–P4-shaped commits (do not squash the `chore(sync): merge upstream …` commit into features).

---

## 2. Mandatory hooks (must exist after every sync)

### P2 — Grok OpenAI-client scrub

**Why:** new-api / Codex send `presencePenalty` / `presence_penalty`. xAI Grok
rejects them. Bridge eligibility treats unknown fields as raw-path → 400 unless
body is normalized first.

| Hook | File | Must contain |
|------|------|----------------|
| Implementation | `backend/internal/service/openai_gateway_grok_openai_compat.go` | `normalizeGrokOpenAIClientBody`, `reapplyGrokChatRouteSignals`, `grokDropPenaltyFields` |
| Chat entry | `backend/internal/service/openai_gateway_chat_completions.go` | `normalizeGrokOpenAIClientBody` + `reapplyGrokChatRouteSignals` inside `PlatformGrok` branch |
| Raw chat | `backend/internal/service/openai_gateway_chat_completions_raw.go` | `normalizeGrokOpenAIClientBody(..., true)` before upstream send for Grok |
| Responses patch | `backend/internal/service/openai_gateway_grok.go` | `normalizeGrokOpenAIClientBody(..., false)` inside `patchGrokResponsesBody` |

**Do not** replace P2 with upstream-only “strip only if model == grok-4.5 exact
string” logic on responses path alone — that regresses chat/completions.

### P1 — Web images

| Hook | File / area | Must contain / behavior |
|------|-------------|-------------------------|
| Driver package | `backend/internal/service/openaiimages/**` | isolated webdriver + quota parse |
| Service | `openai_web_images_service.go` | inflight + cooldown (DB truth + cache) |
| Path select | `openai_images_legacy_web.go` / images handler | `UsesOpenAIWebImagesPath` / legacy web path |
| Scheduler | `openai_account_scheduler.go` | web image path does **not** consume text concurrency slots; filters `IsWebImageRateLimited` |
| Durable RL | **`account_repo_webimg.go`** + `migrations/177_add_web_image_rate_limit.sql` | `SetWebImageRateLimited` / `ClearWebImageRateLimit` / `attachWebImageRateLimits` / capacity list |
| Hot hook only | `account_repo.go` | one call: `attachWebImageRateLimits` after account load |
| Clear RL | `ratelimit_service.go` | `ClearRateLimit` also clears web image DB cooldown (1 call) |
| Account helpers | **`account_webimg.go`** | `IsWebImageRateLimited`, `IsSchedulableIgnoringTextRateLimit` (fields stay on `account.go`) |
| Scheduler slot | **`openai_account_scheduler_webimg.go`** | `acquireAccountSlotForSchedule` skips text slots for web path |
| Hot scheduler | `openai_account_scheduler.go` | `accountBlockedByWebImageCooldown(...)` + text-RL bypass call |
| Default-off | account `extra.openai_web_images.enabled` | must stay **opt-in per account** |

### P3 — Infra / tags

| Hook | Requirement |
|------|-------------|
| Tag scheme | `v99.{upstream_semver}-plus.N` (blocks in-app official OTA) |
| VERSION file | `backend/cmd/server/VERSION` matches release intent |
| Sync workflow | `.github/workflows/upstream-sync.yml` merges **into this fork** |
| Hook check | `deploy/upstream-sync/check-fork-hooks.sh` green after sync |

### P4 — Capacity split

| Hook | Requirement |
|------|-------------|
| Group capacity | image max/used includes text-RL accounts that still can serve webimg |
| Text slots | webimg generations do not inflate text `current_concurrency` |

---

## 3. Upstream merge playbook (agents)

```text
1. git fetch upstream --tags
2. merge upstream tag (prefer merge commit: chore(sync): merge upstream vX.Y.Z)
3. On conflict:
   - Prefer UPSTREAM for overlapping Grok quota/UI that upstream already fixed
   - NEVER drop P2 call sites or delete openai_gateway_grok_openai_compat.go
   - NEVER drop P1 openaiimages/ or migration 177 without explicit user order
4. ./deploy/upstream-sync/check-fork-hooks.sh
5. go test -tags unit (touched packages) + frontend typecheck if UI touched
6. Bump VERSION → tag v99.{X.Y.Z}-plus.N (do not tag [skip ci] only commits)
7. Optional squash: only post-sync fixup commits into P1/P2/P3 — keep sync commit
```

**Conflict anti-pattern (caused production bug):**

```text
# BAD — "upstream wins" on entire openai_gateway_grok.go + delete compat file
# Result: presencePenalty 400 for new-api clients
```

**Good:**

```text
# Keep upstream sanitizers for ModelInput/tools
# Keep/re-add single line: normalizeGrokOpenAIClientBody(...)
# Keep openai_gateway_grok_openai_compat.go untouched from fork
```

---

## 4. Commit hygiene

| Phase | Policy |
|-------|--------|
| Active development | Small commits OK (`fix(webimg): …`, lint, tests) |
| Stabilize on one upstream base | Squash **fixup pile** into 1–3 commits shaped like P1/P2/P3 |
| Never | Squash `chore(sync): merge upstream …` into feature commits |
| Never | Rewrite already-shipped production tags unless user requests force |
| Message prefix | `fork(scope):` for local-only; `feat`/`fix` if potentially upstreamable |

Suggested shape after squash on base `v0.1.155`:

```text
chore(sync): merge upstream v0.1.155
feat(webimg): …                    # P1 (+ P4 if bundled)
fix(grok): OpenAI-client scrub …  # P2
chore(fork): hooks docs + VERSION  # P3 docs/tooling
```

---

## 5. Quick agent checklist (copy into PR/sync notes)

```text
[ ] check-fork-hooks.sh PASS
[ ] P2: chat + raw + patchGrok all call normalizeGrokOpenAIClientBody
[ ] P1: openaiimages package present; webimg default-off
[ ] P1: migration 177 columns still attached on account load
[ ] P3: VERSION / tag is v99.*-plus.*
[ ] No second full rewrite of upstream Grok gateway
[ ] Tests: unit tags for touched service tests
```

---

## 6. File ownership cheat-sheet

**Safe to grow (fork-owned):**

- `backend/internal/service/openaiimages/**`
- `backend/internal/service/openai_web_images_*.go`
- `backend/internal/service/openai_gateway_grok_openai_compat.go`
- `backend/internal/repository/account_repo_webimg.go`
- `backend/internal/service/account_webimg.go`
- `backend/internal/service/openai_account_scheduler_webimg.go`
- `docs/FORK_*.md`, `docs/OPENAI_WEB_IMAGES.md`
- `deploy/upstream-sync/**`

**Hot (minimize lines):**

- `openai_gateway_chat_completions.go`
- `openai_gateway_chat_completions_raw.go`
- `openai_gateway_grok.go`
- `openai_account_scheduler.go`
- `account_repo.go`
- `frontend/.../AccountsView.vue` and shared account components

When a hot file must change, put the bulk of logic in a fork-owned helper and
leave a one-liner call in the hot file.


---

## 7. Extraction policy (no landmines)

When adding fork behavior:

1. **New file first** (`*_webimg.go`, `*_openai_compat.go`, `openaiimages/**`).
2. Hot upstream file gets **only** a call / 3–15 line branch.
3. Prefer **compile-time fail** on bad merges (duplicate method if both upstream and fork define the same full method in different files) over silent drop.
4. After moving logic out of a hot file, update `check-fork-hooks.sh` in the **same commit**.
5. Do not leave “temporary” copies of the same function in both hot and fork files.

Already extracted (do not re-inline):

| Logic | Fork file |
|-------|-----------|
| Webimg DB cooldown + capacity SQL | `repository/account_repo_webimg.go` |
| Account webimg schedule helpers | `service/account_webimg.go` |
| Scheduler text-slot skip | `service/openai_account_scheduler_webimg.go` |
| Grok client scrub | `service/openai_gateway_grok_openai_compat.go` |
| Web driver | `service/openaiimages/**` |
