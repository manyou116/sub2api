import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import BatchWebImageShadowDialog from '../BatchWebImageShadowDialog.vue'

const { createBatch } = vi.hoisted(() => ({ createBatch: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: { createWebImageShadows: createBatch } } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
const render = (parentIds = [1, 2, 3, 4]) => mount(BatchWebImageShadowDialog, {
  props: { show: true, parentIds },
  global: { stubs: { BaseDialog: { props: ['show'], template: '<div v-if="show"><slot /><slot name="footer" /></div>' } } }
})

describe('Batch Web image shadow creation', () => {
  beforeEach(() => { createBatch.mockReset() })

  it('defaults to one slot and retries only failed parents, retaining other results', async () => {
    createBatch.mockResolvedValueOnce({ results: [
      { parent_id: 1, shadow_id: 11, status: 'created' },
      { parent_id: 2, status: 'existing' },
      { parent_id: 3, status: 'skipped', message: 'unsupported parent' },
      { parent_id: 4, status: 'failed', message: 'temporary failure' }
    ] }).mockResolvedValueOnce({ results: [{ parent_id: 4, shadow_id: 14, status: 'created' }] })
    const wrapper = render()
    await wrapper.get('[data-testid="batch-web-image-create"]').trigger('click')
    await flushPromises()
    expect(createBatch).toHaveBeenCalledWith([1, 2, 3, 4], 1)
    expect(wrapper.findAll('li')).toHaveLength(4)
    expect(wrapper.text()).toContain('unsupported parent')
    await wrapper.get('[data-testid="batch-web-image-retry"]').trigger('click')
    await flushPromises()
    expect(createBatch).toHaveBeenLastCalledWith([4], 1)
    expect(wrapper.findAll('li')).toHaveLength(4)
    expect(wrapper.text()).toContain('→ #14')
    expect(wrapper.find('[data-testid="batch-web-image-retry"]').exists()).toBe(false)
    expect(wrapper.emitted('completed')).toHaveLength(2)
    wrapper.unmount()
  })

  it('snapshots cross-page IDs, deduplicates and sends sequential batches of at most 200', async () => {
    let finish!: (value: unknown) => void
    createBatch.mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
      .mockResolvedValueOnce({ results: [{ parent_id: 201, status: 'created' }] })
    const ids = Array.from({ length: 201 }, (_, i) => i + 1)
    const wrapper = render([...ids, 1])
    await wrapper.get('[data-testid="batch-web-image-concurrency"]').setValue(2)
    await wrapper.get('[data-testid="batch-web-image-create"]').trigger('click')
    await flushPromises()
    await wrapper.setProps({ parentIds: [999] })
    expect(createBatch).toHaveBeenCalledTimes(1)
    expect(createBatch.mock.calls[0]).toEqual([ids.slice(0, 200), 2])
    finish({ results: ids.slice(0, 200).map(parent_id => ({ parent_id, status: 'created' })) })
    await flushPromises()
    expect(createBatch).toHaveBeenLastCalledWith([201], 2)
    expect(wrapper.findAll('li')).toHaveLength(201)
    wrapper.unmount()
  })

  it('rejects invalid concurrency and exposes uncertain request results for retry', async () => {
    createBatch.mockRejectedValueOnce(new Error('connection closed'))
    const wrapper = render([1, 2])
    await wrapper.get('[data-testid="batch-web-image-concurrency"]').setValue(0)
    expect(wrapper.get('[data-testid="batch-web-image-create"]').attributes('disabled')).toBeDefined()
    await wrapper.get('[data-testid="batch-web-image-concurrency"]').setValue(1.5)
    expect(wrapper.get('[data-testid="batch-web-image-create"]').attributes('disabled')).toBeDefined()
    await wrapper.get('[data-testid="batch-web-image-concurrency"]').setValue(1)
    await wrapper.get('[data-testid="batch-web-image-create"]').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('connection closed')
    expect(wrapper.findAll('li')).toHaveLength(2)
    expect(wrapper.find('[data-testid="batch-web-image-retry"]').exists()).toBe(true)
    wrapper.unmount()
  })
})
