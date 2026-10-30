import { mount, flushPromises } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createI18n } from 'vue-i18n'
import Settings from '../OpenAIRequestCompressionSettings.vue'
import zh from '@/i18n/locales/zh'
import en from '@/i18n/locales/en'

const mocks = vi.hoisted(() => ({ load: vi.fn(), save: vi.fn(), success: vi.fn(), error: vi.fn() }))
vi.mock('@/api/admin/openaiRequestCompression', () => ({
  getOpenAIRequestCompression: mocks.load,
  updateOpenAIRequestCompression: mocks.save
}))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: mocks.success, showError: mocks.error }) }))

function render(locale = 'zh') {
  return mount(Settings, { global: { plugins: [createI18n({ legacy: false, locale, messages: { zh, en } })] } })
}

describe('global Codex OAuth request compression', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mocks.load.mockResolvedValue({ enabled: true })
    mocks.save.mockImplementation(async (enabled: boolean) => ({ enabled }))
  })

  it('saves an explicit global false and reloads the persisted state', async () => {
    const wrapper = render()
    await flushPromises()
    expect(wrapper.text()).toContain('全局启用请求体压缩')
    expect(wrapper.get('[role="switch"]').attributes('aria-checked')).toBe('true')
    await wrapper.get('[role="switch"]').trigger('click')
    await wrapper.get('[data-testid="request-compression-save"]').trigger('click')
    await flushPromises()
    expect(mocks.save).toHaveBeenCalledTimes(1)
    expect(mocks.save).toHaveBeenCalledWith(false)
    expect(mocks.success).toHaveBeenCalledWith('全局请求体压缩设置已保存')
    mocks.load.mockResolvedValue({ enabled: false })
    wrapper.unmount()
    const reloaded = render('en')
    await flushPromises()
    expect(reloaded.text()).toContain('Enable request compression globally')
    expect(reloaded.get('[role="switch"]').attributes('aria-checked')).toBe('false')
    await reloaded.get('[role="switch"]').trigger('click')
    await reloaded.get('[data-testid="request-compression-save"]').trigger('click')
    await flushPromises()
    expect(mocks.save).toHaveBeenLastCalledWith(true)
  })

  it('blocks saving while loading or after a read failure and supports retry', async () => {
    mocks.load.mockRejectedValueOnce(new Error('offline'))
    const wrapper = render()
    expect(wrapper.get('fieldset').attributes()).toHaveProperty('disabled')
    await wrapper.get('[data-testid="request-compression-save"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('读取压缩设置失败')
    expect(wrapper.get('fieldset').attributes()).toHaveProperty('disabled')
    expect(mocks.save).not.toHaveBeenCalled()
    await wrapper.get('[role="alert"] button').trigger('click')
    await flushPromises()
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
    expect(wrapper.get('fieldset').attributes()).not.toHaveProperty('disabled')
  })

  it('keeps the edit available after a failed write and prevents double saves', async () => {
    let rejectSave: (err: Error) => void = () => {}
    mocks.save.mockReturnValue(new Promise((_, reject) => { rejectSave = reject }))
    const wrapper = render()
    await flushPromises()
    await wrapper.get('[role="switch"]').trigger('click')
    await wrapper.get('[data-testid="request-compression-save"]').trigger('click')
    await wrapper.get('[data-testid="request-compression-save"]').trigger('click')
    expect(mocks.save).toHaveBeenCalledTimes(1)
    expect(wrapper.get('fieldset').attributes()).toHaveProperty('disabled')
    rejectSave(new Error('offline'))
    await flushPromises()
    expect(mocks.error).toHaveBeenCalledWith('保存全局请求体压缩设置失败')
    expect(wrapper.get('[role="switch"]').attributes('aria-checked')).toBe('false')
    expect(wrapper.get('fieldset').attributes()).not.toHaveProperty('disabled')
  })
})
