---
name: sub2api-upstream-release
description: Upgrade this Sub2API fork to the latest stable or a specified Wei-Shaw/sub2api release, replay its custom changes, back up and replace main, and publish and verify a new fork image when requested. Use for requests such as 更新上游、升级最新版本、合并二开、重新打 tag 构建. Version inquiries alone require read-only discovery, not publication.
---

# Sub2API Upstream Release

Use this repository's backup + clean replay workflow. Work directly without
subagents. Resolve the repository root with `git rev-parse --show-toplevel`;
never assume a particular machine path, upstream version, or commit count.

Read root `AGENTS.md` and `docs/FORK_HOOKS.md` before replay or conflict resolution.
The user's current request and existing authorization determine scope: an
upgrade-and-publish request covers its backup, main update and tag push; a
version check or local-only upgrade does not. Do not ask again for actions
already authorized. Image publication does not authorize production deployment.

## Discover And Preserve

1. Inspect status, worktrees, remotes, local `main`, remote `main`, fork tags,
   `backend/cmd/server/VERSION`, and the workflows. Expected upstream is
   `Wei-Shaw/sub2api`, fork is `manyou116/sub2api`; verify the actual remotes.
   Preserve unrelated work and local configuration. Use an isolated worktree
   when the main checkout contains changes or when replay will rewrite history.
2. Discover the latest stable GitHub release with
   `gh api repos/Wei-Shaw/sub2api/releases/latest`. Cross-check its tag and
   dereferenced commit with `git ls-remote --tags upstream`. Fetch the exact
   chosen tag and origin's main; do not force-update existing local tags.
   Honor a user-specified version. Do not substitute upstream branch HEAD,
   a prerelease, or the fork's `v99.*` version for a stable upstream release.
3. Record the original local main SHA, fetched remote main SHA, previous
   upstream base, target tag/SHA, and prospective fork tag. Inspect commits
   between the old base and local main, including commits not yet pushed.
   If origin has new work absent locally, reconcile it before selecting patches.
   If the target is already the base, avoid a redundant release unless there
   are requested unpublished changes.
4. Create distinct timestamped backups of both original tips:
   `backup/main-before-upstream-vX.Y.Z-TIMESTAMP` and
   `backup/origin-main-before-upstream-vX.Y.Z-TIMESTAMP`. For an authorized
   release, push and verify both backups before replacing remote main.
   Keep recorded SHAs immutable; do not refresh an expected lease blindly.

## Replay The Fork

1. Create `codex/upstream-vX.Y.Z` at the verified upstream release in a separate
   worktree, adding a suffix if that branch/path exists. Inventory the actual
   fork delta and replay it in dependency order. Preserve feature ownership,
   with compatibility corrections folded into their owning commits.
2. Keep the existing semantic groups: infrastructure/release, Web Images and
   capacity, Kiro, maintained Grok compatibility, and fork maintenance. Preserve
   later additions such as admin Responses prompt forwarding, proxy budget
   accounting, and this project skill. Do not assume there are still exactly
   five commits or resurrect obsolete historical Grok patches. Current source,
   tests and explicit user choices resolve differences with older hook notes.
3. Inspect both sides of conflicts. Preserve upstream additions alongside fork
   hooks: routing, scheduler escape checks, platform maps, DTO fixtures, wire
   providers, and frontend allowlists. Do not resolve a hot file wholesale by
   choosing ours/theirs. Explain coupled frontend/backend adaptations before
   editing. Keep unrelated refactors and generated lockfile churn out.
4. Treat published migration bytes as immutable. Compare migrations against
   the recorded old local main and last published release, not just upstream.
   New upstream constraint migrations must retain supported fork platform data.
   Verify existing Kiro quota rows survive any new platform CHECK rebuild and
   quota cleanup. If an already-published SQL file differs, investigate checksum
   and upgrade behavior before proceeding; do not silently rewrite it.
5. Preserve custom nonblank prompts in actual Responses/Codex outbound input,
   default `hi` only for missing/empty/whitespace prompts, OAuth `store:false`,
   Agent Identity retries, account/model/proxy selection and the SSE contract.
   Keep budget accounting opt-in and preserve transport capabilities when
   upstream changes wrappers: reader loops, ping counts, frame I/O and force
   close/one-time settlement. Test behavior, not only hook text.
6. Squash fixups into their owning patch before publication. Use the commit body
   fields required by `AGENTS.md`. Compare the final Git tree before and after
   history-only cleanup; the trees must match. Keep release metadata separate.

## Validate Before Publishing

Read [references/validation.md](references/validation.md) for current commands
and the regression checks established by previous upgrades. Reconcile them
with the checked-out CI and tool versions; do not copy old pass counts.

Run focused regressions first, then complete the release checks. Run lint
locally with the version used by CI before tagging. Fix real lint findings;
do not disable a rule globally. A narrow security suppression needs a verified
trust boundary and an explanation at the affected call.

Run `./deploy/upstream-sync/check-fork-hooks.sh` and `git diff --check`.
Verify the upstream release is an ancestor, all inventoried custom work remains,
published migrations are unchanged, and the candidate worktree is clean.

## Publish And Verify

1. Choose an unused `v99.X.Y.Z-plus.N` tag by inspecting remote tags, and write
   `99.X.Y.Z-plus.N` to `backend/cmd/server/VERSION`. Commit metadata before
   final validation. Do not reuse or move a published tag.
2. Recheck the original checkout and local main against the recorded state.
   If clean and unchanged, switch it temporarily to its backup (or detach),
   point the now-unchecked-out main at the candidate, then switch back to main.
   Never use `reset --hard`. If the checkout has changed, preserve that work
   and resolve how to adopt the candidate without overwriting it.
3. Push with the exact observed old remote SHA:
   `git push --force-with-lease=refs/heads/main:OLD_REMOTE_SHA origin main`.
   If the lease fails, inspect and incorporate concurrent changes; do not
   replace it with `--force` or merely copy the new remote SHA.
4. Wait for main CI and security checks for the candidate SHA to succeed
   **before pushing the release tag**. Fix and revalidate failures in their
   owning commits, then update main with an explicit lease when authorized.
   Bound retries to diagnosed fixes or a confirmed transient CI failure;
   stop publication and report unresolved failures or unavailable validation.
5. Create and push an annotated tag at that verified commit. Describe the
   upstream base, preserved fork changes, compatibility fixes and checks.
   Follow `.github/workflows/release.yml` and `.goreleaser.simple.yaml`:
   simple mode defaults to linux/amd64 GHCR, unless the user requests otherwise.
6. Monitor tag CI, security and Release to completion. If release has started
   for a commit with failing checks, cancel the unfinished release, diagnose,
   and publish a new unused `plus.N`; leave the old tag immutable. If publication
   already completed, report that explicitly and publish a corrected version.
7. Inspect the actual GHCR manifest and image labels. The version, architecture
   and `org.opencontainers.image.revision` must match the tag and final main.
   Check mutable aliases only when the workflow publishes them. Optionally run
   the image's `--version` with no network, mounts, or production environment.
   Report code checks, published image verification, and live deployment as
   separate states. Never infer deployment from a green Release.

## Delivery

Reply in Chinese with upstream version/SHA, final main commit, backup branch
names, new tag/release link, exact image reference and architecture, checks
passed or not run, and any cancelled/superseded release attempts. State whether
production was actually deployed and verified. Do not expose credentials or
include local configuration, logs, caches or generated artifacts in commits.
