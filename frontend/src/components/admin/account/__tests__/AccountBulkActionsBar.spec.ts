import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

import AccountBulkActionsBar from '../AccountBulkActionsBar.vue'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => key
  })
}))

describe('AccountBulkActionsBar', () => {
  it('allows selecting all results before any row is selected', async () => {
    const wrapper = mount(AccountBulkActionsBar, {
      props: {
        selectedIds: [],
        totalResults: 45,
        selectingAll: false,
        allResultsSelected: false
      }
    })

    const button = wrapper.findAll('button').find(item =>
      item.text().includes('admin.accounts.bulkActions.selectAllResults')
    )

    expect(button).toBeDefined()
    await button!.trigger('click')
    expect(wrapper.emitted('select-all-results')).toHaveLength(1)
  })

  it('preserves the upstream billing probe action from v0.1.166', async () => {
    const wrapper = mount(AccountBulkActionsBar, {
      props: {
        selectedIds: [1],
        totalResults: 45,
        selectingAll: false,
        allResultsSelected: false
      }
    })

    const button = wrapper.findAll('button').find(item =>
      item.text().includes('admin.accounts.bulkActions.probeUpstreamBilling')
    )

    expect(button).toBeDefined()
    await button!.trigger('click')
    expect(wrapper.emitted('probe-upstream-billing')).toHaveLength(1)
  })

  it('exposes account-level quota reconciliation controls for selected accounts', async () => {
    const wrapper = mount(AccountBulkActionsBar, {
      props: {
        selectedIds: [1, 2],
        totalResults: 45,
        selectingAll: false,
        allResultsSelected: false
      }
    })

    const enable = wrapper.findAll('button').find(item =>
      item.text().includes('admin.accounts.bulkActions.enableRateLimitReconcile')
    )
    const disable = wrapper.findAll('button').find(item =>
      item.text().includes('admin.accounts.bulkActions.disableRateLimitReconcile')
    )

    expect(enable).toBeDefined()
    expect(disable).toBeDefined()
    await enable!.trigger('click')
    await disable!.trigger('click')
    expect(wrapper.emitted('toggle-rate-limit-reconcile')).toEqual([[true], [false]])
    await wrapper.setProps({ rateLimitReconcileBusy: true })
    expect(enable!.attributes('disabled')).toBeDefined()
    expect(disable!.attributes('disabled')).toBeDefined()
  })
})
