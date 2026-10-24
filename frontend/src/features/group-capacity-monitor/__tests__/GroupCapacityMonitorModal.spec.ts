import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import type { GroupCapacityMonitor } from '@/api/admin/groupCapacityMonitor'
import GroupCapacityMonitorModal from '../GroupCapacityMonitorModal.vue'

const { getMonitor } = vi.hoisted(() => ({ getMonitor: vi.fn() }))
vi.mock('@/api/admin/groupCapacityMonitor', () => ({ getGroupCapacityMonitor: getMonitor }))
vi.mock('vue-i18n', async () => {
  const { default: messages } = await import('@/i18n/locales/en')
  const { ref } = await import('vue')
  const lookup = (key: string) => key.split('.').reduce<unknown>((value, part) => value && typeof value === 'object' ? (value as Record<string, unknown>)[part] : undefined, messages)
  return { useI18n: () => ({
    locale: ref('en'), te: (key: string) => typeof lookup(key) === 'string',
    t: (key: string, args: Record<string, unknown> = {}) => String(lookup(key) || key).replace(/\{(\w+)\}/g, (_, name: string) => String(args[name] ?? `{${name}}`)),
  }) }
})
enableAutoUnmount(afterEach)

function fixture(): GroupCapacityMonitor {
  return {
    as_of: '2026-10-24T01:00:00Z', timezone: 'Asia/Shanghai', currency: 'USD', fee_basis: 'account_cost', model_version: 'v1',
    inventory: { member_count: 155, eligible_count: 150, schedulable_count: 140, modeled_count: 106, direct_count: 100, estimated_count: 6,
      stale_count: 2, missing_window_count: 3, shared_count: 70, history_account_count: 120, unknown_identity_count: 1,
      provenance_unknown_count: 106, state_excluded_count: 10, duplicate_count: 1 },
    quality: { history_completeness: 'unknown', window_provenance: 'approximate_shared_timestamp', identity_quality: 'partial', unclassified_cost: 7.5 },
    history: [], capacity_points: [{ at: '2026-10-24T06:00:00Z', hours: 5, capacity: 500, direct_capacity: 450, estimated_capacity: 50, account_count: 106 }],
    demand_bases: [
      { key: 'last24h', available: true, hours: 24, cost: 240, rate_per_hour: 10 },
      { key: 'last7d', available: true, hours: 168, cost: 3360, rate_per_hour: 20 },
    ],
    scenarios: [
      { key: 'last24h', first_shortfall_at: null, minimum_gap: 100, milestones: [{ hours: 5, capacity: 500, demand: 50, gap: 450 }, { hours: 24, capacity: 1000, demand: 240, gap: 760 }] },
      { key: 'last7d', first_shortfall_at: '2026-10-24T03:00:00Z', minimum_gap: -10, milestones: [{ hours: 5, capacity: 500, demand: 100, gap: 400 }, { hours: 24, capacity: 1000, demand: 480, gap: 520 }] },
    ],
    shared_usage: [{ group_id: 3, group_name: 'Shared group', cost: 300 }], limitations: [], cache: { expires_at: '2099-01-01T00:00:00Z' },
  }
}

function render(show = true) {
  return mount(GroupCapacityMonitorModal, {
    props: { show, group: { id: 24, name: 'Pool 24' } },
    global: {
      stubs: {
        BaseDialog: { props: ['show'], template: '<div v-if="show"><slot /></div>' }, LoadingSpinner: true,
        CapacityForecastChart: { props: ['points', 'rate'], template: '<div data-testid="forecast" :data-rate="rate">{{ points[0]?.capacity }}</div>' },
        ConsumptionTrendChart: true,
      },
    },
  })
}

describe('group capacity monitor', () => {
  beforeEach(() => { getMonitor.mockReset(); getMonitor.mockResolvedValue(fixture()) })
  afterEach(() => { vi.clearAllMocks() })

  it('loads only when open and never labels a positive gap as guaranteed sufficiency', async () => {
    const wrapper = render(false)
    expect(getMonitor).not.toHaveBeenCalled()
    await wrapper.setProps({ show: true })
    await flushPromises()
    expect(getMonitor).toHaveBeenCalledOnce()
    expect(wrapper.text()).toContain('Modeled 106 / eligible 150; 155 accounts')
    expect(wrapper.text()).toContain('not an official USD balance')
    expect(wrapper.get('[data-testid="capacity-pressure"]').text()).toContain('No cumulative shortfall')
    expect(wrapper.text()).toContain('positive difference does not establish group sufficiency')
    wrapper.unmount()
  })

  it('switches the scenario without refetching or changing capacity', async () => {
    const wrapper = render()
    await flushPromises()
    await wrapper.get('[data-testid="capacity-basis"]').setValue('last7d')
    expect(getMonitor).toHaveBeenCalledOnce()
    expect(wrapper.get('[data-testid="forecast"]').text()).toBe('500')
    expect(wrapper.get('[data-testid="forecast"]').attributes('data-rate')).toBe('20')
    expect(wrapper.get('[data-testid="capacity-pressure"]').text()).toContain('first cumulative shortfall')
    wrapper.unmount()
  })

  it('retains the last result on a failed refresh and marks it expired', async () => {
    const wrapper = render()
    await flushPromises()
    getMonitor.mockRejectedValueOnce({ message: 'timeout' })
    const button = wrapper.findAll('button').find(b => b.text() === 'Refresh Statistics')!
    await button.trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('statistics have expired')
    expect(wrapper.text()).toContain('$500.00')
    expect(wrapper.get('[role="alert"]').text()).toContain('timeout')
    wrapper.unmount()
  })

  it('keeps unknown capacity and unavailable demand distinct from zero', async () => {
    const result = fixture()
    result.inventory.modeled_count = 0
    result.demand_bases[0]!.available = false
    result.demand_bases[0]!.rate_per_hour = null
    result.scenarios[0]!.milestones.forEach(row => { row.capacity = null; row.demand = null; row.gap = null })
    getMonitor.mockResolvedValueOnce(result)
    const wrapper = render()
    await flushPromises()
    expect(wrapper.get('[data-testid="capacity-pressure"]').text()).toContain('Insufficient capacity estimates')
    expect(wrapper.get('[data-testid="forecast"]').attributes('data-rate')).toBeUndefined()
    expect(wrapper.text()).not.toContain('$0.00')
    wrapper.unmount()
  })
})
