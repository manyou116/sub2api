<template>
  <div v-if="visible" class="min-w-[140px] space-y-1">
    <div class="flex flex-wrap items-center gap-1">
      <span
        class="inline-flex items-center rounded-full px-2 py-0.5 text-[11px] font-medium"
        :class="enabled
          ? 'bg-emerald-100 text-emerald-800 dark:bg-emerald-900/40 dark:text-emerald-200'
          : 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300'"
      >
        {{ enabledLabel }}
      </span>
      <span
        v-if="enabled && isRateLimited"
        class="inline-flex items-center rounded-full px-2 py-0.5 text-[11px] font-medium bg-rose-100 text-rose-800 dark:bg-rose-900/40 dark:text-rose-200"
      >
        {{ reasonLabel }}
      </span>
      <span
        v-else-if="enabled && status?.schedulable === false"
        class="inline-flex items-center rounded-full px-2 py-0.5 text-[11px] font-medium bg-amber-100 text-amber-800 dark:bg-amber-900/40 dark:text-amber-200"
        :title="status?.unschedulable_reason || ''"
      >
        {{ reasonLabel }}
      </span>
    </div>
    <div class="space-y-0.5 text-[11px] leading-4 text-gray-600 dark:text-gray-300">
      <div class="flex items-center justify-between gap-2">
        <span class="text-gray-400">{{ t('admin.accounts.webImages.remaining') }}</span>
        <span class="font-medium text-gray-800 dark:text-gray-100">{{ remainingLabel }}</span>
      </div>
      <div class="flex items-center justify-between gap-2">
        <span class="text-gray-400">{{ t('admin.accounts.webImages.inflight') }}</span>
        <span class="font-medium text-gray-800 dark:text-gray-100">{{ inflightLabel }}</span>
      </div>
      <div class="flex items-center justify-between gap-2">
        <span class="text-gray-400">{{ t('admin.accounts.webImages.success') }}/{{ t('admin.accounts.webImages.fail') }}</span>
        <span class="font-medium">
          <span class="text-emerald-700 dark:text-emerald-300">{{ successCount }}</span>
          <span class="mx-0.5 text-gray-300 dark:text-gray-600">/</span>
          <span class="text-rose-700 dark:text-rose-300">{{ failCount }}</span>
        </span>
      </div>
    </div>
    <div v-if="lastError" class="truncate text-[10px] text-rose-600 dark:text-rose-400" :title="lastError">
      {{ lastError }}
    </div>
  </div>
  <span v-else class="text-sm text-gray-400 dark:text-dark-500">-</span>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import type { OpenAIWebImagesStatus } from '@/api/admin/accounts'
import type { Account } from '@/types'

const props = defineProps<{
  account: Account
  status?: OpenAIWebImagesStatus | null
  loading?: boolean
}>()

const { t } = useI18n()
// Always prefer parent status; never keep a sticky local copy that outlives cleared cooldowns.
const status = computed(() => props.status || null)

const nowTick = ref(Date.now())
let timer: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  timer = setInterval(() => { nowTick.value = Date.now() }, 1000)
})
onUnmounted(() => {
  if (timer) clearInterval(timer)
})

const formatCooldown = (ms: number): string => {
  const totalSec = Math.max(0, Math.ceil(ms / 1000))
  const h = Math.floor(totalSec / 3600)
  const m = Math.floor((totalSec % 3600) / 60)
  const s = totalSec % 60
  if (h > 0) return `${h}h${String(m).padStart(2, '0')}m`
  if (m > 0) return `${m}m${String(s).padStart(2, '0')}s`
  return `${s}s`
}

const cooldownLeftMs = computed(() => {
  void nowTick.value
  const raw = status.value?.cooldown_until
  if (!raw) return 0
  const ts = new Date(raw).getTime()
  if (Number.isNaN(ts)) return 0
  return Math.max(0, ts - Date.now())
})

const isRateLimited = computed(() => {
  if (status.value?.rate_limited && cooldownLeftMs.value > 0) return true
  if (status.value?.unschedulable_reason === 'cooldown' && cooldownLeftMs.value > 0) return true
  return cooldownLeftMs.value > 0
})

const visible = computed(
  () => props.account.platform === 'openai' && (props.account.type === 'oauth' || props.account.type === 'setup-token')
)

const extraCfg = computed(() => {
  const extra = (props.account.extra || {}) as Record<string, any>
  const cfg = extra.openai_web_images
  return cfg && typeof cfg === 'object' ? cfg : null
})

const enabled = computed(() => {
  if (status.value) return Boolean(status.value.enabled)
  return Boolean(extraCfg.value?.enabled === true)
})

const enabledLabel = computed(() => {
  if (!enabled.value) return t('admin.accounts.webImages.off')
  if (status.value?.enabled_source === 'global') {
    return `${t('admin.accounts.webImages.on')}·${t('admin.accounts.webImages.inheritShort')}`
  }
  return t('admin.accounts.webImages.on')
})

const remainingLabel = computed(() => {
  if (isRateLimited.value && cooldownLeftMs.value > 0) {
    return t('admin.accounts.webImages.rateLimitedCountdown', { time: formatCooldown(cooldownLeftMs.value) })
  }
  if (status.value?.quota_known && status.value.remaining != null) return String(status.value.remaining)
  return t('admin.accounts.webImages.unknown')
})

const inflightLabel = computed(() => {
  const cur = status.value?.current_inflight ?? 0
  const max = status.value?.max_inflight ?? extraCfg.value?.max_inflight ?? 1
  return `${cur}/${max}`
})

const successCount = computed(() => status.value?.stats?.success ?? extraCfg.value?.stats?.success ?? 0)
const failCount = computed(() => status.value?.stats?.fail ?? extraCfg.value?.stats?.fail ?? 0)
const lastError = computed(() => {
  if (isRateLimited.value) return t('admin.accounts.webImages.rateLimited')
  const err = status.value?.stats?.last_error || extraCfg.value?.stats?.last_error || ''
  return typeof err === 'string' && err.length > 48 ? `${err.slice(0, 48)}…` : err
})

const reasonLabel = computed(() => {
  if (isRateLimited.value) {
    if (cooldownLeftMs.value > 0) {
      return t('admin.accounts.webImages.rateLimitedCountdown', { time: formatCooldown(cooldownLeftMs.value) })
    }
    return t('admin.accounts.webImages.rateLimited')
  }
  const r = status.value?.unschedulable_reason || ''
  const map: Record<string, string> = {
    disabled: t('admin.accounts.webImages.reason.disabled'),
    quota_unknown: t('admin.accounts.webImages.reason.quotaUnknown'),
    no_quota: t('admin.accounts.webImages.reason.noQuota'),
    inflight_full: t('admin.accounts.webImages.reason.inflightFull'),
    cooldown: t('admin.accounts.webImages.reason.cooldown'),
    account_inactive: t('admin.accounts.webImages.reason.inactive')
  }
  return map[r] || r || t('admin.accounts.webImages.unschedulable')
})
</script>
