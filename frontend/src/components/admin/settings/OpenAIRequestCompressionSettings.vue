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
        <div class="flex items-center justify-between gap-4">
          <label for="request-compression-image-guard" class="text-sm font-medium">{{ t(`${prefix}.imageGuardEnabled`) }}</label>
          <Toggle id="request-compression-image-guard" v-model="imageGuardEnabled" />
        </div>
        <div class="flex items-center justify-between gap-4">
          <div>
            <label for="request-compression-image-limit" class="text-sm font-medium">{{ t(`${prefix}.imageGuardMaxMiB`) }}</label>
            <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t(`${prefix}.imageGuardHint`) }}</p>
          </div>
          <div class="flex items-center gap-2">
            <input id="request-compression-image-limit" v-model.number="imageGuardMaxMiB" type="number" min="1" max="64" step="1" class="input h-9 w-24 text-sm" />
            <span class="text-sm text-gray-500">MiB</span>
          </div>
        </div>
        <div class="flex items-center justify-between gap-4">
          <div>
            <label for="request-compression-bare-400-fallback" class="text-sm font-medium">{{ t(`${prefix}.bare400FallbackEnabled`) }}</label>
            <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t(`${prefix}.bare400FallbackHint`) }}</p>
          </div>
          <Toggle id="request-compression-bare-400-fallback" v-model="bare400FallbackEnabled" />
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
const imageGuardEnabled = ref(true)
const imageGuardMaxMiB = ref(8)
const bare400FallbackEnabled = ref(true)
const loading = ref(true)
const loadFailed = ref(false)
const saving = ref(false)

async function load() {
  loading.value = true
  loadFailed.value = false
  try {
    const settings = await getOpenAIRequestCompression()
    enabled.value = settings.enabled
    imageGuardEnabled.value = settings.image_guard_enabled ?? true
    imageGuardMaxMiB.value = Math.round((settings.image_guard_max_bytes ?? (8 * 1024 * 1024)) / (1024 * 1024))
    bare400FallbackEnabled.value = settings.bare_400_fallback_enabled ?? true
  }
  catch { loadFailed.value = true }
  finally { loading.value = false }
}

async function save() {
  if (loading.value || loadFailed.value || saving.value) return
  saving.value = true
  try {
    const maxMiB = Math.min(64, Math.max(1, Math.round(Number(imageGuardMaxMiB.value) || 8)))
    imageGuardMaxMiB.value = maxMiB
    const settings = await updateOpenAIRequestCompression({
      enabled: enabled.value,
      image_guard_enabled: imageGuardEnabled.value,
      image_guard_max_bytes: maxMiB * 1024 * 1024,
      bare_400_fallback_enabled: bare400FallbackEnabled.value,
    })
    enabled.value = settings.enabled
    imageGuardEnabled.value = settings.image_guard_enabled ?? imageGuardEnabled.value
    imageGuardMaxMiB.value = (settings.image_guard_max_bytes ?? (maxMiB * 1024 * 1024)) / (1024 * 1024)
    bare400FallbackEnabled.value = settings.bare_400_fallback_enabled ?? bare400FallbackEnabled.value
    app.showSuccess(t(`${prefix}.saved`))
  } catch { app.showError(t(`${prefix}.saveFailed`)) }
  finally { saving.value = false }
}

onMounted(load)
</script>
