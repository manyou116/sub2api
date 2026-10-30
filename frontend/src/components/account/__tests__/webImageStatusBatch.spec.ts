import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises } from '@vue/test-utils'
import { loadWebImageStatus } from '../webImageStatusBatch'

const { getStatuses } = vi.hoisted(() => ({ getStatuses: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: { getWebImageStatuses: getStatuses } } }))
const status = { parent_account_id: 7, quota_known: true, remaining: 3, reset_at: null, probed_at: null, cooldown_until: null }

describe('Web quota status batching', () => {
  beforeEach(() => {
    getStatuses.mockReset()
    getStatuses.mockImplementation(async (ids: number[]) => ({ statuses: Object.fromEntries(ids.map(id => [id, status])), errors: {} }))
  })

  it('combines mounted rows and concurrent duplicate readers into one request', async () => {
    const one = loadWebImageStatus(1)
    expect(loadWebImageStatus(1)).toBe(one)
    const two = loadWebImageStatus(2)
    await expect(Promise.all([one, two])).resolves.toEqual([status, status])
    expect(getStatuses).toHaveBeenCalledTimes(1)
    expect(getStatuses).toHaveBeenCalledWith([1, 2])
    await loadWebImageStatus(1)
    expect(getStatuses).toHaveBeenCalledTimes(2)
  })

  it('limits each request to 200 accounts and awaits each batch', async () => {
    let complete!: (result: unknown) => void
    getStatuses.mockImplementationOnce(() => new Promise(resolve => { complete = resolve }))
    const reads = Array.from({ length: 201 }, (_, i) => loadWebImageStatus(i + 1))
    await flushPromises()
    expect(getStatuses).toHaveBeenCalledTimes(1)
    expect(getStatuses.mock.calls[0][0]).toHaveLength(200)
    complete({ statuses: Object.fromEntries(Array.from({ length: 200 }, (_, i) => [i + 1, status])), errors: {} })
    await Promise.all(reads)
    expect(getStatuses).toHaveBeenLastCalledWith([201])
  })

  it('isolates per-account errors and permits another read after failure', async () => {
    getStatuses.mockResolvedValueOnce({ statuses: { 1: status }, errors: { 2: 'parent unavailable' } })
    const results = await Promise.allSettled([loadWebImageStatus(1), loadWebImageStatus(2)])
    expect(results[0].status).toBe('fulfilled')
    expect(results[1]).toMatchObject({ status: 'rejected', reason: new Error('parent unavailable') })
    await expect(loadWebImageStatus(2)).resolves.toEqual(status)
  })

  it('rejects every waiting row on request failure without retaining rejected promises', async () => {
    getStatuses.mockRejectedValueOnce(new Error('network down'))
    const results = await Promise.allSettled([loadWebImageStatus(1), loadWebImageStatus(2)])
    expect(results.every(result => result.status === 'rejected')).toBe(true)
    await expect(loadWebImageStatus(1)).resolves.toEqual(status)
  })
})
