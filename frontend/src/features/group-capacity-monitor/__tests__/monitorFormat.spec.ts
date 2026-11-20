import { describe, expect, it } from 'vitest'
import type { CapacityHistoryPoint } from '@/api/admin/groupCapacityMonitor'
import { historySummary, usd } from '../monitorFormat'

function hour(index: number, cost: number | null = 10): CapacityHistoryPoint {
  return {
    start_at: new Date(Date.UTC(2026, 9, 20, index)).toISOString(), end_at: new Date(Date.UTC(2026, 9, 20, index + 1)).toISOString(),
    complete: true, pool_account_cost: cost, group_route_cost: 1, pool_via_group_cost: 1, pool_other_cost: cost == null ? null : cost - 1,
    pool_requests: 2, group_route_requests: 1,
  }
}

describe('capacity consumption comparisons', () => {
  it('compares complete contiguous 24h windows and excludes the partial hour from totals and peak', () => {
    const points = Array.from({ length: 48 }, (_, i) => hour(i, i < 24 ? 20 : 10))
    const partial = { ...hour(48, 1000), complete: false }
    const result = historySummary([...points, partial])
    expect(result.recentCost).toBe(240)
    expect(result.change).toBe(-50)
    expect(result.peak?.pool_account_cost).toBe(20)
  })

  it('keeps unknown consumption unknown while preserving recorded zero', () => {
    const points = Array.from({ length: 24 }, (_, i) => hour(i, 0))
    expect(historySummary(points).recentCost).toBe(0)
    points[5] = hour(5, null)
    expect(historySummary(points).recentCost).toBeNull()
    expect(historySummary(points).change).toBeNull()
    expect(usd(null, 'en')).toBe('—')
  })

  it('does not treat duplicated or noncontiguous buckets as a full day', () => {
    const points = Array.from({ length: 24 }, (_, i) => hour(i))
    points[10] = hour(9)
    expect(historySummary(points).recentCost).toBeNull()
  })
})
