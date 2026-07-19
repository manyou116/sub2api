#!/usr/bin/env bash
# Verify fork hooks still exist after upstream merge / local edits.
# Usage: ./deploy/upstream-sync/check-fork-hooks.sh
# Exit 0 = OK, 1 = missing hooks (print failures).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

fail=0
ok() { printf '  OK  %s\n' "$*"; }
bad() { printf '  BAD %s\n' "$*"; fail=1; }

need_file() {
  local f="$1"
  if [[ -f "$f" ]]; then ok "file $f"
  else bad "missing file $f"; fi
}

need_rg() {
  local desc="$1" pattern="$2" path="$3"
  if command -v rg >/dev/null 2>&1; then
    if rg -n --fixed-strings "$pattern" "$path" >/dev/null 2>&1; then
      ok "$desc"
    else
      bad "$desc (pattern not in $path): $pattern"
    fi
  else
    if grep -F -q "$pattern" "$path" 2>/dev/null; then
      ok "$desc"
    else
      bad "$desc (pattern not in $path): $pattern"
    fi
  fi
}

echo "== Fork hook check (see docs/FORK_HOOKS.md) =="

# --- P3 docs/tooling ---
echo "-- P3 docs/tooling --"
need_file docs/FORK_HOOKS.md
need_file deploy/upstream-sync/check-fork-hooks.sh
need_file .github/workflows/upstream-sync.yml

# VERSION should look like 99.x...-plus.N or at least not pure upstream-only without plus when fork files exist
if [[ -f backend/cmd/server/VERSION ]]; then
  ver="$(tr -d '[:space:]' < backend/cmd/server/VERSION)"
  if [[ "$ver" == *plus* ]] || [[ "$ver" == 99.* ]]; then
    ok "VERSION fork-shaped ($ver)"
  else
    bad "VERSION not fork-shaped (want 99.* or *-plus.*), got: $ver"
  fi
else
  bad "missing backend/cmd/server/VERSION"
fi

echo
if [[ "$fail" -ne 0 ]]; then
  echo "FAIL: one or more fork hooks missing. Restore from docs/FORK_HOOKS.md"
  exit 1
fi
echo "PASS: fork hooks present"
exit 0
