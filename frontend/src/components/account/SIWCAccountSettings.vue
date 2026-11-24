<template>
  <section class="space-y-3 border-t border-gray-200 py-4 dark:border-dark-600">
    <div>
      <h3 class="input-label">{{ title }}</h3>
      <p class="input-hint" aria-live="polite">{{ statusText }}</p>
    </div>
    <div class="flex flex-wrap gap-2">
      <button type="button" class="btn btn-secondary" :disabled="busy" @click="begin">
        {{ copy.authorize }}
      </button>
      <button v-if="status?.bound" type="button" class="btn btn-secondary" :disabled="busy" @click="remove">
        {{ locale?.startsWith('zh') ? '解除本地绑定' : 'Remove local authorization' }}
      </button>
      <button v-if="status?.bound || status?.enabled" type="button" class="btn btn-secondary" :disabled="busy" @click="select">
        {{ status.enabled ? copy.useCodex : enableText }}
      </button>
    </div>
    <div v-if="flow" class="space-y-2">
      <a :href="flow.auth_url" target="_blank" rel="noopener noreferrer" class="text-sm text-primary-600 underline">{{ copy.open }}</a>
      <label class="input-label">
        {{ copy.callback }}
        <input v-model.trim="callback" type="text" autocomplete="off" spellcheck="false" class="input mt-1" />
      </label>
      <button type="button" class="btn btn-primary" :disabled="busy || !callback" @click="complete">{{ copy.bind }}</button>
    </div>
    <div v-if="isExcel && status?.bound" class="space-y-2">
      <label class="input-label">
        {{ locale?.startsWith('zh') ? 'Excel / BPS 模型（每行一个，留空使用全部）' : 'Excel / BPS models (one per line, empty for all)' }}
        <textarea v-model="modelList" rows="2" class="input mt-1" />
      </label>
      <button type="button" class="btn btn-secondary" :disabled="busy" @click="saveModels">
        {{ locale?.startsWith('zh') ? '保存模型范围' : 'Save model scope' }}
      </button>
    </div>
    <label v-if="isExcel && status?.bound" class="flex items-center gap-2 text-sm">
      <input type="checkbox" :checked="status.search_enabled" :disabled="busy" @change="toggleSearch" />
      {{ locale?.startsWith('zh') ? '允许 Excel / BPS 使用网页搜索' : 'Enable web search for Excel / BPS' }}
    </label>
    <p v-if="error" role="alert" class="text-sm text-red-600">{{ error }}</p>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import apiClient from '@/api/client'

interface Status {
  bound: boolean
  enabled: boolean
  search_enabled?: boolean
  models?: string[]
}
interface Flow {
  auth_url: string
  session_id: string
}

const props = defineProps<{ accountId: number; kind?: "siwc" | "excel" }>()
const { locale } = useI18n()
const copy = computed(() => locale?.value?.startsWith('zh') ? {
  title: 'ChatGPT 登录授权（SIWC）', authorize: '创建浏览器授权', open: '打开 OpenAI 授权页面',
  callback: '粘贴浏览器地址栏中的完整回调 URL', bind: '完成授权',
  useCodex: '切换到 Codex', useSIWC: '启用 SIWC', failed: '授权操作失败，请重试。',
} : {
  title: 'Sign in with ChatGPT (SIWC)', authorize: 'Start browser authorization', open: 'Open OpenAI authorization',
  callback: 'Paste the full callback URL from the browser address bar', bind: 'Finish authorization',
  useCodex: 'Switch to Codex', useSIWC: 'Enable SIWC',
  failed: 'Authorization action failed. Please try again.',
})

const kind = computed(() => props.kind || 'siwc')
const isExcel = computed(() => kind.value === 'excel')
const title = computed(() => isExcel.value ? 'Excel / Basispoints' : copy.value.title)
const enableText = computed(() => isExcel.value ? (locale?.value?.startsWith('zh') ? '启用 Excel / BPS' : 'Enable Excel / BPS') : copy.value.useSIWC)
const path = computed(() => `/admin/openai/accounts/${props.accountId}/${kind.value}`)
const status = ref<Status>()
const flow = ref<Flow>()
const callback = ref('')
const modelList = ref('')
const error = ref('')
const busy = ref(false)
const statusText = computed(() => {
  if (locale?.value?.startsWith('zh')) return status.value?.enabled ? '当前启用' : status.value?.bound ? '已绑定，未启用' : '等待授权'
  return status.value?.enabled ? 'Active' : status.value?.bound ? 'Authorized' : 'Authorization required'
})

let requestVersion = 0
async function run<T>(action: () => Promise<T>, apply: (value: T) => void) {
  const request = ++requestVersion
  busy.value = true
  error.value = ''
  try {
    const result = await action()
    if (request === requestVersion) apply(result)
  } catch {
    if (request === requestVersion) error.value = copy.value.failed
  } finally {
    if (request === requestVersion) busy.value = false
  }
}

watch(path, () => {
  status.value = undefined
  flow.value = undefined
  callback.value = ''
  void run(async () => (await apiClient.get<Status>(path.value)).data, applyStatus)
}, { immediate: true })

function begin() {
  return run(async () => (await apiClient.post<Flow>(`${path.value}/authorize`)).data, (result) => {
    flow.value = result
    callback.value = ''
  })
}
function complete() {
  if (!flow.value) return
  const body = { session_id: flow.value.session_id, callback_url: callback.value }
  return run(async () => (await apiClient.post<Status>(`${path.value}/callback`, body, { timeout: 50_000 })).data, (result) => {
    applyStatus(result)
    flow.value = undefined
    callback.value = ''
  })
}
onMounted(() => window.addEventListener("openai-connection-selection", reload))
onBeforeUnmount(() => window.removeEventListener("openai-connection-selection", reload))
function reload() {
  return run(async () => (await apiClient.get<Status>(path.value)).data, applyStatus)
}
function applyStatus(result: Status) {
  status.value = result
  modelList.value = (result.models ?? []).join('\n')
}
function saveModels() {
  const models = modelList.value.split(/\r?\n/).map((model) => model.trim()).filter(Boolean)
  return run(async () => (await apiClient.put<Status>(path.value, { models })).data, applyStatus)
}
function remove() {
  const message = locale?.value?.startsWith('zh') ? '移除本地凭据？重新启用此连接需要再次授权。' : 'Remove the local grant? This connection will require a new authorization.'
  if (!window.confirm(message)) return
  return run(async () => (await apiClient.delete<Status>(path.value)).data, applyStatus)
}
function toggleSearch(event: Event) {
  const checked = (event.target as HTMLInputElement).checked
  return run(async () => (await apiClient.put<Status>(path.value, { search_enabled: checked })).data, (result) => {
    applyStatus(result)
  })
}
function select() {
  return run(async () => (await apiClient.put<Status>(path.value, { enabled: !status.value?.enabled })).data, (result) => {
    applyStatus(result)
    window.dispatchEvent(new Event("openai-connection-selection"))
  })
}
</script>
