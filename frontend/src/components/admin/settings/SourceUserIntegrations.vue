<template>
  <section class="card" data-testid="source-user-integrations">
    <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
      <h2 class="text-lg font-semibold">{{ t('usage.sourceIntegration.title') }}</h2>
      <p class="mt-1 text-sm text-gray-500">{{ t('usage.sourceIntegration.description') }}</p>
    </div>
    <div class="space-y-4 p-6">
      <p v-if="loading">{{ t('common.loading') }}</p>
      <div v-if="loadFailed" role="alert">
        {{ t('usage.sourceIntegration.loadFailed') }}
        <button type="button" class="btn btn-secondary" @click="load">{{ t('common.tryAgain') }}</button>
      </div>
      <fieldset :disabled="loading || loadFailed || saving" class="space-y-4">
        <div v-for="(source, i) in sources" :key="i" class="grid gap-3 rounded-lg border p-4 sm:grid-cols-3">
          <label class="text-sm">{{ t('usage.sourceIntegration.sourceId') }}
            <input v-model="source.id" class="input mt-1" maxlength="64" pattern="[a-zA-Z0-9_-]{1,64}" required />
          </label>
          <label class="text-sm">{{ t('usage.sourceIntegration.name') }}
            <input v-model="source.name" class="input mt-1" maxlength="128" />
          </label>
          <label class="text-sm">{{ t('usage.sourceIntegration.keys') }}
            <input v-model="source.keyText" class="input mt-1" placeholder="3, 2, 23" />
          </label>
          <button type="button" class="btn btn-secondary btn-sm justify-self-start" @click="sources.splice(i, 1)">{{ t('common.delete') }}</button>
        </div>
        <p v-if="error" role="alert" class="text-sm text-red-600">{{ error }}</p>
        <div class="flex gap-3">
          <button type="button" class="btn btn-secondary" :disabled="sources.length >= 32" @click="sources.push({ id: '', name: '', keyText: '' })">{{ t('common.add') }}</button>
          <button type="button" class="btn btn-primary" @click="save">{{ saving ? t('common.saving') : t('common.save') }}</button>
        </div>
      </fieldset>
    </div>
  </section>
</template>
<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { apiClient } from '@/api/client'
import { useAppStore } from '@/stores/app'

interface Integration { id: string; name: string; api_key_ids: number[] }
const { t } = useI18n()
const app = useAppStore()
const sources = ref<{ id: string; name: string; keyText: string }[]>([])
const loading = ref(true), loadFailed = ref(false), saving = ref(false), error = ref('')
async function load() {
  loading.value = true; loadFailed.value = false
  try {
    const { data } = await apiClient.get<Integration[]>('/admin/settings/source-user-integrations')
    sources.value = (data || []).map(s => ({ id: s.id, name: s.name, keyText: (s.api_key_ids || []).join(', ') }))
  } catch { loadFailed.value = true }
  finally { loading.value = false }
}
async function save() {
  if (loading.value || loadFailed.value || saving.value) return
  error.value = ''
  const ids = new Set<string>(), keys = new Set<number>()
  const data = sources.value.map(s => ({ id: s.id.trim(), name: s.name.trim(), api_key_ids: s.keyText.trim() ? s.keyText.split(/[,，\s]+/).map(Number) : [] }))
  for (const s of data) {
    if (!/^[a-zA-Z0-9_-]{1,64}$/.test(s.id) || ids.has(s.id) || s.api_key_ids.some(k => !Number.isSafeInteger(k) || k <= 0 || keys.has(k))) {
      error.value = t('usage.sourceIntegration.invalid'); return
    }
    ids.add(s.id)
    for (const k of s.api_key_ids) {
      if (keys.has(k)) { error.value = t('usage.sourceIntegration.invalid'); return }
      keys.add(k)
    }
  }
  saving.value = true
  try { await apiClient.put('/admin/settings/source-user-integrations', data); app.showSuccess(t('common.saved')) }
  catch { error.value = t('usage.sourceIntegration.saveFailed') }
  finally { saving.value = false }
}
onMounted(load)
</script>
