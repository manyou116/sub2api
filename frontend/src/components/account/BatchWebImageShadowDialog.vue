<template>
  <BaseDialog :show="show" :title="t('admin.accounts.webImageShadow.batchCreate')" :close-on-escape="!running" :show-close-button="!running" @close="emit('close')">
    <div class="space-y-4">
      <p class="text-sm text-gray-600 dark:text-gray-300">{{ t('admin.accounts.webImageShadow.batchConfirm', { count: targets.length }) }}</p>
      <label class="input-label">
        {{ t('admin.accounts.concurrency') }}
        <input v-model.number="concurrency" type="number" min="1" step="1" :disabled="running || results.length > 0" class="input mt-1" data-testid="batch-web-image-concurrency" />
      </label>
      <p v-if="running" role="status" class="text-sm">{{ t('admin.accounts.webImageShadow.batchProgress', { done: completed, total: total }) }}</p>
      <template v-if="results.length">
        <p role="status" class="text-sm">{{ t('admin.accounts.webImageShadow.batchSummary', counts) }}</p>
        <ul class="max-h-64 space-y-2 overflow-auto text-sm" data-testid="batch-web-image-results">
          <li v-for="item in results" :key="item.parent_id" class="flex flex-wrap gap-x-2 border-b border-gray-100 py-2 dark:border-dark-700">
            <span>#{{ item.parent_id }}</span>
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
        <button v-if="!results.length" type="button" class="btn btn-primary" :disabled="running || !validConcurrency || !targets.length" data-testid="batch-web-image-create" @click="run(targets)">{{ t('common.confirm') }}</button>
        <button v-else-if="failedIds.length" type="button" class="btn btn-primary" :disabled="running" data-testid="batch-web-image-retry" @click="run(failedIds)">{{ t('admin.accounts.webImageShadow.retryFailed') }}</button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type { WebImageShadowBatchItem } from '@/api/admin/accounts'
import BaseDialog from '@/components/common/BaseDialog.vue'

const props = defineProps<{ show: boolean; parentIds: number[] }>()
const emit = defineEmits<{ close: []; completed: [] }>()
const { t } = useI18n()
const targets = ref<number[]>([])
const concurrency = ref(1)
const running = ref(false)
const completed = ref(0)
const total = ref(0)
const results = ref<WebImageShadowBatchItem[]>([])
const validConcurrency = computed(() => Number.isSafeInteger(concurrency.value) && concurrency.value > 0)
const failedIds = computed(() => results.value.filter(item => item.status === 'failed').map(item => item.parent_id))
const counts = computed(() => ({
  created: results.value.filter(item => item.status === 'created').length,
  existing: results.value.filter(item => item.status === 'existing').length,
  skipped: results.value.filter(item => item.status === 'skipped').length,
  failed: failedIds.value.length
}))

watch(() => props.show, show => {
  if (!show || running.value) return
  targets.value = [...new Set(props.parentIds)]
  concurrency.value = 1
  results.value = []
}, { immediate: true })

async function run(parentIds: number[]) {
  if (running.value || !validConcurrency.value) return
  const ids = [...parentIds]
  running.value = true
  completed.value = 0
  total.value = ids.length
  const accumulated = new Map(results.value.map(item => [item.parent_id, item]))
  try {
    for (let offset = 0; offset < ids.length; offset += 200) {
      const chunk = ids.slice(offset, offset + 200)
      let items: WebImageShadowBatchItem[]
      try {
        const response = await adminAPI.accounts.createWebImageShadows(chunk, concurrency.value)
        const byId = new Map(response.results.map(item => [item.parent_id, item]))
        items = chunk.map(parent_id => byId.get(parent_id) || { parent_id, status: 'failed', message: t('admin.accounts.webImageShadow.missingResult') })
      } catch (error: any) {
        const message = error?.response?.data?.message || error?.message || t('admin.accounts.webImageShadow.createFailed')
        items = chunk.map(parent_id => ({ parent_id, status: 'failed', message }))
      }
      items.forEach(item => accumulated.set(item.parent_id, item))
      results.value = [...accumulated.values()]
      completed.value += chunk.length
    }
  } finally {
    running.value = false
    emit('completed')
  }
}
</script>
