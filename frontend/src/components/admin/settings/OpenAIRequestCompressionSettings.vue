<template>
  <section class="card" data-testid="openai-request-compression-settings">
    <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
      <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ t(`${prefix}.title`) }}</h2>
      <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t(`${prefix}.description`) }}</p>
    </div>
    <div class="space-y-5 p-6">
      <p v-if="loading" class="text-sm text-gray-500">{{ t('common.loading') }}</p>
      <div v-else-if="loadFailed" role="alert" class="text-sm text-red-600">
        {{ t(`${prefix}.loadFailed`) }}
        <button type="button" class="btn btn-secondary btn-sm ml-3" @click="load">{{ t('common.tryAgain') }}</button>
      </div>
      <fieldset :disabled="loading || loadFailed || saving" class="space-y-5">
        <div class="flex items-center justify-between gap-4">
          <label for="request-compression-enabled" class="text-sm font-medium">{{ t(`${prefix}.enabled`) }}</label>
          <Toggle id="request-compression-enabled" v-model="enabled" />
        </div>
        <p class="text-xs text-gray-500 dark:text-gray-400">{{ t(`${prefix}.hint`) }}</p>
        <button type="button" data-testid="request-compression-save" class="btn btn-primary" @click="save">
          {{ saving ? t('common.saving') : t('common.save') }}
        </button>
      </fieldset>
    </div>
  </section>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import Toggle from '@/components/common/Toggle.vue'
import { useAppStore } from '@/stores/app'
import { getOpenAIRequestCompression, updateOpenAIRequestCompression } from '@/api/admin/openaiRequestCompression'

const prefix = 'admin.settings.openaiRequestCompression'
const { t } = useI18n()
const app = useAppStore()
const enabled = ref(true)
const loading = ref(true)
const loadFailed = ref(false)
const saving = ref(false)

async function load() {
  loading.value = true
  loadFailed.value = false
  try { enabled.value = (await getOpenAIRequestCompression()).enabled }
  catch { loadFailed.value = true }
  finally { loading.value = false }
}

async function save() {
  if (loading.value || loadFailed.value || saving.value) return
  saving.value = true
  try {
    enabled.value = (await updateOpenAIRequestCompression(enabled.value)).enabled
    app.showSuccess(t(`${prefix}.saved`))
  } catch { app.showError(t(`${prefix}.saveFailed`)) }
  finally { saving.value = false }
}

onMounted(load)
</script>
