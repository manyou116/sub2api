<template>
  <section class="space-y-4 rounded-lg border border-sky-200 p-4 dark:border-sky-900" data-testid="web-image-settings">
    <p class="text-sm text-gray-600 dark:text-gray-300">{{ t('admin.accounts.webImageShadow.settingsHint') }}</p>
    <div class="grid gap-4 sm:grid-cols-2">
      <label class="input-label">
        {{ t('admin.accounts.webImageShadow.modelMode') }}
        <select v-model="settings.model_mode" class="input mt-1" data-testid="web-image-model-mode">
          <option value="auto">{{ t('admin.accounts.webImageShadow.auto') }}</option>
          <option value="fixed">{{ t('admin.accounts.webImageShadow.fixed') }}</option>
        </select>
      </label>
      <label v-if="settings.model_mode === 'fixed'" class="input-label">
        {{ t('admin.accounts.webImageShadow.model') }}
        <input v-model="settings.model" class="input mt-1" required data-testid="web-image-model" placeholder="gpt-5-6-thinking" />
      </label>
      <label v-if="settings.model_mode === 'fixed'" class="input-label">
        {{ t('admin.accounts.webImageShadow.thinking') }}
        <select v-model="settings.thinking_effort" class="input mt-1" data-testid="web-image-thinking">
          <option value="">{{ t('admin.accounts.webImageShadow.auto') }}</option>
          <option v-for="effort in ['min', 'low', 'medium', 'high', 'extended']" :key="effort" :value="effort">{{ effort }}</option>
        </select>
      </label>
    </div>
    <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
      <input v-model="settings.require_download_attachment" type="checkbox" data-testid="web-image-download" />
      {{ t('admin.accounts.webImageShadow.download') }}
    </label>
    <WebImageQuotaCell :account-id="accountId" />
  </section>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import WebImageQuotaCell from './WebImageQuotaCell.vue'
import type { WebImageSettings } from './webImageShadow'

defineProps<{ accountId: number }>()
const settings = defineModel<WebImageSettings>({ required: true })
const { t } = useI18n()
</script>
