<template>
  <details v-if="stats" class="min-w-36 text-xs" data-testid="upstream-body-stats">
    <summary class="cursor-pointer space-y-0.5">
      <span class="tabular-nums">{{ bytes(stats.raw_bytes) }} → {{ bytes(stats.encoded_bytes) }}</span>
      <span class="block text-gray-500">{{ t('usage.requestBody.saved', { percent: saved }) }}</span>
    </summary>
    <div class="mt-2 max-w-64 space-y-1 text-gray-500">
      <p>{{ t('usage.requestBody.attempts', { count: stats.attempts, compressed: stats.compressed_attempts, fallbacks: stats.fallbacks }) }}</p>
      <p>{{ t('usage.requestBody.final', { encoding: stats.encoding, raw: bytes(stats.final_raw_bytes), encoded: bytes(stats.final_encoded_bytes) }) }}</p>
      <p>{{ t(`usage.requestBody.reasons.${stats.reason}`) }}</p>
      <p>{{ t('usage.requestBody.scope') }}</p>
    </div>
  </details>
  <span v-else class="text-xs text-gray-400" :title="t('usage.requestBody.unavailable')">—</span>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { UpstreamRequestBodyStats } from '@/types'

const props = defineProps<{ stats?: UpstreamRequestBodyStats | null }>()
const { t } = useI18n()
const saved = computed(() => props.stats && props.stats.raw_bytes > 0
  ? ((1 - props.stats.encoded_bytes / props.stats.raw_bytes) * 100).toFixed(1)
  : '0.0')

function bytes(value: number): string {
  if (value < 1024) return `${value} B`
  if (value < 1024 * 1024) return `${(value / 1024).toFixed(1)} KiB`
  return `${(value / (1024 * 1024)).toFixed(2)} MiB`
}
</script>
