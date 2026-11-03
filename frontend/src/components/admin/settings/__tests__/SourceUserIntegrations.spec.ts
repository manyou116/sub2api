import { mount, flushPromises } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createI18n } from 'vue-i18n'
import Settings from '../SourceUserIntegrations.vue'
import en from '@/i18n/locales/en'

const mocks = vi.hoisted(() => ({ get: vi.fn(), put: vi.fn(), success: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: { get: mocks.get, put: mocks.put } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: mocks.success }) }))

function render() {
  return mount(Settings, { global: { plugins: [createI18n({ legacy: false, locale: 'en', messages: { en } })] } })
}

describe('trusted source settings', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mocks.get.mockResolvedValue({ data: [{ id: 'factory', name: 'Factory', api_key_ids: [3] }] })
    mocks.put.mockResolvedValue({})
  })
  it('loads bindings and saves edited key IDs without a user directory', async () => {
    const wrapper = render(); await flushPromises()
    const inputs = wrapper.findAll('input')
    expect(inputs[0].element.value).toBe('factory')
    await inputs[2].setValue('3, 23')
    await wrapper.get('button.btn-primary').trigger('click'); await flushPromises()
    expect(mocks.put).toHaveBeenCalledWith('/admin/settings/source-user-integrations', [{ id: 'factory', name: 'Factory', api_key_ids: [3, 23] }])
  })
  it('blocks duplicate keys and keeps load failures disabled until retry succeeds', async () => {
    const wrapper = render(); await flushPromises()
    await wrapper.findAll('input')[2].setValue('3, 3')
    await wrapper.get('button.btn-primary').trigger('click'); await flushPromises()
    expect(mocks.put).not.toHaveBeenCalled()
    expect(wrapper.find('[role="alert"]').exists()).toBe(true)
    wrapper.unmount()
    mocks.get.mockRejectedValueOnce(new Error('offline'))
    const failed = render(); await flushPromises()
    expect(failed.get('fieldset').attributes('disabled')).toBeDefined()
    await failed.get('button').trigger('click'); await flushPromises()
    expect(failed.get('fieldset').attributes('disabled')).toBeUndefined()
  })
})
