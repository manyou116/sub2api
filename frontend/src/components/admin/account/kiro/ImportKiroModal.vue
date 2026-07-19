<script setup lang="ts">
import { ref } from 'vue'
import { importKiro } from '@/api/admin/accounts'

defineProps<{ show: boolean }>()
const emit = defineEmits<{ (e: 'close'): void; (e: 'imported'): void }>()
const raw = ref('')
const loading = ref(false)
const resultText = ref('')
const errorText = ref('')

async function submit() {
  loading.value = true
  resultText.value = ''
  errorText.value = ''
  try {
    const parsed: unknown = JSON.parse(raw.value)
    let items: Record<string, unknown>[]
    if (Array.isArray(parsed)) {
      items = parsed as Record<string, unknown>[]
    } else if (parsed && typeof parsed === 'object' && Array.isArray((parsed as { items?: unknown }).items)) {
      items = (parsed as { items: Record<string, unknown>[] }).items
    } else {
      throw new Error('JSON must be an array or { items: [...] }')
    }
    const res = await importKiro({ items })
    resultText.value = `total=${res.summary.total} ok=${res.summary.succeeded} fail=${res.summary.failed}`
    emit('imported')
  } catch (e: unknown) {
    errorText.value = e instanceof Error ? e.message : 'Import failed'
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div v-if="show" class="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4" @click.self="emit('close')">
    <div class="w-full max-w-2xl rounded-lg bg-white p-5 shadow-xl dark:bg-dark-800">
      <div class="mb-3 flex items-center justify-between">
        <h3 class="text-lg font-semibold text-gray-900 dark:text-white">Import Kiro Accounts</h3>
        <button class="text-gray-500 hover:text-gray-800 dark:hover:text-gray-200" @click="emit('close')">✕</button>
      </div>
      <p class="mb-2 text-sm text-gray-500 dark:text-gray-400">
        Paste kiro-account-manager export JSON (array or { items: [...] }).
      </p>
      <textarea
        v-model="raw"
        rows="14"
        class="w-full rounded border border-gray-300 bg-white p-3 font-mono text-xs dark:border-dark-600 dark:bg-dark-900 dark:text-gray-100"
        placeholder='[{"authMethod":"Social","refreshToken":"...","machineId":"...","accessToken":"...","profileArn":"..."}]'
      />
      <p v-if="errorText" class="mt-2 text-xs text-red-600">{{ errorText }}</p>
      <div class="mt-3 flex items-center justify-between gap-3">
        <span class="text-xs text-gray-500">{{ resultText }}</span>
        <div class="flex gap-2">
          <button class="rounded px-3 py-1.5 text-sm text-gray-600 hover:bg-gray-100 dark:text-gray-300 dark:hover:bg-dark-700" @click="emit('close')">Cancel</button>
          <button
            class="rounded bg-cyan-700 px-3 py-1.5 text-sm text-white hover:bg-cyan-800 disabled:opacity-50"
            :disabled="loading || !raw.trim()"
            @click="submit"
          >
            {{ loading ? 'Importing…' : 'Import' }}
          </button>
        </div>
      </div>
    </div>
  </div>
</template>
