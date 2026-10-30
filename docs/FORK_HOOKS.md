# Fork Hooks & Patch Set (Agent Required Reading)

> **Agents must read this file before changing merge-sensitive code or
> resolving upstream-sync conflicts.**
> Goal: keep `manyou116/sub2api` easy to merge onto `Wei-Shaw/sub2api` without
> silently dropping fork-owned features.

Related docs:

- `docs/FORK_RELEASE_INVENTORY.md` — all published feature lines, including non-main tags
- `AGENTS.md` — hard rules (small diffs, reversible changes, isolated files)
- `docs/FORK_MAINTENANCE.md` — tags / rebuild routine
- `docs/FORK_DELTA.md` — historical large-delta notes
- `docs/WEB_IMAGE_SHADOWS.md` — Web shadow behavior contract
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
| **P1** | Web shadows | ChatGPT Web protocol, image-only shadow routing and parent quota | `backend/internal/service/openaiimages/**`, shadow adapters |
| **P2** | grok | **Upstream-owned from v0.1.161 onward; do not replay fork Grok patches** | N/A |
| **P3** | infra | graceful drain, GHCR release defaults, `v99.*-plus.N` tags, upstream-sync | `.github/**`, `deploy/upstream-sync/**`, small `main` drain |
| **P4** | capacity | Account concurrency classified by account purpose | `group_capacity_service.go`, capacity UI |
| **P5** | kiro | Kiro CodeWhisperer platform (chat, token, sticky+conversation cache) | `service/kiro_*.go`, `account_kiro.go`, `pkg/kiroeventstream/`, `handler/kiro_*.go`, `routes/kiro_admin.go` |

**Rules**

1. Implement logic in **fork-owned files** whenever possible.
2. Touch upstream hot files only with a **3–15 line hook** (if/call/return).
3. Never “take entire upstream file” during conflict resolve if it deletes a P1/P4/P5 hook.
4. After each upstream merge: run `./deploy/upstream-sync/check-fork-hooks.sh`.
5. Develop with small commits if needed; **before/after a stable upstream base**, squash into P1/P3/P4/P5-shaped commits (do not squash the `chore(sync): merge upstream …` commit into features).

---

## 2. Mandatory hooks (must exist after every sync)

### P2 — Grok upstream ownership

Grok behavior follows upstream starting from `v0.1.161`. Do not replay fork
Grok compatibility files, settings toggles, or free-usage cooldown patches.
When a sync conflict touches Grok, prefer upstream unless the user explicitly
approves a new fork-only Grok patch.

### P1 — Web image shadows

Web shadows use the shared account concurrency mechanism and parent credential
identity. The protocol driver remains an isolated dependency. See
`WEB_IMAGE_SHADOWS.md` for the behavior contract.
Do not reintroduce legacy control-plane hooks when merging upstream or old patches.
Published migrations 177, 246_web_image_shadows.sql and 247_work_image_shadows.sql
must remain byte-identical. Mandatory checks cover both modes, lifecycle/batch UI,
parent quota ownership, ordinary concurrency, accepted submission guards, bounded
read recovery, download validation, log redaction and the non-main release guard.
Check **all published feature refs**, not just the previous main, as specified in
`FORK_RELEASE_INVENTORY.md`. The 2026-10 plus.13 omission must not recur.

### P5 — Kiro platform

**Why:** Kiro (AWS CodeWhisperer) as a first-class platform with sticky account
routing and tenant-isolated `conversationId` for prompt-cache affinity.

| Hook | File / area | Must contain / behavior |
|------|-------------|-------------------------|
| Platform const | `domain/constants.go`, `service/domain_constants.go` | `PlatformKiro = "kiro"` |
| Core services | `service/kiro_*.go`, `account_kiro.go` | token/chat/responses-bridge/selector/cache/quarantine |
| EventStream | `pkg/kiroeventstream/` | frame decoder |
| Gateway | `routes/gateway.go` | `isKiroGatewayPlatform` + `KiroChatCompletions` + `KiroResponses` |
| Admin | `routes/kiro_admin.go` + call site in `admin.go` | `registerKiroAdminRoutes` → `/kiro/import` |
| Group/quota allowlists | `domain/platforms.go`, `group_platform` validator, `AllowedQuotaPlatforms` | Kiro registered in the shared platform catalog; migration 242 removes database platform CHECK constraints; published migrations stay unchanged |
| Token refresh | `token_refresh_service.go` | `KiroTokenRefresher` registered |
| Wire | `wire.go` / `wire_gen.go` | `ProvideKiroTokenProvider` + `SetKiroTokenProvider` |
| Cache identity | `kiro_prompt_cache.go` | `ResolveKiroCacheIdentity` (no raw client key upstream) |
| Conversation id | `kiro_chat_service.go` `buildKiroPayload` | stable id from cache identity, not random UUID only |
| Usage cache estimate | `kiro_chat_service.go`, `kiro_responses_service.go` | `applyKiroEstimatedCacheUsage` fills `cache_read` only when upstream omitted it |
| Live model discovery | `kiro_model_discovery_service.go`, `account_handler.go` | account model list tries Kiro Q `ListAvailableModels` when no explicit mapping exists |
| Frontend models | `frontend/src/composables/useModelWhitelist.ts` | `kiroModels` + `kiroPresetMappings` for account mapping UI |

**Default:** off when no `platform=kiro` group/account exists.

**Shape:** runtime stays in `package service` (`kiro_*.go` + `account_kiro.go`) to avoid
Account/gateway import cycles; EventStream decoder is the only isolated package
(`pkg/kiroeventstream`). Do not reintroduce an empty `service/kiro` ownership stub.



### P3 — Infra / tags

| Hook | Requirement |
|------|-------------|
| Tag scheme | `v99.{upstream_semver}-plus.N` (blocks in-app official OTA) |
| VERSION file | `backend/cmd/server/VERSION` matches release intent |
| Sync workflow | `.github/workflows/upstream-sync.yml` merges **into this fork** |
| Hook check | `deploy/upstream-sync/check-fork-hooks.sh` green after sync |
| Graceful drain | `backend/cmd/server/main.go`, `backend/cmd/server/cleanup.go`, `server.shutdown_*` config | `routes.MarkShuttingDown()`, configurable timeout/drain delay, cleanup honors deadline, no `log.Fatalf` during shutdown |
| Swarm rolling update | `deploy/docker-compose.swarm*.yml`, `Caddyfile.swarm-local`, `SWARM_DEPLOY.md` | Caddy -> `tasks.sub2api`, `endpoint_mode: dnsrr`, `start-first`, stop grace > shutdown timeout |

### P4 — Capacity split

Web shadows use the normal account concurrency service. Capacity reports
must classify accounts by purpose, without a second Web inflight store.

---

## 3. Upstream merge playbook (agents)

```text
1. git fetch upstream --tags
2. merge upstream tag (prefer merge commit: chore(sync): merge upstream vX.Y.Z)
3. On conflict:
   - Prefer UPSTREAM for overlapping Grok quota/UI that upstream already fixed
   - NEVER drop P1 openaiimages/ or migration 177 without explicit user order
4. ./deploy/upstream-sync/check-fork-hooks.sh
5. go test -tags unit (touched packages) + frontend typecheck if UI touched
6. Bump VERSION → tag v99.{X.Y.Z}-plus.N (do not tag [skip ci] only commits)
7. Optional squash: only post-sync fixup commits into P1/P3/P4/P5-shaped commits — keep sync commit
```

**Grok policy:** keep upstream Grok files as the source of truth. Fork Grok
patches from pre-`v0.1.161` history are reference-only and should not be
cherry-picked into this branch.

---

## 4. Commit hygiene

| Phase | Policy |
|-------|--------|
| Active development | Small commits OK (`fix(webimg): …`, lint, tests) |
| Stabilize on one upstream base | Squash **fixup pile** into focused P1/P3/P4/P5 commits |
| Never | Squash `chore(sync): merge upstream …` into feature commits |
| Never | Rewrite already-shipped production tags unless user requests force |
| Message prefix | `fork(scope):` for local-only; `feat`/`fix` if potentially upstreamable |

Suggested shape after squash on base `v0.1.155`:

```text
chore(sync): merge upstream v0.1.155
fork(webimg): …                    # P1 (+ P4 if bundled)
fork(kiro): …                      # P5
chore(fork): hooks docs + VERSION  # P3 docs/tooling
```

---

## 5. Quick agent checklist (copy into PR/sync notes)

```text
[ ] check-fork-hooks.sh PASS
[ ] P1: protocol package present; no Web route without a Web shadow
[ ] P1: published migration 177 remains immutable
[ ] P3: VERSION / tag is v99.*-plus.*
[ ] No second full rewrite of upstream Grok gateway
[ ] Tests: unit tags for touched service tests
```

---

## 6. File ownership cheat-sheet

**Safe to grow (fork-owned):**

- `backend/internal/service/openaiimages/**`
- `backend/internal/service/openai_web_image_*.go`
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

Retained protocol boundary: `backend/internal/service/openaiimages/webdriver/`.
The replacement control-plane invariants are documented in `WEB_IMAGE_SHADOWS.md`.

## 8. OpenAI repeated capacity cooldown (default off)

`docs/OPENAI_CAPACITY_COOLDOWN.md` describes the independent setting and runtime
contract. Keep the Lua state machine in `repository/openai_capacity_cooldown_cache.go`
and the scheduling adapter in `service/openai_capacity_cooldown.go`. The original
scheduler remains in `selectAccountWithSchedulerBase`; its wrapper performs
optional admission and reselects when a probe lease loses a race.

Preserve the runtime-blocked check, result/error observation hooks, and the
optional capacity switch policy in the common failover constructor. The OAuth
passthrough HTTP error branch must also consult `shouldSwitchOpenAICapacity`
before committing a 503 response. Preserve the separate admin GET/PUT routes
and settings card. Capacity pauses now cover the account's text models and
project into the existing temporary-unschedulable fields for admin filtering.
Preserve the Web-image exception for this reason alone, including Work Mode
and the candidate/capacity queries. Auth/transport pauses must retain priority.
Admin recovery must reset the shared Redis generation under the control lock
so delayed results cannot reapply an old DB pause. Quota, 429, images and
WebSocket ingress do not generate capacity observations.
Run `TestCapacityCooldown` in repository/service/admin/handler
packages, plus the settings component tests, when replaying these hooks.

The independent slow first-output rule reuses this text pause and recovery
circuit, with its own rolling counter. Preserve the metadata hooks in both
Responses request builders and the observer around `doOpenAIUpstream`, plus
the pre-output timeout ownership check. Successful slow requests must retain
their slow event; only a successful request with timely meaningful content may
advance recovery while this rule is enabled. Run `TestSlowFirstOutput` too.
