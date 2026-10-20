<template>
  <div class="text-xs" data-testid="web-image-quota">
    <div class="flex items-center justify-between gap-3">
      <span>{{ t('admin.accounts.webImageShadow.remaining') }}: <strong>{{ status?.quota_known && status.remaining != null ? status.remaining : t('admin.accounts.webImageShadow.unknown') }}</strong></span>
      <button type="button" class="btn btn-secondary btn-sm" :disabled="loading" @click="loadStatus(true)">{{ t('admin.accounts.webImageShadow.probe') }}</button>
    </div>
    <p v-if="status" class="mt-2 text-gray-500">{{ t('admin.accounts.webImageShadow.parent', { id: status.parent_account_id }) }}</p>
    <p v-if="status?.reset_at" class="mt-1 text-gray-500">{{ t('admin.accounts.webImageShadow.resetAt') }}: {{ formatDate(status.reset_at) }}</p>
    <p v-if="status?.cooldown_until" class="mt-1 text-amber-600">{{ t('admin.accounts.webImageShadow.cooldownUntil') }}: {{ formatDate(status.cooldown_until) }}</p>
    <p v-if="status?.probed_at" class="mt-1 text-gray-500">{{ t('admin.accounts.webImageShadow.probedAt') }}: {{ formatDate(status.probed_at) }}</p>
    <p v-if="error" role="alert" class="mt-2 text-red-600">{{ error }}</p>
  </div>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type { WebImageStatus } from '@/api/admin/accounts'

const props = defineProps<{ accountId: number; refreshToken?: number }>()
const { t } = useI18n()
const status = ref<WebImageStatus | null>(null)
const loading = ref(false)
const error = ref('')
let requestID = 0
const formatDate = (value: string) => new Date(value).toLocaleString()

async function loadStatus(probe = false) {
  const id = ++requestID
  loading.value = true
  error.value = ''
  try {
    const result = await (probe ? adminAPI.accounts.probeWebImageStatus(props.accountId) : adminAPI.accounts.getWebImageStatus(props.accountId))
    if (id === requestID) status.value = result
  } catch (cause: any) {
    if (id === requestID) error.value = cause?.response?.data?.message || cause?.message || t('admin.accounts.webImageShadow.probeFailed')
  } finally {
    if (id === requestID) loading.value = false
  }
}

watch(() => [props.accountId, props.refreshToken], () => { status.value = null; void loadStatus() }, { immediate: true })
</script>
