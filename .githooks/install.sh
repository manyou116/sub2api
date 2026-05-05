#!/usr/bin/env bash
# Enable repo-tracked git hooks (.githooks/) for this clone.
# Run once after clone:  bash scripts/install-hooks.sh

set -euo pipefail
REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

git config core.hooksPath .githooks
chmod +x .githooks/* 2>/dev/null || true

echo "✅ git hooks installed (core.hooksPath = .githooks)"
echo "   active hooks:"
ls .githooks
echo ""
echo "Skip a hook for one commit with:  git commit --no-verify"
