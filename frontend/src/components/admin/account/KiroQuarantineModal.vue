<template>
  <BaseDialog :show="show" :title="t('admin.kiro.quarantine.title')" width="wide" @close="$emit('close')">
    <div class="space-y-3">
      <div class="flex items-center justify-between">
        <p class="text-sm text-gray-600 dark:text-gray-400">
          {{ t('admin.kiro.quarantine.description') }}
        </p>
        <button class="btn btn-sm" :disabled="loading" @click="reload">
          {{ loading ? t('common.loading') : t('common.refresh') }}
        </button>
      </div>

      <div v-if="entries.length === 0 && !loading" class="py-8 text-center text-sm text-gray-500">
        {{ t('admin.kiro.quarantine.empty') }}
      </div>

      <table v-else class="min-w-full text-sm">
        <thead class="bg-gray-50 dark:bg-gray-800">
          <tr>
            <th class="px-3 py-2 text-left">Account ID</th>
            <th class="px-3 py-2 text-left">{{ t('admin.kiro.quarantine.scope') }}</th>
            <th class="px-3 py-2 text-left">{{ t('admin.kiro.quarantine.remaining') }}</th>
            <th class="px-3 py-2 text-left">{{ t('admin.kiro.quarantine.attempts') }}</th>
            <th class="px-3 py-2 text-right">{{ t('common.actions') }}</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="(entry, idx) in entries" :key="idx" class="border-t dark:border-gray-700">
            <td class="px-3 py-2 font-mono">{{ entry.account_id }}</td>
            <td class="px-3 py-2">
              <span v-if="entry.model" class="inline-flex items-center gap-1">
                <span class="rounded bg-orange-100 dark:bg-orange-900/40 px-2 py-0.5 text-xs">model</span>
                <code class="text-xs">{{ entry.model }}</code>
              </span>
              <span v-else class="rounded bg-red-100 dark:bg-red-900/40 px-2 py-0.5 text-xs">account</span>
            </td>
            <td class="px-3 py-2">{{ formatDuration(entry.remaining_ms) }}</td>
            <td class="px-3 py-2">{{ entry.attempts ?? '-' }}</td>
            <td class="px-3 py-2 text-right">
              <button class="btn btn-sm btn-danger" :disabled="clearing === idx" @click="clearOne(entry, idx)">
                {{ clearing === idx ? t('common.loading') : t('admin.kiro.quarantine.clear') }}
              </button>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { listKiroQuarantine, clearKiroQuarantine, type KiroQuarantineEntry } from '@/api/admin/accounts'

const props = defineProps<{ show: boolean }>()
defineEmits<{ (e: 'close'): void }>()

const { t } = useI18n()
const entries = ref<KiroQuarantineEntry[]>([])
const loading = ref(false)
const clearing = ref<number | null>(null)

async function reload() {
  loading.value = true
  try {
    const resp = await listKiroQuarantine()
    entries.value = resp.items || []
  } finally {
    loading.value = false
  }
}

async function clearOne(entry: KiroQuarantineEntry, idx: number) {
  clearing.value = idx
  try {
    await clearKiroQuarantine(entry.account_id, entry.model)
    await reload()
  } finally {
    clearing.value = null
  }
}

function formatDuration(ms: number): string {
  if (ms <= 0) return '-'
  const s = Math.floor(ms / 1000)
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m ${s % 60}s`
  const h = Math.floor(m / 60)
  return `${h}h ${m % 60}m`
}

watch(() => props.show, (v) => { if (v) reload() })
</script>
