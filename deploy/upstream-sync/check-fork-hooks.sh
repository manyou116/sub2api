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

# --- P2 grok-compat ---
echo "-- P2 grok-compat --"
need_file backend/internal/service/openai_gateway_grok_openai_compat.go
need_rg "normalizeGrokOpenAIClientBody defined" \
  "func normalizeGrokOpenAIClientBody" \
  backend/internal/service/openai_gateway_grok_openai_compat.go
need_rg "penalty fields list" \
  "presencePenalty" \
  backend/internal/service/openai_gateway_grok_openai_compat.go
need_rg "chat entry normalizes Grok body" \
  "normalizeGrokOpenAIClientBody" \
  backend/internal/service/openai_gateway_chat_completions.go
need_rg "chat entry reapplies route signals" \
  "reapplyGrokChatRouteSignals" \
  backend/internal/service/openai_gateway_chat_completions.go
need_rg "raw chat normalizes Grok body" \
  "normalizeGrokOpenAIClientBody" \
  backend/internal/service/openai_gateway_chat_completions_raw.go
need_rg "patchGrokResponsesBody normalizes" \
  "normalizeGrokOpenAIClientBody" \
  backend/internal/service/openai_gateway_grok.go

# --- P1 webimg ---
echo "-- P1 webimg --"
need_file backend/internal/service/openaiimages/webdriver/types.go
need_file backend/internal/service/openai_web_images_service.go
need_file backend/migrations/177_add_web_image_rate_limit.sql
need_file backend/internal/repository/account_repo_webimg.go
need_file backend/internal/service/account_webimg.go
need_file backend/internal/service/openai_account_scheduler_webimg.go
need_rg "SetWebImageRateLimited repo (fork file)" \
  "SetWebImageRateLimited" \
  backend/internal/repository/account_repo_webimg.go
need_rg "attachWebImageRateLimits (fork file)" \
  "attachWebImageRateLimits" \
  backend/internal/repository/account_repo_webimg.go
need_rg "capacity list includes text-RL webimg accounts" \
  "ListSchedulableCapacityByGroupIDs" \
  backend/internal/repository/account_repo_webimg.go
need_rg "IsWebImageRateLimited helper (fork file)" \
  "IsWebImageRateLimited" \
  backend/internal/service/account_webimg.go
need_rg "web path skip text slot (fork file)" \
  "acquireAccountSlotForSchedule" \
  backend/internal/service/openai_account_scheduler_webimg.go
need_rg "hot account_repo still attaches webimg cooldown" \
  "attachWebImageRateLimits" \
  backend/internal/repository/account_repo.go
need_rg "hot scheduler still calls webimg cooldown gate" \
  "accountBlockedByWebImageCooldown" \
  backend/internal/service/openai_account_scheduler.go


# --- P1 webimg (extra call-site hooks) ---
echo "-- P1 webimg call sites --"
need_rg "UsesOpenAIWebImagesPath helper" \
  "UsesOpenAIWebImagesPath" \
  backend/internal/service/openai_images_legacy_web.go
need_rg "ClearWebImageRateLimit repo" \
  "ClearWebImageRateLimit" \
  backend/internal/repository/account_repo_webimg.go
need_rg "ClearRateLimit clears web image cooldown" \
  "ClearWebImageRateLimit" \
  backend/internal/service/ratelimit_service.go
need_rg "webimg package import or path" \
  "openaiimages" \
  backend/internal/service/openai_web_images_service.go

# --- P4 capacity (lightweight) ---
echo "-- P4 capacity --"
need_rg "image concurrency capacity fields" \
  "image_concurrency_used" \
  backend/internal/service/group_capacity_service.go
need_rg "scheduler webimg path check" \
  "UsesOpenAIWebImagesPath" \
  backend/internal/service/openai_account_scheduler_webimg.go


# --- P5 kiro ---
echo "-- P5 kiro --"
need_file backend/internal/pkg/kiroeventstream/decoder.go
need_file backend/internal/service/kiro_chat_service.go
need_file backend/internal/service/kiro_responses_service.go
need_file backend/internal/service/kiro_prompt_cache.go
need_file backend/internal/service/kiro_token_provider.go
need_file backend/internal/service/account_kiro.go
need_file backend/internal/handler/kiro_gateway_handler.go
need_file backend/internal/server/routes/kiro_admin.go
need_rg "PlatformKiro constant" \
  "PlatformKiro" \
  backend/internal/domain/constants.go
need_rg "gateway kiro chat route" \
  "KiroChatCompletions" \
  backend/internal/server/routes/gateway.go
need_rg "gateway kiro responses route" \
  "KiroResponses" \
  backend/internal/server/routes/gateway.go
need_rg "admin kiro routes registered" \
  "registerKiroAdminRoutes" \
  backend/internal/server/routes/admin.go
need_rg "kiro token refresher registered" \
  "NewKiroTokenRefresher" \
  backend/internal/service/token_refresh_service.go
need_rg "kiro cache identity export" \
  "ResolveKiroCacheIdentity" \
  backend/internal/service/kiro_prompt_cache.go
need_rg "wire kiro token provider" \
  "ProvideKiroTokenProvider" \
  backend/internal/service/wire.go

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
