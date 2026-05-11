<template>
  <BaseDialog
    :show="show"
    title="批量导入 Kiro Social RefreshToken"
    width="normal"
    close-on-click-outside
    @close="handleClose"
  >
    <form id="kiro-social-bulk-form" class="space-y-4" @submit.prevent="handleSubmit">
      <div class="text-sm text-gray-600 dark:text-dark-300">
        粘贴 Google / GitHub 登录的 Kiro <code>refresh_token</code>，每行一个。后端会逐条调用 Kiro 刷新接口验证，<b>失败的不会落库</b>。
      </div>

      <div>
        <label class="input-label">Provider（统一应用到这一批）</label>
        <div class="flex gap-3 text-sm">
          <label class="flex cursor-pointer items-center gap-2">
            <input v-model="provider" type="radio" value="Google" />
            <span class="text-gray-700 dark:text-dark-200">Google</span>
          </label>
          <label class="flex cursor-pointer items-center gap-2">
            <input v-model="provider" type="radio" value="Github" />
            <span class="text-gray-700 dark:text-dark-200">GitHub</span>
          </label>
        </div>
      </div>

      <div>
        <label class="input-label">RefreshToken（一行一个）</label>
        <textarea
          v-model="tokensText"
          rows="8"
          class="input w-full font-mono text-xs"
          placeholder="eyJxxxx...
eyJxxxx...
eyJxxxx..."
        />
        <div class="mt-1 text-xs text-gray-500 dark:text-dark-400">
          有效行数：{{ tokenLines.length }}
        </div>
      </div>

      <div
        v-if="result"
        class="space-y-2 rounded-xl border border-gray-200 p-4 dark:border-dark-700"
      >
        <div class="text-sm font-medium text-gray-900 dark:text-white">导入结果</div>
        <div class="text-sm text-gray-700 dark:text-dark-300">
          总计 {{ result.summary.total }}，成功 {{ result.summary.imported }}，失败 {{ result.summary.failed }}
        </div>
        <div v-if="failedItems.length" class="mt-2">
          <div class="text-sm font-medium text-red-600 dark:text-red-400">失败项</div>
          <div
            class="mt-2 max-h-48 overflow-auto rounded-lg bg-gray-50 p-3 font-mono text-xs dark:bg-dark-800"
          >
            <div
              v-for="(item, idx) in failedItems"
              :key="idx"
              class="whitespace-pre-wrap"
            >
              #{{ item.index + 1 }} {{ item.token_preview }} — {{ item.error }}
            </div>
          </div>
        </div>
      </div>
    </form>

    <template #footer>
      <div class="flex justify-end gap-3">
        <button class="btn btn-secondary" type="button" :disabled="importing" @click="handleClose">
          {{ t('common.cancel') }}
        </button>
        <button
          class="btn btn-primary"
          type="submit"
          form="kiro-social-bulk-form"
          :disabled="importing || tokenLines.length === 0"
        >
          {{ importing ? '导入中...' : `导入 ${tokenLines.length} 个` }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { adminAPI } from '@/api/admin'
import type { KiroSocialImportResponse } from '@/api/admin/accounts'
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

const provider = ref<'Google' | 'Github'>('Google')
const tokensText = ref('')
const importing = ref(false)
const result = ref<KiroSocialImportResponse | null>(null)

const tokenLines = computed(() =>
  tokensText.value
    .split(/\r?\n/)
    .map((s) => s.trim())
    .filter((s) => s.length > 0)
)

const failedItems = computed(() => result.value?.results.filter((r) => r.error) || [])

watch(
  () => props.show,
  (open) => {
    if (open) {
      provider.value = 'Google'
      tokensText.value = ''
      result.value = null
      importing.value = false
    }
  }
)

const handleSubmit = async () => {
  if (tokenLines.value.length === 0) {
    appStore.showError('请粘贴至少一个 refresh_token')
    return
  }
  importing.value = true
  try {
    const res = await adminAPI.accounts.importKiroSocialTokens({
      provider: provider.value,
      tokens: tokenLines.value,
    })
    result.value = res
    if (res.summary.failed > 0) {
      appStore.showError(`导入完成：成功 ${res.summary.imported}，失败 ${res.summary.failed}`)
    } else {
      appStore.showSuccess(`成功导入 ${res.summary.imported} 个 Kiro 账号`)
    }
    if (res.summary.imported > 0) emit('imported')
  } catch (error: any) {
    appStore.showError(error?.message || '导入失败')
  } finally {
    importing.value = false
  }
}

const handleClose = () => {
  if (importing.value) return
  emit('close')
}
</script>
