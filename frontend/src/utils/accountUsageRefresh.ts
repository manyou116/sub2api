import type { Account } from '@/types'

const normalizeUsageRefreshValue = (value: unknown): string => {
  if (value == null) return ''
  return String(value)
}

export const buildOpenAIUsageRefreshKey = (account: Pick<Account, 'id' | 'platform' | 'type' | 'updated_at' | 'last_used_at' | 'rate_limit_reset_at' | 'extra'>): string => {
  if (account.platform !== 'openai' || account.type !== 'oauth') {
    return ''
  }

  const extra = account.extra ?? {}
  // v2 网关写入的扁平 image_* 字段（参见 account_probe.go::write）。
  // 兼容旧 openai_legacy_image_quota 嵌套对象，保证历史数据仍能触发刷新。
  const legacy = (extra as any).openai_legacy_image_quota ?? {}
  return [
    account.id,
    account.updated_at,
    account.last_used_at,
    account.rate_limit_reset_at,
    extra.codex_usage_updated_at,
    extra.codex_5h_used_percent,
    extra.codex_5h_reset_at,
    extra.codex_5h_reset_after_seconds,
    extra.codex_5h_window_minutes,
    extra.codex_7d_used_percent,
    extra.codex_7d_reset_at,
    extra.codex_7d_reset_after_seconds,
    extra.codex_7d_window_minutes,
    (extra as any).image_account_plan,
    (extra as any).image_quota_remaining,
    (extra as any).image_quota_total,
    (extra as any).image_cooldown_until,
    (extra as any).image_last_probed_at,
    legacy.status,
    legacy.quota_remaining,
    legacy.restore_at
  ].map(normalizeUsageRefreshValue).join('|')
}
