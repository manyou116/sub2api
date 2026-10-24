import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { _steppedLineTo } from 'chart.js/helpers'
import type { ChartData } from 'chart.js'
import CapacityForecastChart from '../CapacityForecastChart.vue'
import ConsumptionTrendChart from '../ConsumptionTrendChart.vue'

vi.mock('vue-i18n', async () => {
  const { ref } = await import('vue')
  return { useI18n: () => ({ locale: ref('en'), t: (key: string) => key }) }
})
vi.mock('vue-chartjs', () => ({ Line: { name: 'LineStub', props: ['data', 'options'], template: '<div />' } }))

describe('quota chart event timing', () => {
  it('holds cumulative capacity until its release timestamp rather than moving the jump earlier', () => {
    const wrapper = mount(CapacityForecastChart, {
      props: { id: 24, rate: 10, maxHours: 24, timezone: 'UTC', points: [
        { at: '2026-10-24T00:00:00Z', hours: 0, capacity: 20, direct_capacity: 20, estimated_capacity: 0, account_count: 1 },
        { at: '2026-10-24T05:00:00Z', hours: 5, capacity: 100, direct_capacity: 100, estimated_capacity: 0, account_count: 1 },
      ] },
    })
    const data = wrapper.getComponent({ name: 'LineStub' }).props('data') as ChartData<'line'>
    const mode = data.datasets[0]!.stepped
    expect(mode).toBe('before')
    const lineTo = vi.fn()
    _steppedLineTo({ lineTo } as unknown as CanvasRenderingContext2D, { x: 0, y: 20 }, { x: 5, y: 100 }, false, String(mode))
    expect(lineTo.mock.calls).toEqual([[5, 20], [5, 100]])
    wrapper.unmount()
  })

  it('keeps each hourly consumption value inside its own hour', () => {
    const wrapper = mount(ConsumptionTrendChart, {
      props: { timezone: 'UTC', asOf: '2026-10-24T02:00:00Z', points: [0, 1].map(hour => ({
        start_at: `2026-10-24T0${hour}:00:00Z`, end_at: `2026-10-24T0${hour + 1}:00:00Z`, complete: true,
        pool_account_cost: hour ? 100 : 20, group_route_cost: hour ? 10 : 2, pool_via_group_cost: 0, pool_other_cost: 0, pool_requests: 1, group_route_requests: 1,
      })) },
    })
    const data = wrapper.getComponent({ name: 'LineStub' }).props('data') as ChartData<'line'>
    expect(data.datasets.map(dataset => dataset.stepped)).toEqual(['before', 'before'])
    wrapper.unmount()
  })
})
