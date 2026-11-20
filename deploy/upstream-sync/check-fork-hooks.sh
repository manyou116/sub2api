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

# --- P1 Web and WM image shadows ---
echo "-- P1 Web and WM image shadows --"
need_file backend/internal/service/openaiimages/webdriver/types.go
need_file backend/migrations/177_add_web_image_rate_limit.sql
need_file backend/migrations/246_web_image_shadows.sql
need_file backend/migrations/247_work_image_shadows.sql
need_file backend/internal/service/web_image_shadow_lifecycle.go
need_file backend/internal/service/openai_web_image_service.go
need_file backend/internal/service/openai_web_image_forward.go
need_file backend/internal/service/openai_image_shadow_catalog.go
need_file backend/internal/service/openai_image_shadow_routing.go
need_file backend/internal/service/openai_work_image_quota.go
need_rg "shadow quota dimension" 'QuotaDimensionWebImages' backend/internal/service/domain_constants.go
need_rg "WM shadow quota purpose" 'QuotaDimensionWorkImages' backend/internal/service/domain_constants.go
need_rg "image-only scheduler gate" 'webImageSchedulingReason' backend/internal/service/openai_account_scheduler.go
need_rg "image dispatch uses typed shadow identity" 'account.IsImageShadow()' backend/internal/service/openai_images.go
need_rg "fixed mode routing before selection" 'imageShadowRoutingContext' backend/internal/service/openai_account_scheduler.go
need_rg "mixed pool requires explicit mode" 'ErrImageShadowModeRequired' backend/internal/handler/openai_images.go
need_rg "driver receives explicit execution mode" 'Mode: imageShadowMode(fresh)' backend/internal/service/openai_web_image_forward.go
need_rg "WM errors update parent resource" 'observeImageShadowError' backend/internal/service/openai_web_image_forward.go
need_rg "images use ordinary account slots" 'h.acquireResponsesAccountSlot' backend/internal/handler/openai_images.go
need_rg "shadow create route" '/:id/web-image-shadow' backend/internal/server/routes/admin.go
need_rg "shared model catalog route" '/image-shadow-models' backend/internal/server/routes/admin.go
need_rg "parent resource state" 'WebImageCooldown' backend/internal/repository/account_web_image_state_repo.go
need_rg "shared concurrency capacity classification" 'QuotaDimensionWebImages' backend/internal/service/group_capacity_service.go
need_rg "WM shares image concurrency accounting" 'QuotaDimensionWorkImages' backend/internal/service/group_capacity_service.go
need_rg "WM quota refresh uses parent identity" 'quotaResourceAccountID' backend/internal/handler/admin/openai_oauth_handler.go
need_rg "WM backup configuration" 'work_image_shadow' backend/internal/handler/admin/account_data.go
need_rg "frontend creates Web shadows" 'createWebImageShadow' frontend/src/api/admin/accounts.ts

# Published feature retention must survive source history rewrites.
need_file docs/FORK_RELEASE_INVENTORY.md
need_rg "entry requires published inventory" 'docs/FORK_RELEASE_INVENTORY.md' AGENTS.md
need_rg "release skill inventories non-main features" 'docs/FORK_RELEASE_INVENTORY.md' .agents/skills/sub2api-upstream-release/SKILL.md
need_rg "CI runs fork hooks" 'run: /bin/bash deploy/upstream-sync/check-fork-hooks.sh' .github/workflows/backend-ci.yml
need_file backend/internal/service/openaiimages/webdriver/recovery.go
need_file backend/internal/service/openaiimages/webdriver/download_validation.go
need_file backend/internal/service/openaiimages/webdriver/poll_gate.go
need_file backend/internal/service/openai_web_image_errors_test.go
need_file backend/internal/repository/account_web_image_shadow_integration_test.go
need_file frontend/src/components/account/BatchWebImageShadowDialog.vue
need_file frontend/src/components/account/WebImageShadowSettings.vue
need_rg "submission accepted guard" 'driverErr.MayHaveGenerated()' backend/internal/service/openai_web_image_forward.go
need_rg "bounded download validation" 'validateDownloadedImage' backend/internal/service/openaiimages/webdriver/driver.go
need_rg "stage log redaction" 'safeStageLogMessage(msg)' backend/internal/service/openaiimages/webdriver/driver.go
need_rg "shadow UI tests are mandatory" 'src/views/admin/__tests__/AccountsView.sparkShadow.spec.ts' Makefile
need_rg "release ancestry guard" 'git merge-base --is-ancestor "$RELEASE_SHA" HEAD' .github/workflows/release.yml
need_rg "release source SHA" 'RELEASE_SHA: ${{ needs.prepare.outputs.sha }}' .github/workflows/release.yml
need_rg "release fetches history" 'fetch-depth: 0' .github/workflows/release.yml
for legacy in backend/internal/service/openai_images_legacy_web.go backend/internal/service/openai_web_images_service.go; do
  if [[ -e "$legacy" ]]; then bad "superseded legacy Web control plane: $legacy"; fi
done

# Published SQL is immutable even when upstream uses the same numeric prefix.
if [[ -f backend/migrations/177_add_web_image_rate_limit.sql ]] && \
   [[ "$(shasum -a 256 backend/migrations/177_add_web_image_rate_limit.sql | cut -d ' ' -f 1)" == "560de3625c6b47fb24fece445dbe2105d6e191cfa04979267690e99dfff3e49c" ]]; then
  ok "published migration 177_add_web_image_rate_limit.sql unchanged"
else
  bad "published migration 177_add_web_image_rate_limit.sql missing or changed"
fi
if [[ -f backend/migrations/246_web_image_shadows.sql ]] && \
   [[ "$(shasum -a 256 backend/migrations/246_web_image_shadows.sql | cut -d ' ' -f 1)" == "b042ef431374acb4e4f5abfdead768474b9dbc90d17c0f0a9ffae894a27cc509" ]]; then
  ok "published migration 246_web_image_shadows.sql unchanged"
else
  bad "published migration 246_web_image_shadows.sql missing or changed"
fi
if [[ -f backend/migrations/247_work_image_shadows.sql ]] && \
   [[ "$(shasum -a 256 backend/migrations/247_work_image_shadows.sql | cut -d ' ' -f 1)" == "7e24709d1c08e88d97cf0f0fb462d4c101a1c86f22bec37acb51a967615446fa" ]]; then
  ok "published migration 247_work_image_shadows.sql unchanged"
else
  bad "published migration 247_work_image_shadows.sql missing or changed"
fi

# --- P5 kiro ---
echo "-- P5 kiro --"
need_file backend/internal/pkg/kiroeventstream/decoder.go
need_file backend/internal/service/kiro_chat_service.go
need_file backend/internal/service/kiro_responses_service.go
need_file backend/internal/service/kiro_model_discovery_service.go
need_file backend/internal/service/kiro_prompt_cache.go
need_file backend/internal/service/kiro_token_provider.go
need_file backend/internal/service/account_kiro.go
need_file backend/internal/handler/kiro_gateway_handler.go
need_file backend/internal/server/routes/kiro_admin.go
need_file backend/migrations/186_user_platform_quotas_add_kiro.sql
need_rg "MiniMax migration preserves Kiro quotas" \
  "'kimi', 'zhipu', 'deepseek', 'minimax', 'kiro'" \
  backend/migrations/237_add_minimax_platform.sql
need_rg "OpenCode migration preserves Kiro quotas" \
  "'opencode_go', 'kiro'" \
  backend/migrations/238_opencode_go_platform.sql
need_rg "PlatformKiro constant" \
  "PlatformKiro" \
  backend/internal/domain/constants.go
need_rg "group platform accepts kiro" \
  "group_platform" \
  backend/internal/handler/admin/group_handler.go
need_rg "Kiro registered in platform catalog" \
  '{ID: PlatformKiro, DisplayName: "Kiro", Gateway: PlatformGatewayKiro}' \
  backend/internal/domain/platforms.go
need_rg "group platform validator uses catalog" \
  'domain.IsGroupPlatform' \
  backend/internal/handler/admin/platform_validation.go
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
need_rg "kiro estimated cache usage hook" \
  "applyKiroEstimatedCacheUsage" \
  backend/internal/service/kiro_chat_service.go
need_rg "kiro responses estimated cache usage hook" \
  "applyKiroEstimatedCacheUsage" \
  backend/internal/service/kiro_responses_service.go
need_rg "kiro live model discovery service" \
  "ListAvailableModels" \
  backend/internal/service/kiro_model_discovery_service.go
need_rg "admin Kiro model list uses live discovery" \
  "kiroModelDiscovery.ListAvailableModels" \
  backend/internal/handler/admin/account_handler.go
need_rg "wire kiro token provider" \
  "ProvideKiroTokenProvider" \
  backend/internal/service/wire.go
need_rg "wire kiro model discovery provider" \
  "NewKiroModelDiscoveryService" \
  backend/internal/service/wire.go
need_rg "wire injects kiro token provider into gateway/admin tests" \
  "SetKiroTokenProvider" \
  backend/cmd/server/wire_gen.go
need_rg "wire injects kiro model discovery into account handler" \
  "NewKiroModelDiscoveryService" \
  backend/cmd/server/wire_gen.go
need_rg "kiro quota migration" \
  "'kiro'" \
  backend/migrations/186_user_platform_quotas_add_kiro.sql
need_rg "frontend kiro models list" \
  "const kiroModels" \
  frontend/src/composables/useModelWhitelist.ts
need_rg "frontend kiro preset mappings" \
  "kiroPresetMappings" \
  frontend/src/composables/useModelWhitelist.ts

# --- P3 docs/tooling ---
echo "-- P3 docs/tooling --"
need_file docs/FORK_HOOKS.md
need_file deploy/upstream-sync/check-fork-hooks.sh
need_file .github/workflows/upstream-sync.yml
need_file deploy/docker-compose.swarm.yml
need_file deploy/docker-compose.swarm-local.yml
need_file deploy/Caddyfile.swarm-local
need_file deploy/SWARM_DEPLOY.md
need_file backend/cmd/server/cleanup.go
need_file backend/cmd/server/cleanup_test.go
need_rg "server marks health draining on shutdown" \
  "routes.MarkShuttingDown()" \
  backend/cmd/server/main.go
need_rg "server shutdown timeout config field" \
  "ShutdownTimeout" \
  backend/internal/config/config.go
need_rg "server shutdown drain delay config field" \
  "ShutdownDrainDelay" \
  backend/internal/config/config.go
need_rg "server shutdown timeout default" \
  "server.shutdown_timeout" \
  backend/internal/config/config.go
need_rg "cleanup parallel honors deadline" \
  "RunCleanupParallel" \
  backend/cmd/server/wire_gen.go
need_rg "cleanup sequential honors deadline" \
  "RunCleanupSequential" \
  backend/cmd/server/wire_gen.go
need_rg "compose stop grace for in-flight requests" \
  "stop_grace_period: 320s" \
  deploy/docker-compose.yml
need_rg "swarm stop grace for in-flight requests" \
  "stop_grace_period: 320s" \
  deploy/docker-compose.swarm.yml
need_rg "swarm dnsrr app service" \
  "endpoint_mode: dnsrr" \
  deploy/docker-compose.swarm.yml
need_rg "swarm start-first rolling update" \
  "order: start-first" \
  deploy/docker-compose.swarm.yml
need_rg "swarm drain delay env" \
  "SERVER_SHUTDOWN_DRAIN_DELAY" \
  deploy/docker-compose.swarm.yml
need_rg "swarm caddy routes to task DNS" \
  "tasks.sub2api:8080" \
  deploy/Caddyfile.swarm-local

echo "-- OpenAI capacity cooldown --"
need_file backend/internal/service/openai_slow_first_output.go
need_rg "slow first-output metadata on native Responses" \
  "withOpenAISlowFirstOutputMetadata" backend/internal/service/openai_gateway_forward.go
need_rg "slow first-output metadata on OAuth passthrough" \
  "withOpenAISlowFirstOutputMetadata" backend/internal/service/openai_gateway_passthrough.go
need_rg "slow first-output observer on common HTTP dispatch" \
  "startOpenAISlowFirstOutput" backend/internal/service/openai_plugin_transport.go
need_rg "slow first-output owns pre-output timeout penalties" \
  "openAISlowFirstOutputOwnsTimeout" backend/internal/service/ratelimit_service.go
need_file backend/internal/service/openai_capacity_cooldown.go
need_file backend/internal/repository/openai_capacity_cooldown_cache.go
need_rg "capacity scheduler wrapper retains original flow" \
  "selectAccountWithSchedulerBase" backend/internal/service/openai_account_scheduler.go
need_rg "capacity admission on scheduler candidates" \
  "openAICapacityBlocked(ctx, account, requestedModel)" backend/internal/service/openai_gateway_scheduling.go
need_rg "capacity observations on schedule result" \
  "observeOpenAICapacityResult(account, success" backend/internal/service/openai_account_scheduler.go
need_rg "capacity switch policy before common failover" \
  "applyOpenAICapacitySwitchPolicy(account, failoverErr)" backend/internal/service/openai_gateway_upstream_errors.go
need_rg "capacity switch before passthrough HTTP response" \
  "s.shouldSwitchOpenAICapacity(account, probeBody)" backend/internal/service/openai_gateway_passthrough.go
need_rg "capacity settings admin routes" \
  "openai-capacity-cooldown" backend/internal/server/routes/admin.go
need_rg "capacity settings card" \
  "<OpenAICapacityCooldownSettings" frontend/src/views/admin/SettingsView.vue

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
