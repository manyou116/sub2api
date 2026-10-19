import { mount, flushPromises } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import OpenAICapacityCooldownSettings from '../OpenAICapacityCooldownSettings.vue'

const mocks = vi.hoisted(() => ({ load: vi.fn(), save: vi.fn(), success: vi.fn(), error: vi.fn() }))
vi.mock('@/api/admin/openaiCapacityCooldown', () => ({
  getOpenAICapacityCooldownSettings: mocks.load,
  updateOpenAICapacityCooldownSettings: mocks.save
}))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: mocks.success, showError: mocks.error }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

const defaults = {
  enabled: false, switch_account_on_overload: true, window_seconds: 60,
  failure_threshold: 3, failure_rate_percent: 50, cooldown_seconds: 60, max_cooldown_seconds: 600
}

describe('OpenAI capacity cooldown settings', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mocks.load.mockResolvedValue({ ...defaults })
    mocks.save.mockImplementation(async (value) => ({ ...value }))
  })

  it('starts disabled, and saves switches and numeric settings independently', async () => {
    const wrapper = mount(OpenAICapacityCooldownSettings)
    await flushPromises()
    expect(wrapper.get('#capacity-enabled').attributes('aria-checked')).toBe('false')
    expect(wrapper.find('input').exists()).toBe(false)
    await wrapper.get('#capacity-enabled').trigger('click')
    await wrapper.get('#capacity-switch').trigger('click')
    await wrapper.get('#capacity-window_seconds').setValue(120)
    await wrapper.get('#capacity-failure_threshold').setValue(5)
    await wrapper.get('#capacity-failure_rate_percent').setValue(80)
    await wrapper.get('#capacity-cooldown_seconds').setValue(90)
    await wrapper.get('#capacity-max_cooldown_seconds').setValue(900)
    await wrapper.get('[data-testid="capacity-save"]').trigger('click')
    await flushPromises()
    expect(mocks.save).toHaveBeenCalledWith({
      enabled: true, switch_account_on_overload: false, window_seconds: 120,
      failure_threshold: 5, failure_rate_percent: 80, cooldown_seconds: 90, max_cooldown_seconds: 900
    })
    expect(mocks.success).toHaveBeenCalledOnce()
  })

  it('loads current values and constrains the maximum cooldown to at least the initial value', async () => {
    mocks.load.mockResolvedValue({ ...defaults, enabled: true, cooldown_seconds: 180 })
    const wrapper = mount(OpenAICapacityCooldownSettings)
    await flushPromises()
    expect((wrapper.get('#capacity-cooldown_seconds').element as HTMLInputElement).value).toBe('180')
    await wrapper.get('#capacity-max_cooldown_seconds').setValue(60)
    expect(wrapper.get('#capacity-max_cooldown_seconds').attributes('min')).toBe('180')
    expect((wrapper.get('#capacity-max_cooldown_seconds').element as HTMLInputElement).checkValidity()).toBe(false)
    await wrapper.get('[data-testid="capacity-save"]').trigger('click')
    expect(mocks.save).not.toHaveBeenCalled()
    await wrapper.get('#capacity-max_cooldown_seconds').setValue(300)
    await wrapper.get('#capacity-enabled').trigger('click')
    await wrapper.get('[data-testid="capacity-save"]').trigger('click')
    await flushPromises()
    expect(mocks.save).toHaveBeenCalledWith({ ...defaults, enabled: false, cooldown_seconds: 180, max_cooldown_seconds: 300 })
  })

  it('prevents saving after a load failure and allows reloading', async () => {
    mocks.load.mockRejectedValueOnce(new Error('offline'))
    const wrapper = mount(OpenAICapacityCooldownSettings)
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('loadFailed')
    expect(wrapper.get('[data-testid="capacity-save"]').attributes('disabled')).toBeDefined()
    await wrapper.get('[data-testid="capacity-save"]').trigger('click')
    expect(mocks.save).not.toHaveBeenCalled()
    await wrapper.get('[role="alert"] button').trigger('click')
    await flushPromises()
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="capacity-save"]').attributes('disabled')).toBeUndefined()
  })

  it('keeps edits after an API error and submits once while saving', async () => {
    mocks.load.mockResolvedValue({ ...defaults, enabled: true })
    const wrapper = mount(OpenAICapacityCooldownSettings)
    await flushPromises()
    await wrapper.get('#capacity-failure_threshold').setValue(7)
    let rejectSave!: (error: Error) => void
    mocks.save.mockImplementationOnce(() => new Promise((_, reject) => { rejectSave = reject }))
    await wrapper.get('[data-testid="capacity-save"]').trigger('click')
    await wrapper.get('[data-testid="capacity-save"]').trigger('click')
    expect(mocks.save).toHaveBeenCalledOnce()
    rejectSave(new Error('offline'))
    await flushPromises()
    expect(mocks.error).toHaveBeenCalledOnce()
    expect((wrapper.get('#capacity-failure_threshold').element as HTMLInputElement).value).toBe('7')
    expect(wrapper.get('[data-testid="capacity-save"]').attributes('disabled')).toBeUndefined()
  })

  it('saves with click or Enter inside the parent settings form without submitting that form', async () => {
    mocks.load.mockResolvedValue({ ...defaults, enabled: true })
    const parentSave = vi.fn()
    const wrapper = mount({
      components: { OpenAICapacityCooldownSettings },
      setup: () => ({ parentSave }),
      template: '<form @submit.prevent="parentSave"><OpenAICapacityCooldownSettings /></form>'
    })
    await flushPromises()
    expect(wrapper.findAll('form')).toHaveLength(1)
    await wrapper.get('[data-testid="capacity-save"]').trigger('click')
    await flushPromises()
    await wrapper.get('#capacity-window_seconds').trigger('keydown', { key: 'Enter' })
    await flushPromises()
    expect(mocks.save).toHaveBeenCalledTimes(2)
    expect(parentSave).not.toHaveBeenCalled()
  })
})
