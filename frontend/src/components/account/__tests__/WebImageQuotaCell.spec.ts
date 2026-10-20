import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import WebImageQuotaCell from '../WebImageQuotaCell.vue'
import GroupCapacityBadge from '@/components/common/GroupCapacityBadge.vue'

const { getStatus, probeStatus } = vi.hoisted(() => ({ getStatus: vi.fn(), probeStatus: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: { getWebImageStatus: getStatus, probeWebImageStatus: probeStatus } } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
const unknown = { parent_account_id: 7, quota_known: false, remaining: null, reset_at: null, probed_at: null, cooldown_until: null }

describe('Web image quota and capacity', () => {
  beforeEach(() => { vi.clearAllMocks(); getStatus.mockResolvedValue(unknown) })

  it('keeps unknown quota unknown and reads the shared parent snapshot', async () => {
    const wrapper = mount(WebImageQuotaCell, { props: { accountId: 12 } })
    await flushPromises()
    expect(getStatus).toHaveBeenCalledWith(12)
    expect(wrapper.text()).toContain('admin.accounts.webImageShadow.unknown')
    probeStatus.mockResolvedValue({ ...unknown, quota_known: true, remaining: 0, cooldown_until: '2026-10-22T00:00:00Z' })
    await wrapper.get('button').trigger('click')
    await flushPromises()
    expect(probeStatus).toHaveBeenCalledWith(12)
    expect(wrapper.get('strong').text()).toBe('0')
    expect(wrapper.text()).toContain('admin.accounts.webImageShadow.cooldownUntil')
    wrapper.unmount()
  })

  it('preserves a valid snapshot when a probe fails and displays the error', async () => {
    getStatus.mockResolvedValue({ ...unknown, quota_known: true, remaining: 4 })
    probeStatus.mockRejectedValue(new Error('quota probe unavailable'))
    const wrapper = mount(WebImageQuotaCell, { props: { accountId: 12 } })
    await flushPromises()
    await wrapper.get('button').trigger('click')
    await flushPromises()
    expect(wrapper.get('strong').text()).toBe('4')
    expect(wrapper.get('[role="alert"]').text()).toBe('quota probe unavailable')
    wrapper.unmount()
  })

  it('displays image capacity separately from text', () => {
    const wrapper = mount(GroupCapacityBadge, { props: { concurrencyUsed: 2, concurrencyMax: 10, imageConcurrencyUsed: 1, imageConcurrencyMax: 1, sessionsUsed: 0, sessionsMax: 0, rpmUsed: 0, rpmMax: 0 } })
    expect(wrapper.text()).toContain('admin.accounts.webImageShadow.textCapacity')
    expect(wrapper.get('[data-testid="image-capacity"]').text()).toContain('1/1')
    expect(wrapper.get('[data-testid="image-capacity"] span').classes()).toContain('bg-red-100')
  })
})
