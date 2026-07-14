#!/usr/bin/env bash
# Local helper: merge latest (or pinned) upstream tag into a sync branch.
set -euo pipefail

FORK_SLUG="${FORK_SLUG:-plus}"
BASE_BRANCH="${SYNC_BASE_BRANCH:-main}"
UPSTREAM_REMOTE="${UPSTREAM_REMOTE:-upstream}"
ORIGIN_REMOTE="${ORIGIN_REMOTE:-origin}"

root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$root"

if ! git remote get-url "$UPSTREAM_REMOTE" >/dev/null 2>&1; then
  echo "missing remote '$UPSTREAM_REMOTE' (git remote add upstream https://github.com/Wei-Shaw/sub2api.git)"
  exit 1
fi

git fetch "$UPSTREAM_REMOTE" --tags --prune
git fetch "$ORIGIN_REMOTE" --tags --prune 2>/dev/null || true

if [[ -n "${UPSTREAM_TAG:-}" ]]; then
  tag="$UPSTREAM_TAG"
else
  # Latest pure semver tag on upstream (no hyphen suffix).
  tag="$(git tag -l 'v*' --sort=-v:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -1 || true)"
fi

if [[ -z "${tag:-}" ]]; then
  echo "no upstream semver tag found"
  exit 1
fi

echo "upstream base: $tag"

# Next fork revision on this base: v99.{upstream}-plus.N
# (99. prefix avoids false "update available" vs official Wei-Shaw tags)
base_ver="${tag#v}"
n=1
while git rev-parse -q --verify "refs/tags/v99.${base_ver}-${FORK_SLUG}.${n}" >/dev/null 2>&1; do
  n=$((n + 1))
done
fork_tag="v99.${base_ver}-${FORK_SLUG}.${n}"
echo "next fork tag candidate: $fork_tag"

branch="sync/upstream-${tag#v}"
git checkout "$BASE_BRANCH"
git pull --ff-only "$ORIGIN_REMOTE" "$BASE_BRANCH" 2>/dev/null || true

if git show-ref --verify --quiet "refs/heads/$branch"; then
  git checkout "$branch"
else
  git checkout -b "$branch" "$BASE_BRANCH"
fi

echo "merging ${UPSTREAM_REMOTE}/${tag} ..."
if git merge --no-ff -m "chore(sync): merge upstream ${tag}" "${tag}"; then
  echo "MERGE_CLEAN=1"
  echo "Merge HERE then tag:"
  echo "  git push origin HEAD:${BASE_BRANCH}"
  echo "  git tag -a ${fork_tag} -m \"fork on ${tag}\" && git push origin ${fork_tag}"
else
  echo "MERGE_CLEAN=0 — resolve conflicts, then commit"
  echo "Hot files: openai_*scheduler*, openai_images*, AccountsView, wire*, config.go"
  exit 2
fi
