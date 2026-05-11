<template>
  <BaseDialog
    :show="show"
    title="Kiro IdC OAuth 登录"
    width="normal"
    :close-on-click-outside="!polling"
    @close="handleClose"
  >
    <div class="space-y-4">
      <div class="text-sm text-gray-600 dark:text-dark-300">
        通过 AWS IAM Identity Center / Builder ID 在浏览器内登录 Kiro 账号，无需手动复制 token。
      </div>

      <!-- 配置（启动前可改） -->
      <div v-if="!session" class="space-y-3">
        <div class="rounded-lg border border-gray-200 p-3 dark:border-dark-700">
          <div class="text-sm font-medium text-gray-900 dark:text-white">登录方式</div>
          <div class="mt-2 flex gap-3 text-sm">
            <label class="flex cursor-pointer items-center gap-2">
              <input v-model="mode" type="radio" value="builderid" />
              <span class="text-gray-700 dark:text-dark-200">AWS Builder ID（默认）</span>
            </label>
            <label class="flex cursor-pointer items-center gap-2">
              <input v-model="mode" type="radio" value="enterprise" />
              <span class="text-gray-700 dark:text-dark-200">企业 IdC（自定义 startUrl）</span>
            </label>
          </div>
        </div>

        <div v-if="mode === 'enterprise'" class="space-y-3">
          <div>
            <label class="input-label">Start URL</label>
            <input
              v-model="startUrl"
              type="text"
              class="input w-full"
              placeholder="https://my-company.awsapps.com/start"
            />
          </div>
          <div>
            <label class="input-label">Region</label>
            <input v-model="region" type="text" class="input w-full" placeholder="us-east-1" />
          </div>
        </div>

        <div>
          <label class="input-label">备注名称（可选）</label>
          <input v-model="label" type="text" class="input w-full" placeholder="留空自动生成" />
        </div>
      </div>

      <!-- 进行中：显示 user code + 链接 + 倒计时 -->
      <div v-if="session" class="space-y-3">
        <div class="rounded-lg border border-blue-200 bg-blue-50 p-4 dark:border-blue-800 dark:bg-blue-900/20">
          <div class="text-xs text-blue-600 dark:text-blue-300">
            1. 在浏览器中打开下面链接 → 2. 确认下面的 user code 一致 → 3. 点同意
          </div>
          <div class="mt-3 flex items-center gap-2">
            <a
              :href="session.verificationUriComplete"
              target="_blank"
              rel="noopener"
              class="flex-1 truncate rounded bg-white px-2 py-1 text-xs text-blue-600 underline dark:bg-dark-800"
            >
              {{ session.verificationUriComplete }}
            </a>
            <button
              type="button"
              class="btn btn-secondary btn-xs shrink-0"
              @click="copyLink"
            >
              复制
            </button>
          </div>
          <div class="mt-3 text-center">
            <div class="text-[11px] text-blue-600 dark:text-blue-300">User Code</div>
            <div class="select-all font-mono text-2xl font-bold tracking-widest text-blue-700 dark:text-blue-200">
              {{ session.userCode }}
            </div>
          </div>
          <div class="mt-3 text-center text-xs text-gray-500 dark:text-dark-400">
            <template v-if="status === 'pending'">
              等待用户授权… 剩余 {{ remainingSeconds }}s
            </template>
            <template v-else-if="status === 'done'">
              ✅ 登录成功，账号 ID #{{ pollResult?.accountId }}{{ pollResult?.email ? ` (${pollResult.email})` : '' }}
            </template>
            <template v-else-if="status === 'error'">
              ❌ {{ pollResult?.error || '登录失败' }}
            </template>
          </div>
        </div>
      </div>
    </div>

    <template #footer>
      <div class="flex justify-end gap-3">
        <button
          v-if="!session"
          class="btn btn-secondary"
          type="button"
          @click="handleClose"
        >
          {{ t('common.cancel') }}
        </button>
        <button
          v-if="!session"
          class="btn btn-primary"
          type="button"
          :disabled="starting"
          @click="handleStart"
        >
          {{ starting ? '启动中…' : '开始登录' }}
        </button>

        <button
          v-if="session && status === 'pending'"
          class="btn btn-secondary"
          type="button"
          @click="handleClose"
        >
          关闭（已在后台进行）
        </button>
        <button
          v-if="session && status !== 'pending'"
          class="btn btn-primary"
          type="button"
          @click="handleFinish"
        >
          {{ status === 'done' ? '完成' : '关闭' }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { adminAPI } from '@/api/admin'
import type { KiroOAuthStartResponse, KiroOAuthPollResponse } from '@/api/admin/accounts'
import { useAppStore } from '@/stores/app'

interface Props {
  show: boolean
}
interface Emits {
  (e: 'close'): void
  (e: 'imported'): void
}
const props = defineProps<Props>()
const emit = defineEmits<Emits>()

const { t } = useI18n()
const appStore = useAppStore()

const mode = ref<'builderid' | 'enterprise'>('builderid')
const startUrl = ref('')
const region = ref('us-east-1')
const label = ref('')

const starting = ref(false)
const session = ref<KiroOAuthStartResponse | null>(null)
const pollResult = ref<KiroOAuthPollResponse | null>(null)
const status = computed(() => pollResult.value?.status ?? 'pending')

const polling = ref(false)
let pollTimer: ReturnType<typeof setInterval> | null = null
const remainingSeconds = ref(0)
let countdownTimer: ReturnType<typeof setInterval> | null = null

const reset = () => {
  mode.value = 'builderid'
  startUrl.value = ''
  region.value = 'us-east-1'
  label.value = ''
  starting.value = false
  session.value = null
  pollResult.value = null
  remainingSeconds.value = 0
  stopPolling()
}

const stopPolling = () => {
  if (pollTimer) {
    clearInterval(pollTimer)
    pollTimer = null
  }
  if (countdownTimer) {
    clearInterval(countdownTimer)
    countdownTimer = null
  }
  polling.value = false
}

watch(
  () => props.show,
  (open) => {
    if (open) reset()
    else stopPolling()
  }
)

onBeforeUnmount(() => stopPolling())

const handleStart = async () => {
  starting.value = true
  try {
    const payload: { startUrl?: string; region?: string; label?: string } = {}
    if (mode.value === 'enterprise') {
      const url = startUrl.value.trim()
      if (!url) {
        appStore.showError('企业 IdC 需要填写 Start URL')
        starting.value = false
        return
      }
      payload.startUrl = url
      if (region.value.trim()) payload.region = region.value.trim()
    }
    if (label.value.trim()) payload.label = label.value.trim()

    const res = await adminAPI.accounts.startKiroOAuth(payload)
    session.value = res
    remainingSeconds.value = res.expiresIn

    // 启动轮询
    polling.value = true
    pollTimer = setInterval(() => void doPoll(), Math.max(res.interval, 3) * 1000)
    countdownTimer = setInterval(() => {
      if (remainingSeconds.value > 0) remainingSeconds.value--
    }, 1000)
    // 立即先打开浏览器
    window.open(res.verificationUriComplete, '_blank', 'noopener')
  } catch (error: any) {
    appStore.showError(error?.message || '启动 OAuth 失败')
  } finally {
    starting.value = false
  }
}

const doPoll = async () => {
  if (!session.value) return
  try {
    const r = await adminAPI.accounts.pollKiroOAuth(session.value.sessionId)
    pollResult.value = r
    if (r.status === 'done') {
      stopPolling()
      appStore.showSuccess(`Kiro 账号登录成功${r.email ? ` (${r.email})` : ''}`)
      emit('imported')
    } else if (r.status === 'error') {
      stopPolling()
      appStore.showError(r.error || 'OAuth 登录失败')
    }
  } catch (error: any) {
    // 偶发网络错误不停止轮询
    console.warn('kiro oauth poll error', error)
  }
}

const copyLink = async () => {
  if (!session.value) return
  try {
    await navigator.clipboard.writeText(session.value.verificationUriComplete)
    appStore.showSuccess('已复制链接')
  } catch {
    appStore.showError('复制失败，请手动选择')
  }
}

const handleClose = () => {
  emit('close')
}

const handleFinish = () => {
  emit('close')
}
</script>
