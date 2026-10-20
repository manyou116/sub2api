<template>
  <section class="space-y-4 rounded-lg border border-sky-200 p-4 dark:border-sky-900" data-testid="web-image-settings">
    <p class="font-medium text-sm">{{ t(`admin.accounts.webImageShadow.${mode === 'work' ? 'workQuota' : 'webQuota'}`) }}</p>
    <p class="text-sm text-gray-600 dark:text-gray-300">{{ t('admin.accounts.webImageShadow.settingsHint') }}</p>
    <div class="grid gap-4 sm:grid-cols-2">
      <label class="input-label">
        {{ t('admin.accounts.webImageShadow.modelMode') }}
        <select v-model="settings.model_mode" class="input mt-1" data-testid="web-image-model-mode">
          <option value="recommended">{{ t('admin.accounts.webImageShadow.recommended') }}</option>
          <option value="fixed">{{ t('admin.accounts.webImageShadow.fixed') }}</option>
        </select>
      </label>
      <label v-if="settings.model_mode === 'fixed'" class="input-label">
        {{ t('admin.accounts.webImageShadow.model') }}
        <select v-model="settings.model" class="input mt-1" required data-testid="web-image-model" @change="settings.thinking_effort = ''">
          <option value="" disabled>{{ t('admin.accounts.webImageShadow.selectModel') }}</option>
          <option v-for="model in models" :key="model.id" :value="model.id">{{ model.id === 'auto' ? t('admin.accounts.webImageShadow.upstreamAuto') : `${model.label} · ${model.id}` }}</option>
        </select>
      </label>
      <label v-if="settings.model_mode === 'fixed' && selectedModel?.thinking_efforts.length" class="input-label">
        {{ t('admin.accounts.webImageShadow.thinking') }}
        <select v-model="settings.thinking_effort" class="input mt-1" data-testid="web-image-thinking">
          <option value="">{{ t('admin.accounts.webImageShadow.modelDefault') }}</option>
          <option v-for="effort in selectedModel.thinking_efforts" :key="effort" :value="effort">{{ effort }}</option>
        </select>
      </label>
    </div>
    <p v-if="selectedModel" class="text-sm text-gray-500">
      {{ selectedModel.id === 'auto' ? t('admin.accounts.webImageShadow.upstreamAuto') : `${selectedModel.label} · ${selectedModel.id}` }}
      <span v-if="!selectedModel.verified" class="text-amber-600"> · {{ t('admin.accounts.webImageShadow.unverified') }}</span>
    </p>
    <p v-if="catalogError" role="alert" class="text-sm text-red-600">{{ catalogError }}</p>
    <p v-if="mode === 'work'" class="text-sm text-gray-700 dark:text-gray-300">{{ t('admin.accounts.webImageShadow.workAttachment') }}</p>
    <label v-else class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
      <input v-model="settings.require_download_attachment" type="checkbox" data-testid="web-image-download" />
      {{ t('admin.accounts.webImageShadow.download') }}
    </label>
    <p class="text-xs text-gray-500">{{ t('admin.accounts.webImageShadow.downloadHint') }}</p>
    <p class="text-xs text-gray-500">{{ t('admin.accounts.webImageShadow.routeHint', { model: mode === 'work' ? 'gpt-image-2-work' : 'gpt-image-2-web' }) }}</p>
    <WebImageQuotaCell v-if="mode === 'web'" :account-id="accountId" />
    <p v-else class="text-sm text-gray-500">{{ t('admin.accounts.webImageShadow.workParent', { id: parentId }) }}</p>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type { ImageShadowMode, ImageShadowModelCatalog } from '@/api/admin/accounts'
import WebImageQuotaCell from './WebImageQuotaCell.vue'
import type { WebImageSettings } from './webImageShadow'

const props = withDefaults(defineProps<{ accountId: number; mode?: ImageShadowMode; parentId?: number | null }>(), { mode: 'web', parentId: null })
const settings = defineModel<WebImageSettings>({ required: true })
const { t } = useI18n()
const catalog = ref<ImageShadowModelCatalog | null>(null)
const catalogError = ref('')
const models = computed(() => catalog.value?.models.filter(model => model.modes.includes(props.mode)) || [])
const selectedModel = computed(() => models.value.find(model => model.id === (settings.value.model_mode === 'fixed' ? settings.value.model : catalog.value?.recommended[props.mode])))
watch(selectedModel, model => {
  if (model && !model.thinking_efforts.includes(settings.value.thinking_effort)) settings.value.thinking_effort = ''
})
onMounted(async () => {
  try {
    catalog.value = await adminAPI.accounts.getImageShadowModels()
  } catch {
    catalogError.value = t('admin.accounts.webImageShadow.modelsUnavailable')
  }
})
</script>
