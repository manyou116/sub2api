import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import WebImageShadowSettings from '../WebImageShadowSettings.vue'
import { isImageShadow, readWebImageSettings } from '../webImageShadow'
import type { Account } from '@/types'

const { getModels } = vi.hoisted(() => ({ getModels: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: { getImageShadowModels: getModels } } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
const parent = { parent_account_id: 10, quota_dimension: 'work_images' } as Account
const catalog = {
  recommended: { web: 'web-model', work: 'wm-model' },
  models: [
    { id: 'web-model', label: 'Web model', modes: ['web'], thinking_efforts: ['high', 'extended'], default_thinking_effort: 'extended', verified: true },
    { id: 'wm-model', label: 'WM model', modes: ['work'], thinking_efforts: ['low', 'high'], default_thinking_effort: 'high', verified: false },
    { id: 'auto', label: 'Auto', modes: ['web'], thinking_efforts: [], default_thinking_effort: '', verified: false }
  ]
}

describe('Image shadow settings', () => {
  beforeEach(() => getModels.mockResolvedValue(catalog))

  it('keeps WM type fixed, requires attachments, and never renders a Web quota probe', async () => {
    const settings = readWebImageSettings(parent)
    expect(isImageShadow(parent)).toBe(true)
    expect(settings).toMatchObject({ model_mode: 'recommended', thinking_effort: '', require_download_attachment: true })
    const wrapper = mount(WebImageShadowSettings, {
      props: { modelValue: settings, accountId: 20, parentId: 10, mode: 'work' },
      global: { stubs: { WebImageQuotaCell: true } }
    })
    await flushPromises()
    expect(wrapper.text()).toContain('admin.accounts.webImageShadow.workQuota')
    expect(wrapper.text()).toContain('admin.accounts.webImageShadow.unverified')
    expect(wrapper.find('web-image-quota-cell-stub').exists()).toBe(false)
    expect(wrapper.find('[data-testid="web-image-download"]').exists()).toBe(false)
    await wrapper.get('[data-testid="web-image-model-mode"]').setValue('fixed')
    const options = wrapper.get('[data-testid="web-image-model"]').findAll('option').map(option => option.attributes('value'))
    expect(options).toEqual(['', 'wm-model'])
    await wrapper.get('[data-testid="web-image-model"]').setValue('wm-model')
    expect(wrapper.get('[data-testid="web-image-thinking"]').findAll('option').map(option => option.attributes('value'))).toEqual(['', 'low', 'high'])
    expect(wrapper.text()).not.toContain('extended')
    wrapper.unmount()
  })

  it('treats legacy auto as recommended and labels real upstream auto separately', async () => {
    const settings = readWebImageSettings({ ...parent, quota_dimension: 'web_images', extra: { openai_web_images: { model_mode: 'auto' } } } as Account)
    expect(settings.model_mode).toBe('recommended')
    expect(settings.require_download_attachment).toBe(false)
    const wrapper = mount(WebImageShadowSettings, {
      props: { modelValue: { ...settings, model_mode: 'fixed' }, accountId: 21, mode: 'web' },
      global: { stubs: { WebImageQuotaCell: true } }
    })
    await flushPromises()
    expect(wrapper.find('web-image-quota-cell-stub').exists()).toBe(true)
    expect(wrapper.get('[data-testid="web-image-model"]').text()).toContain('admin.accounts.webImageShadow.upstreamAuto')
    expect(wrapper.get('[data-testid="web-image-model"]').text()).not.toContain('WM model')
    wrapper.unmount()
  })
})
