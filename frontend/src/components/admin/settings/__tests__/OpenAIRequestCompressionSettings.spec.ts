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
    mocks.save.mockImplementation(async (settings: { enabled: boolean; image_guard_enabled?: boolean; image_guard_max_bytes?: number; bare_400_fallback_enabled?: boolean }) => ({
      enabled: settings.enabled,
      image_guard_enabled: settings.image_guard_enabled ?? true,
      image_guard_max_bytes: settings.image_guard_max_bytes ?? (8 * 1024 * 1024),
      bare_400_fallback_enabled: settings.bare_400_fallback_enabled ?? true,
    }))
  })

  it('saves an explicit global false and reloads the persisted state', async () => {
    const wrapper = render()
    await flushPromises()
    expect(wrapper.text()).toContain('全局启用请求体压缩')
    expect(wrapper.get('#request-compression-enabled').attributes('aria-checked')).toBe('true')
    await wrapper.get('#request-compression-enabled').trigger('click')
    await wrapper.get('[data-testid="request-compression-save"]').trigger('click')
    await flushPromises()
    expect(mocks.save).toHaveBeenCalledTimes(1)
    expect(mocks.save).toHaveBeenCalledWith({ enabled: false, image_guard_enabled: true, image_guard_max_bytes: 8 * 1024 * 1024, bare_400_fallback_enabled: true })
    expect(mocks.success).toHaveBeenCalledWith('全局请求体压缩设置已保存')
    mocks.load.mockResolvedValue({ enabled: false })
    wrapper.unmount()
    const reloaded = render('en')
    await flushPromises()
    expect(reloaded.text()).toContain('Enable request compression globally')
    expect(reloaded.get('#request-compression-enabled').attributes('aria-checked')).toBe('false')
    await reloaded.get('#request-compression-enabled').trigger('click')
    await reloaded.get('[data-testid="request-compression-save"]').trigger('click')
    await flushPromises()
    expect(mocks.save).toHaveBeenLastCalledWith({ enabled: true, image_guard_enabled: true, image_guard_max_bytes: 8 * 1024 * 1024, bare_400_fallback_enabled: true })
  })

  it('saves and reloads both protections and the threshold independently', async () => {
    mocks.load.mockResolvedValue({ enabled: true, image_guard_enabled: true, image_guard_max_bytes: 8 * 1024 * 1024, bare_400_fallback_enabled: true })
    const wrapper = render()
    await flushPromises()
    await wrapper.get('#request-compression-image-guard').trigger('click')
    await wrapper.get('#request-compression-bare-400-fallback').trigger('click')
    await wrapper.get('#request-compression-image-limit').setValue('16')
    await wrapper.get('[data-testid="request-compression-save"]').trigger('click')
    await flushPromises()
    const persisted = { enabled: true, image_guard_enabled: false, image_guard_max_bytes: 16 * 1024 * 1024, bare_400_fallback_enabled: false }
    expect(mocks.save).toHaveBeenLastCalledWith(persisted)
    wrapper.unmount()
    mocks.load.mockResolvedValue(persisted)
    const reloaded = render('en')
    await flushPromises()
    expect(reloaded.get('#request-compression-enabled').attributes('aria-checked')).toBe('true')
    expect(reloaded.get('#request-compression-image-guard').attributes('aria-checked')).toBe('false')
    expect(reloaded.get('#request-compression-bare-400-fallback').attributes('aria-checked')).toBe('false')
    expect((reloaded.get('#request-compression-image-limit').element as HTMLInputElement).value).toBe('16')
    reloaded.unmount()
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
    await wrapper.get('#request-compression-enabled').trigger('click')
    await wrapper.get('[data-testid="request-compression-save"]').trigger('click')
    await wrapper.get('[data-testid="request-compression-save"]').trigger('click')
    expect(mocks.save).toHaveBeenCalledTimes(1)
    expect(wrapper.get('fieldset').attributes()).toHaveProperty('disabled')
    rejectSave(new Error('offline'))
    await flushPromises()
    expect(mocks.error).toHaveBeenCalledWith('保存全局请求体压缩设置失败')
    expect(wrapper.get('#request-compression-enabled').attributes('aria-checked')).toBe('false')
    expect(wrapper.get('fieldset').attributes()).not.toHaveProperty('disabled')
  })
})
