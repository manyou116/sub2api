import { adminAPI } from '@/api/admin'
import type { WebImageStatus } from '@/api/admin/accounts'

type Pending = {
  promise: Promise<WebImageStatus>
  resolve: (status: WebImageStatus) => void
  reject: (error: unknown) => void
}
const pending = new Map<number, Pending>()
const queued = new Set<number>()
let flushing = false

// Coalesce mounted rows and duplicate readers without keeping a stale quota cache.
export function loadWebImageStatus(accountId: number): Promise<WebImageStatus> {
  const existing = pending.get(accountId)
  if (existing) return existing.promise
  let resolve!: Pending['resolve']
  let reject!: Pending['reject']
  const promise = new Promise<WebImageStatus>((yes, no) => { resolve = yes; reject = no })
  pending.set(accountId, { promise, resolve, reject })
  queued.add(accountId)
  if (!flushing) {
    flushing = true
    queueMicrotask(() => { void flush() })
  }
  return promise
}

async function flush() {
  try {
    while (queued.size) {
      const ids = [...queued].slice(0, 200)
      ids.forEach(id => queued.delete(id))
      try {
        const result = await adminAPI.accounts.getWebImageStatuses(ids)
        for (const id of ids) {
          const entry = pending.get(id)!
          const status = result.statuses[String(id)]
          if (status) entry.resolve(status)
          else entry.reject(new Error(result.errors[String(id)] || 'Image quota status unavailable'))
        }
      } catch (error) {
        ids.forEach(id => pending.get(id)!.reject(error))
      } finally {
        ids.forEach(id => pending.delete(id))
      }
    }
  } finally {
    flushing = false
  }
}
