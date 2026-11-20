import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { Account } from '@/types'
import BPSShadowDialog from '../BPSShadowDialog.vue'

const { post, get } = vi.hoisted(() => ({ post: vi.fn(), get: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: { post, get } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

const render = () => mount(BPSShadowDialog, {
  props: {
    target: { id: 7 } as Account
  },
  global: { stubs: { BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' } } }
})
const create = (wrapper: ReturnType<typeof render>) =>
  wrapper.findAll('button').find(button => button.text() === 'common.confirm')!

describe('BPS shadow creation', () => {
  beforeEach(() => { post.mockReset(); get.mockReset() })

  it('imports a pasted grant during creation without selecting a group or model', async () => {
    const fixture = { access_token: 'test-fixture' }
    post.mockResolvedValueOnce({ data: { id: 21 } })
      .mockResolvedValueOnce({ data: { state: 'pending', session: 'fixture-session' } })
    get.mockResolvedValueOnce({ data: { state: 'authorized' } })
    const wrapper = render()
    expect(wrapper.find('input').exists()).toBe(false)
    expect(wrapper.find('select').exists()).toBe(false)
    await wrapper.get('textarea').setValue(JSON.stringify(fixture))
    await create(wrapper).trigger('click')
    await flushPromises()
    expect(post.mock.calls).toEqual([
      ['/admin/accounts/7/bps-shadow', {}],
      ['/admin/accounts/21/bps-authorization', { method: 'paste', grant: fixture }]
    ])
    expect(wrapper.get('textarea').element.value).toBe('')
    expect(wrapper.text()).toContain('admin.accounts.bps.states.authorized')
    wrapper.unmount()
  })

  it('starts device authorization and shows normalized errors on the saved shadow', async () => {
    post.mockResolvedValueOnce({ data: { id: 21 } })
      .mockRejectedValueOnce({ status: 503, message: 'BPS bridge unavailable' })
    const wrapper = render()
    await wrapper.findAll('button').find(button => button.text() === 'admin.accounts.bps.device')!.trigger('click')
    await create(wrapper).trigger('click')
    await flushPromises()
    expect(post).toHaveBeenLastCalledWith('/admin/accounts/21/bps-authorization', { method: 'device', grant: undefined })
    expect(wrapper.get('[role="alert"]').text()).toBe('BPS bridge unavailable')
    expect(wrapper.text()).toContain('admin.accounts.bps.states.authorization_required')
    wrapper.unmount()
  })
})
