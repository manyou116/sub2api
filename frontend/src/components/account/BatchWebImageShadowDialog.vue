<template>
  <BaseDialog :show="show" :title="t('admin.accounts.webImageShadow.batchCreate')" :close-on-escape="!running" :show-close-button="!running" @close="emit('close')">
    <div class="space-y-4">
      <p class="text-sm text-gray-600 dark:text-gray-300">{{ t('admin.accounts.webImageShadow.batchConfirm', { count: targets.length, maximum: targets.length * modes.length }) }}</p>
      <label class="input-label">
        {{ t('admin.accounts.webImageShadow.mode') }}
        <select v-model="selection" class="input mt-1" :disabled="running || results.length > 0" data-testid="batch-image-mode">
          <option value="web">{{ t('admin.accounts.webImageShadow.webQuota') }}</option>
          <option value="work">{{ t('admin.accounts.webImageShadow.workQuota') }}</option>
          <option value="both">{{ t('admin.accounts.webImageShadow.both') }}</option>
        </select>
      </label>
      <p v-if="selection === 'both'" class="text-sm text-amber-600">{{ t('admin.accounts.webImageShadow.mixedHint') }}</p>
      <label class="input-label">
        {{ t('admin.accounts.concurrency') }}
        <input v-model.number="concurrency" type="number" min="1" step="1" :disabled="running || results.length > 0" class="input mt-1" data-testid="batch-web-image-concurrency" />
      </label>
      <p v-if="running" role="status" class="text-sm">{{ t('admin.accounts.webImageShadow.batchProgress', { done: completed, total: total }) }}</p>
      <template v-if="results.length">
        <p role="status" class="text-sm">{{ t('admin.accounts.webImageShadow.batchSummary', counts) }}</p>
        <ul class="max-h-64 space-y-2 overflow-auto text-sm" data-testid="batch-web-image-results">
          <li v-for="item in results" :key="itemKey(item)" class="flex flex-wrap gap-x-2 border-b border-gray-100 py-2 dark:border-dark-700">
            <span>#{{ item.parent_id }} · {{ item.mode === 'work' ? 'WM' : 'Web' }}</span>
            <strong :class="item.status === 'failed' ? 'text-red-600' : ''">{{ t(`admin.accounts.webImageShadow.batchStatus.${item.status}`) }}</strong>
            <span v-if="item.shadow_id">→ #{{ item.shadow_id }}</span>
            <span v-if="item.message" class="w-full break-words text-gray-500">{{ item.message }}</span>
          </li>
        </ul>
      </template>
    </div>
    <template #footer>
      <div class="flex justify-end gap-3">
        <button type="button" class="btn btn-secondary" :disabled="running" @click="emit('close')">{{ t('common.close') }}</button>
        <button v-if="!results.length" type="button" class="btn btn-primary" :disabled="running || !validConcurrency || !targets.length" data-testid="batch-web-image-create" @click="run(initialTargets)">{{ t('common.confirm') }}</button>
        <button v-else-if="failedTargets.length" type="button" class="btn btn-primary" :disabled="running" data-testid="batch-web-image-retry" @click="run(failedTargets)">{{ t('admin.accounts.webImageShadow.retryFailed') }}</button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type { ImageShadowMode, WebImageShadowBatchItem } from '@/api/admin/accounts'
import BaseDialog from '@/components/common/BaseDialog.vue'

const props = defineProps<{ show: boolean; parentIds: number[] }>()
const emit = defineEmits<{ close: []; completed: [] }>()
const { t } = useI18n()
const targets = ref<number[]>([])
const selection = ref<ImageShadowMode | 'both'>('web')
const modes = computed<ImageShadowMode[]>(() => selection.value === 'both' ? ['web', 'work'] : [selection.value])
type Target = { parent_id: number; mode: ImageShadowMode }
const itemKey = (item: Target) => `${item.parent_id}:${item.mode}`
const initialTargets = computed(() => targets.value.flatMap(parent_id => modes.value.map(mode => ({ parent_id, mode }))))
const concurrency = ref(1)
const running = ref(false)
const completed = ref(0)
const total = ref(0)
const results = ref<WebImageShadowBatchItem[]>([])
const validConcurrency = computed(() => Number.isSafeInteger(concurrency.value) && concurrency.value > 0)
const failedTargets = computed(() => results.value.filter(item => item.status === 'failed'))
const counts = computed(() => ({
  created: results.value.filter(item => item.status === 'created').length,
  existing: results.value.filter(item => item.status === 'existing').length,
  skipped: results.value.filter(item => item.status === 'skipped').length,
  failed: failedTargets.value.length
}))

watch(() => props.show, show => {
  if (!show || running.value) return
  targets.value = [...new Set(props.parentIds)]
  selection.value = 'web'
  concurrency.value = 1
  results.value = []
}, { immediate: true })

async function run(requested: Target[]) {
  if (running.value || !validConcurrency.value) return
  const entries = [...requested]
  running.value = true
  completed.value = 0
  total.value = entries.length
  const accumulated = new Map(results.value.map(item => [itemKey(item), item]))
  try {
    // Group retries by type so a failed WM item never repeats a successful Web item.
    for (const mode of ['web', 'work'] as const) {
      const ids = entries.filter(item => item.mode === mode).map(item => item.parent_id)
      for (let offset = 0; offset < ids.length; offset += 200) {
        const chunk = ids.slice(offset, offset + 200)
        let items: WebImageShadowBatchItem[]
        try {
          const response = await adminAPI.accounts.createWebImageShadows(chunk, concurrency.value, [mode])
          const byId = new Map(response.results.filter(item => item.mode === mode).map(item => [item.parent_id, item]))
          items = chunk.map(parent_id => byId.get(parent_id) || { parent_id, mode, status: 'failed', message: t('admin.accounts.webImageShadow.missingResult') })
        } catch (error: any) {
          const message = error?.response?.data?.message || error?.message || t('admin.accounts.webImageShadow.createFailed')
          items = chunk.map(parent_id => ({ parent_id, mode, status: 'failed', message }))
        }
        items.forEach(item => accumulated.set(itemKey(item), item))
        results.value = [...accumulated.values()]
        completed.value += chunk.length
      }
    }
  } finally {
    running.value = false
    emit('completed')
  }
}
</script>
