<template>
  <div class="flex flex-col gap-0.5">
    <!-- 并发槽位 -->
    <CapacityBadge :color-class="concurrencyClass" :current="currentConcurrency" :max="account.concurrency">
      <svg class="h-2.5 w-2.5" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2">
        <path stroke-linecap="round" stroke-linejoin="round" d="M3.75 6A2.25 2.25 0 016 3.75h2.25A2.25 2.25 0 0110.5 6v2.25a2.25 2.25 0 01-2.25 2.25H6a2.25 2.25 0 01-2.25-2.25V6zM3.75 15.75A2.25 2.25 0 016 13.5h2.25a2.25 2.25 0 012.25 2.25V18a2.25 2.25 0 01-2.25 2.25H6A2.25 2.25 0 013.75 18v-2.25zM13.5 6a2.25 2.25 0 012.25-2.25H18A2.25 2.25 0 0120.25 6v2.25A2.25 2.25 0 0118 10.5h-2.25a2.25 2.25 0 01-2.25-2.25V6zM13.5 15.75a2.25 2.25 0 012.25-2.25H18a2.25 2.25 0 012.25 2.25V18A2.25 2.25 0 0118 20.25h-2.25A2.25 2.25 0 0113.5 18v-2.25z" />
      </svg>
    </CapacityBadge>

    <!-- 5h窗口费用限制 -->
    <CapacityBadge v-if="showWindowCost" :color-class="windowCostClass" :tooltip="windowCostTooltip" :current="'$' + formatCost(currentWindowCost)" :max="'$' + formatCost(account.window_cost_limit)">
      <svg class="h-2.5 w-2.5" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2">
        <path stroke-linecap="round" stroke-linejoin="round" d="M12 6v12m-3-2.818l.879.659c1.171.879 3.07.879 4.242 0 1.172-.879 1.172-2.303 0-3.182C13.536 12.219 12.768 12 12 12c-.725 0-1.45-.22-2.003-.659-1.106-.879-1.106-2.303 0-3.182s2.9-.879 4.006 0l.415.33M21 12a9 9 0 11-18 0 9 9 0 0118 0z" />
      </svg>
    </CapacityBadge>

    <!-- 会话数量限制 -->
    <CapacityBadge v-if="showSessionLimit" :color-class="sessionLimitClass" :tooltip="sessionLimitTooltip" :current="activeSessions" :max="account.max_sessions!">
      <svg class="h-2.5 w-2.5" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2">
        <path stroke-linecap="round" stroke-linejoin="round" d="M15 19.128a9.38 9.38 0 002.625.372 9.337 9.337 0 004.121-.952 4.125 4.125 0 00-7.533-2.493M15 19.128v-.003c0-1.113-.285-2.16-.786-3.07M15 19.128v.106A12.318 12.318 0 018.624 21c-2.331 0-4.512-.645-6.374-1.766l-.001-.109a6.375 6.375 0 0111.964-3.07M12 6.375a3.375 3.375 0 11-6.75 0 3.375 3.375 0 016.75 0zm8.25 2.25a2.625 2.625 0 11-5.25 0 2.625 2.625 0 015.25 0z" />
      </svg>
    </CapacityBadge>

    <!-- RPM 限制 -->
    <CapacityBadge v-if="showRpmLimit" :color-class="rpmClass" :tooltip="rpmTooltip" :current="currentRPM" :max="account.base_rpm!" :suffix="rpmStrategyTag">
      <svg class="h-2.5 w-2.5" fill="none" viewBox="0 0 24 24" stroke-width="1.5" stroke="currentColor">
        <path stroke-linecap="round" stroke-linejoin="round" d="M12 6v6h4.5m4.5 0a9 9 0 1 1-18 0 9 9 0 0 1 18 0Z" />
      </svg>
    </CapacityBadge>

    <!-- API Key 账号配额限制 -->
    <QuotaBadge v-if="showDailyQuota" :used="account.quota_daily_used ?? 0" :limit="account.quota_daily_limit!" label="D" />
    <QuotaBadge v-if="showWeeklyQuota" :used="account.quota_weekly_used ?? 0" :limit="account.quota_weekly_limit!" label="W" />
    <QuotaBadge v-if="showTotalQuota" :used="account.quota_used ?? 0" :limit="account.quota_limit!" />

    <!-- ChatGPT Web 生图额度/在途（仅 OpenAI OAuth） -->
    <div v-if="showWebImages" class="inline-flex items-center gap-0.5">
      <CapacityBadge
        :color-class="webImagesClass"
        :tooltip="webImagesTooltip"
        :current="webImagesCurrent"
        :max="webImagesMax"
        :suffix="webImagesSuffix"
      >
        <span class="text-[9px] font-semibold opacity-80">W</span>
      </CapacityBadge>
      <button
        type="button"
        class="inline-flex h-4 w-4 items-center justify-center rounded text-gray-400 transition-colors hover:bg-gray-100 hover:text-cyan-600 disabled:cursor-not-allowed disabled:opacity-50 dark:hover:bg-dark-700 dark:hover:text-cyan-300"
        :disabled="probing"
        :title="t('admin.accounts.webImages.probe')"
        @click.stop="probeWebImages"
      >
        <svg
          class="h-2.5 w-2.5"
          :class="{ 'animate-spin': probing }"
          fill="none"
          stroke="currentColor"
          viewBox="0 0 24 24"
        >
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" />
        </svg>
      </button>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import type { Account } from '@/types'
import type { OpenAIWebImagesStatus } from '@/api/admin/accounts'
import CapacityBadge from '@/components/account/CapacityBadge.vue'
import QuotaBadge from '@/components/account/QuotaBadge.vue'

const props = defineProps<{
  account: Account
  webImagesStatus?: OpenAIWebImagesStatus | null
}>()

const nowTick = ref(Date.now())
let cooldownTimer: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  cooldownTimer = setInterval(() => {
    nowTick.value = Date.now()
  }, 1000)
})
onUnmounted(() => {
  if (cooldownTimer) clearInterval(cooldownTimer)
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

const webCooldownLeftMs = computed(() => {
  void nowTick.value
  const raw = props.webImagesStatus?.cooldown_until
  if (!raw) return 0
  const ts = new Date(raw).getTime()
  if (Number.isNaN(ts)) return 0
  return Math.max(0, ts - Date.now())
})

const webIsRateLimited = computed(() => {
  if (props.webImagesStatus?.rate_limited) return webCooldownLeftMs.value > 0 || props.webImagesStatus.unschedulable_reason === 'cooldown'
  if (props.webImagesStatus?.unschedulable_reason === 'cooldown') return true
  return webCooldownLeftMs.value > 0
})

const emit = defineEmits<{
  (e: 'web-images-probe', account: Account): void
}>()

const probing = ref(false)

const probeWebImages = () => {
  if (probing.value) return
  probing.value = true
  emit('web-images-probe', props.account)
  // parent handles async; release spin shortly for UX
  window.setTimeout(() => {
    probing.value = false
  }, 1200)
}

const { t } = useI18n()

// ====== 并发 ======
const currentConcurrency = computed(() => props.account.current_concurrency || 0)

const concurrencyClass = computed(() => {
  const current = currentConcurrency.value
  const max = props.account.concurrency
  if (current >= max) return 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400'
  if (current > 0) return 'bg-yellow-100 text-yellow-700 dark:bg-yellow-900/30 dark:text-yellow-400'
  return 'bg-gray-100 text-gray-600 dark:bg-gray-800 dark:text-gray-400'
})

// ====== 窗口费用 ======
const isAnthropicOAuthOrSetupToken = computed(() =>
  props.account.platform === 'anthropic' &&
  (props.account.type === 'oauth' || props.account.type === 'setup-token')
)

const showWindowCost = computed(() =>
  isAnthropicOAuthOrSetupToken.value &&
  props.account.window_cost_limit != null &&
  props.account.window_cost_limit > 0
)

const currentWindowCost = computed(() => props.account.current_window_cost ?? 0)

const windowCostClass = computed(() => {
  if (!showWindowCost.value) return ''
  const current = currentWindowCost.value
  const limit = props.account.window_cost_limit || 0
  const reserve = props.account.window_cost_sticky_reserve || 10
  if (current >= limit + reserve) return 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400'
  if (current >= limit) return 'bg-orange-100 text-orange-700 dark:bg-orange-900/30 dark:text-orange-400'
  if (current >= limit * 0.8) return 'bg-yellow-100 text-yellow-700 dark:bg-yellow-900/30 dark:text-yellow-400'
  return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-400'
})

const windowCostTooltip = computed(() => {
  if (!showWindowCost.value) return ''
  const current = currentWindowCost.value
  const limit = props.account.window_cost_limit || 0
  const reserve = props.account.window_cost_sticky_reserve || 10
  if (current >= limit + reserve) return t('admin.accounts.capacity.windowCost.blocked')
  if (current >= limit) return t('admin.accounts.capacity.windowCost.stickyOnly')
  return t('admin.accounts.capacity.windowCost.normal')
})

// ====== 会话限制 ======
const showSessionLimit = computed(() =>
  isAnthropicOAuthOrSetupToken.value &&
  props.account.max_sessions != null &&
  props.account.max_sessions > 0
)

const activeSessions = computed(() => props.account.active_sessions ?? 0)

const sessionLimitClass = computed(() => {
  if (!showSessionLimit.value) return ''
  const current = activeSessions.value
  const max = props.account.max_sessions || 0
  if (current >= max) return 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400'
  if (current >= max * 0.8) return 'bg-yellow-100 text-yellow-700 dark:bg-yellow-900/30 dark:text-yellow-400'
  return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-400'
})

const sessionLimitTooltip = computed(() => {
  if (!showSessionLimit.value) return ''
  const current = activeSessions.value
  const max = props.account.max_sessions || 0
  const idle = props.account.session_idle_timeout_minutes || 5
  if (current >= max) return t('admin.accounts.capacity.sessions.full', { idle })
  return t('admin.accounts.capacity.sessions.normal', { idle })
})

// ====== RPM ======
const showRpmLimit = computed(() =>
  isAnthropicOAuthOrSetupToken.value &&
  props.account.base_rpm != null &&
  props.account.base_rpm > 0
)

const currentRPM = computed(() => props.account.current_rpm ?? 0)
const rpmStrategy = computed(() => props.account.rpm_strategy || 'tiered')
const rpmStrategyTag = computed(() => rpmStrategy.value === 'sticky_exempt' ? '[S]' : '[T]')

const rpmBuffer = computed(() => {
  const base = props.account.base_rpm || 0
  return props.account.rpm_sticky_buffer ?? (base > 0 ? Math.max(1, Math.floor(base / 5)) : 0)
})

const rpmClass = computed(() => {
  if (!showRpmLimit.value) return ''
  const current = currentRPM.value
  const base = props.account.base_rpm ?? 0
  const buffer = rpmBuffer.value
  if (rpmStrategy.value === 'tiered') {
    if (current >= base + buffer) return 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400'
    if (current >= base) return 'bg-orange-100 text-orange-700 dark:bg-orange-900/30 dark:text-orange-400'
  } else {
    if (current >= base) return 'bg-orange-100 text-orange-700 dark:bg-orange-900/30 dark:text-orange-400'
  }
  if (current >= base * 0.8) return 'bg-yellow-100 text-yellow-700 dark:bg-yellow-900/30 dark:text-yellow-400'
  return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-400'
})

const rpmTooltip = computed(() => {
  if (!showRpmLimit.value) return ''
  const current = currentRPM.value
  const base = props.account.base_rpm ?? 0
  const buffer = rpmBuffer.value
  if (rpmStrategy.value === 'tiered') {
    if (current >= base + buffer) return t('admin.accounts.capacity.rpm.tieredBlocked', { buffer })
    if (current >= base) return t('admin.accounts.capacity.rpm.tieredStickyOnly', { buffer })
    if (current >= base * 0.8) return t('admin.accounts.capacity.rpm.tieredWarning')
    return t('admin.accounts.capacity.rpm.tieredNormal')
  } else {
    if (current >= base) return t('admin.accounts.capacity.rpm.stickyExemptOver')
    if (current >= base * 0.8) return t('admin.accounts.capacity.rpm.stickyExemptWarning')
    return t('admin.accounts.capacity.rpm.stickyExemptNormal')
  }
})

// 格式化费用显示
const formatCost = (value: number | null | undefined) => {
  if (value === null || value === undefined) return '0'
  return value.toFixed(2)
}

// ====== 配额 ======
const isQuotaEligible = computed(() => props.account.type === 'apikey' || props.account.type === 'bedrock')

const showDailyQuota = computed(() =>
  isQuotaEligible.value && props.account.quota_daily_limit != null && props.account.quota_daily_limit > 0
)
const showWeeklyQuota = computed(() =>
  isQuotaEligible.value && props.account.quota_weekly_limit != null && props.account.quota_weekly_limit > 0
)
const showTotalQuota = computed(() =>
  isQuotaEligible.value && props.account.quota_limit != null && props.account.quota_limit > 0
)

// ====== ChatGPT Web 生图 ======
const isOpenAIOAuthLike = computed(() =>
  props.account.platform === 'openai' &&
  (props.account.type === 'oauth' || props.account.type === 'setup-token')
)

const webExtra = computed(() => {
  const extra = (props.account.extra || {}) as Record<string, any>
  const cfg = extra.openai_web_images
  return cfg && typeof cfg === 'object' ? cfg : null
})

const webEnabled = computed(() => {
  if (props.webImagesStatus) return Boolean(props.webImagesStatus.enabled)
  return Boolean(webExtra.value?.enabled)
})

const showWebImages = computed(() => isOpenAIOAuthLike.value)

const webRemaining = computed<number | null>(() => {
  if (props.webImagesStatus?.quota_known && props.webImagesStatus.remaining != null) {
    return props.webImagesStatus.remaining
  }
  return null
})

const webInflight = computed(() => props.webImagesStatus?.current_inflight ?? 0)
const webMaxInflight = computed(() => {
  return props.webImagesStatus?.max_inflight
    ?? webExtra.value?.max_inflight
    ?? 1
})

// 展示：在途/最大并发（容量语义），suffix 显示剩余额度 rN
const webImagesCurrent = computed(() => {
  if (!webEnabled.value) return t('admin.accounts.webImages.offShort')
  if (webIsRateLimited.value) {
    if (webCooldownLeftMs.value > 0) return formatCooldown(webCooldownLeftMs.value)
    return t('admin.accounts.webImages.rateLimitedShort')
  }
  return webInflight.value
})

const webImagesMax = computed(() => {
  if (!webEnabled.value) return '-'
  return webMaxInflight.value
})

const webImagesSuffix = computed(() => {
  if (!webEnabled.value) return ''
  if (webIsRateLimited.value) return t('admin.accounts.webImages.rateLimitedShort')
  if (webRemaining.value == null) return 'r?'
  return `r${webRemaining.value}`
})

const webImagesClass = computed(() => {
  if (!webEnabled.value) {
    return 'bg-gray-100 text-gray-500 dark:bg-gray-800 dark:text-gray-400'
  }
  if (webIsRateLimited.value) {
    return 'bg-rose-100 text-rose-800 dark:bg-rose-900/40 dark:text-rose-300'
  }
  if (props.webImagesStatus?.schedulable === false) {
    return 'bg-amber-100 text-amber-800 dark:bg-amber-900/30 dark:text-amber-300'
  }
  if (webRemaining.value != null && webRemaining.value <= 0) {
    return 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400'
  }
  if (webInflight.value >= Number(webMaxInflight.value || 1)) {
    return 'bg-orange-100 text-orange-700 dark:bg-orange-900/30 dark:text-orange-400'
  }
  if (webRemaining.value != null && webRemaining.value <= 2) {
    return 'bg-yellow-100 text-yellow-700 dark:bg-yellow-900/30 dark:text-yellow-400'
  }
  return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-400'
})

const webImagesTooltip = computed(() => {
  if (!showWebImages.value) return ''
  if (!webEnabled.value) return t('admin.accounts.capacity.webImages.disabled')
  const rem = webRemaining.value == null ? t('admin.accounts.webImages.unknown') : String(webRemaining.value)
  const reason = props.webImagesStatus?.unschedulable_reason
  const base = t('admin.accounts.capacity.webImages.tooltip', {
    remaining: rem,
    inflight: webInflight.value,
    max: webMaxInflight.value,
    success: props.webImagesStatus?.stats?.success ?? webExtra.value?.stats?.success ?? 0,
    fail: props.webImagesStatus?.stats?.fail ?? webExtra.value?.stats?.fail ?? 0
  })
  const resolved = props.webImagesStatus?.resolved_model
    ? ` | ${props.webImagesStatus.resolved_model}/${props.webImagesStatus.resolved_thinking_effort || '-'} (${props.webImagesStatus.resolve_source || props.webImagesStatus.model_mode || 'auto'})`
    : ''
  let limitInfo = ''
  if (webIsRateLimited.value) {
    limitInfo = webCooldownLeftMs.value > 0
      ? ` | ${t('admin.accounts.webImages.rateLimitedCountdown', { time: formatCooldown(webCooldownLeftMs.value) })}`
      : ` | ${t('admin.accounts.webImages.rateLimited')}`
  }
  if (reason) return `${base}${resolved}${limitInfo} (${reason})`
  return `${base}${resolved}${limitInfo}`
})

</script>
