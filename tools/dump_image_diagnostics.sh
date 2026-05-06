#!/usr/bin/env bash
# dump_image_diagnostics.sh
# 一键导出 sub2api 生图/改图诊断包，便于 AI 离线分析失败原因。
#
# 用法:
#   bash tools/dump_image_diagnostics.sh                       # 默认 6h 窗口
#   SINCE=24h bash tools/dump_image_diagnostics.sh
#   APP_CONTAINER=sub2api DB_HOST=127.0.0.1 DB_PORT=5432 \
#     DB_USER=sub2api DB_NAME=sub2api DB_PASS=xxxx \
#     bash tools/dump_image_diagnostics.sh
#
# 环境变量 (全部可选, 有合理默认):
#   APP_CONTAINER  app 容器名,        默认: sub2api
#   PG_CONTAINER   postgres 容器名,   默认: 空(走宿主机 psql)
#   DB_HOST        PG host,           默认: 127.0.0.1
#   DB_PORT        PG port,           默认: 5432
#   DB_USER        PG user,           默认: sub2api
#   DB_NAME        PG database,       默认: sub2api
#   DB_PASS        PG password,       默认: 空(读 ~/.pgpass / PGPASSWORD)
#   SINCE          docker logs --since,默认: 6h
#   OUT_DIR        输出根目录,        默认: /tmp
#   KEEP_RAW       保留 raw 目录(1=保留),默认: 0(只保留 tgz)
#
# 产物: $OUT_DIR/sub2api-diag-<UTC时间戳>.tgz

set -euo pipefail

APP_CONTAINER="${APP_CONTAINER:-sub2api}"
PG_CONTAINER="${PG_CONTAINER:-}"
DB_HOST="${DB_HOST:-127.0.0.1}"
DB_PORT="${DB_PORT:-5432}"
DB_USER="${DB_USER:-sub2api}"
DB_NAME="${DB_NAME:-sub2api}"
DB_PASS="${DB_PASS:-}"
SINCE="${SINCE:-6h}"
OUT_DIR="${OUT_DIR:-/tmp}"
KEEP_RAW="${KEEP_RAW:-0}"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
SUMMARY_SH="$SCRIPT_DIR/sse_signals_summary.sh"

ts="$(date -u +%Y%m%d-%H%M%S)"
work="$OUT_DIR/sub2api-diag-$ts"
mkdir -p "$work"
cd "$work"

log() { printf '\033[36m[diag]\033[0m %s\n' "$*"; }
warn() { printf '\033[33m[diag]\033[0m %s\n' "$*" >&2; }

#------------------------------------------------------------------
# Step 1. 容器日志
#------------------------------------------------------------------
log "==> 1/4 抓取 $APP_CONTAINER 最近 $SINCE 日志"
if ! docker ps --format '{{.Names}}' | grep -qx "$APP_CONTAINER"; then
  warn "容器 '$APP_CONTAINER' 不在运行,可设置 APP_CONTAINER=<name> 重试"
  exit 2
fi
docker logs "$APP_CONTAINER" --since "$SINCE" > app.log 2>&1 || true
wc -l app.log | awk '{printf "    app.log: %s 行\n", $1}'

# 抽取所有 openaiimages.* / webdriver.* 事件
grep -E "openaiimages\.|webdriver\." app.log > image_events.log || true
wc -l image_events.log | awk '{printf "    image_events.log: %s 行\n", $1}'

# 仅抽 ERROR / WARN 级别
grep -E "ERROR|WARN" app.log > errors_warnings.log || true
wc -l errors_warnings.log | awk '{printf "    errors_warnings.log: %s 行\n", $1}'

#------------------------------------------------------------------
# Step 2. SSE 信号聚合 (复用现有脚本)
#------------------------------------------------------------------
log "==> 2/4 SSE 信号聚合 (sse_signals_summary.sh)"
if [ -x "$SUMMARY_SH" ] || [ -f "$SUMMARY_SH" ]; then
  bash "$SUMMARY_SH" app.log > signals.txt 2>&1 || true
  head -20 signals.txt | sed 's/^/    /'
else
  warn "未找到 $SUMMARY_SH, 跳过聚合"
  echo "(sse_signals_summary.sh not found)" > signals.txt
fi

#------------------------------------------------------------------
# Step 3. 数据库快照
#------------------------------------------------------------------
log "==> 3/4 PostgreSQL 快照 (db=$DB_NAME user=$DB_USER host=$DB_HOST:$DB_PORT)"

run_psql() {
  local sql="$1"
  if [ -n "$PG_CONTAINER" ]; then
    docker exec -e PGPASSWORD="$DB_PASS" "$PG_CONTAINER" \
      psql -h "$DB_HOST" -p "$DB_PORT" -U "$DB_USER" -d "$DB_NAME" -c "$sql"
  else
    PGPASSWORD="$DB_PASS" psql -h "$DB_HOST" -p "$DB_PORT" -U "$DB_USER" -d "$DB_NAME" -c "$sql"
  fi
}

if ! command -v psql >/dev/null 2>&1 && [ -z "$PG_CONTAINER" ]; then
  warn "宿主机没装 psql,且未设置 PG_CONTAINER,跳过 DB 快照"
  echo "(psql unavailable)" > db_skipped.txt
else
  # 账号: 配额/冷却/legacy 开关
  run_psql "SELECT id, name, platform, type,
                   extra->>'email'                    AS email,
                   extra->>'image_account_plan'       AS plan,
                   extra->>'image_quota_remaining'    AS quota_left,
                   extra->>'image_quota_total'        AS quota_total,
                   extra->>'image_cooldown_until'     AS cooldown,
                   extra->>'image_last_probed_at'     AS last_probed,
                   extra->>'openai_oauth_legacy_images' AS web_enabled,
                   status
            FROM accounts
            ORDER BY id;" > accounts.txt 2>&1 || warn "accounts 查询失败"

  # 分组: legacy 默认值 + proxy 绑定
  run_psql "SELECT id, name, openai_legacy_images_default, proxy_id
            FROM groups
            ORDER BY id;" > groups.txt 2>&1 || warn "groups 查询失败"

  # 账号-分组 绑定关系
  run_psql "SELECT ag.account_id, ag.group_id, g.name AS group_name,
                   g.openai_legacy_images_default AS group_legacy
            FROM account_groups ag
            LEFT JOIN groups g ON g.id = ag.group_id
            ORDER BY ag.account_id, ag.group_id;" > account_groups.txt 2>&1 || warn "account_groups 查询失败"

  # 最近 N 小时失败请求
  run_psql "SELECT id, request_path, status_code,
                   substring(error_message, 1, 200) AS err,
                   created_at
            FROM ops_error_logs
            WHERE status_code != 200
              AND created_at > now() - interval '$SINCE'
            ORDER BY id DESC
            LIMIT 100;" > errors_recent.txt 2>&1 || warn "ops_error_logs 查询失败(可能表不存在)"
fi

#------------------------------------------------------------------
# Step 4. 元信息 + 打包
#------------------------------------------------------------------
log "==> 4/4 元信息 & 打包"
{
  echo "# sub2api diagnostic bundle"
  echo "generated_at_utc: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "host: $(hostname)"
  echo "since: $SINCE"
  echo "app_container: $APP_CONTAINER"
  echo "db: $DB_USER@$DB_HOST:$DB_PORT/$DB_NAME"
  echo ""
  echo "## container info"
  docker inspect --format \
    'image={{.Config.Image}} | started={{.State.StartedAt}} | health={{if .State.Health}}{{.State.Health.Status}}{{else}}n/a{{end}}' \
    "$APP_CONTAINER" 2>/dev/null || echo "(inspect failed)"
  echo ""
  echo "## VERSION (in container)"
  docker exec "$APP_CONTAINER" cat /app/VERSION 2>/dev/null \
    || docker exec "$APP_CONTAINER" cat /app/backend/cmd/server/VERSION 2>/dev/null \
    || echo "(VERSION not found in container)"
} > meta.txt

ls -la

cd "$OUT_DIR"
tarball="sub2api-diag-$ts.tgz"
tar czf "$tarball" "sub2api-diag-$ts"
log "==> 完成: $OUT_DIR/$tarball ($(du -h "$tarball" | awk '{print $1}'))"

if [ "$KEEP_RAW" != "1" ]; then
  rm -rf "$work"
  log "    raw 目录已清理 (KEEP_RAW=1 可保留)"
fi

echo ""
echo "把这个 tgz 发给 AI 即可: $OUT_DIR/$tarball"
