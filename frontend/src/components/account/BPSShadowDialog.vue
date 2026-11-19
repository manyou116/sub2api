<template>
  <BaseDialog :show="true" :title="t(childId ? 'admin.accounts.bps.authorize' : 'admin.accounts.bps.create')" @close="close">
    <div class="space-y-4">
      <div class="space-y-4">
        <div role="group" :aria-label="t('admin.accounts.bps.authorize')" class="flex flex-wrap gap-2">
          <button v-for="tab in methods" :key="tab" type="button" class="btn" :class="method === tab ? 'btn-primary' : 'btn-secondary'" :aria-pressed="method === tab" :disabled="busy || pending" @click="select(tab)">
            {{ t('admin.accounts.bps.' + tab) }}
          </button>
        </div>
        <label v-if="method === 'paste' && !pending" class="input-label">
          {{ t('admin.accounts.bps.json') }}
          <textarea v-model="grant" class="input mt-1 font-mono" rows="5" autocomplete="off" autocapitalize="off" spellcheck="false" maxlength="32768" />
        </label>
        <div v-if="pending && state?.prompt" class="flex flex-wrap items-center gap-3 text-sm">
          <a :href="state.prompt.url" target="_blank" rel="noopener noreferrer" class="btn btn-secondary">{{ t('admin.accounts.bps.open') }}</a>
          <code class="font-semibold">{{ state.prompt.code }}</code>
        </div>
        <div v-if="state" role="status" class="text-sm">
          <span>{{ t('admin.accounts.bps.states.' + state.state) }}</span>
          <time v-if="state.expires_at" class="ml-2 text-gray-500" :datetime="state.expires_at">{{ new Date(state.expires_at).toLocaleString() }}</time>
        </div>
      </div>
      <p v-if="error" role="alert" class="text-sm text-red-600">{{ error }}</p>
    </div>
    <template #footer>
      <div class="flex flex-wrap justify-end gap-2">
        <button type="button" class="btn btn-secondary" @click="close">{{ t('common.close') }}</button>
        <button v-if="pending" class="btn btn-secondary" type="button" @click="cancel">{{ t('common.cancel') }}</button>
        <button v-else class="btn btn-primary" type="button" :disabled="busy || (method === 'paste' && !grant.trim())" @click="submit">
          {{ t(childId ? 'admin.accounts.bps.authorize' : 'common.confirm') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { apiClient } from '@/api/client'
import type { Account } from '@/types'
import BaseDialog from '@/components/common/BaseDialog.vue'

const props = defineProps<{ target: Account }>()
const emit = defineEmits<{ close: []; changed: [] }>()
const { t } = useI18n()
type State = { state: string; expires_at?: string; session?: string; prompt?: { url: string; code: string } }
const methods = ['paste', 'device'] as const
const method = ref<'paste' | 'device'>('paste')
const childId = ref(props.target.quota_dimension === 'bps' ? props.target.id : 0)
const grant = ref('')
const busy = ref(false)
const error = ref('')
const state = ref<State>()
const pending = computed(() => state.value?.state === 'pending')
let timer: ReturnType<typeof setTimeout> | undefined
let closed = false
const endpoint = () => '/admin/accounts/' + childId.value + '/bps-authorization'
const headers = (session?: string) => session ? { 'X-BPS-Authorization': session } : {}

function errorMessage(value: unknown) {
  const response = (value as { response?: { data?: { message?: unknown } } } | null)?.response
  const detail = response?.data?.message
  if (typeof detail === 'string' && detail) return detail
  const message = (value as { message?: unknown } | null)?.message
  return typeof message === 'string' && message ? message : t('admin.accounts.bps.requestFailed')
}

function select(tab: 'paste' | 'device') {
  method.value = tab
  if (tab === 'device') grant.value = ''
}

async function refresh(session?: string) {
  try {
    const response = await apiClient.get<State>(endpoint(), { headers: headers(session) })
    if (closed) return
    state.value = response.data
    if (response.data.state === 'pending' && response.data.session) {
      timer = setTimeout(() => { void refresh(response.data.session) }, 1500)
    } else if (session && response.data.state === 'authorized') emit('changed')
  } catch (e: unknown) {
    if (!closed) error.value = errorMessage(e)
  }
}

async function submit() {
  if (busy.value || pending.value) return
  busy.value = true
  error.value = ''
  try {
    let parsed: unknown
    if (method.value === 'paste') {
      try { parsed = JSON.parse(grant.value) } catch { error.value = t('admin.accounts.bps.invalidJSON'); return }
      if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
        error.value = t('admin.accounts.bps.invalidJSON')
        return
      }
    }
    if (!childId.value) {
      const response = await apiClient.post<Account>('/admin/accounts/' + props.target.id + '/bps-shadow', {})
      childId.value = response.data.id
      emit('changed')
      if (closed) return
      state.value = { state: 'authorization_required' }
    }
    const response = await apiClient.post<State>(endpoint(), { method: method.value, grant: parsed })
    grant.value = ''
    if (closed) {
      if (response.data.session) await apiClient.delete(endpoint(), { headers: headers(response.data.session) })
      return
    }
    state.value = response.data
    if (response.data.session) await refresh(response.data.session)
  } catch (e: unknown) {
    if (!closed) error.value = errorMessage(e)
  } finally {
    busy.value = false
  }
}

async function cancel() {
  clearTimeout(timer)
  const session = state.value?.session
  if (session && pending.value) {
    try { await apiClient.delete(endpoint(), { headers: headers(session) }) } catch { if (!closed) error.value = t('admin.accounts.bps.requestFailed') }
  }
  if (!closed) await refresh()
}

function close() {
  closed = true
  grant.value = ''
  void cancel()
  emit('close')
}

onMounted(() => { if (childId.value) void refresh() })
onUnmounted(() => { closed = true; clearTimeout(timer); grant.value = '' })
</script>
