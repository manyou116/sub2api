import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import SIWCAccountSettings from '../SIWCAccountSettings.vue'

const { get, post, put } = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn(), put: vi.fn() }))
vi.mock('@/api/client', () => ({ default: { get, post, put } }))
vi.mock('vue-i18n', async () => {
  const { ref } = await import('vue')
  return { useI18n: () => ({ locale: ref('en') }) }
})

describe('OpenAI authorization connections', () => {
  beforeEach(() => {
    get.mockReset()
    post.mockReset()
    put.mockReset()
  })

  it('binds a browser callback and enables SIWC explicitly', async () => {
    const root = '/admin/openai/accounts/7/siwc'
    get.mockResolvedValueOnce({ data: { bound: false, enabled: false } })
    post.mockResolvedValueOnce({ data: { auth_url: 'https://auth.openai.com/authorize', session_id: 'fixture' } })
    post.mockResolvedValueOnce({ data: { bound: true, enabled: false } })
    put.mockResolvedValueOnce({ data: { bound: true, enabled: true } })
    get.mockResolvedValueOnce({ data: { bound: true, enabled: true } })
    const wrapper = mount(SIWCAccountSettings, { props: { accountId: 7 } })
    const button = (label: string) => wrapper.findAll('button').find(el => el.text() === label)!
    await flushPromises()
    await button('Start browser authorization').trigger('click')
    await flushPromises()
    expect(wrapper.get('a').attributes('href')).toBe('https://auth.openai.com/authorize')
    const callback = 'http://127.0.0.1:1455/auth/callback?state=test&code=fixture'
    await wrapper.get('input').setValue(callback)
    await button('Finish authorization').trigger('click')
    await flushPromises()
    expect(post).toHaveBeenLastCalledWith(root + '/callback', { session_id: 'fixture', callback_url: callback }, { timeout: 50_000 })
    expect(wrapper.find('input').exists()).toBe(false)
    expect(put).not.toHaveBeenCalled()
    await button('Enable SIWC').trigger('click')
    await flushPromises()
    expect(put).toHaveBeenLastCalledWith(root, { enabled: true })
    expect(wrapper.text()).toContain('Switch to Codex')
    wrapper.unmount()
  })

  it('saves the Excel model scope and search switch on the Excel connection', async () => {
    const root = '/admin/openai/accounts/8/excel'
    get.mockResolvedValueOnce({ data: { bound: true, enabled: false } })
    put.mockResolvedValue({ data: { bound: true, enabled: false, models: ['model-a', 'model-b'], search_enabled: true } })
    const wrapper = mount(SIWCAccountSettings, { props: { accountId: 8, kind: 'excel' } })
    await flushPromises()
    await wrapper.get('textarea').setValue('model-a\n\nmodel-b')
    await wrapper.findAll('button').find(el => el.text() === 'Save model scope')!.trigger('click')
    await flushPromises()
    expect(put).toHaveBeenLastCalledWith(root, { models: ['model-a', 'model-b'] })
    const search = wrapper.get('input[type="checkbox"]')
    await search.setValue(false)
    await flushPromises()
    expect(put).toHaveBeenLastCalledWith(root, { search_enabled: false })
    wrapper.unmount()
  })
})
