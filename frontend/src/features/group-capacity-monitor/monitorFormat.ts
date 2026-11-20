import type { CapacityHistoryPoint } from '@/api/admin/groupCapacityMonitor'

export function usd(value: number | null | undefined, locale?: string): string {
  if (value == null || !Number.isFinite(value)) return '—'
  return new Intl.NumberFormat(locale || undefined, {
    style: 'currency', currency: 'USD', minimumFractionDigits: 2, maximumFractionDigits: 2,
  }).format(Object.is(value, -0) ? 0 : value)
}

export function dateTime(value: string | null | undefined, timezone: string, locale?: string): string {
  if (!value || !Number.isFinite(Date.parse(value))) return '—'
  return new Intl.DateTimeFormat(locale || undefined, {
    timeZone: timezone || 'UTC', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hourCycle: 'h23',
  }).format(new Date(value))
}

export const rangeHours = { '24h': 24, '7d': 168, '15d': 360 } as const

/** Complete-hour comparisons never extrapolate the current partial bucket. */
export function historySummary(points: CapacityHistoryPoint[]) {
  const complete = points.filter(p => p.complete)
  const end = complete.length ? Math.max(...complete.map(p => Date.parse(p.end_at))) : 0
  const window = (offset: number) => complete.filter(p => {
    const start = Date.parse(p.start_at)
    return start >= end - (offset + 24) * 3_600_000 && start < end - offset * 3_600_000
  })
  const recent = window(0)
  const previous = window(24)
  const sum = (items: CapacityHistoryPoint[]) => items.reduce((v, p) => v + (p.pool_account_cost ?? 0), 0)
  const validDay = (items: CapacityHistoryPoint[]) => {
    if (items.length !== 24 || items.some(p => p.pool_account_cost == null || !Number.isFinite(p.pool_account_cost))) return false
    const sorted = [...items].sort((a, b) => Date.parse(a.start_at) - Date.parse(b.start_at))
    return sorted.every((p, i) => Date.parse(p.end_at) - Date.parse(p.start_at) === 3_600_000 && (!i || Date.parse(p.start_at) === Date.parse(sorted[i - 1]!.end_at)))
  }
  const recentCost = sum(recent)
  const previousCost = sum(previous)
  const peak = complete.reduce<CapacityHistoryPoint | null>((best, p) =>
    p.pool_account_cost != null && (best?.pool_account_cost == null || p.pool_account_cost > best.pool_account_cost) ? p : best, null)
  return {
    end, recentCost: validDay(recent) ? recentCost : null,
    change: validDay(recent) && validDay(previous) && previousCost > 0
      ? (recentCost - previousCost) / previousCost * 100 : null,
    peak,
  }
}
